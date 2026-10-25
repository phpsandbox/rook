# Host execution contract

Rook is an authenticated host executor and traffic relay. It advertises the `host-execution.v1` capability. Deployment configuration and lifecycle decisions are supplied by the control plane.

## Operations

A `host` command carries `payload.id`, `payload.action`, and the fields used by that action:

- `exec`: argv, working directory, environment, stdin, and an optional timeout in seconds. Shell execution requires explicit shell argv. Commands run in a separate Unix process group; cancellation stops the group. A one-second output wait bounds inherited-pipe waiting. Failed commands also stop remaining descendants.
- `write`, `read`, `remove`, `archive`: private atomic file writes, byte reads, recursive removal, and tar.gz extraction. Archive entries cannot escape the destination. `executable` selects private executable permissions for a written file.
- `binding.read`, `binding.set`, `binding.remove`: read and persist a named port binding with opaque caller metadata. Switching a binding does not stop either process.
- `info`: report the host state directory.

Fields have matching JSON and MessagePack names. File bytes use JSON base64 or native MessagePack bytes. Credentials are supplied in command or file values; Rook does not generate them or resolve secret references.

## Operation outcomes

Rook records `running` before starting an effect, then `completed` or `failed` with output, data, and errors. Output is streamed and retained in a bounded private result. The journal stores a request digest rather than the request itself. Command output and read results may contain sensitive data and must be handled accordingly.

Repeating an ID with the identical request returns its recorded outcome, including `running`. Reusing an ID for a different request fails. The journal lock protects registration only; independent effects can execute concurrently. Request timeouts begin before registration. `host.status` reads an outcome without executing the operation.

A lost connection does not cancel an executing process. After a host-agent restart, unfinished operations become `interrupted`. The caller must inspect and reconcile uncertain effects before retrying with a new ID. These outcomes provide duplicate suppression and durable observation, not transactional host execution.

## Traffic bindings

A binding contains a local port and optional opaque metadata. The relay forwards HTTP and WebSocket traffic to that port. The caller owns process startup, readiness checks, binding changes, and retirement.

## Validation

Run `go test -race ./...` and `go vet ./...`. Tests cover concurrent operations, duplicate suppression, cancellation, descendant termination, inherited output, private files, archive containment, restart outcomes, and HTTP/WebSocket forwarding.

## Local release commands

`rook releases` inspects and restores retained container releases without loading
agent credentials, connecting to the control plane, or starting the daemon.
Commands return JSON. They require the existing state directory and deployment ID:

```sh
sudo rook releases list --state-dir /var/lib/rook/state --deployment app-123
sudo rook releases inspect release-a --state-dir /var/lib/rook/state --deployment app-123
sudo rook releases restore release-a \
  --state-dir /var/lib/rook/state --deployment app-123 \
  --expect-current release-b --rook-service rook.service
```

List and inspect are read-only and report the current release, retained releases,
recovery fence and uncertain host operations. Restore requires the named systemd
service to be stopped and takes an exclusive local restore lock. It writes a
persistent `recovery-fence` before container effects. Every reported running or
interrupted operation must first be inspected and explicitly acknowledged with a
repeated `--acknowledge-operation ID` argument. Acknowledgement does not cancel,
replay or mark an operation complete; verify its actual effects first.

Restore starts only the exact retained container IDs, using their existing image
layers, commands, environment and mounts. It does not build, pull images, create
containers, run migrations or restore database contents. All target images and
containers must still exist. It discovers the actual loopback web port and
requires HTTP 200 without following redirects, then checks every retained role
is running and not restarting. Only then does it persist a recovery verification
and switch the binding. A failed start/readiness check leaves the binding intact
and attempts bounded cleanup of only the containers it restarted. Cancellation
also permits bounded cleanup; Docker effects may require inspection afterward.

After switching the binding, restore stops the preceding release's containers
using its recorded shutdown timeout. If this fails, the error reports that the
restored binding is already saved. Persistent services, mounted data and unrelated
bindings are preserved. The restored application must remain compatible with the
existing database and configuration.

The service stays stopped and the recovery fence remains on success and failure.
Verify the application locally, restart Rook when its authentication service is
available, reconcile the verified release and uncertain operations with the
control plane, then remove the fence. Public traffic still requires an available
relay. These commands replace the former Python recovery utility; Python is no
longer required for release inspection or restoration.
