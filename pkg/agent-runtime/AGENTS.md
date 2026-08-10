# Agent Runtime

This subtree contains runtime-side storage contracts, the storage CLI, and the
envd startup wrapper.

## Local Invariants

- Preserve the base64-encoded CSI `NodePublishVolumeRequest` contract shared
  with control-plane callers.
- Derive read-only mode from both the requested mount and the PersistentVolume
  access modes. Do not weaken either source.
- Storage CLI providers register under a unique CSI driver name during
  startup; lookup and driver listing remain concurrency-safe.
- Provider validation must not mutate its request. Never log Secrets. Omit
  credential-bearing publish context from normal logs; expose it only through
  the CLI's explicit non-production debug mode.
- Keep the storage CLI's mount-root resolution, per-target directory, and
  symlink behavior paired so the user-visible target remains stable.
- In `envd-run.sh`, arguments after `--` remain an argv array executed without
  shell reinterpretation.
