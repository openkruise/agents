# Security Token Refresh Controller

This controller proactively refreshes security tokens for eligible claimed
Sandboxes before their recorded expiration.

## Local Invariants

- Refresh only eligible, non-deleting Sandboxes. Schedule work from recorded
  expiration and configured timing policy; unrelated status churn must not
  trigger refreshes.
- Preserve the side-effect order: issue through `pkg/identity`, propagate to
  the runtime, then patch the annotation. Never publish a new expiration when
  issue or propagation failed.
- Refresh metadata updates must not mutate informer state or overwrite
  unrelated concurrent fields.
- Initial token issuance and deletion cleanup are outside this package.
- Shared token behavior belongs in `pkg/identity`; this controller must not
  depend on sandbox-manager packages.
