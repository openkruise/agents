# Sandbox Manager

This directory implements the Manager layer defined by the repository guide.

## Sandbox Identity

- A Sandbox ID identifies one user delivery rather than the reusable Sandbox
  CR. When short-ID assignment is enabled, each Claim or Clone delivery gets a
  new ID, including a Claim that reuses a recycled Sandbox. Keep assignment
  policy in Manager while shared primitives and Infra remain policy-neutral.

## Route Orchestration

- Compose neutral backend observations into the shared route model without a
  Manager-specific projection or state machine. Preserve observation scope
  during ingestion; apply lifecycle and authorization policy only at admission.

## Quota Orchestration

- Manager owns quota orchestration over neutral Infra capabilities. Wire it only
  after those capabilities are available, and do not move its policy into API
  or concrete Infra implementations.
- Release quota only after an accepted sandbox deletion. API-key cleanup is a
  separate Manager operation and must not roll back an already accepted key
  deletion on backend failure.
- Anti-drift mutations are primary-only. Losing primary status must stop the
  active repair cycle.
- Preserve typed quota-exceeded errors and fail-open handling for quota backend
  transport failures. HTTP status mapping remains in the API layer.
