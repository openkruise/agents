# Sandbox Controller

This package reconciles each `Sandbox` CR with its owned Pod and status.

## Local Invariants

- Keep top-level files focused on reconciliation, event handling, expectations,
  finalizers, timeout decisions, phase transitions, status updates, and
  metrics.
- Keep Pod lifecycle actions, pause and resume, in-place update, lifecycle
  hooks, recycle, and post-recreate initialization in `core`.
- Use expectations around Pod create/delete and resource-version-sensitive
  writes; do not assume the informer cache is immediately current.
- When changing a phase, condition, or desired-state transition, update all
  coupled status calculation, control handling, metrics, and event filtering.
- Preserve feature-gate and CRD-discovery checks during controller registration.
- In-place updates must continue to reject immutable template changes and
  preserve vertical-resize compatibility handling.
