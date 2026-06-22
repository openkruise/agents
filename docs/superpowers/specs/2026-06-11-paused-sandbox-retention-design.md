# Paused Sandbox Retention Design

## Background

OpenKruise Agents currently has several related but inconsistent timeout behaviors for paused sandboxes:

- Manual `PauseSandbox` in the E2B server pushes `ShutdownTime` to `now + 1000 years` and, when `PauseTime` is set, moves `PauseTime` to the same far future value.
- E2B create with `autoPause=true` uses the server `maxTimeout` to compute `ShutdownTime`, while `PauseTime` is based on the request timeout.
- The sandbox controller auto-pauses a sandbox when `PauseTime` is reached by patching only `Spec.Paused=true`; it does not recalculate `ShutdownTime`.
- Shared timeout primitives already exist in `pkg/utils/timeout`, but there is no single helper for paused retention semantics.

This creates a coupling between running timeout limits and paused sandbox retention, and manual pause and auto-pause can produce different retention deadlines.

## Goals

Define one paused sandbox retention model used by E2B create, E2B manual pause, and controller auto-pause.

- Use a default paused retention of 100 years.
- Stop using E2B `maxTimeout` for paused sandbox retention.
- Let E2B create accept a retention extension metadata key:
  `e2b.agents.kruise.io/reserve-paused-sandbox-for`.
- Let E2B manual pause accept a retention extension header:
  `x-e2b-kruise-reserve-paused-sandbox-for`.
- Persist the resolved retention preference on the Sandbox with an internal annotation that does not use the E2B prefix:
  `agents.kruise.io/reserve-paused-sandbox-for`.
- Persist `default` when E2B creates or manages a sandbox without an explicit paused retention value.
- Let the sandbox controller overwrite `ShutdownTime` during auto-pause only when this internal annotation exists.
- Preserve `never-timeout`: sandboxes with no timeout deadline must not receive a shutdown deadline from paused retention.
- Preserve first-writer-wins behavior for manual pause.
- For every E2B auto-pause timeout write, set `ShutdownTime` from the paused retention value instead of `maxTimeout`.

## Non-Goals

- Do not rewrite the timeout subsystem.
- Do not change connect/resume extend-only behavior.
- Do not remove or weaken normal running timeout validation against `maxTimeout`.
- Do not change the E2B API response schema.
- Do not support `never` or `forever` sentinels for paused retention in this change.
- Do not migrate historical E2B-created sandboxes that lack the new annotation.
- Do not change checkpoint-created paused states that do not pass through E2B pause or controller `PauseTime` auto-pause.
- Do not add admission webhook validation as part of this design; invalid direct annotation edits are handled in the controller.

## Terms And Keys

External E2B metadata key:

```text
e2b.agents.kruise.io/reserve-paused-sandbox-for
```

External E2B pause header:

```text
x-e2b-kruise-reserve-paused-sandbox-for
```

Persisted Sandbox annotation:

```text
agents.kruise.io/reserve-paused-sandbox-for
```

Accepted values:

- `default`: use the built-in paused retention of 100 years.
- Positive Go duration strings accepted by `time.ParseDuration`, such as `30m`, `1h`, or `240h`. Positive means strictly greater than zero.

Rejected values:

- Negative durations.
- Zero durations, including `0` and `0s`.
- Invalid duration strings.
- `never` and `forever`.
- Empty explicit values.

Errors for `never`, `forever`, zero, negative, and malformed values should tell callers to use `default` for the built-in 100-year retention.

## Options Considered

### Option A: Controller Overwrites Every Auto-Pause ShutdownTime

The controller could recalculate `ShutdownTime` for all sandboxes when `PauseTime` expires.

This is simple and makes all auto-pause behavior uniform, but it changes declarative CRD behavior for users who set `PauseTime` and `ShutdownTime` directly. Their explicit shutdown deadline would be replaced by controller logic they did not opt into.

### Option B: Controller Only Acts On E2B-Prefixed Annotation

The controller could check an E2B annotation such as `e2b.agents.kruise.io/reserve-paused-sandbox-for`.

This is easy to trace back to the E2B API, but it leaks E2B naming into controller-owned behavior. It also makes the persisted state look like an external API extension rather than an internal lifecycle policy.

### Option C: Controller Acts On Internal Agents Annotation

E2B accepts external metadata and header names, but persists a controller-owned annotation under `agents.kruise.io/reserve-paused-sandbox-for`. The controller only applies paused retention when that annotation exists.

This keeps API compatibility at the E2B boundary, avoids coupling controller code to E2B server package constants, protects non-E2B CRD users, and provides a clear opt-in marker for controller auto-pause shutdown recalculation.

Option C is recommended.

## Recommended Design

### Auto-Pause Timeout Model

Every E2B timeout write for an auto-pause sandbox should use the same model:

```text
PauseTime = now + requestedTimeout
ShutdownTime = PauseTime + pausedRetention
```

This applies to create, connect, resume placeholder timeout writes, and set-timeout. `maxTimeout` remains the validation limit for requested running timeout seconds, but it is no longer used as the paused retention duration.

Actual pause transitions still synchronize the deadline:

- Manual E2B pause computes paused retention immediately inside `PauseSandbox`.
- Controller auto-pause recomputes `ShutdownTime=now+pausedRetention` when `PauseTime` is reached, using the persisted internal annotation.

The distinction is the anchor time:

- Running auto-pause timeout writes anchor retention at the scheduled pause time: `PauseTime + pausedRetention`.
- Actual pause transitions anchor retention at the actual pause time: `now + pausedRetention`.

This handles controller scheduling delays without allowing connect, resume, or set-timeout to shrink paused retention back to `maxTimeout`.

### Shared Timeout Helpers

Add shared paused retention helpers under `pkg/utils/timeout`.

Responsibilities:

- Define the default paused retention as 100 years.
- Define the persisted default sentinel string as `default`.
- Parse paused retention values from strings.
- Resolve an annotation value into an effective duration.
- Compute paused shutdown time from an anchor time and a retention duration.
- Apply paused retention to `timeout.Options` without changing unrelated running timeout semantics.
- Build auto-pause timeout options where `ShutdownTime` is derived from `PauseTime + pausedRetention`.

The helper should distinguish between:

- annotation absent: not managed by this paused retention model.
- annotation present with `default`: managed, use 100 years.
- annotation present with duration: managed, use that duration.
- invalid annotation value: return a parse error to the caller; each caller decides whether that error is fatal. The controller handles this error by pausing without recalculating `ShutdownTime`.

### Constants

Add the persisted annotation key in `api/v1alpha1`, for example:

```go
AnnotationReservePausedSandboxFor = InternalPrefix + "reserve-paused-sandbox-for"
```

Keep E2B external API constants in `pkg/servers/e2b/models`:

```go
ExtensionKeyReservePausedSandboxFor = v1alpha1.E2BPrefix + "reserve-paused-sandbox-for"
ExtensionHeaderReservePausedSandboxFor = ExtensionHeaderPrefix + "reserve-paused-sandbox-for"
```

The controller must import only the API-level annotation key and `pkg/utils/timeout`, not E2B server models.

### E2B Create

`NewSandboxRequest.ParseExtensions` parses `e2b.agents.kruise.io/reserve-paused-sandbox-for` from request metadata.

Behavior:

- If absent, set the parsed extension value to `default`.
- If present as `default`, accept it.
- If present as a positive duration, accept it.
- If invalid, return a clear bad extension error.
- Always delete the external metadata key from user metadata so it is not persisted as ordinary user metadata.

`basicSandboxCreateModifier` persists the parsed value to:

```text
agents.kruise.io/reserve-paused-sandbox-for
```

For non-`never-timeout` create:

- `autoPause=false`: keep existing running timeout behavior, `ShutdownTime = createNow + request.Timeout`.
- `autoPause=true`: set `PauseTime = createNow + request.Timeout`, then set `ShutdownTime = PauseTime + pausedRetention`.

For `never-timeout` create:

- Leave `PauseTime` and `ShutdownTime` unset.
- Still persist the annotation so future explicit timeout operations can retain the E2B-managed marker when applicable, but the annotation alone must not create a deadline.

### E2B Connect, Resume, And SetTimeout

The existing connect, resume, and set-timeout flow still owns the running timeout window:

- Validate requested timeout seconds against the existing min/max rules.
- Preserve the existing update policies, including extend-only behavior for connect/resume.
- Preserve the never-timeout short-circuit: if the current sandbox has no timeout deadline, do not create one.

Only the auto-pause `ShutdownTime` calculation changes.

For `autoPause=false`, timeout writes continue to set:

```text
ShutdownTime = now + requestedTimeout
PauseTime = nil
```

For `autoPause=true`, timeout writes should resolve paused retention from the internal annotation, falling back to `default` if the annotation is missing or invalid, then set:

```text
PauseTime = now + requestedTimeout
ShutdownTime = PauseTime + pausedRetention
```

This requires replacing or extending `buildSetTimeoutOptions` so auto-pause callers can provide the resolved paused retention. The helper should not read annotations by itself; request handlers already have the sandbox and should resolve retention before building timeout options.

For historical sandboxes missing the internal annotation, connect/resume/set-timeout should compute with the default 100-year retention. If the endpoint actually writes timeout fields under its existing update policy, it should also persist `agents.kruise.io/reserve-paused-sandbox-for=default` in the same optimistic update. If ExtendOnly skips a timeout write, leave the object unchanged.

If the internal annotation is present but invalid, these request paths fail open: compute with the default 100-year retention, log a warning, and preserve the invalid annotation. They should not return an API error solely because a persisted internal annotation was edited incorrectly. This keeps connect/resume usable for E2B users and matches the controller's fail-open behavior.

### E2B Manual Pause

`PauseSandbox` parses `x-e2b-kruise-reserve-paused-sandbox-for`.

Resolution order:

1. Header value, if present.
2. Persisted annotation on the Sandbox.
3. `default`, when the Sandbox has no persisted annotation.

If the persisted annotation exists but is invalid, manual pause also fails open: use `default`, log a warning, and preserve the invalid annotation unless a valid header is provided. A valid header remains an explicit user override and is persisted when the pause update wins.

For a timed sandbox, manual pause computes `ShutdownTime = now + pausedRetention`. If the current timeout options include `PauseTime`, the pause path moves `PauseTime` to the same paused retention deadline to preserve current E2B `EndAt` behavior for auto-pause mode.

For a `never-timeout` sandbox, manual pause must not set a shutdown deadline unless the product later introduces an explicit override for never-timeout. This design does not introduce that override.

When the pause update wins, the resolved value is persisted to the internal annotation in the same optimistic update that writes `Spec.Paused` and timeout options:

- header present: persist the validated header value.
- header absent and annotation present: preserve the existing annotation.
- header absent and annotation absent: persist `default`.

This backfills the internal annotation for historical sandboxes when they are later managed through E2B manual pause. It also prevents a pause from succeeding while the persisted preference fails to update.

Manual pause preserves first-writer-wins:

- If the latest Sandbox is already `Spec.Paused=true`, the pause call does not update timeout or annotation.

### Sandbox Controller Auto-Pause

When `Spec.PauseTime` has expired and `Spec.Paused=false`, the controller keeps the current optimistic-lock patch behavior.

New behavior:

- If `agents.kruise.io/reserve-paused-sandbox-for` is absent, keep existing CRD behavior: patch only `Spec.Paused=true`.
- If the annotation is present and `Spec.ShutdownTime` is non-nil, parse it and patch both `Spec.Paused=true` and `Spec.ShutdownTime=now+pausedRetention`.
- If the annotation is present but `Spec.ShutdownTime` is nil, patch only `Spec.Paused=true`. This preserves the never-timeout meaning of an absent shutdown deadline even if a direct CRD writer adds the annotation.
- If the annotation value is `default`, use 100 years.
- If the annotation value is invalid, patch `Spec.Paused=true` without recalculating `ShutdownTime`, preserve the invalid annotation, log the problem, and emit a Warning event. The controller should not return a reconcile error after successfully patching `Paused=true`; otherwise a bad annotation would keep the sandbox running and cause repeated reconcile failures.

The controller should not depend on E2B-specific package constants. The internal annotation is the boundary between the E2B server and controller behavior.

The current `SandboxReconciler` setup already creates an event recorder for sandbox controls, but the reconciler itself does not store one. If the implementation emits Warning events from the auto-pause branch, add a narrow `record.EventRecorder` field to `SandboxReconciler` rather than routing this through unrelated controls.

### Infra Timeout And Pause Atomicity

Several infra write paths must be able to persist the internal paused retention annotation in the same optimistic update as their timeout or pause mutation. Use narrow fields such as `ReservePausedFor *string` rather than a generic annotations map, so the infra contract remains specific to this lifecycle policy.

Required contract changes:

- Timeout save path: replace or extend `SaveTimeoutWithPolicy(ctx, opts, policy)` with an options form that can carry `Timeout`, `Policy`, and `ReservePausedFor *string`.
- Resume path: extend `infra.ResumeOptions` with `ReservePausedFor *string` so the atomic resume placeholder timeout write can also backfill the annotation.
- Pause path: extend `infra.PauseOptions` with `ReservePausedFor *string` so manual pause can persist header/default retention atomically with `Spec.Paused=true`.

Timeout annotation writes must be coupled to accepted timeout writes:

- `UpdatePolicyAlways`: write the annotation only when the timeout mutator decides to update timeout fields.
- `UpdatePolicyExtendOnly`: write the annotation only when the requested timeout extends the effective deadline and the timeout write is accepted.
- If the policy skips the timeout update, do not perform an annotation-only write.

`sandboxcr.Pause` already uses `retryUpdate` to atomically set `Spec.Paused` and timeout fields. The same mutator should update the internal annotation only when the pause request wins the first-writer-wins race.

If the latest object is already paused, `Pause` returns without modifying timeout or annotation.

## Data Flow

Create with default retention:

1. Client calls E2B create without paused retention metadata.
2. E2B parser records extension value `default`.
3. Create modifier writes `agents.kruise.io/reserve-paused-sandbox-for=default`.
4. If `autoPause=true` and not never-timeout, initial `ShutdownTime` is computed from `PauseTime + 100 years`.
5. Later connect, resume, or set-timeout calls keep the same model: `ShutdownTime = new PauseTime + pausedRetention`.

Create with custom retention:

1. Client sends `e2b.agents.kruise.io/reserve-paused-sandbox-for=240h`.
2. Parser validates and removes it from user metadata.
3. Create modifier writes `agents.kruise.io/reserve-paused-sandbox-for=240h`.
4. Auto-pause initial `ShutdownTime` uses `PauseTime + 240h`.
5. Later connect, resume, or set-timeout calls keep the same model: `ShutdownTime = new PauseTime + 240h`.

Connect, resume, or set-timeout on an auto-pause sandbox:

1. Handler parses and validates the requested running timeout as it does today.
2. Handler resolves paused retention from `agents.kruise.io/reserve-paused-sandbox-for`, falling back to `default` when missing.
3. Handler builds timeout options with `PauseTime=now+requestedTimeout` and `ShutdownTime=PauseTime+pausedRetention`.
4. Handler saves the timeout using the existing update policy for that endpoint.

Manual pause with header:

1. Client calls pause with `x-e2b-kruise-reserve-paused-sandbox-for=1h`.
2. E2B validates the header and computes `ShutdownTime=now+1h`.
3. Infra pause writes `Spec.Paused=true`, timeout fields, and `agents.kruise.io/reserve-paused-sandbox-for=1h` in one winning update.

Manual pause without header on a sandbox that lacks the internal annotation:

1. E2B resolves the retention value to `default`.
2. For a timed sandbox, E2B computes `ShutdownTime=now+100 years`.
3. Infra pause writes `Spec.Paused=true`, timeout fields, and `agents.kruise.io/reserve-paused-sandbox-for=default` in one winning update.

Controller auto-pause:

1. Controller sees expired `PauseTime`.
2. If internal annotation exists and `Spec.ShutdownTime` is non-nil, it computes paused shutdown deadline from annotation.
3. Controller patches `Spec.Paused=true` and the recalculated `ShutdownTime` under optimistic lock.
4. If annotation is absent, controller does not recalculate `ShutdownTime`.
5. If annotation is invalid, controller patches `Spec.Paused=true` only, records a warning, and leaves the existing `ShutdownTime` and annotation unchanged.

## Error Handling

Invalid create metadata should return HTTP 400 with a clear extension parsing error.

Invalid pause header should return HTTP 400 before attempting to pause. These are request input errors.

Invalid persisted retention annotation should not make E2B connect, resume, set-timeout, or manual pause fail. Those paths should use `default`, log a warning, preserve the invalid annotation, and continue with the existing timeout or pause operation. If a valid pause header is supplied, the header overrides the invalid annotation and is persisted when the pause update wins.

Invalid controller annotation should not block auto-pause. The controller should patch `Spec.Paused=true`, leave `ShutdownTime` unchanged, preserve the invalid annotation, log the validation error, and emit a Warning event. This deliberately favors stopping the running sandbox over enforcing retention parsing in the controller loop.

Timeout calculation should normalize timestamps through existing timeout normalization behavior before persistence.

## Compatibility

Running timeout validation and update policy behavior remain unchanged:

- create/connect/resume/set-timeout still enforce the existing max timeout rules.
- connect/resume retain extend-only behavior.
- auto-pause `ShutdownTime` changes from `now+maxTimeout` to `PauseTime+pausedRetention` for create/connect/resume/set-timeout.
- `never-timeout` remains represented by absent `PauseTime` and absent `ShutdownTime`.

Existing CRD users without the internal annotation keep current controller auto-pause behavior. Their explicit `ShutdownTime` is not recalculated by this feature.

Historical E2B-created sandboxes without the internal annotation are not bulk-migrated by this design. Later E2B timeout writes may use the default retention for calculation, and E2B manual pause backfills the annotation when the pause update wins.

Rollout order is less fragile than a controller-only recalculation design because new E2B timeout writes already store `ShutdownTime=PauseTime+pausedRetention`. Upgrade the sandbox controller before or together with the E2B server changes when possible so actual auto-pause still refreshes the deadline from the real pause time. If the old controller is temporarily running with the new E2B server, retention is still anchored at scheduled `PauseTime`, not actual controller processing time.

Manual pause user-visible `EndAt` changes from approximately `now + 1000 years` to `now + 100 years` by default. This is an intentional behavior change. The 100-year value matches the existing `noServerTimeout` convention used by create server timeouts and stays within `time.Duration`'s roughly 292-year representable range; 1000 years does not.

The old auto-pause model also gave running auto-pause sandboxes a `ShutdownTime=now+maxTimeout` hard deletion fallback. The new model changes that fallback to `PauseTime+pausedRetention`, which can be much later. If controller auto-pause keeps failing while delete reconciliation would otherwise work, a sandbox can run longer than before. This is an accepted tradeoff for making paused retention independent of running timeout limits; pause and delete are both controller-driven paths and usually share the same failure domain.

## Testing Plan

Add or update focused table-driven tests.

Shared timeout tests:

- `default` parses to 100 years.
- Positive duration parses successfully.
- Invalid strings, empty strings, zero durations, negative durations, `never`, and `forever` fail.
- Absent annotation is distinguishable from `default`.

E2B create tests:

- Missing metadata persists internal annotation value `default`.
- Explicit `default` persists `default`.
- Explicit duration persists the duration.
- Invalid metadata returns a bad request.
- `autoPause=true` computes `ShutdownTime` from `PauseTime + retention`, not `sc.maxTimeout`.
- `never-timeout` keeps timeout fields nil.
- Internal annotation is not returned as ordinary response metadata.
- Connect-after-create regression: after connect on an auto-pause sandbox, `ShutdownTime` is `new PauseTime + retention`, not `now + maxTimeout`.

E2B connect/resume/set-timeout tests:

- Auto-pause timeout writes use `ShutdownTime=PauseTime+defaultRetention` when annotation is `default`.
- Auto-pause timeout writes use `ShutdownTime=PauseTime+customRetention` when annotation is a duration.
- Auto-pause timeout writes fall back to default retention when annotation is absent.
- Auto-pause timeout writes persist `default` in the same update when annotation is absent and the timeout write is accepted.
- Invalid persisted retention annotation on these request paths fails open: use default retention, preserve the invalid annotation, and still write timeout when the existing update policy accepts the update.
- Non-auto-pause timeout writes keep existing `ShutdownTime=now+requestedTimeout` behavior.
- Connect/resume extend-only behavior is unchanged except that the accepted requested timeout writes a retention-based `ShutdownTime`.
- Resume placeholder timeout write backfills `default` when annotation is absent and the resume mutation accepts the timeout write.
- ExtendOnly skip does not create an annotation-only update.

E2B pause tests:

- No header uses persisted annotation.
- Header `default` uses 100 years and persists `default`.
- Header duration uses that duration and persists it.
- No header and no annotation uses `default` and persists `default` when the pause update wins.
- No header and invalid annotation uses `default`, preserves the invalid annotation, and completes pause when the pause update wins.
- Valid header with invalid annotation uses the header value and persists it when the pause update wins.
- Invalid header returns a bad request.
- Already paused sandbox preserves first-writer-wins and does not update annotation.
- Never-timeout pause does not set a shutdown deadline.

Controller tests:

- Expired `PauseTime` with internal annotation `default` patches `Paused=true` and `ShutdownTime=now+100 years`.
- Expired `PauseTime` with duration annotation patches `ShutdownTime=now+duration`.
- Expired `PauseTime` without annotation patches only `Paused=true`.
- Expired `PauseTime` with annotation and nil `ShutdownTime` patches only `Paused=true`.
- Conflict behavior still requeues and leaves stored spec unchanged.
- Invalid annotation patches only `Paused=true`, leaves `ShutdownTime` and annotation unchanged, and records a warning.

Infra pause tests:

- Pause writes annotation and timeout atomically when it wins.
- Pause does not write annotation when latest Sandbox is already paused.
- Existing annotations are preserved.

Infra timeout/resume tests:

- Accepted timeout save writes timeout fields and `ReservePausedFor` annotation in one retry update.
- Skipped timeout save under ExtendOnly does not write an annotation-only update.
- Resume placeholder timeout write persists `ReservePausedFor` when the resume mutation wins.
- Resume does not write `ReservePausedFor` when the latest Sandbox is already resumed and no timeout mutation is accepted.

Lifecycle regression tests:

- create with `autoPause=true` and custom retention, connect, controller auto-pause, resume, and manual pause.
- create with `autoPause=true` and default retention, connect, controller auto-pause, and verify connect writes `ShutdownTime=PauseTime+retention` while controller auto-pause refreshes it to actual auto-pause time plus retention.

## Acceptance Criteria

- Manual pause without a header uses the persisted annotation; E2B-created sandboxes default to 100 years through persisted `default`.
- Manual pause without a header on a sandbox missing the internal annotation persists `default` if the pause update wins.
- Manual pause with `x-e2b-kruise-reserve-paused-sandbox-for` uses that value and persists it to `agents.kruise.io/reserve-paused-sandbox-for` only when the pause update wins.
- E2B create always persists `agents.kruise.io/reserve-paused-sandbox-for` as `default` or a validated duration.
- E2B create with `autoPause=true` computes initial `ShutdownTime` from `PauseTime + pausedRetention`.
- E2B connect, resume, and set-timeout on auto-pause sandboxes compute `ShutdownTime` from `PauseTime + pausedRetention`, not `maxTimeout`.
- E2B connect, resume, and set-timeout backfill `agents.kruise.io/reserve-paused-sandbox-for=default` for auto-pause sandboxes missing the annotation when the timeout update is accepted.
- Controller auto-pause recalculates `ShutdownTime` only for sandboxes with `agents.kruise.io/reserve-paused-sandbox-for` and an existing non-nil `ShutdownTime`.
- Controller auto-pause with an invalid retention annotation still patches `Paused=true` and records a warning instead of blocking reconcile.
- Sandboxes without the internal annotation keep existing CRD behavior.
- `never-timeout` sandboxes do not receive a shutdown deadline from paused retention.
- Invalid metadata, header, or annotation values are handled explicitly and never panic.
- Invalid persisted annotation values in E2B request paths are fail-open; invalid request metadata/header values remain client errors.
- Existing create/connect/resume/set-timeout requested-timeout validation and extend-only semantics are preserved.

## Implementation Notes For Coding Agent

- Helper names and exact file organization under `pkg/utils/timeout` can be chosen during implementation.
- Test file placement should follow nearby package conventions.
- Do not add new generic utility packages.
- Do not import `pkg/servers/e2b/models` from controller code.
- Do not keep the old `buildSetTimeoutOptions(autoPause, now, timeoutSeconds)` shape if it forces auto-pause callers to use `sc.maxTimeout`; pass resolved paused retention explicitly or introduce a purpose-specific helper.
- Keep annotation backfill coupled to accepted timeout updates for connect/resume ExtendOnly paths; skipped timeout updates should not create annotation-only writes.
- Keep comments in English.
