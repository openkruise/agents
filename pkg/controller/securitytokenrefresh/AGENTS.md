# Security Token Refresh Controller

This controller proactively refreshes security tokens for eligible claimed
Sandboxes before the expiration recorded in
`identity.AgentKeyTokenRefreshStatus`.

## Local Invariants

- Watch only claimed, non-deleting Sandboxes with a non-empty token-status
  annotation. Timing-based work uses `RequeueAfter`; ordinary status churn
  must not enqueue refreshes.
- Decode the recorded expiration, apply the configured lead time and jitter,
  and use the configured retry interval for transient failures.
- Preserve the side-effect order: issue through `pkg/identity`, propagate to
  the runtime, then patch the annotation. Never publish a new expiration when
  issue or propagation failed.
- Patch from a deep-copied object so informer state and unrelated concurrent
  fields are not overwritten.
- Initial token issuance and deletion cleanup are outside this package.
- `SetupHook` remains the extension point for extra controller-runtime manager
  runnables. Run
  non-nil hooks in registration order and fail setup on the first hook error.
- Shared token behavior belongs in `pkg/identity`; this controller must not
  depend on sandbox-manager packages.
