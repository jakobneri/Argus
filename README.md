# argus

SSH-based server-management TUI for Linux (target platform: Raspberry Pi 5 /
ARM64, generic Linux otherwise). Privilege-separated architecture:

| Binary      | Role                                                                  |
|-------------|-----------------------------------------------------------------------|
| `argusd`    | Privileged daemon. Owns all state, executes all (later privileged) actions, serves gRPC on a Unix domain socket. |
| `argus`     | Unprivileged terminal bridge. gRPC + rendering only — no logic.       |
| `argus-mcp` | MCP bridge. Empty stub until M7.                                      |

## Status: M1 — read-only inventory + headless TUI

`argusd` listens on `/run/argus/argusd.sock` and serves three RPCs: `Ping`,
`GetInventory` (snapshot of systemd services + Docker containers) and
`Attach`, a bidirectional stream that hosts one ephemeral headless Bubble Tea
dashboard per connection. `argus` puts the terminal into raw mode, attaches,
forwards stdin bytes and resizes upstream and writes rendered frames to
stdout; `q` quits. Everything is read-only — no mutations until M4. Metrics,
logs, persistent sessions, detach/reattach, panes etc. come in later
milestones.

## Building

```sh
make build   # all three binaries -> ./bin (host platform)
make cross   # statically linked linux/arm64 build -> ./bin/linux-arm64
make test    # unit tests (incl. bufconn gRPC test)
make lint    # gofumpt + golangci-lint
make proto   # regenerate gRPC code from proto/ via buf
make tools   # one-time: install buf, protoc-gen-go, protoc-gen-go-grpc, gofumpt
```

`CGO_ENABLED=0` is enforced by the Makefile; every dependency is pure Go, so
`make cross` produces a static ARM64 binary.

## Running locally (no root)

The socket path defaults to `/run/argus/argusd.sock` and can be overridden
with `--socket` or the `ARGUS_SOCKET` environment variable:

```sh
# terminal 1
./bin/argusd --socket /tmp/argusd.sock

# terminal 2
./bin/argus --socket /tmp/argusd.sock   # attaches to the live dashboard; q quits
```

## Deploying

`deploy/install.sh` (run as root) creates the `argus` system group, sets up
`/run/argus` (root:argus, 0750), installs the binaries and the systemd unit
`deploy/argusd.service`. Clients must be members of the `argus` group to reach
the socket.

## Assumptions / decisions

- **Codegen via buf** (pure Go, reproducible): `buf.yaml` + `buf.gen.yaml` at
  the repo root, output into `proto/argusv1`. No protoc required.
- **One integration surface at module level**: the in-daemon TUI calls the
  module interfaces (`services.Manager`, `containers.Manager`) directly
  in-process — not via self-gRPC. The `GetInventory` RPC wraps the same
  interfaces and is the external API surface (MCP/automation later).
- The daemon renders the TUI headless onto the Attach stream (no TTY):
  resizes are injected as `tea.WindowSizeMsg` from client resize events, and
  the lipgloss color profile is pinned to ANSI 256 since terminal
  capabilities cannot be autodetected.
- An unreachable Docker daemon is non-fatal: the service list still renders
  and a hint is shown; `GetInventory` sets `container_error`.
- Socket permissions: directory 0750 root:argus, socket 0660 — the daemon is
  reachable only by root and the `argus` group.
