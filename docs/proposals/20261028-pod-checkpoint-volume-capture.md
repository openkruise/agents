---
title: Capture Pod Volumes and Container Mounts in the Pause Checkpoint
authors:
  - "@bh4aws"
creation-date: 2026-10-28
last-updated: 2026-10-28
status: provisional
---

# Capture Pod Volumes and Container Mounts in the Pause Checkpoint

## Summary

The pause/resume checkpoint currently stores only container-level drift
(resource changes, runtime- and webhook-injected containers). Pod volumes and
the `volumeMounts`/`env` that runtime injection adds to template containers are
rebuilt from the current template and injection configuration at resume time.
If either changes while the sandbox is paused, the resumed Pod either fails
admission (`volumeMounts[].name: Not found`), mounts the wrong volume source, or
silently loses injected environment.

This proposal extends the checkpoint delta to capture every volume of the live
Pod and the drifted `volumeMounts`/`env` of template containers, so resume
reconstructs the pause-time mount topology instead of the current one.

## Motivation

`BuildPodTemplateDelta` runs at pause time and produces the Strategic Merge
Patch stored in `CheckpointStatus.PodTemplateDelta`. It captures:

- template container resource drift (e.g. VPA);
- containers injected at Pod create time by the runtime injection path and by
  third-party admission webhooks.

It does not capture `spec.volumes`, and it does not capture mounts/env that
injection adds to a container defined in the template. The resume base Pod is
regenerated from `sandbox.spec.template` plus the injection configuration read
at resume time, so the following drift breaks the resume:

1. A template volume changed its source or was removed while paused: the
   pause-time container still mounts it, but the regenerated Pod either has a
   different source or no such volume.
2. The injection configuration changed while paused (ConfigMap update, runtime
   removed): a template container loses the mounts/env it had at pause time.
3. Same-name volume with a different source: naïve merge-by-name in Strategic
   Merge Patch leaves two sources set on one volume, which is invalid.

### Goals

- Capture every volume of the live Pod in the checkpoint delta.
- Capture `volumeMounts` and `env` of template containers when they drift from
  the template definition, and restore the pause-time lists exactly at resume.
- Keep the existing behavior for containers and volumes that do not drift.
- Fix delta application so replaced lists cannot retain stale fields from the
  base Pod.

### Non-Goals

- Freezing containers injected after the pause in the resumed base. Injected
  containers are still merged by name with full pause-time copies; new
  containers produced by a changed injection configuration are kept.
- Freezing other main-container fields changed by injection (lifecycle,
  probes). Those follow the current injection configuration at resume.
- Handling template containers removed from the template while paused. Appending
  a partial container is already invalid today; pause/resume assumes template
  containers stay present.

## Proposal

### Delta encoding

All changes are in `pkg/controller/checkpoint/utils.go`.

**Volumes.** Every volume of the live Pod is emitted with a delete directive
followed by the full pause-time definition:

```json
{
  "spec": {
    "volumes": [
      {"name": "workspace", "$patch": "delete"},
      {"name": "config",    "$patch": "delete"},
      {"name": "workspace", "csi": {"driver": "fuse.csi.example.com"}},
      {"name": "config",    "persistentVolumeClaim": {...}}
    ]
  }
}
```

Strategic Merge Patch applies the delete directives to the base list before
merging, so each pause-time volume replaces the base volume with the same name
regardless of its source type. Base-only volumes are intentionally left in
place: a sidecar injected after the pause may legitimately mount them.
When the live Pod has no volumes, no `volumes` field is emitted and a delta with
no other drift stays empty.

**Template containers.** Besides resources, the delta now includes
`volumeMounts` and `env` when they differ from the template container, and
replaces the whole list:

```json
{
  "spec": {
    "containers": [{
      "name": "main",
      "volumeMounts": [
        {"$patch": "replace"},
        {"name": "workspace", "mountPath": "/workspace"},
        {"name": "config", "mountPath": "/config"}
      ],
      "env": [
        {"$patch": "replace"},
        {"name": "RUNTIME", "value": "paused-time"}
      ]
    }]
  }
}
```

The replace directive is only valid inside an element that matches the base
element by name, because Strategic Merge Patch appends unmatched elements
verbatim without interpreting nested directives. Template containers are
guaranteed to match the resume base under the pause/resume contract; injected
containers are still emitted as full copies without directives.

**Apply fix.** `ApplyPodTemplateDelta` now decodes the patched JSON into a fresh
`corev1.Pod` and assigns it to the caller. Decoding into the pre-populated Pod
made `encoding/json` reuse existing slice elements, so fields absent from the
patched JSON survived from the old element — a replaced volume that changed its
source would keep the previous source field and produce an invalid Pod.

### Interaction with `$setElementOrder`

Container order directives are unchanged. No element-order directive is emitted
for volumes: volume order has no runtime meaning and the delete/re-add encoding
appends pause-time volumes after any base-only volume.

## Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| Resume base gained a volume required only by a newly injected sidecar; delta keeps it, but the sidecar's pause-time copy does not mount it | Volumes are merged per name; base-only volumes are preserved, so the new sidecar keeps working. |
| Checkpoint delta grows with large volume specs (projected volumes, config maps) | Volume specs are small compared to container images; the checkpoint CR size risk is unchanged in practice. |
| Template container removed while paused makes the delta append a partial container | Pre-existing limitation of the delta; documented as a non-goal. |
| Nested `$patch: replace` leaks if a template container no longer matches the base | Same pre-existing limitation; the resulting Pod was already invalid because the appended container lacked an image. |

## Test Plan

- Unit tests for template container delta: resource drift, volumeMounts/env
  drift, and no drift.
- Unit test for volume delta: delete entries plus full definitions, empty when
  the Pod has no volumes.
- Round-trip test: built delta applied to a resume base with a changed volume
  source, a dropped volume, an extra base volume, and changed injected
  mounts/env must restore the pause-time volumes, mounts and env.
- Existing round-trip and container-order tests must keep passing.
