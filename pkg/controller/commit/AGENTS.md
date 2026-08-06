# Commit Controller

This package turns a `Commit` into a Kubernetes Job and tracks its result.

## Local Invariants

- A Commit UID has at most one effective Job. Preserve the UID field index and
  create expectations instead of relying on generated Job names.
- Keep reconciliation and status transitions at the package root, provider
  behavior in `core`, and Job construction and execution helpers in `job`.
- Finalizers protect external or Job-backed work; terminal TTL cleanup must not
  bypass required deletion handling.
- Invalid input and Job-generation failures may mark the Commit failed.
  Transient Kubernetes errors remain retryable and must not be converted into
  a successful terminal state.
- Registry credentials come only from referenced Docker config Secrets. Never
  copy secret data into status, events, command arguments, or logs.
