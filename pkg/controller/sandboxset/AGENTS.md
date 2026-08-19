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
- `NewSandboxFromSandboxSet` in `utils.go` is the single place that copies
  `SandboxSetSpec` fields onto every pool-created `Sandbox`. When a new field
  is added to `SandboxSetSpec` for this purpose, this function must copy it;
  a field present on the type but missing here silently never reaches created
  Sandboxes.
- `buildSandboxTemplateSpec` in `revision.go` has two return paths (templateRef
  and inline). Both must include every `SandboxSetSpec` pool-level field that
  should drive the rolling-update revision hash. A field missing from either
  path will not affect the hash for that path, so changes to that field will
  not trigger a rolling update for SandboxSets using that mode.
