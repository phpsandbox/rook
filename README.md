# Rook

Rook is the PHPSandbox server agent.

It is intentionally small and stable. The agent keeps an outbound WebSocket connection to PHPSandbox Edge, receives deployment commands for its registered server, builds and runs Docker containers locally, streams command output back, and proxies public deployment traffic to the matching local container.

Most product policy should remain in Core, Okra, and Edge. Rook should stay focused on durable server-side primitives that can run for a long time with minimal updates.

## Development

```bash
go test ./...
go run ./cmd/rook --config ./rook.yaml
```

Example config:

```yaml
server_id: "srv_..."
token: "..."
control_plane: "wss://okra-edge.ciroue.test/_okra/agent/connect"
state_dir: ".rook/state"
```

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/phpsandbox/rook/main/scripts/install.sh | sudo bash -s -- \
  --server-id srv_... \
  --token ... \
  --control-plane wss://okra-edge.ciroue.com/_okra/agent/connect
```

The installer creates a `rook` system user, installs the binary to `/usr/local/bin/rook`, writes `/etc/rook/rook.yaml`, and starts a `rook.service` systemd unit.

## Release

Tags named `v*` build Linux `amd64` and `arm64` binaries and attach them to a GitHub release.
