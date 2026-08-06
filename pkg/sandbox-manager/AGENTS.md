# Sandbox Manager

This directory implements the Manager layer defined by the repository guide.

## Quota Orchestration

- `InitQuota` wires quota only after Manager construction has provided the
  required neutral Infra capabilities.
- Keep `QuotaEnforcer` as the Manager-facing surface for admission, accepted
  delete release, and API-key cleanup.
- Build create and clone admissions here from the caller, quota spec, and
  neutral `infra.SandboxResource`; do not make the API or concrete Infra
  implementation own quota policy.
- Release quota only after an accepted sandbox deletion. API-key cleanup is a
  separate Manager operation and must not roll back an already accepted key
  deletion on backend failure.
- Anti-drift mutations are primary-only. Losing primary status must stop the
  active repair cycle.
- Preserve typed quota-exceeded errors and fail-open handling for quota backend
  transport failures. HTTP status mapping remains in the API layer.
