# Handover — argus M0: Scaffold & Tooling

Stand: 2026-06-11 · Branch `claude/dreamy-pasteur-6mq2va` · Status: **M0 fertig, alle Akzeptanzkriterien erfüllt**

## Was gebaut wurde

Lauffähiges Skelett mit drei Binaries und Privilegien-Trennung:

- **`argusd`** (Daemon): lauscht per gRPC auf einem Unix Domain Socket
  (Default `/run/argus/argusd.sock`), beantwortet den `Ping`-RPC, strukturiertes
  `log/slog`-Logging, sauberes Shutdown bei SIGINT/SIGTERM (stale Socket wird
  beim Start entfernt, beim Stop aufgeräumt, Socket-Mode 0660 für die Gruppe `argus`).
- **`argus`** (Terminal-Bridge, unprivilegiert): verbindet sich über den Socket,
  Ping→Pong, rendert den Argus-Splash. Bei Terminalbreite < 44 Spalten kompakte
  Marke `◉ argus · v0.1.0`. Keine Logik außer gRPC + Rendering.
- **`argus-mcp`**: leerer Entrypoint-Stub.

## Repo-Struktur

```
argus/
  cmd/argusd/           Daemon-Entrypoint (Flags, Signale, slog)
  cmd/argus/            Bridge-Entrypoint (Dial, Ping, Splash)
  cmd/argus-mcp/        leerer Stub
  internal/
    daemon/daemon.go    Wiring: Socket-Lifecycle + gRPC-Server
    api/server.go       gRPC-Serverimpl (Ping-Handler)
    api/server_test.go  bufconn-Test für Ping
    session/ services/ containers/ ports/ metrics/ logs/ store/ audit/
                        leer (.gitkeep), für spätere Milestones reserviert
  tui/banner.go         Splash (lipgloss) + Breiten-Fallback
  proto/argus.proto     Service-Definition (Quelle der Wahrheit)
  proto/argusv1/        generierter Code (eingecheckt)
  deploy/argusd.service systemd-Unit (RuntimeDirectory=argus, Hardening)
  deploy/install.sh     Gruppe argus, /run/argus (root:argus 0750), Unit-Install
  buf.yaml, buf.gen.yaml, Makefile, .golangci.yml, .gitignore
```

## Make-Targets

| Target | Zweck |
|---|---|
| `make build` | alle drei Binaries → `./bin` (Host-Plattform) |
| `make cross` | statisch gelinktes linux/arm64 → `./bin/linux-arm64` (verifiziert: `file` → "ARM aarch64 … statically linked") |
| `make test`  | Unit-Tests inkl. bufconn-gRPC-Test |
| `make lint`  | gofumpt-Check + golangci-lint (v2-Config) |
| `make proto` | `buf lint` + `buf generate` |
| `make tools` | einmalig: buf v1.47.2, protoc-gen-go, protoc-gen-go-grpc v1.5.1, gofumpt |

`CGO_ENABLED=0` ist im Makefile global exportiert; alle Dependencies sind pure Go
(grpc, protobuf, lipgloss, x/term).

## Lokaltest (ohne root)

```sh
./bin/argusd --socket /tmp/argusd.sock     # Terminal 1
./bin/argus  --socket /tmp/argusd.sock     # Terminal 2 → Splash
```

Socket-Pfad per `--socket`-Flag oder `ARGUS_SOCKET`-Env überschreibbar.

## Entscheidungen & Stolpersteine (wichtig für M1)

1. **Codegen via buf** (pure Go, kein protoc nötig). Config: `buf.yaml` +
   `buf.gen.yaml` im Root, Output via `module=`-Option nach `proto/argusv1`.
2. **Proto-Naming**: Die Spec verlangt `message Ping{}` + `rpc Ping`. Innerhalb
   des `service`-Blocks löst der nackte Name `Ping` zur Methode auf — der
   Request-Typ ist deshalb voll qualifiziert referenziert:
   `rpc Ping(.argus.v1.Ping) returns (Pong)`. Die zugehörigen buf-Lint-Regeln
   (RPC_REQUEST_STANDARD_NAME u.a.) sind in `buf.yaml` mit Begründung deaktiviert.
   **Empfehlung für M1**: bei der nächsten Proto-Erweiterung auf
   `PingRequest`/`PingResponse`-Konvention migrieren, solange es noch keine
   externen Consumer gibt.
3. **Tool-Versionen gepinnt**, weil `@latest` mit Go 1.24 bricht:
   protoc-gen-go-grpc **v1.5.1** (v1.6.x braucht Go ≥ 1.25),
   buf **v1.47.2**. Bei Go-Upgrade auf 1.25 kann beides angehoben werden.
4. **golangci-lint v2-Configformat** (`version: "2"` in `.golangci.yml`);
   aktiv u.a. errorlint, wrapcheck (alle Fehler werden gewrappt), revive.
   Generierter Code (`proto/argusv1`) ist von Lint und Format ausgenommen.
5. **Socket-Rechte**: Verzeichnis `/run/argus` root:argus 0750 (systemd
   `RuntimeDirectory=` legt es bei jedem Start neu an), Socket 0660 — Clients
   brauchen Mitgliedschaft in der Gruppe `argus` (`usermod -aG argus <user>`).
6. **Terminalbreite** für den Splash-Fallback kommt aus `golang.org/x/term`
   (pure Go); wenn stdout kein Terminal ist, wird der volle Splash gerendert.

## Verifikation (alles auf diesem Stand ausgeführt)

- `make build` ✅ — drei Binaries
- `make cross` ✅ — `file bin/linux-arm64/argusd` → „ELF 64-bit … ARM aarch64 … statically linked“
- `make test` ✅ — inkl. `TestPing` gegen bufconn-In-Memory-Server
- `make lint` ✅ — 0 Issues, gofumpt clean
- E2E ✅ — argusd auf `/tmp`-Socket gestartet, `argus` → Ping→Pong→Splash,
  SIGTERM → graceful stop, Socket-Datei entfernt

## Nächste Schritte (M1+, nicht begonnen)

- Attach-Stream/Session: headless Bubble-Tea-Programm im Daemon, Key/Resize-Forwarding.
- Befüllen von `internal/{session,services,containers,ports,metrics,logs,store,audit}`.
- MCP-Logik in `cmd/argus-mcp`.
- Optional: CI-Workflow (build/cross/lint/test), `-ldflags "-s -w"` für Release-Builds,
  Version aus Git-Tag statt Konstante in `tui/banner.go`.
