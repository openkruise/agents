# Cache

`pkg/cache` provides informer-backed reads, wait tasks, event registration,
and cache health signals. The repository-wide layering rules still apply.

## Local Invariants

- Consumers should depend on `Provider`, not the concrete `Cache`.
- Informer objects are exposed with unsafe deep-copy disabled. Treat every CRD
  pointer returned by Get/List methods as shared read-only state and call
  `.DeepCopy()` before any mutation.
- Get/List methods read the informer store only. Use the API reader explicitly
  only when a live read is required.
- An empty namespace option adds no namespace filter; the effective scope is
  whatever the configured cache can see. Set a namespace when the caller
  requires one.
- Wait-task factories bind their action and completion check. Pause and Resume
  tasks pre-acquire a hook; release a constructed task if `Wait` will not run.
  `Release` is idempotent.
- Keep every event-handler registration removable and include all active
  registrations in conservative informer health reporting during startup or
  watch recovery.
- Quota-facing enumeration may return filtered CRD objects, but quota footprint,
  admission policy, backend behavior, and HTTP semantics do not belong here.

Use `cachetest.NewTestCache` for package consumers' unit tests. Test-only
ad-hoc wait tasks must remain in test code.
