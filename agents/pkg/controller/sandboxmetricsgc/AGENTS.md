# Sandbox Metrics GC Controller

Reconciles synthetic `GenericEvent`s carrying `(namespace, name)` for deleted
Sandboxes and invokes `pkg/controller/sandbox.DeleteSandboxMetrics` to drop
all owned Prometheus series off the Sandbox controller's hot path.

## Responsibilities

- Receive `(namespace, name)` enqueues from the Sandbox controller's `NotFound`
  branch via a buffered `chan event.GenericEvent`.
- Translate each event into a normal `Reconcile` request through
  `source.Channel` + `EnqueueRequestForObject` so workqueue dedup applies.
- Call `DeleteSandboxMetrics` exactly once per request. The function is
  idempotent; duplicate work is harmless.

## Non-Responsibilities

- Reading or mutating Sandbox API objects. The `Object` field carried in the
  synthetic event holds only `ObjectMeta.Namespace`/`Name` so
  `EnqueueRequestForObject` can derive a `ctrl.Request`.
- Multi-kind cleanup. If SandboxSet/SandboxClaim need this pattern, give them
  their own controller — do not generalize this one.

## Local Guidance

- Keep `Reconcile` to a single statement: the package exists to swap a
  goroutine pool for a controller, not to grow new behavior.
- The only metric this package owns is `sandbox_metrics_gc_dropped_total`
  (channel-full drops). All other observability comes free from
  controller-runtime's `controller_runtime_*` series.
- `Enqueue` must be non-blocking. Blocking would re-introduce the very
  serialization that motivated this controller.
