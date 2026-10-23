# Runtime Injection and Pod Creation Extension Points

## Motivation
Injection configuration now retains default and user sources. Deployments may
need to customize a subset of declared runtimes without replacing the remaining
injection flow, or validate the final generated Pod before persisting it.

Provide optional extension points without introducing deployment-specific
runtime names, configuration keys, annotations, or process management.

## Per-Creation Context
Add `RenderFromTemplate` to `CreatePodArgs` and `PodGenerateArgs` in the sandbox
controller and to `sidecarutils.InjectionOptions`.

- Initial Pod creation and Recreate/CheckpointRestore template upgrades set it
  to true.
- Resume paths leave it false, including resumes that create a new instance.
- It is independent of `IsResume`, which controls the instance-resume protocol.
- Existing injection callers can omit options; the zero value remains valid.
  `InjectSandboxRuntimes` accepts optional `InjectionOptions` values and uses the
  last supplied value.

The flag only supplies context to extensions; it does not select a new runtime
or change the baseline injection behavior.

## Runtime Injection Hook
`sidecarutils.RuntimeInjectionHook` is an optional function receiving context,
Sandbox, Pod, the default and user configuration maps, and `InjectionOptions`.
It returns the names of runtimes it has handled and an error. The maps keep the
loader's source semantics: nil means the ConfigMap was not found, an empty map
means it contained no data, and user keys override matching defaults. Passing
plain maps keeps the loader's source type private to `sidecarutils`.

- A nil hook or empty result preserves the existing injection flow.
- Only runtimes declared on the supplied Sandbox may be reported as handled.
  Undeclared names are errors; duplicate names are treated as one.
- Handled runtimes skip baseline template parsing/injection, but other declared
  runtimes still run in the existing order.
- After all injection, the existing traffic-proxy probe rewrite and CA injection
  still run, including for runtimes handled by the hook.
- A hook error aborts injection and remains discoverable through `errors.Is`.
- The hook may mutate the generated Pod, but must treat the Sandbox and
  configuration maps as read-only. On any error the caller discards the Pod.

No template merge/deduplication policy is changed by this proposal.

## Before-Create Hook
`core.BeforeCreatePodHook` is an optional function receiving context, the
`PodControl`, `CreatePodArgs`, and the final generated Pod.

- It runs after generation, checkpoint delta application, and the existing TLS
  stamp, but before scale expectations, creation tracing, and `Client.Create`.
- It can use the control's client to persist prerequisite state, inspect the
  per-call context, or validate/mutate the Pod. It must not recursively create
  Pods.
- Errors abort Pod creation and retain the original error through wrapping.
  The hook owns any deployment-specific warning events and prerequisite
  mutation; it must be idempotent because creation can be retried.
- Generation or TLS-stamp errors prevent the hook from running.
- The existing best-effort checkpoint-delta behavior is unchanged.

## Registration and Isolation
Both hooks default to nil. Deployments register them during initialization,
before reconciliation starts, and must not replace them while workers run.
Test overrides are serial and restore the previous function with `t.Cleanup`.
These callbacks are extension registration, not process-wide feature switches:
activation policy stays in the callback and is evaluated for each call.

Implementations are registered in the package owning each extension point or
in a deployment-specific package. The injection package does not import the
sandbox controller, so no reverse dependency is introduced.

## Compatibility
- No CRD, ConfigMap format, existing runtime key, or exported controller method
  signature changes.
- Default/template merging remains user-over-default at the key level.
- No new API reads/writes, annotations, events, or probes when hooks are nil.
- Unknown/custom runtimes are not lost when only built-in runtimes are handled.
- Builds on the injection configuration source-retention change (#1098).

## Validation
Extend the existing tests for:
- Nil hooks and empty results, subset/all-runtime handling, duplicate and
  undeclared handled names, missing templates for unhandled runtimes, and errors.
- Preservation of source information and per-call generation context.
- Probe rewrite and CA injection after handled runtimes.
- Before-create ordering, checkpoint delta visibility, prerequisite errors,
  zero-value options, and absence of creation expectations on hook failure.
- First creation, template upgrades, and resume flag propagation.

Run targeted unit tests and controller builds. Do not require E2E execution for
this change's ordinary validation.
