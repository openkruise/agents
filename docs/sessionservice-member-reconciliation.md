# SessionService member creation and convergence

This document describes the fixed-replica controller implementation. It does not
claim implementation of every feature in the SessionService proposal.

## Creation and observation

Like Kubernetes ReplicaSet, the controller creates Sandbox CRs with
`metadata.generateName` rather than assigning an ordinal or replaying a stable
member name. The prefix identifies the parent for readability; UID labels and
controller owner references enforce membership isolation.

Each reconciliation validates the current template and ensures its immutable
ControllerRevision before calculating the replica deficit and rollout budget.
New members copy that template and carry its target Revision label. Reverting
the template reuses its matching Revision; old snapshots remain available.
There is no persisted or in-memory plan to finish creating an older Revision.

The existing expectations implementation tracks names. After a successful
Create response, the returned name is registered before this parent's next
serialized reconciliation observes the cache. This differs from ReplicaSet's
count-based creation expectations but preserves normal cache-observation waits.
No new shared expectations implementation or APIReader read is introduced.

A failed Create returns its error and establishes no wait for that attempt,
including when the server committed the object but its response was lost.
A partial batch retains waits for its earlier successful creates. Once all
waits are observed, or an observation wait expires, capacity is decided again.
Expiration clears waits without replaying a write;
the next reconciliation uses a new cached snapshot, current replicas, and the
latest target Revision. It never resubmits a particular old name or template.

## Accepted limits

Lost Create responses, cache lag, and controller restarts can temporarily create
extra Sandbox CRs. Expectations reduce ordinary duplication; they do not provide
exactly-once creation or a strict physical capacity limit. Normal reconciliation
can converge after cache visibility recovers and writes succeed.

Ordinary scale-down only deletes unbound members with valid binding data, using
the UID and resourceVersion from the same observed snapshot as preconditions.
Occupied or malformed bindings can therefore delay surplus convergence until
bindings are released or repaired. SessionService UID/owner checks and the
same-name replacement checks for deletion observation remain in place.

`maxSurge` is a planning budget based on observed active and terminating CRs,
not a strict instantaneous upper bound. It also does not count Pods still exiting
after their CRs disappear. Rollout replacement retains its existing Ready budget
and deletion policy, distinct from ordinary binding-protected scale-down.

## Validation

Regression coverage includes generated names, normal observation waits, failed
and lost responses, partial caches, restarts, expired waits after replica/template
changes, surplus binding protection, rollback snapshots, and conditional deletion.
On office machines, compile those tests with:

```sh
go test -mod=readonly -exec=/usr/bin/true ./pkg/controller/sessionservice -run '^Test' -count=1
```

This command does not execute tests and cannot prove runtime or API-server
behavior. Run the tests in an approved execution environment; real cluster
acceptance is separate.

Upstream references:

- [ReplicaSet reconciliation](https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/replicaset/replica_set.go)
- [Pod creation and expectations](https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/controller_utils.go)

Expectations use the parent namespace/name. Parent deletion events (including
unknown-delete-state events), UID-changing updates, and NotFound reconciliation
clear outstanding waits without a separate identity map. Membership still uses
parent UID labels and owner UID checks. Deletion completion is observed from the
cached snapshot. A same-name replacement can retain a deletion wait until timeout;
UID/resourceVersion DELETE preconditions still protect the replacement object.
