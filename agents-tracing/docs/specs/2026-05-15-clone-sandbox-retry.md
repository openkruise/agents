# Clone Sandbox Retry and Failed Sandbox Reservation Design

## Context

`ClaimSandbox` already has an outer retry loop in `Infra.ClaimSandbox`. Each
single claim attempt returns `retriableError` for failures that should trigger a
new attempt, and failed locked sandboxes are cleaned up by the claim path.

`CloneSandbox` currently runs one attempt only. If a cloned sandbox is created
and then fails during readiness wait or runtime initialization, the caller gets
the error without the same retry behavior as claim. Failed sandboxes are also
deleted immediately today, which removes debugging evidence too quickly.

## Goals

- Give `CloneSandbox` retry behavior aligned with `ClaimSandbox`.
- Use one failed-sandbox cleanup helper for both claim and clone.
- Keep failed sandboxes for 24 hours by default before deletion.
- Allow E2B metadata to configure failed sandbox reservation as `never`,
  `forever`, or a Go duration string such as `600s`, `24h`, or `3600m`.
- Preserve the old E2B `reserve-failed-sandbox=true` protocol by converting it
  at the E2B layer to the new internal reservation duration.

## Non-Goals

- Do not add a new CRD field in this change.
- Do not change the existing `SandboxClaim` API shape.
- Do not make clone CSI mount failures retriable.
- Do not change normal sandbox timeout semantics outside failed-sandbox cleanup.

## Option Model

Add this field to both `infra.ClaimSandboxOptions` and
`infra.CloneSandboxOptions`:

```go
ReserveFailedSandboxFor *time.Duration
```

After `ValidateAndInitClaimOptions` or `ValidateAndInitCloneOptions`, the field
is always non-nil.

Internal duration semantics:

- `nil` before validation means no explicit preference. Validation sets the
  default to `24 * time.Hour`.
- `0` means delete the failed sandbox immediately. This corresponds to metadata
  value `never`.
- A positive duration means reserve the failed sandbox for that amount of time.
- A negative duration means reserve forever. This corresponds to metadata value
  `forever`.

`infra.ClaimSandboxOptions` should no longer use `ReserveFailedSandbox bool`.
Compatibility is handled before options reach infra:

- E2B old metadata `reserve-failed-sandbox=true` becomes
  `ReserveFailedSandboxFor = -1`.
- `SandboxClaimSpec.ReserveFailedSandbox=true` becomes
  `ReserveFailedSandboxFor = -1` when building claim options for the controller.
- The new duration field has priority over the old boolean protocol wherever
  both are present.

## E2B Metadata

Add a new metadata extension key:

```text
e2b.agents.kruise.io/reserve-failed-sandbox-for
```

Parsing rules:

- `never` maps to duration `0`.
- `forever` maps to duration `-1`.
- Any other value is parsed with `time.ParseDuration`.
- Parsed negative duration strings, such as `-1h`, are invalid. The user must
  use `forever`.
- Parsed zero duration strings are accepted and have the same behavior as
  `never`.

The parser stores the result in `NewSandboxRequestExtension`, and
`createSandboxWithClaim` and `createSandboxWithClone` both pass it to infra.

The old `e2b.agents.kruise.io/reserve-failed-sandbox` key remains supported for
claim requests. It only applies when the new key is not present.

## Failed Sandbox Cleanup

Replace the claim-only cleanup helper with a shared helper that accepts the
normalized reservation duration:

```go
func clearFailedSandbox(ctx context.Context, sbx infra.Sandbox, err error, reserveFor time.Duration)
```

Behavior:

- If `err == nil` or `sbx == nil`, do nothing.
- If `reserveFor == 0`, delete immediately with `Kill`.
- If `reserveFor > 0`, set `ShutdownTime` to `time.Now().Add(reserveFor)` by
  calling `SaveTimeoutWithPolicy` with `timeout.UpdatePolicyAlways`.
- If `reserveFor < 0`, keep the sandbox forever and do not write
  `ShutdownTime`.

Cleanup errors are logged and do not replace the original claim or clone error.
This preserves the user-facing failure reason.

## Clone Retry Design

Keep retry orchestration in `Infra.CloneSandbox`, mirroring
`Infra.ClaimSandbox`:

- Validate and initialize clone options.
- Create a timeout context using `CloneTimeout`.
- Retry with the same `retry.OnError` / `retriableError` pattern used by claim.
- Accumulate clone metrics across attempts.
- Return the cloned sandbox from the first successful attempt.

The lower-level `CloneSandbox` function remains one concrete clone attempt. It
tracks the sandbox once creation succeeds. A defer calls the shared cleanup
helper if that attempt fails after a sandbox has been created.

Retriable clone failures:

- Sandbox create API call fails. Product preference: callers would rather wait
  out a retry loop than see a 500 on transient apiserver/network errors. The
  trade-off is orphan amplification: if `Create` returned an error to the client
  after the apiserver had already persisted the CR, the retry produces a second
  CR. The clone path uses `GenerateName` and does not get an `IsAlreadyExists`
  signal, so these orphans are not visible to `clearFailedSandbox` and must be
  reaped out-of-band.
- Waiting for sandbox readiness fails.
- Re-initializing runtime fails.

Non-retriable clone failures:

- Checkpoint/template lookup fails.
- CSI mount fails.
- CSI mount options cannot be resolved from annotations.

Even for non-retriable failures after sandbox creation, the shared cleanup
helper still applies the configured reservation behavior to the failed sandbox.

## Metrics and Errors

`CloneMetrics` should continue to report timing for wait, template lookup,
create, wait ready, runtime init, CSI mount, and total. The outer retry loop
should aggregate timings from every attempt, similar to claim metrics.

No new public error type is required. Clone uses `retriableError` internally to
control retry, then returns the final error to callers.

## Tests

Use table-driven tests.

Add or update tests for:

- `ValidateAndInitClaimOptions` defaulting `ReserveFailedSandboxFor` to `24h`.
- `ValidateAndInitCloneOptions` defaulting `ReserveFailedSandboxFor` to `24h`.
- Explicit internal values `0`, positive duration, and negative duration.
- E2B metadata parsing for `never`, `forever`, positive durations, zero
  durations, invalid duration strings, and negative duration strings.
- New metadata overriding old `reserve-failed-sandbox=true`.
- E2B claim and clone option construction passing `ReserveFailedSandboxFor`.
- SandboxClaim controller option construction converting
  `ReserveFailedSandbox=true` to `ReserveFailedSandboxFor=-1`.
- Failed cleanup deleting immediately.
- Failed cleanup setting `ShutdownTime` with `UpdatePolicyAlways`.
- Failed cleanup keeping forever.
- Clone retrying readiness and runtime-init failures.
- Clone not retrying CSI mount failures.
- Clone applying failed-sandbox reservation for non-retriable failures after
  sandbox creation.
