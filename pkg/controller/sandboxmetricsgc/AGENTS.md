# Sandbox Metrics GC Controller

This controller removes Prometheus series for deleted Sandboxes outside the
Sandbox controller's hot path.

## Local Invariants

- `Enqueue` must remain non-blocking. When its channel is full, drop and count
  the event rather than blocking Sandbox reconciliation.
- Synthetic events carry only namespace and name; this controller must not read
  or mutate Sandbox objects.
- Reconcile only delegates to the idempotent
  `sandbox.DeleteSandboxMetrics`.
- Keep this controller Sandbox-specific. Add a separate controller if another
  resource kind needs the same pattern.
- Its only package-owned failure metric is
  `sandbox_metrics_gc_dropped_total`; use controller-runtime metrics for
  reconcile behavior.
