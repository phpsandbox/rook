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
