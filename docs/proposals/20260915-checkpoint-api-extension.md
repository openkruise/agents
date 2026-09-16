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
checkpoint backend. Its initial source type is remote storage, while the
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
artifact was stored. A remote endpoint identifies a storage root but not a
particular artifact within that storage.

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
- Provisioning, configuring, or mounting a specific remote storage product.
- Defining backend-specific protocols for CPFS, NAS, or object storage.
- Adding PVC or node-local implementations in this proposal.
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
    Type   CheckpointSourceType    `json:"type"`
    Remote *RemoteCheckpointSource `json:"remote,omitempty"`
}

type RemoteCheckpointSource struct {
    Endpoint string `json:"endpoint"`
    Path     string `json:"path"`
}
```

`CheckpointSource` is a discriminated union. When `type` is `Remote`, the
`remote` member must be present. Only the member selected by `type` may be set.

The initial type is:

```go
CheckpointSourceTypeRemote CheckpointSourceType = "Remote"
```

This design follows the extensible `status.checkpointLocation` model proposed
in Kubernetes KEP-5823. The Kubernetes proposal initially defines a node-local
source with a relative path. This proposal applies the same pattern while
using a backend-neutral remote source.

#### Remote Endpoint

`endpoint` is an opaque remote storage endpoint. For example, an NAS
backend may report:

```text
xxx.nas.xxx.com
```

#### Artifact Path

`path` identifies the checkpoint artifact relative to the storage root
represented by `endpoint`. It remains explicit in status even when the
controller derives it from the originating Pod UID or checkpoint ID.

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
    type: Remote
    remote:
      endpoint: xxx.nas.xxx.com
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
