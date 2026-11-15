---
title: Per-Claim CSI Volume Attributes Passthrough
authors:
  - "@jicheng"
reviewers:
  - "@TBD"
creation-date: 2026-10-18
last-updated: 2026-10-18
status: provisional
---

# Per-Claim CSI Volume Attributes Passthrough

> The behavior below remains the default in `trusted-domain` mode. Opt-in
> `isolated` mode validates shared-PV resource scopes and relies on IdP/storage
> customer authorization, without requiring a PVC; see
> [compatible resource access modes](20261029-resource-access-modes.md).

## Summary

This proposal defines how `SandboxClaim.spec.dynamicVolumesMount[].attributes`
are passed to a CSI driver for one mount. The control plane copies non-sensitive,
provider-specific attributes into `NodePublishVolumeRequest.VolumeContext` while
preserving platform-owned fields.

The change extends the existing on-demand CSI mount design without adding API
fields or changing the wire format. It applies uniformly to every registered CSI
driver.

## Motivation

A PersistentVolume describes defaults shared by all mounts of that volume, but
some CSI drivers also need non-sensitive identifiers selected for one Sandbox
session, such as an instance ID, filesystem ID, or access-point ID. Encoding
those values in the PersistentVolume would require one PV per session-specific
combination and would prevent a warm Sandbox pool from serving heterogeneous
mount requests.

`CSIMountConfig.Attributes` already provides a map for provider-specific
configuration, but the community implementation currently does not copy it into
the CSI request. Consequently, a driver cannot consume per-Claim parameters.

## Goals

- Copy arbitrary non-sensitive Claim attributes into CSI `VolumeContext`.
- Preserve attribute keys and values exactly, including domain-prefixed keys.
- Use PersistentVolume attributes as defaults and let Claim attributes override
  ordinary keys for one mount only.
- Prevent users from overriding platform-owned routing, authentication, and Pod
  identity fields.
- Preserve the existing provider-specific enrichment hook.
- Keep resume, checkpoint clone, and recovery behavior consistent.

## Non-Goals

- Passing credentials, tokens, passwords, or other secrets through attributes.
- Defining schemas or business semantics for provider-specific keys.
- Selecting or installing a CSI driver from Claim attributes.
- Changing `PublishContext`, `Secrets`, or the CSI transport protocol.

## API Contract

No new API field is introduced. The existing field becomes an effective input
to CSI request construction:

```go
type CSIMountConfig struct {
    MountID    string            `json:"mountID,omitempty"`
    PvName     string            `json:"pvName"`
    MountPath  string            `json:"mountPath"`
    SubPath    string            `json:"subPath,omitempty"`
    ReadOnly   bool              `json:"readOnly,omitempty"`
    Attributes map[string]string `json:"attributes,omitempty"`
}
```

Example:

```yaml
spec:
  dynamicVolumesMount:
    - pvName: shared-storage-pv
      mountPath: /data
      attributes:
        storage.example.com/instance-id: instance-123
        storage.example.com/access-point-id: ap-456
```

The CSI driver receives the same keys and values:

```go
req.VolumeContext["storage.example.com/instance-id"] == "instance-123"
req.VolumeContext["storage.example.com/access-point-id"] == "ap-456"
```

Domain-prefixed keys are recommended to avoid collisions, but the platform does
not interpret or require a particular provider prefix.

## Merge and Validation Semantics

Request construction follows this order:

1. Read `PersistentVolume.Spec.CSI.VolumeAttributes` as defaults.
2. Validate Claim attributes against the reserved-key policy.
3. Copy ordinary Claim attributes over the defaults.
4. Derive the effective `path` from the PV path and `subPath`.
5. Run the optional provider-specific `NodePublishVolumeEnricher`.
6. Copy the resulting map into `NodePublishVolumeRequest.VolumeContext`.

Claim values therefore override ordinary PV values without mutating the cached
PersistentVolume. Derived platform values and provider enrichment run after the
generic merge and remain authoritative.

The following Claim keys are reserved:

- `path`, because it is derived from the PV base path and validated `subPath`;
- `authType`, because it controls the platform credential-loading path;
- every key beginning with `csi.storage.k8s.io/`, because this namespace carries
  Kubernetes-managed CSI metadata such as the Pod UID.

A request containing a reserved key is rejected. It is not silently ignored,
which prevents a caller from assuming that a security-sensitive override took
effect.

## Persistence and Recovery

The original `[]CSIMountConfig` is serialized into the Sandbox CSI mount
annotation. The annotation is restored during Pod recreation and propagated to
Checkpoints, so attributes naturally follow the existing resume, clone, and
recovery paths. Those paths rebuild the CSI request through the same
`CSIMountHandler` and therefore apply the same merge and validation rules.

No annotation format or migration is required.

## Security

Attributes are not a credential channel. They are stored in Kubernetes object
annotations, propagated to Checkpoints, and may survive longer than one mount.
The platform preserves provider-specific keys without interpreting them and
cannot reliably detect whether a value is secret. Callers are therefore
responsible for keeping credentials out of this map and must use the PV's
`NodePublishSecretRef` or an approved workload identity mechanism for sensitive
material.

The control plane must not log the complete `CSIMountConfig`, because arbitrary
provider attributes may contain resource identifiers. Error logs identify a
mount by PV name and target path only, and the raw mount-intent persistence
payload is excluded from diagnostic JSON. CSI request logging continues to
exclude `VolumeContext`, `PublishContext`, and `Secrets` at normal verbosity.

The CSI driver remains responsible for validating provider-specific attribute
semantics and authorizing the authenticated caller to access the referenced
resource. Possession of a resource ID alone must not grant access.

## Compatibility

Claims without attributes are unchanged. Existing PV-only mounts continue to
produce the same `VolumeContext`. Existing enrichment hooks continue to run and
can add or override provider-owned derived values.

The behavior is driver-neutral. A driver receives attributes only after it has
already been registered in the existing storage provider registry.

## Test Plan

Unit tests cover:

- passthrough of arbitrary and domain-prefixed keys;
- Claim override of an ordinary PV default;
- nil and empty attribute maps;
- rejection of each exact reserved key and the reserved CSI prefix;
- interaction with `subPath` derivation;
- provider enrichment after generic merging;
- immutability of the cached PersistentVolume.

The focused `pkg/utils/csiutils` test suite validates request construction. CRD
manifests are regenerated only to publish the clarified API field description.
