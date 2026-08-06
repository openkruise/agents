# API v1alpha1

This package defines the persisted Kubernetes API contract for
`agents.kruise.io/v1alpha1`.

## Local Invariants

- Treat field names, JSON tags, enum strings, defaults, validation markers,
  list semantics, subresources, and printer columns as compatibility surfaces.
- Do not rename, repurpose, or remove persisted fields or reserved label and
  annotation constants without an explicit migration and compatibility plan.
- Keep API types declarative. Admission behavior belongs in `pkg/webhook`,
  reconciliation policy in controllers, and transport compatibility in API
  servers.
- Preserve pointer and `omitempty` choices where absent, zero, and explicit
  values have different meanings.
- Keep status fields observational. Changes to phases, reasons, or conditions
  must remain aligned with every controller that writes or interprets them.
