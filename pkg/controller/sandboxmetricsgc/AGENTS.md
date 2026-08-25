# Sandbox Metrics GC Controller

This controller removes Prometheus series for deleted Sandboxes outside the
Sandbox controller's hot path.

## Local Invariants

- Metrics cleanup remains outside the Sandbox controller's hot path and must
  never block Sandbox reconciliation. Overload may drop and account for work.
- Work items carry only Sandbox identity. Cleanup is idempotent and must not
  read or mutate Sandbox objects.
- Keep this controller Sandbox-specific. Add a separate controller if another
  resource kind needs the same pattern.
