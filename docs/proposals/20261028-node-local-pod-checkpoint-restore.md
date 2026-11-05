---
title: Node-Local Pod Checkpoint and Restore
authors:
  - "@zmberg"
reviewers:
  - "@TBD"
creation-date: 2026-10-28
last-updated: 2026-10-28
status: experimental
see-also:
  - "/docs/proposals/20260915-checkpoint-api-extension.md"
---

# Node-Local Pod Checkpoint and Restore

## Summary

A Sandbox that pauses today loses everything the agent had in memory: the pod is
deleted and the next resume starts a fresh process tree. This proposal keeps
that state by dumping the paused pod to the local disk of the node it ran on,
and by scheduling the resumed pod back to the same node so the dump can be read
again.

The dump itself is performed by the container runtime through the Kubernetes
v1.37 CRI `CheckpointPod` RPC. The Sandbox controller stays declarative: it
records the intent in a `Checkpoint` CR, and a new per-node DaemonSet, the
checkpoint agent, is the only component that talks to the CRI.

The restore path is not implemented in this repository. The controller hands the
checkpoint identifier to the runtime through a pod annotation; the containerd
and Kata side that consumes it is out of scope.

## Motivation

`PauseStrategy: Stop` is the only strategy the common Sandbox control implements.
It trades all in-memory state for the ability to release the pod's resources,
which is the wrong trade for a long-running coding agent: the model context,
the warmed language server, and the open shell sessions are exactly what makes
a resumed sandbox useful.

Kubernetes v1.37 ships the `CheckpointPod` CRI RPC, which lets a runtime dump a
whole pod sandbox. For a Kata pod this is a VM-level dump, so the restored
sandbox resumes the guest rather than restarting it. The declarative
`PodCheckpoint` API of KEP-5823 that would normally drive this RPC is targeted
at v1.38 and is not merged, so the orchestration has to live in this project
for now.

## Goals

- Persist the full state of a hibernating Sandbox, memory included, without
  requiring remote storage.
- Keep CRI access confined to a node-local component with a node-scoped cache
  and node-scoped permissions.
- Make the node that holds the artifacts an explicit part of the Checkpoint
  status, so the resume path can honour it.
- Pin the resumed pod back to that node through the scheduler, not around it.
- Leave the existing `Stop` pause strategy and the remote checkpoint source
  untouched.

## Non-Goals

- Restoring across nodes, or copying artifacts off the node that wrote them.
- The containerd annotation adapter and the Kata `RestoreSandbox` work that
  consume the restore annotation.
- Checkpointing a pod with more than one checkpointable container, or a pod
  carrying probes. The first stage targets a single-container Kata sandbox.
- Garbage collecting artifacts left behind by a node that never comes back.
- Adopting the community `PodCheckpoint` CRD. The `Checkpoint` CR remains the
  transitional API until KEP-5823 stabilizes.

## API Contract

`CheckpointSource` is a discriminated union keyed by `type`. A new `Local`
member locates artifacts on one node's disk:

```go
const CheckpointSourceTypeLocal CheckpointSourceType = "Local"

// LocalCheckpointSource locates checkpoint artifacts on a node's local disk.
type LocalCheckpointSource struct {
    // NodeName is the node that holds the checkpoint artifacts.
    NodeName string `json:"nodeName"`

    // Path is the absolute directory of the artifacts on NodeName.
    Path string `json:"path"`
}
```

Both members are now mutually exclusive and mandatory for their discriminator,
enforced by CEL on the union: `local` must be set when `type` is `Local` and
must be absent otherwise, and symmetrically for `remote`. The previous schema
only required `remote` to be present for type `Remote`, so a `Local` location is
the first one the API server can fully validate.

`status.checkpointLocation` is written by the component that owns the artifacts.
For a node-local checkpoint that is the checkpoint agent, and the value is the
agent's claim on the work:

```yaml
status:
  phase: Creating
  checkpointId: cp-7f3a1c2e-...
  checkpointLocation:
    type: Local
    local:
      nodeName: node-a
      path: /var/lib/kruise-agents/checkpoints/cp-7f3a1c2e-...
```

No new field is added to `CheckpointSpec`. Which node runs the dump is a
consequence of where the target pod is, not a user input.

The union member is named `Local` rather than the `NodeLocal` of KEP-5823. The
`Checkpoint` CR is already a transitional API with its own field names, and
`Local` matches the naming of the existing `Remote` member; renaming is part of
the eventual migration to the community API, not of this change.

## Pause Flow

`PauseStrategy: Hibernate` becomes functional in the common Sandbox control. It
reuses the checkpoint pause path the Sandbox-level control already had:

1. The Sandbox controller creates a `Checkpoint` CR owned by the Sandbox, with
   `persistentContents` derived from `sandbox.spec.persistentContents`. The
   Paused condition moves to `CheckpointCreating`.
2. The pod is kept alive. Deleting it first would leave nothing to dump.
3. The checkpoint agent on the node running the target pod claims the
   Checkpoint by publishing a `Local` location, then performs the dump.
4. Once the Checkpoint reaches `Succeeded`, the pause resumes its normal course
   and deletes the pod.

A cluster without the checkpoint agent deployed keeps the pause waiting in
`CheckpointCreating`. This is deliberate: silently degrading to `Stop` would
discard the state the user explicitly asked to preserve, and the stuck
condition names the missing component. There is no feature gate — a Sandbox only
reaches this path by asking for `Hibernate`.

## Checkpoint Agent

The agent is a DaemonSet with no leader election: each instance owns the pods of
its own node, so the instances partition the work by construction.

Ownership is derived from the pod cache rather than from a separate assignment
protocol. The manager restricts the pod cache to `spec.nodeName == $NODE_NAME`,
which both bounds the agent's memory and answers the ownership question: a
Checkpoint whose target pod is not in this cache belongs to another node's
agent. On top of that, a pod is only eligible when its `RuntimeClass` is in the
configured allow-list, because the dump only works on a runtime that implements
`CheckpointPod`.

A reconcile proceeds as follows:

- **Claim.** A Checkpoint with no location and an eligible local pod gets a
  `Local` location and a checkpoint ID derived from its UID. Publishing the
  destination before writing anything means a crash mid-dump still leaves an
  owner for the artifacts.
- **Execute.** The claiming agent takes the Checkpoint finalizer, creates the
  artifact directory, resolves the ready pod sandbox and its running containers
  over the CRI, and calls `CheckpointPod` with that directory as `outputPath`.
  A pod-info-only checkpoint skips the dump entirely.
- **Record.** On success the agent computes the pod template delta against the
  owning Sandbox and writes the terminal phase. On failure it reclaims the
  directory it created and records `Failed` with the reason, rather than leaving
  a partial dump behind.
- **Release.** On deletion the owning agent removes the directory and drops the
  finalizer. An agent that does not own the location leaves both alone, so the
  node that holds the artifacts is the only one that can delete them.

`CheckpointPod` requires the output directory to exist, to be empty, and to be
owned by the caller, and requires the container ID list to be exactly the
containers being dumped. The agent therefore resets the directory before each
attempt and fails the whole selection when a requested container is not
running, instead of dumping a subset.

The checkpoint ID is used as a single path element under the configured root and
is rejected if it is not, which keeps a malformed status field from escaping the
root. The response carries no digest, so none is recorded.

The agent mounts the CRI socket and the artifact root from the host. The root
must be mounted at the same path inside the container as on the host, because
that path is handed to the runtime and resolved in the host's mount namespace.
The artifacts contain guest memory, so the directory is created without
world-readable permissions and the agent runs as root to be able to read it
back.

## Resume Flow

When a Hibernate-paused Sandbox resumes and its pod is gone, the controller
reads the resume data from the Sandbox's checkpoints in one list: the pod
template delta that rebuilds the spec drift, the checkpoint ID, and — only for a
`Local` location — the node name. The same data feeds the `CheckpointRestore`
recreate upgrade.

The checkpoint ID reaches the runtime as a pod annotation whose key is
configured by the controller's `--checkpoint-id-annotation-key` flag. Without
that flag no annotation is written, so the restore path stays inert until a
cluster opts in.

A node-local checkpoint constrains where the pod may run. The pod is pinned with
a required node affinity term on the `metadata.name` field rather than by
setting `spec.NodeName`:

```go
req := corev1.NodeSelectorRequirement{
    Key:      "metadata.name",
    Operator: corev1.NodeSelectorOpIn,
    Values:   []string{nodeName},
}
```

Keeping the pod in the scheduler matters for failure behavior. A cordoned,
drained, or exhausted node leaves the pod `Pending` with the usual scheduling
events, whereas a direct node binding hands the kubelet a pod it can only
reject. The requirement is ANDed into every existing node selector term, since
terms are ORed against each other; preferred terms are left untouched.

## Risks and Mitigations

**The artifacts are tied to one node.** If that node never returns, the
restored pod stays `Pending` indefinitely and the state is unrecoverable. This
is inherent to node-local storage and is the main reason the union is
extensible: a remote or PVC-backed source removes the constraint without
changing the Checkpoint contract. Operators who cannot accept it should keep
using `Stop`.

**No artifact garbage collection across node loss.** Artifacts are reclaimed
through the Checkpoint finalizer, which requires the owning agent to run. A
Checkpoint whose node is permanently gone keeps its finalizer, and the disk
space is only reclaimed when the node is. Cleaning this up needs a node-liveness
signal the agent does not currently have.

**No dump integrity check.** `CheckpointPodResponse` is empty, so there is no
digest to record or verify. A corrupted dump is only discovered at restore time.

**Agent cache footprint.** The pod cache is node-scoped, but the Checkpoint and
Sandbox caches are cluster-wide, so every agent holds every Checkpoint and
Sandbox in the cluster. This is acceptable at current object counts and is the
first thing to narrow if it is not.

**The runtime side does not exist yet.** The CRI RPC is defined in v1.37, but
the containerd implementation tracked by containerd#13979 is not merged, and
Kata has no `RestoreSandbox`. Until both land, a cluster can create checkpoints
only against a runtime that has been patched, which is why the agent is gated on
an explicit `RuntimeClass` allow-list and an explicit node label.

**Dump duration.** A VM-level dump takes minutes and holds the pause open for
that long. A single RPC is bounded by `--checkpoint-timeout`, and the dump runs
at most once per Checkpoint: the terminal status write has to survive the
resource version bumped by the finalizer patch, otherwise the reconcile requeues
and pays for a second dump.

## Alternatives

**Drive the dump from the Sandbox controller.** It would avoid a new component,
but it would give a cluster-wide controller a CRI socket on every node and would
make the controller's blast radius the whole cluster. A DaemonSet keeps the
privileged surface per-node and its failure domain with it.

**Bind the restored pod with `spec.NodeName`.** Simpler, and it guarantees the
node, but it bypasses scheduler admission: an unschedulable node yields a pod
the kubelet rejects instead of a `Pending` pod with a diagnosable reason.

**Wait for the community `PodCheckpoint` API.** It is the long-term target, but
it is not merged and its fields are still moving. The existing `Checkpoint` CR
already carries the pause and upgrade flows, so extending its status union is
the smaller step and keeps the migration additive.

**Add a feature gate for Hibernate.** Rejected because `PauseStrategy` is
already the user's explicit opt-in, and a gate would turn a missing agent into a
silent fallback to `Stop`.

## Upgrade Strategy

The API change is additive: `Local` is a new union member and a new enum value,
so existing `Remote` locations and clusters that never set a location are
unaffected. The new CEL rules only constrain objects that set a union member
inconsistent with their discriminator, which no existing writer does.

Running the checkpoint agent requires Go 1.26 and `k8s.io/cri-api` v0.37.1,
which raised the toolchain for the whole repository. No existing component
changed behavior as a result.

Enabling the feature in a cluster is three independent steps: label the nodes
whose runtime implements `CheckpointPod`, deploy the DaemonSet from
`config/checkpoint-agent`, and set `--checkpoint-id-annotation-key` on the
controller once a runtime can consume the annotation. A Sandbox only uses any of
it by selecting `PauseStrategy: Hibernate`.

## Test Plan

Unit tests cover the agent reconcile table — claim, dump, terminal recording,
deletion, and the rejection paths for a foreign node, an ineligible
`RuntimeClass`, a missing ready sandbox, a failing `CheckpointPod`, a
path-escaping checkpoint ID, and a misconfigured artifact root — plus the CRI
helpers that resolve the sandbox and container IDs and build the filters.

Controller-side tests cover the pause strategy dispatch, the node name reported
by `GetCheckpointResumeData` for a local versus a remote location, and the node
pinning applied at pod creation, including the merge into pre-existing affinity
terms and the absence of `spec.NodeName`.

End-to-end validation requires a runtime that implements `CheckpointPod` and is
therefore out of scope for CI.
