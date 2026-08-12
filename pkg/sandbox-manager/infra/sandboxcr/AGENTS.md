# Sandbox CR Infrastructure

This package is the Kubernetes CRD implementation of the neutral Infra
contracts.

## Local Invariants

- Keep concrete Kubernetes reads, writes, cached observations, and CRD
  conversion inside this implementation.
- Prefer informer-backed state; use a live API-server read only when stale
  observations cannot safely decide a transition.
- Preserve cleanup and retry classification across multi-step claim and clone
  operations. A retriable error must mean the outer operation can safely try
  again.
- Concurrent lifecycle transitions must not overwrite a winning state.
- Convert Kubernetes objects and observations into neutral Infra values at this
  boundary.
- Publish neutral route observations only. Shared routing owns projection and
  state semantics, serving components own traffic admission, and route
  maintenance must not create a second source of truth.
