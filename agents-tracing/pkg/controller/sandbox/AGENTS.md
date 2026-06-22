# Sandbox Controller

This package reconciles each `Sandbox` CR with its owned Pod and status. It is
controller logic, not the sandbox-manager request API.

## Responsibilities

- Register the reconciler only when the Sandbox feature gate and CRD discovery allow it.
- Reconcile Sandbox and same-named Pod events through expectations, finalizers, timeout handling, phase transitions, and status updates.
- Keep top-level files focused on orchestration, event handling, and metrics.
- Keep Pod lifecycle actions, pause/resume, in-place update, lifecycle hooks, and post-recreate initialization inside `core`.

## Dependency Direction

The sandbox-manager (E2B API layer) depends on the controller, not the other way around.
When reasoning about controller behavior, do NOT consider sandbox-manager or E2B API
semantics. The controller is the source of truth for sandbox lifecycle; upper layers
conform to the contracts it establishes.

## Local Guidance

- Preserve the split between reconcile orchestration in this package and Pod control/status mutation in `core`.
- When adding a phase or condition, update status calculation, control handling, metrics, and pod event filtering where relevant.
- Use expectations around Pod create/delete and resource-version-sensitive writes instead of assuming informer cache freshness.
- Keep feature-gate behavior in `Add` and event handlers intact.
- For in-place updates, reject immutable template changes and preserve vertical resize compatibility handling.
