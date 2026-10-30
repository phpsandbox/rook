# Rook

Rook is the PHPSandbox server agent.

It is intentionally small and stable. The agent connects a registered server to PHPSandbox, executes host instructions, and routes deployment traffic to them.

Rook stays focused on durable server-side primitives that can run for a long time with minimal updates.

The host execution contract and local `rook releases` commands are documented in
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

## Managed updates

The standard installer enables owner-requested updates and automatic patch
updates by default. No update flags are required. Add `--manual-updates` to
require owner approval while retaining updates through the authenticated control
plane. Requesting an individual update does not change the automatic-update policy.

Rook persists the request before acknowledging it, reports progress through
heartbeats, and retains the outcome across restarts. Requests are idempotent;
a second update cannot replace an active request. Targets must be newer stable
versions with binaries, checksums, and signed provenance for this architecture.
Managed updates stay within the installed minor release line. Changes of release
line require a manual installation upgrade. Arbitrary download URLs, commands,
and downgrades are rejected.

The installer provides a root-owned systemd helper. It checks for requests
approximately once a minute; the daemon does not receive permission to run
arbitrary privileged commands. The helper independently verifies releases and
uses the same drain, replacement, and rollback path as automatic updates.

Run the installer to upgrade an older installation to this behavior. It preserves
existing configuration and deployment state. Pass `--manual-updates` when
installing or reinstalling to keep automatic updates disabled.

## Automatic updates

Automatic patch updates are enabled by default in Rook. The installer starts the
service without update flags. Pass `--manual-updates` to either the installer or
the daemon to require owner approval while retaining owner-requested updates.
Use the systemd installer to provision the privileged helper and private runtime
socket required by both update modes. Release signature verification is built
into Rook; no additional tool or GitHub login is required.

The helper checks for automatic updates at most once every 24 hours.
In manual mode, release requests are made only for an explicit owner update
request.
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

To stop automatic updates, rerun the installer with `--manual-updates`.
Alternatively, add `--manual-updates` to `rook.service`'s `ExecStart`, run
`sudo systemctl daemon-reload`, and restart `rook.service`. A later installer run
without `--manual-updates` enables automatic patch updates again.

## Release

The release workflow can also be dispatched against an existing version tag.
Dispatches against branches do not build or publish release assets.

Tags named `v*` run the test suite, build Linux `amd64` and `arm64` binaries,
scan source and binaries for known vulnerabilities, generate mandatory SHA-256
checksums and signed build attestations, and publish both architectures together
in a GitHub release.
