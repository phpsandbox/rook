# Host execution contract

This is a pre-launch draft, not compatibility with a previously deployed customer agent. Rook advertises `host-execution.v1`. Okra requires that capability before dispatch; the edge checks it again when forwarding each command.

## Boundary

Rook is an authenticated remote host executor and traffic relay. It knows neither publishing strategies nor resource engines. Okra owns Git checkout commands, Docker/Compose configuration and invocation, application environment merging, health policy, release sequencing, resource ownership, retention, and cleanup. Docker and Git are ordinary executable commands supplied by Okra.

The `internal/host.Executor` accepts one request, stores its operation outcome, and returns a result. The agent package handles the connection and forwards traffic through persisted bindings. There is no deployment workflow, health loop, or resource planner in Rook.

## Operations

A `host` command carries `payload.id`, `payload.action`, and the fields used by that action:

- `exec`: argv, working directory, environment, stdin, optional timeout in seconds. Output is streamed and retained in a bounded private result. Rook starts a process directly; shell execution requires an explicitly supplied shell argv.
- `write`, `read`, `remove`, `archive`: private atomic file writes, byte reads, recursive removal, and tar.gz extraction. Archive paths cannot escape their destination. `executable` selects private executable permissions for a written file.
- `binding.read`, `binding.set`, `binding.remove`: persist and inspect a named port binding with opaque caller metadata. Rook does not interpret release or resource fields in that metadata. The proxy follows the binding's host port. Switching the binding does not stop either process; Okra decides when to retire a previous release.
- `info`: report the host state directory so Okra can resolve paths.

Fields have matching JSON and MessagePack names. File bytes use JSON base64 or native MessagePack bytes. `host.status` reads the persisted result for a payload ID without executing its operation.

Core encrypts resource-owned credentials. Okra generates and retrieves them, resolves references, and supplies values in commands and private Compose files. Rook has no secret store or reference resolver. Unpublishing retains credentials; Core removes them only after successful resource cleanup.

## Operation outcomes

Rook saves `running` before starting an effect, then `completed` or `failed` with output, data, and errors. Repeating an ID with the identical request returns its saved outcome; using it for a different request fails. The private journal stores a request digest, not plaintext command credentials. A reconnect does not cancel an executing process. Okra uses stable IDs for source checkout, build, and container start within a release. It inspects the original ID when a response stream is lost or expires, including while the effect is still running.

A host-process restart marks unfinished operations `interrupted`. Rook never automatically replays uncertain effects. Okra must inspect the host and reconcile them before issuing another operation. This is durable observation and duplicate suppression, not a promise of transactional or exactly-once host effects.

## Publishing policy in Okra

The current SSH provider checks Docker, Compose, Git, and curl before building. It prepares source/build files, builds, applies resources, starts a candidate, and probes it using centrally supplied timing and accepted statuses. It switches the local traffic binding only after health succeeds, then retires the predecessor. Failed candidates are removed and the previous binding remains.

Resource cleanup ownership is an Okra document stored through private host files. Omitted records remain owned; explicit release IDs execute and remove records. Allocation cleanup precedes service/network removal. Compose down does not implicitly delete volumes. Unpublish keeps resources unless deletion was requested; deletion preserves instructions on failure for retry. Shared allocation cleanup does not own the shared service.

The installer checks the publishing host prerequisites. The generic agent can connect independently of Docker availability; Okra's publishing preflight is authoritative for the chosen strategy.

## Validation and evolution

Rook tests cover private writes of supplied values, operation replay/restart outcomes, archive containment, and traffic bindings. Okra's separate-consumer tests launch a built Rook binary over control/data WebSockets and send JSON/MessagePack host requests; Docker tests cover release replacement, unpublish/cleanup, resource omission/release, and shared MySQL allocations.

Run the consumer tests with `ROOK_BINARY=/absolute/path/to/rook ROOK_DOCKER_TEST=1 go test ./internal/deployment/sshagent -run TestSeparateRook` from Okra's Go module. The binary is an independent consumer input, not an import of Rook internals.

At the first customer release, record its tag and artifact digest as the compatibility baseline. Subsequent Okra validation should exercise that independently released consumer. New host primitives or incompatible wire semantics require a negotiated capability; ordinary changes to publishing commands, images, health checks, and lifecycle decisions remain central.
