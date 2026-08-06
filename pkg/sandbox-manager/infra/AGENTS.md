# Sandbox Manager Infrastructure

This directory defines the protocol-neutral Infra contracts used by the Manager
layer.

## Local Invariants

- Keep `Infrastructure`, `Sandbox`, `Builder`, operation options, metrics,
  quota snapshots, and quota events independent of any concrete backend.
- Interfaces expose capabilities needed by Manager; they do not encode an API
  request, HTTP result, authentication rule, or Manager business policy.
- Shared option and result types must not expose Kubernetes CRD objects or
  concrete client/cache types.
- Concrete Kubernetes behavior belongs in subpackages such as `sandboxcr`.
- Extend a shared interface only for a capability that Manager genuinely needs
  across implementations.
- Keep claim/clone metrics and log inputs implementation-neutral and safe to
  serialize.
- Redis, breaker decisions, anti-drift policy, and transport error mapping do
  not belong in these contracts.
