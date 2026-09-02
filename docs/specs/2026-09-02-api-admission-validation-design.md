# API Admission Validation Design

## Goal

Close the API-admission gaps identified for probe names and PoolAutoscaler configuration without changing controller behavior.

## Scope

- Preserve the existing wake-on-traffic annotation constants and their recycle cleanup entries. They already satisfy the compatibility requirement and require no change.
- Declare `SandboxSpec.Probes`, `SandboxSetSpec.Probes`, and `SandboxTemplateSpec.Probes` as map lists keyed by `name`.
- Declare `PoolAutoscalerSpec.CronPolicies` and `PoolAutoscalerStatus.AppliedCronPolicies` as map lists keyed by `name`.
- Make `PoolAutoscaler.Spec` required by removing its optional marker and `omitempty` JSON tag.
- Validate `Probe.Name` as a non-empty condition-type suffix. It must match `^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$` and be no longer than 299 characters, so `agents.kruise.io/<name>` is valid against the `metav1.Condition.Type` grammar and its 316-character maximum.

## Generated Artifacts

The change is made only in `api/v1alpha1` hand-written types. `make generate manifests` regenerates DeepCopy methods, typed clients, and CRD schemas. Generated files are not edited manually.

## Validation

Add a focused API-package test covering the maximum valid probe-name length and invalid suffix shapes through the condition-type construction invariant. Run `make generate manifests`, inspect the generated CRD schema for required `spec`, list-map keys, and probe-name validation, then run the affected Go tests, formatting checks, and `go vet`.

## Delivery

Use a new worktree to preserve the source repository's existing untracked files. Create a signed local Git commit containing the implementation and regenerated outputs.
