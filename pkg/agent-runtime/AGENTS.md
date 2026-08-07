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
- Keep internal mount resolution separate from the user-visible target so
  storage providers preserve stable mount behavior.
- The runtime launcher preserves user argument boundaries and does not
  reinterpret commands through a shell.
