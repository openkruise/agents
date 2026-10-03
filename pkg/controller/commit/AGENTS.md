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
- Every transition to a terminal phase (Succeeded/Failed) must record at least
  one status condition. Never finalize a terminal phase while the Job pod's
  exit code is still unobservable: requeue within a bounded wait, then record a
  Job-derived fallback condition. Terminal statuses are never recomputed, so a
  missing condition is permanent.
- Commit Job pods must carry the `agents.kruise.io/created-by` label with value
  `commit` (label only, no annotation): the Pod informer cache is
  label-filtered, and the value must not be `sandbox` or the pod delete
  validating webhook will protect the job pod like a sandbox pod.
