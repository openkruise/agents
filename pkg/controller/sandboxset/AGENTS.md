# SandboxSet Controller

This package maintains the pool of unclaimed Sandboxes for each `SandboxSet`.

## Local Invariants

- Scale and rolling-update logic operates only on unclaimed pool members.
  Claimed Sandboxes must not be deleted or replaced by this controller.
- Keep create and delete expectations around cache-delayed writes. Do not
  start conflicting scale or update work while expectations are unsatisfied.
- Preserve availability budgets across scaling and rolling updates, and keep
  candidate ordering deterministic.
- Treat both the current revision and the supported legacy revision as
  up-to-date where compatibility requires it.
- Keep template materialization, revision calculation, cleanup, status,
  metrics, and event handling consistent when pool membership changes.
