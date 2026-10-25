# Host execution contract

Rook is an authenticated host executor and traffic relay. It advertises the `host-execution.v1` capability. Deployment configuration and lifecycle decisions are supplied by the control plane.

## Independent recovery fence

Agents advertising `host-recovery-fence.v1` refuse new host mutations while a
`recovery-fence` file exists in their state directory. Status inspection, file
reads, binding reads, and traffic forwarding remain available. The fence is
checked on each request and survives agent restart. It does not cancel effects
already running; stop the agent and inspect outstanding host operations before
performing offline recovery. Rejected requests do not acquire operation IDs.

An operator with direct host access can create this file before restoring a
retained application release. Keep the fence until interrupted operations and
control-plane state have been reconciled. Removing the fence re-enables host
mutations. This mechanism does not restore a database, recreate missing images
or credentials, or provide ingress while the agent or routing service is down.

The bundled `scripts/recover-release.py` utility inspects local bindings and
retained release snapshots. Its restore operation requires Python 3 on Linux,
Docker, systemd, a stopped Rook service, and a Rook binary advertising the fence
capability. It needs direct host access rather than a platform login. Store and
verify that access before an outage. Keep a verified copy of the utility and
binary outside the application being recovered.

```sh
sudo python3 recover-release.py --state-dir /var/lib/rook/state \
  --deployment DEPLOYMENT_ID inspect

sudo systemctl stop rook
sudo python3 recover-release.py --state-dir /var/lib/rook/state \
  --deployment DEPLOYMENT_ID restore --release RETAINED_RELEASE_ID \
  --expect-current CURRENT_RELEASE_ID --rook-service rook
```

Inspect every reported running or interrupted operation and its actual host
effects before acknowledging its exact ID with `--acknowledge-operation`. The
utility refuses a stale current-release identity, checks that the retained
containers and images exist, restarts those containers without rebuilding or
pulling, verifies readiness on the actual loopback port, and atomically saves
the binding. It then gracefully stops the replaced processes. A failed readiness
check preserves the existing binding and stops processes started by the attempt.
The database and mounted data are never restored or migrated by this operation.

The utility leaves Rook stopped and the host-wide mutation fence in place.
Verify the application locally before starting Rook. Reconnection may require
the authentication service to be available. After reconnecting, inspect routing
and reconcile platform release state and uncertain operations before explicitly
removing `recovery-fence`. Other deployments on the host remain fenced until
then. Do not run host-wide outage exercises on a shared production server.

This path requires retained containers, their image layers, their private
configuration, and persistent data to remain intact. Host loss needs a separate
tested backup of those prerequisites; a release snapshot alone is insufficient.

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
