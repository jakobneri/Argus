# argus

SSH-based server-management TUI for Linux (target platform: Raspberry Pi 5 /
ARM64, generic Linux otherwise). Privilege-separated architecture:

| Binary      | Role                                                                  |
|-------------|-----------------------------------------------------------------------|
| `argusd`    | Privileged daemon. Owns all state, executes all (later privileged) actions, serves gRPC on a Unix domain socket. |
| `argus`     | Unprivileged terminal bridge. gRPC + rendering only — no logic.       |
| `argus-mcp` | MCP bridge. Empty stub in M0.                                         |

## Status: M0 — scaffold & tooling

`argusd` listens on `/run/argus/argusd.sock` and answers a `Ping` RPC.
`argus` connects, pings, and renders the Argus splash. Everything else
(sessions, services, containers, metrics, logs, audit, MCP) comes in later
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
./bin/argus --socket /tmp/argusd.sock   # ping -> pong -> splash
```

## Deploying

`deploy/install.sh` (run as root) creates the `argus` system group, sets up
`/run/argus` (root:argus, 0750), installs the binaries and the systemd unit
`deploy/argusd.service`. Clients must be members of the `argus` group to reach
the socket.

## Assumptions / decisions

- **Codegen via buf** (pure Go, reproducible): `buf.yaml` + `buf.gen.yaml` at
  the repo root, output into `proto/argusv1`. No protoc required.
- The spec mandates `rpc Ping(Ping) returns (Pong)`; inside a proto service
  block the bare name `Ping` would resolve to the method, so the request type
  is referenced fully qualified (`.argus.v1.Ping`). The corresponding buf lint
  naming rules are disabled in `buf.yaml`.
- Socket permissions: directory 0750 root:argus, socket 0660 — the daemon is
  reachable only by root and the `argus` group.
