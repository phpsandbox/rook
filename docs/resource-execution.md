# Resource execution contract for the first customer release

This contract is still a pre-launch draft. No customer-agent migration or compatibility with a prior release is required. `resource-execution.v1` identifies the host operations below; settle these semantics before the first customer release.

## Boundary

Okra resolves resource types, images, health checks, SQL, application bindings, names, and lifecycle decisions. Rook's `internal/resources.Executor` accepts a resource `Plan` directly and returns `Runtime` configuration. The agent coordinates source checkout, application build/run, and environment merging. Resources never import agent.

## Host primitives

- Start a supplied Compose JSON document in the supplied project using `up -d --wait --wait-timeout 120`. Compose service fields pass through; Rook does not maintain an engine/image catalog.
- Create a named Docker network when no Compose services are supplied, and create the supplied runtime volumes. Return network, mount targets, and environment bindings to the application deployment.
- Generate explicitly named secrets in the resource's private local store and resolve `{{secret:<scope>:<name>}}` references. Credentials remain on the host and survive agent restarts.
- Execute argv/environment/stdin in a declared container, or connect/disconnect it from a supplied network. Verify the container's actual Compose project label before operating on it. No shell command concatenation or provider-specific SQL generation happens in Rook.
- Execute explicit cleanup ownership: container operations, Compose project down with orphan cleanup, named volume removal, then named network removal. Compose down does not delete volumes implicitly; volume deletion is a separate owned primitive.

The installer and startup require a reachable Docker daemon and a Compose plugin supporting `up --wait`, `--wait-timeout`, and `down --remove-orphans`. Rook connects and advertises the capability only after these checks pass.

## Durable ownership and transitions

`ownership` maps stable IDs to cleanup instructions. Each instruction describes one host primitive: a Compose project/document, named volume, named network, or ordered container operations with their dependency scopes. IDs and targets are assigned by Okra, not derived by Rook from Compose services.

The first cleanup instruction recorded under an ID remains authoritative. Later plans can omit that ID, rename runtime services, or stop mounting its volume; the record remains durable. Omission means retain, not delete. This prevents losing cleanup ownership when the current runtime document changes.

`release` explicitly lists recorded IDs to execute before applying the next plan. A successfully released ID is atomically forgotten. An ID supplied again in `ownership` after release starts a replacement lifecycle; an omitted ID remains deleted. Releasing an absent ID is idempotent. Okra must resolve the sequence and ensure resources are no longer in use; Rook propagates Docker failures rather than forcing deletion of shared/in-use resources.

Unpublish with resource deletion executes every retained ownership record. Allocation cleanup runs before project/network teardown. Saved instruction files are private and replaced atomically. Operational failures retain instructions for retry. Shared resources are not owned merely because an application attaches to them: an allocation record can drop that application's database and detach its network without deleting the shared server or another application's data.

## Transport and evolution

Plan fields have identical JSON and MessagePack names. Compose JSON stays opaque, so new images, health checks, commands, SQL, and application environment bindings fit the existing primitives without changing the customer binary. A new host primitive, changed wire representation, or incompatible cleanup semantics requires a new negotiated capability before dispatch.

Okra generates the creation/reuse fixtures in `go/internal/deployment/sshagent/testdata`; the Rook consumer tests decode them and exercise real Docker persistence, omission/explicit release, and isolated allocation cleanup. During pre-launch these fixtures evolve together. They are development contract tests, not evidence of compatibility with an older released agent.

At the first customer release, record its tag and artifact digest as the consumer baseline. Subsequent Okra CI must feed its generated plans to that released consumer (including JSON/MessagePack and host execution), independently of the current Rook source. Only then establish the released v1 compatibility gate; do not freeze the current draft prematurely.
