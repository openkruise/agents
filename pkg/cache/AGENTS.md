# Cache

`pkg/cache` provides informer-backed reads, wait tasks, event registration,
and cache health signals. The repository-wide layering rules still apply.

## Local Invariants

- Consumers depend on the cache contract rather than its concrete
  implementation.
- Informer-backed objects are shared read-only state. Copy an object before any
  mutation, and make a live read explicit when cache freshness is insufficient.
- An unspecified namespace does not narrow cache visibility. Callers that
  require tenant or namespace isolation must provide that scope explicitly.
- A prepared wait operation may hold resources before execution. Release it
  when it will not run, and keep cleanup safe to repeat.
- Event subscriptions must be removable, and health reporting must account for
  every active subscription during startup and watch recovery.
- Sandbox ID lookup is an exact match over resolved IDs and fails closed when
  more than one Sandbox matches.
- Quota-facing enumeration may return filtered CRD objects, but quota footprint,
  admission policy, backend behavior, and HTTP semantics do not belong here.
