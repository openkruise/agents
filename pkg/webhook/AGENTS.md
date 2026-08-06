# Admission Webhooks

This subtree owns controller admission defaulting, validation, registration,
and certificate configuration.

## Local Invariants

- Keep each handler's path, registration getter, Kubebuilder marker, resource,
  verbs, versions, and generated webhook configuration aligned.
- Mutating handlers must be deterministic and idempotent, and return a patch
  only when the decoded object changes.
- Validating handlers must not mutate admitted objects. Report invalid fields
  with accurate `field.Path` values and preserve update immutability rules.
- Treat `failurePolicy`, side effects, admission review versions, and enabled
  gates as externally visible behavior, not incidental configuration.
- Preserve feature-gate and CRD-discovery checks for handlers whose resources
  may be disabled or absent.
