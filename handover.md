# Handover — argus M1: Inventory (read-only) + headless TUI über Attach-Stream

Stand: 2026-06-12 · Branch `claude/pensive-pascal-qg1bif` · Status: **M1 fertig, alle Akzeptanzkriterien erfüllt** (M0-Handover ersetzt)

## Was gebaut wurde

Beim Start von `argus` erscheint jetzt ein Live-Dashboard mit echten
systemd-Services und Docker-Containern. Das TUI läuft headless **im Daemon**
und wird über einen bidirektionalen gRPC-Attach-Stream an die Bridge geliefert.
Alles read-only — keine Mutationen bis M4.

- **Proto-Migration**: `Ping/Pong` → `PingRequest`/`PingResponse`; die in M0
  deaktivierten buf-Naming-Regeln (`RPC_REQUEST_STANDARD_NAME` etc.) sind
  wieder aktiv. Verbleibende Ausnahmen in `buf.yaml`: nur noch
  `PACKAGE_DIRECTORY_MATCH` und `SERVICE_SUFFIX` (Layout/Servicename per Spec).
- **`GetInventory`-RPC**: liefert Services + Container in einem Snapshot;
  `container_error` trägt den Hinweis, wenn Docker nicht erreichbar ist
  (Container-Liste dann leer, RPC trotzdem erfolgreich).
- **`Attach`-RPC** (bidirektionaler Stream): Client→Daemon ein `oneof` aus
  rohen stdin-Bytes und Resize-Events (cols/rows); Daemon→Client gerenderte
  ANSI-Frames. Pro Attach-Call genau **eine ephemere Session** — kein
  Detach/Reattach, keine Persistenz, keine Panes (M3).
- **ServiceManager** (`internal/services`): Interface `Manager` + Impl
  `SystemdManager` via `coreos/go-systemd/v22/dbus` (pure Go). Filtert auf
  `*.service`, sortiert nach Name; pro `List`-Call frische dbus-Verbindung.
- **ContainerManager** (`internal/containers`): Interface `Manager` + Impl
  `DockerManager` via offizielles Docker-SDK (`client.FromEnv`, also
  `DOCKER_HOST` / `/var/run/docker.sock`). `All: true`, führender `/` der
  Namen wird gestrippt. Nicht erreichbarer Daemon → gewrappter Fehler, von
  den Callern als nicht-fatal behandelt.
- **Session** (`internal/session`): fährt ein headless Bubble-Tea-Programm.
  Input über `io.Pipe` → `tea.WithInput`; Output direkt auf den Stream via
  `tea.WithOutput`; Resize per `prog.Send(tea.WindowSizeMsg{...})`, weil der
  Output ein Stream und kein TTY ist (SIGWINCH-Autodetect greift nicht).
  `tea.WithAltScreen()` aktiv. `Close()` ist idempotent (Kill + Pipe zu).
- **Dashboard** (`tui/dashboard.go`): zwei `bubbles/table`-Tabellen
  (Services ⅔, Container ⅓ der Höhe), Statusfarben (active/running grün,
  failed/exited rot, Rest gelb), Refresh alle 3 s via `tea.Tick`,
  Fetch-Timeout 5 s, `q`/`ctrl+c`/`esc` beendet, `tab` wechselt Fokus
  (Scrollen mit ↑/↓). Docker-Hinweis ersetzt die Container-Tabelle.
- **Bridge** (`cmd/argus`): Raw-Mode via `x/term` (Restore auf jedem
  Exit-Pfad + defensives `ESC[?1049l ESC[?25h` falls der Stream mitten in
  der Session stirbt), initiale Größe + SIGWINCH-Resizes und stdin-Bytes
  upstream, Frames nach stdout. Sends laufen durch einen Mutex-`sender`,
  weil gRPC kein konkurrierendes `Send` erlaubt.
- **`argus-mcp`**: unverändert leerer Stub (M7).

## Architektur-Entscheidung (verbindlich, auch im README)

Das in-daemon TUI ruft die Modul-Interfaces (`services.Manager`,
`containers.Manager`) **direkt in-process** auf — kein Self-gRPC. Die
`GetInventory`-RPC wrappt dieselben Interfaces als externe API-Fläche
(MCP/Hermes später). „Eine Integrationsfläche" gilt auf Ebene der
Modul-Interfaces. Gemeinsamer Code: `Server.inventory()` in
`internal/api/server.go`, genutzt vom RPC-Handler und als `tui.Fetch` der
Dashboard-Session.

## Repo-Struktur (Änderungen ggü. M0)

```
  internal/
    api/server.go         Ping-, GetInventory-, Attach-Handler + inventory()
    api/server_test.go    bufconn-Tests: Ping + GetInventory (gemockte Manager)
    services/             Manager-Interface + SystemdManager + Tests
    containers/           Manager-Interface + DockerManager + Tests
    session/session.go    ephemere headless Bubble-Tea-Session
    daemon/daemon.go      Wiring: Manager-Konstruktion, tui.ForceColors()
  tui/
    banner.go             Splash (bleibt) + ForceColors (ANSI-256-Pin)
    dashboard.go          Dashboard-Model (Tabellen, Tick-Refresh, Farben)
  cmd/argus/main.go       Raw-Mode-Bridge über den Attach-Stream
  proto/argus.proto       Ping/GetInventory/Attach (Quelle der Wahrheit)
```

## Make-Targets (unverändert), Lokaltest

```sh
./bin/argusd --socket /tmp/argusd.sock     # Terminal 1
./bin/argus  --socket /tmp/argusd.sock     # Terminal 2 → Dashboard, q beendet
```

`CGO_ENABLED=0` global; alle neuen Deps pure Go: go-systemd/v22 (godbus),
docker/docker v28 (SDK), bubbletea v1.3, bubbles v1.0, lipgloss.

## Entscheidungen & Stolpersteine (wichtig für M2/M3)

1. **Go auf 1.25 angehoben** (go.mod; das Docker-SDK zog den Toolchain-Bump).
   Die in M0 gepinnten buf/protoc-gen-Versionen funktionieren weiter; mit
   Go 1.25 könnten sie jetzt angehoben werden.
2. **Headless-Farben**: termenv kann am Stream keine Terminal-Caps erkennen →
   `tui.ForceColors()` pinnt das lipgloss-Profil auf ANSI 256 (Aufruf in
   `daemon.Run`). M1-Annahme; M3 (virtuelles Terminal) kann das verfeinern.
3. **`prog.Send` vor `Run` ist safe**: bubbletea v1.3 initialisiert msgs-Channel
   und Context schon in `NewProgram` — Resize-Events, die vor dem Programmstart
   eintreffen, blockieren nur kurz und gehen nicht verloren.
4. **Attach-Lifecycle**: Recv-Loop läuft als Goroutine und killt die Session
   bei Stream-Fehler; `tea.ErrProgramKilled` wird im Handler als normales
   Disconnect-Ende behandelt (kein RPC-Fehler). Endet das Programm via „q",
   schließt der Handler den Stream und die Bridge bekommt `io.EOF` → Exit 0.
5. **Bridge-Sends serialisieren**: stdin-Goroutine und SIGWINCH-Goroutine
   senden beide auf den Stream → Mutex-Wrapper `sender` in `cmd/argus`.
6. **systemd-Fehler vs. Docker-Fehler**: systemd nicht erreichbar → RPC-Fehler
   bzw. rote Fehlerzeile im Dashboard (Dashboard läuft weiter und retryt beim
   nächsten Tick). Docker nicht erreichbar → nicht-fatal, Service-Liste
   erscheint trotzdem + ⚠-Hinweis. Bewusste Asymmetrie: ohne systemd ist auf
   der Zielplattform etwas grundlegend kaputt.
7. **docker `Summary.State` ist `string`** (v28-SDK, nicht `ContainerState`) —
   unconvert meckert sonst.
8. **ctrl+c in Raw-Mode** erreicht die Bridge als Byte 0x03 und wird ans TUI
   weitergeleitet (das darauf quittet); SIGINT/SIGTERM von außen canceln den
   Stream-Context und räumen sauber auf.

## Verifikation (alles auf diesem Stand ausgeführt)

- `make proto` ✅ — buf lint mit aktivierten Standard-Naming-Regeln
- `make build` / `make cross` ✅ — `file` → „ARM aarch64 … statically linked"
- `make test` ✅ — table-driven Tests für SystemdManager- und DockerManager-
  Parsing gegen gemockte Backends (Filter, Sortierung, Slash-Strip,
  Fehler-Wrapping) + bufconn-Tests für Ping und GetInventory (inkl.
  Docker-down-mit-Hint und systemd-Fehler-als-RPC-Fehler)
- `make lint` ✅ — 0 Issues, gofumpt clean
- E2E (Pseudo-TTY via `script`) ✅ — Attach rendert das Dashboard
  (Alt-Screen, Tabellen-Header, Statusbar), „q" → Exit 0 + Terminal
  restauriert; `kill -9` der Bridge mitten in der Session → Daemon läuft
  weiter und akzeptiert weitere Attaches (drei Sessions nacheinander getestet)
- ⚠ Sandbox hat kein systemd/Docker — der Live-Datenpfad zeigte hier korrekt
  den Fehlerpfad; echte Daten bitte einmal auf dem Pi gegenchecken

## Nächste Schritte (nicht begonnen)

- M2: Metriken + Logs.
- M3: persistente/benannte Sessions, Detach/Reattach, Multi-Attach,
  virtuelles Terminal mit Delta-Diffing, Panes — `internal/session` ist der
  Ansatzpunkt, der Attach-Stream bleibt als Transport.
- M4: start/stop/restart (erste Mutationen, dann Audit-Log).
- Offen/optional: CI-Workflow, Release-ldflags, Version aus Git-Tag.
