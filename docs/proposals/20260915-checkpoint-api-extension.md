---
title: Checkpoint API Extension for Container Selection and Artifact Storage
authors:
  - "@nce3xin"
creation-date: 2026-09-15
last-updated: 2026-09-15
status: provisional
---

# Checkpoint API Extension for Container Selection and Artifact Storage

## Summary

This proposal extends the `Checkpoint` API to support container-level
selection, cross-cluster Pod correlation, GPU memory checkpointing, and
structured checkpoint artifact locations.

The proposed API adds `spec.podUID`, `spec.containers`, a `gpuMemory`
`persistentContents` value, and `status.checkpointLocation`. The location is
reported in status because it describes the artifact actually produced by the
checkpoint backend. Its initial storage backend is CPFS, while the
discriminated-union shape allows additional backends to be introduced without
changing existing representations.

## Motivation

In some deployments, workload intent and workload execution are managed by
different Kubernetes clusters. A Pod represented in the execution cluster may
correspond to a Pod with the same name in the originating cluster, while the
two Pods have different Kubernetes UIDs.

Checkpoint artifacts need a stable identity from the originating cluster so
that they can be correlated and discovered across cluster boundaries. The UID
of the execution Pod cannot provide this identity because it is local to the
execution cluster.

The API also cannot select a subset of containers. This is needed by model
serving workloads where only model containers need to be checkpointed and
sidecars should remain outside the checkpoint.

`persistentContents` currently describes Pod information, process memory, and
the writable filesystem. GPU workloads additionally need to preserve GPU
device memory.

Finally, consumers need a structured way to discover where the checkpoint
artifact was stored. A CPFS mount target alone identifies a filesystem but not
a particular artifact within that filesystem.

### Goals

- Carry the originating Pod UID across cluster boundaries.
- Allow callers to select regular containers by name.
- Represent GPU device memory in `persistentContents`.
- Report the complete, resolved checkpoint artifact location.
- Leave room for future storage backends without changing the API shape.
- Preserve compatibility with existing Checkpoint objects and controllers.

### Non-Goals/Future Work

- Defining the checkpoint archive format.
- Defining how a container runtime captures or restores GPU device memory.
- Configuring or mounting CPFS on nodes.
- Adding NAS, object storage, PVC, or node-local implementations in this
  proposal.
- Changing the existing Checkpoint phase state machine.
- Changing the existing `status.checkpointId` backend identifier semantics.

## Proposal

### Cross-Cluster Pod Identity

Add an optional `spec.podUID` field:

```go
// PodUID identifies the corresponding Pod in the originating Kubernetes
// cluster. It may differ from the UID of the execution Pod referenced by
// PodName.
//
// The value is treated as an opaque identifier and may be used for
// cross-cluster correlation and checkpoint artifact namespacing.
// +optional
PodUID *types.UID `json:"podUID,omitempty"`
```

The value is assigned by the originating cluster and is intentionally not
compared with the execution Pod UID. Implementations may use it for
cross-cluster correlation and as part of checkpoint artifact namespacing.

The field is optional for backward compatibility, treated as an opaque
identifier, and immutable after Checkpoint creation.

### Container Selection

Add `spec.containers` as a set of regular container names:

```go
// +listType=set
Containers []string `json:"containers,omitempty"`
```

Semantics:

- An omitted or empty list selects all eligible regular containers.
- A non-empty list selects exactly the named regular containers.
- Every name must exist in `pod.spec.containers`.
- Duplicate names are rejected by set list semantics.
- The field is immutable after Checkpoint creation.

The Kubernetes Pod-level checkpoint proposal captures the complete Pod. This
API intentionally adds subset selection because model-serving workloads may
need to exclude infrastructure sidecars.

### GPU Memory

Add `gpuMemory` as a supported `spec.persistentContents` value:

```go
CheckpointPersistentContentGPUMemory = "gpuMemory"
```

`gpuMemory` means GPU device memory associated with the selected containers.
It does not imply a particular vendor, driver, or checkpoint format. Backend
implementations remain responsible for determining support and reporting an
actionable failure when the requested content cannot be captured.

### Checkpoint Artifact Location

Add `status.checkpointLocation`:

```go
type CheckpointStatus struct {
    CheckpointLocation *CheckpointSource `json:"checkpointLocation,omitempty"`
}

type CheckpointSource struct {
    Type CheckpointSourceType  `json:"type"`
    CPFS *CPFSCheckpointSource `json:"cpfs,omitempty"`
}

type CPFSCheckpointSource struct {
    MountPoint string `json:"mountPoint"`
    Path       string `json:"path"`
}
```

`CheckpointSource` is a discriminated union. When `type` is `CPFS`, the
`cpfs` member must be present. Only the member selected by `type` may be set.

The initial type is:

```go
CheckpointSourceTypeCPFS CheckpointSourceType = "CPFS"
```

This design follows the extensible `status.checkpointLocation` model in
Kubernetes KEP-5823. The Kubernetes Alpha design initially supports a
node-local source with a relative path. This proposal applies the same
principle to CPFS.

#### CPFS Mount Point

`mountPoint` is the CPFS mount target, for example:

```text
cpfs-xxx.aliyuncs.com
```

It identifies the remote filesystem and is not a container mount path.

#### Artifact Path

`path` identifies the checkpoint artifact relative to the mounted CPFS
filesystem root. It remains explicit in status even when the controller derives
it from the originating Pod UID or checkpoint ID.

A recommended layout is:

```text
checkpoints/<podUID>
```

### Example

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: Checkpoint
metadata:
  name: model-server-checkpoint
  namespace: workload-a
spec:
  podName: model-server
  podUID: 8c4ab56d-54a0-4b2d-b65e-f29292c8ee45
  containers:
  - model-server
  persistentContents:
  - memory
  - filesystem
  - gpuMemory
status:
  phase: Succeeded
  checkpointId: cp-123456
  checkpointLocation:
    type: CPFS
    cpfs:
      mountPoint: cpfs-xxx.aliyuncs.com
      path: checkpoints/8c4ab56d-54a0-4b2d-b65e-f29292c8ee45
```

### Existing Status Fields

This proposal does not change the existing phase values:

- `Pending`
- `Creating`
- `Succeeded`
- `Failed`
- `Terminating`

## Compatibility and Upgrade Strategy

All proposed fields are optional and additive.

Existing Checkpoint objects remain valid:

- An absent `spec.podUID` means that no originating-cluster Pod identity is
  available; existing checkpoint behavior is preserved.
- An absent or empty `spec.containers` selects all eligible containers.
- Existing `persistentContents` values are unchanged.
- An absent `status.checkpointLocation` means the backend has not reported a
  structured location.
- Existing phase and checkpoint ID semantics are unchanged.

Controllers should tolerate objects created before the CRD upgrade and only
write the new status field when the backend provides enough location
information.

Rollback is safe because older controllers ignore unknown fields. Kubernetes
will preserve fields declared by the installed CRD schema.

## Implementation History

- 2026-09-15: Initial proposal drafted for community discussion.
