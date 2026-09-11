# Rook

Rook is the PHPSandbox server agent.

It is intentionally small and stable. The agent connects a registered server to PHPSandbox, runs deployments in local Docker containers, and routes deployment traffic to them.

Rook stays focused on durable server-side primitives that can run for a long time with minimal updates.

## Development

```bash
go test ./...
go run ./cmd/rook --config ./rook.yaml
```

Example config:

```yaml
server_id: "srv_..."
token: "..."
control_plane: "https://rook.ciroue.test/connect"
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
  --control-plane https://rook.ciroue.com/connect
```

Rook converts HTTP and HTTPS control-plane URLs to their WebSocket equivalents when connecting.

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
