# Sandbox Manager Infrastructure

This directory defines protocol-neutral Sandbox backend contracts.

## Local Invariants

- Extend a shared interface only for a current cross-implementation need.
- Inputs and results must not expose backend objects, clients, or caches, or
  encode API, authentication, or Manager policy.
- Keep backend-specific behavior, resilience, and repair policy in
  implementation subpackages.
- Keep observability data implementation-neutral and safe to serialize.
