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

Rook supports Linux on AMD64 and ARM64. The host must use systemd and have a
running Docker daemon plus either `curl` or `wget`.

> [!WARNING]
> Rook belongs to the Docker group and executes deployment instructions received
> from PHPSandbox. Docker access is effectively root access. Install Rook only on
> a dedicated host that you trust PHPSandbox to manage.

```bash
curl -fsSL https://install.phpsandbox.io/agent | sudo bash -s -- \
  --server-id srv_... \
  --token ... \
  --control-plane wss://okra-edge.ciroue.com/_okra/agent/connect
```

The installer creates a `rook` system user, installs the binary to `/usr/local/bin/rook`, writes `/etc/rook/rook.yaml`, and starts a `rook.service` systemd unit.

The server token is a credential. Avoid saving the install command in shell
history or CI logs. To remove Rook while retaining its configuration and state:

```bash
curl -fsSL https://install.phpsandbox.io/agent | sudo bash -s -- --uninstall
```

Add `--purge` to also remove the stored token, deployment state, and system user.

## Release

Tags named `v*` run the test suite, build Linux `amd64` and `arm64` binaries,
generate mandatory SHA-256 checksums, and attach them to a GitHub release.
