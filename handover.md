# Handover — argus M2: Live-Metriken + streamende Logs

Stand: 2026-06-15 · Branch `claude/charming-maxwell-8vywr5` · Status: **M2 fertig,
alle Akzeptanzkriterien außer dem Pi-E2E erfüllt** (M1-Handover ersetzt)

## Doku-Hinweis (wichtig)
Die im Task referenzierten `ARCHITECTURE.md`, `CONVENTIONS.md`, `ROADMAP.md`
existieren im Repo **nicht**. Quelle der Wahrheit sind weiterhin `README.md`
(Architektur-Entscheidungen) und diese `handover.md`. Die geforderte Auflösung
der journald-CGO-Frage ist daher in `README.md` unter „Assumptions / decisions"
dokumentiert (statt in einer nicht vorhandenen `ARCHITECTURE.md`).

## Was gebaut wurde

Das Dashboard zeigt jetzt eine **Host-Metrik-Zeile** (CPU/RAM/Disk/Net) und hat
einen zweiten Tab **Logs** (scrollende, filterbare Liste). Zwei neue
Server-Streaming-RPCs liefern dieselben Daten als externe API-Fläche. Alles
read-only — keine Mutationen bis M4.

- **Proto** (`proto/argus.proto`): zwei neue RPCs
  `StreamMetrics(StreamMetricsRequest) returns (stream StreamMetricsResponse)`
  und `StreamLogs(StreamLogsRequest) returns (stream StreamLogsResponse)`.
  - **Naming-Hinweis (verbindlich):** Die in M1 reaktivierten buf-Regeln
    `RPC_REQUEST_STANDARD_NAME`/`RPC_RESPONSE_STANDARD_NAME` verlangen
    `<Rpc>Request`/`<Rpc>Response`. Die im Task genannten Domänentypen
    `MetricsSnapshot` und `LogEntry` bleiben erhalten, indem die Responses sie
    dünn umhüllen (`StreamMetricsResponse{ MetricsSnapshot snapshot }`,
    `StreamLogsResponse{ LogEntry entry }`). So bleibt `buf lint` grün **ohne**
    die M1-Regeln wieder aufzuweichen. Messages: `MetricsSnapshot`,
    `HostMetrics`, `ContainerMetrics`, `LogEntry`, Enum `LogSource`
    (UNSPECIFIED=ALL, SYSTEMD, DOCKER).
- **MetricsCollector** (`internal/metrics/`): Interface `Collector` + Impl.
  Host via gopsutil (`cpu`/`mem`/`disk`/`net`, pure Go); pro Container CPU% +
  Mem via Docker Stats-API. Backends (`hostSource`, `containerSource`) sind
  Interfaces → in Tests gemockt. CPU%-Formel (`docker stats`-Formel) und
  Mem-Working-Set (Cache-Abzug, cgroup v1/v2) sind reine, getestete Funktionen.
- **LogStreamer** (`internal/logs/`): Interface `Manager` + Impl, die journald-
  und Docker-Quelle in einen Kanal mergt und über `emit` ausliefert (ein
  Consumer-Goroutine → emit nie nebenläufig, gRPC-Send safe).
  - `journald.go`: `journalctl --output=json --follow --no-pager` via
    `exec.CommandContext` (Kill bei ctx.Done), stderr wird geloggt nicht fatal.
    Parser `parseJournalLine` (String- **und** Byte-Array-MESSAGE,
    `__REALTIME_TIMESTAMP`→`time`, PRIORITY→Name, Unit/Syslog-Fallback).
  - `docker.go`: `ContainerLogs(Follow)` je laufendem Container; TTY-Erkennung
    via `ContainerInspect`; 8-Byte-Multiplex-Demux mit Teilzeilen-Puffer;
    RFC3339Nano-Timestamp-Parse; Level = stdout/stderr.
- **API-Server** (`internal/api/`): `StreamMetrics`- + `StreamLogs`-Handler;
  Mapping in `convert.go` (Snapshot/Entry → Proto **und** → TUI-View-Typen). Die
  In-Process-Adapter `metricsFetch`/`logStream` füttern das in-daemon-TUI
  (gleiche Module wie die RPCs — kein Self-gRPC).
- **Daemon** (`internal/daemon/daemon.go`): `metrics.NewCollector()` +
  `logs.NewManager(log)` injiziert.
- **TUI**: `tui/metrics.go` (View-Typen + Metrik-Bar + Byte-Formatter),
  `tui/logs.go` (viewport + textinput, Generations-getaggter Stream, `/` Filter,
  `1/2/3` Quelle), `tui/dashboard.go` ist jetzt das Tab-Root-Model.
  Navigation: **`tab`** wechselt Dashboard↔Logs, **`shift+tab`** togglet den
  Pane-Fokus (Services/Container) im Dashboard, `q`/`ctrl+c`/`esc` beenden
  (im Filter-Eingabemodus tippt `q` in das Feld; nur `ctrl+c` beendet hart).

## Architektur-Entscheidungen (verbindlich)

1. **journald exec-basiert, kein CGO.** Auflösung der M1-offenen Frage:
   `journalctl -o json --follow` + JSON-Parser. `CGO_ENABLED=0` bleibt,
   `make cross` (ARM64) bleibt statisch. Prozess-Lifecycle an Stream-Context.
2. **Self-contained Collect statt geteiltem Vorgänger-State.** Der Task schlug
   „previousCPU aus letztem Snapshot merken" vor. Stattdessen ist jeder
   `Collect` in sich abgeschlossen: Host-CPU wird über ein kurzes Fenster
   gesampelt (`cpu.Percent(window)`), Container-CPU aus **zwei** aufeinander
   folgenden Docker-Stats-Frames (Frame 2 trägt gültige `PreCPUStats`)
   berechnet. Grund: derselbe Collector wird vom In-Process-TUI **und** der
   `StreamMetrics`-RPC (ggf. mehrere Clients) genutzt — geteilter Delta-State
   würde nebenläufig korrumpieren. So ist eine einzige Instanz sicher.
3. **Metriken im TUI per Poll-Tick (3 s), Logs per Kanal-Subscription.** Konsistent
   mit M1-Inventory: das TUI ruft die Module in-process; die Streaming-RPCs sind
   nur die externe Fläche.

## Stolpersteine / Hinweise

- **gopsutil** zieht `tklauser/*`, `power-devops/perfstat`, `yusufpapurcu/wmi`
  (letztere zwei nur AIX/Windows, indirekt) + `golang.org/x/sys` — alle pure
  Go; ARM64-Cross verifiziert.
- **Docker-Stats** sind nicht-blockierend: pro Container ein Stream mit 2 Frames,
  nebenläufig (Semaphore=8). Einzelne fehlschlagende Container werden
  übersprungen, nicht der ganze Snapshot.
- **Goroutine-Lifecycle Logs:** `manager.Stream` cancelt einen abgeleiteten
  `streamCtx` per `defer`, alle Quellen beenden sich (journalctl-Kill,
  Docker-Reader-Close). Race-Tests (`go test -race`) grün.
- **TUI-Logs Generations-Tag:** Filter-/Quellwechsel cancelt den alten Stream;
  späte `logEntryMsg` aus dem alten Stream werden per `gen` verworfen.
- **Zeitstempel im Proto:** `int64` Unix-Nanos; Zero-Time → 0 (kein negativer
  Müllwert für externe Clients). Im In-Process-Pfad wird `time.Time` direkt
  durchgereicht.

## Verifikation (auf diesem Stand ausgeführt)

- `make proto` ✅ — buf lint grün mit aktiven M1-Naming-Regeln (nur
  PACKAGE_DIRECTORY_MATCH + SERVICE_SUFFIX bleiben Ausnahme).
- `make build` / `make cross` ✅ — ARM64 statisch.
- `make lint` ✅ — 0 Issues, gofumpt clean.
- `make test` ✅ + `go test -race ./internal/{logs,api,metrics}` ✅.
  - metrics: Collect-Mapping, CPU%-Formel, Mem-Cache-Abzug, Host-Fehler fatal,
    Docker-Fehler nicht-fatal.
  - logs: journalctl-JSON-Parser gegen festen Output, journalArgs (unit/since/
    priority), Docker-Multiplex-Demux inkl. zerteilter Zeile, Manager-Merge/
    Quellauswahl/ctx-Cancel/emit-Fehler.
  - api: bufconn StreamMetrics (echte Felder, Cancel beendet sauber, Collect-
    Fehler = RPC-Fehler) und StreamLogs (alle Entries, Filter wirkt).
- **Echtdaten-Smoke (Sandbox):** argusd gestartet, `StreamMetrics` lieferte echte
  Host-Werte (CPU 0,7 %→1,7 % über zwei Snapshots, RAM/Disk/Net real, Net-Zähler
  stieg). `StreamLogs` ohne Docker → Quelle nicht-fatal übersprungen; journalctl
  ist vorhanden und folgt (Stream bleibt offen, bis Client-Deadline → sauberer
  Server-Abbau).
- ⚠ **Pi-E2E offen:** Die Sandbox hat kein Docker und keinen befüllten Journal-
  Zugriff. Bitte auf dem Pi gegenchecken, dass die Metrik-Zeile echte Werte
  zeigt und der Logs-Tab journald- + Docker-Zeilen mit Filter rendert.

## Nächste Schritte (nicht begonnen)

- M3: persistente/benannte Sessions, Detach/Reattach, Multi-Attach, virtuelles
  Terminal, Panes (dritter Tab kommt hier).
- M4: start/stop/restart (erste Mutationen, dann Audit-Log in `internal/audit`).
- M5: Port-Manager (`internal/ports`). M6+: Metrik-Alerts/Schwellwerte.
  M7: Hermes/MCP (`cmd/argus-mcp` ist noch Stub).
