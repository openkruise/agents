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
- `NewSandboxFromSandboxSet` in `pkg/lifecycle/pool/materialize.go` (a neutral
  package shared with the Sandbox CR infra claim path) materializes the effective
  definition from shared `pkg/templateconfig.ResolveTemplateSpec`. Revision
  calculation must use the same resolver so created Sandboxes and rolling-update
  hashes describe the same configuration.
- The resolver's non-companion projection must contain every `SandboxSetSpec`
  pool policy copied onto Sandboxes or included in the revision hash. When adding
  such a field, update that projection and its materialization/hash consumers;
  omissions silently drop runtime configuration or prevent rolling updates.
  Actual companions use the entire version definition instead of that projection,
  without changing non-companion inheritance.
