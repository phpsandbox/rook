# Rook

Rook is the PHPSandbox server agent.

It is intentionally small and stable. The agent connects a registered server to PHPSandbox, executes host instructions, and routes deployment traffic to them.

Rook stays focused on durable server-side primitives that can run for a long time with minimal updates.

The host execution contract are documented in
[Host execution](docs/host-execution.md).

## Development

```bash
go test ./...
go run ./cmd/rook --config ./rook.yaml
```

Example config:

```yaml
server_id: "srv_..."
token: "..."
control_plane: "https://rook.phpsandbox.test/connect"
state_dir: ".rook/state"
```

## Install

Rook supports Linux on AMD64 and ARM64. The host must use systemd and have a
running Docker daemon, the Docker Compose plugin with `up --wait`,
`--wait-timeout`, and `down --remove-orphans`, plus Git and curl.
Installation checks these prerequisites. The agent connection itself does not
depend on Docker availability.

> [!WARNING]
> Rook belongs to the Docker group and executes deployment instructions received
> from PHPSandbox. Docker access is effectively root access. Install Rook only on
> a dedicated host that you trust PHPSandbox to manage.

```bash
curl -fsSL https://install.phpsandbox.io/rook | sudo bash -s -- \
  --server-id srv_... \
  --token ... \
  --control-plane https://rook.phpsandbox.io/connect
```

Rook converts HTTP and HTTPS control-plane URLs to their WebSocket equivalents when connecting.

The installer creates a `rook` system user, installs the binary to `/usr/local/bin/rook`, writes `/etc/rook/rook.yaml`, and starts a `rook.service` systemd unit.

The server token is a credential. Avoid saving the install command in shell
history or CI logs. To remove Rook while retaining its configuration and state:

```bash
curl -fsSL https://install.phpsandbox.io/rook | sudo bash -s -- --uninstall
```

Add `--purge` to also remove the stored token, deployment state, and system user.

## Automatic updates

Automatic updates are disabled by default. Add `--auto-update` to the install
command to start the daemon with that flag. Release signature verification is
built into Rook; no additional tool or GitHub login is required.

The installer provides a daily systemd update check with a randomized delay.
It makes no release requests when the daemon runs without `--auto-update`.
Only newer stable patch releases within the installed minor version are eligible:
for example, `v0.2.1` can update to `v0.2.2`, but not `v0.3.0`. Development
builds do not update automatically.

Before replacement, Rook verifies the checksum and signed build attestation
against this repository's release workflow and the exact release tag. Sigstore
trust metadata is refreshed over HTTPS and verified using its built-in TUF trust
anchor. Updates wait until host commands finish, then allow up to 30 seconds
for relays to drain.
The restart briefly disconnects tunnels; deployed applications keep running.
Configuration and deployment state are preserved.

The previous binary is retained and restored if the new agent fails to reconnect
both control and data channels. Failed versions are skipped on subsequent
checks. Inspect update attempts with `journalctl -u rook-update.service` or run
a check immediately with `sudo systemctl start rook-update.service`.

To stop automatic updates, remove `--auto-update` from `rook.service`'s
`ExecStart`, run `sudo systemctl daemon-reload`, and restart `rook.service`.
Rerunning the installer without the flag also disables them.

## Release

Tags named `v*` run the test suite, build Linux `amd64` and `arm64` binaries,
scan source and binaries for known vulnerabilities, generate mandatory SHA-256
checksums and signed build attestations, and publish both architectures together
in a GitHub release.
