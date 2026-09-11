---
title: SandboxClaim Auto-Pause Overlay
authors:
  - "@sophon"
reviewers:
  - "@TBD"
creation-date: 2026-09-09
last-updated: 2026-09-11
status: implementable
see-also:
  - "/docs/proposals/20260626-sandbox-auto-pause.md"
  - "/docs/proposals/20260627-wake-on-traffic-via-spec-patch.md"
  - "/docs/proposals/20251229-sandbox-claim-crd.md"
---

# SandboxClaim Auto-Pause Overlay

## Summary

Sandbox can already perform a one-shot scheduled pause via `spec.pauseTime`, and probe-driven pause/resume and inbound-traffic wake-up via `spec.autoPausePolicy`. SandboxClaim currently only overlays `shutdownTime`, so callers cannot specify either kind of pause when claiming. The final state is: Claim adds three optional fields, `spec.pauseTime`, `spec.autoPausePolicy`, and `spec.probes`, and writes them to the Sandbox in one operation before the claim is persisted; the policy and deadline inherit the pool configuration when omitted; probes are merged by name into the candidate's existing probe set (same-name entries are replaced and new names are appended); invalid configuration completes the Claim with `InvalidClaimSpec` and is not retried. The pause runtime is unchanged.

## Background

Pool Sandboxes copy `probes` and `autoPausePolicy` from the SandboxSet when they are created. After a Claim, only claimed members enter the auto-pause decision, while unclaimed pool members are skipped; recycle clears `pauseTime`/`shutdownTime` and restores the policy from the SandboxSet. E2B create can already use `autoPause` to write `PauseTime`, and use `autoResume` to partially write `OnIngressTraffic`. As a declarative object, the Claim CR is missing an overlay with the same shape as `shutdownTime`; it does not need another set of E2B relative seconds and a bool.

Without a Claim-level overlay, each tenant cannot override the pool's default policy or declare “pause at a specific time”; it can only modify the SandboxSet (affecting the entire pool) or bypass Claim and modify the Sandbox directly.

## Final Design

### Scope and Non-Goals

In scope:

- `SandboxClaim.spec.pauseTime`: an absolute time, written to `Sandbox.spec.pauseTime`
- `SandboxClaim.spec.autoPausePolicy`: reuses the existing `AutoPausePolicy` type and writes the entire value to `Sandbox.spec.autoPausePolicy`
- `SandboxClaim.spec.probes`: reuses the existing `Probe` type and merges by name into the candidate's `Sandbox.spec.probes` (same-name entries are replaced and new names are appended)
- The Claim controller validates and overlays these fields before every claim attempt, at the same time as the existing labels, annotations, and `shutdownTime`

Out of scope:

- Do not add an `autoPause` bool, relative timeout, or paused-retention annotation, and do not derive `shutdownTime` from `pauseTime`
- Do not add an independent `autoResume` field; traffic wake-up is expressed through `autoPausePolicy.resume.onIngressTraffic`
- Do not create a new SandboxClaim webhook; do not change the sandbox controller, gateway, recycle, or E2B HTTP (E2B does not expose probe configuration, keeping this capability exclusive to SandboxClaim)
- Do not turn Claim into a continuous controller: after Completed, it still does not write back to already claimed Sandboxes

### Ownership and Data Flow

```
SandboxClaim spec --> SandboxClaim controller --overlay at claim time--> claimed Sandbox
SandboxSet probes and autoPausePolicy --> pool Sandbox --> claimed Sandbox
claimed Sandbox --> checkTimers (pauseTime)
claimed Sandbox --> handleAutoPause (autoPausePolicy)
claimed Sandbox --> gateway (OnIngressTraffic)
claimed Sandbox --recycle--> pool Sandbox
```

- Claim is responsible only for writing the caller's intent into the Sandbox spec
- Pause and resume execution remains entirely owned by the existing Sandbox controller and gateway
- The pool default is still declared by the SandboxSet; recycle still restores policy from the SandboxSet and clears deadlines

### Exposed Interface

`SandboxClaimSpec` adds two optional pointer fields, with semantics aligned with the existing `shutdownTime`:

- `pauseTime *metav1.Time`: omitted or nil means do not overlay `Sandbox.spec.pauseTime`
- `autoPausePolicy *AutoPausePolicy`: omitted or nil means do not overlay the policy, preserving the policy already on the pool Sandbox

No new policy type is introduced. The fields, defaults, and cross-field rules of `AutoPausePolicy` are the same as for Sandbox / SandboxSet.

### Write Rules

For every Sandbox claimed successfully:

1. **One-time application.** The overlay occurs only in the update that successfully claims that Sandbox. After the Claim enters Completed, later changes to the Claim spec do not modify an already claimed Sandbox.
2. **Inherit when omitted.** When both fields are nil, the Sandbox preserves its pre-claim state: a pool member's `autoPausePolicy` comes from the SandboxSet, and `pauseTime` is already empty on the pool.
3. **Whole-value replacement.** When `autoPausePolicy` is non-nil, the entire policy in the Claim replaces the policy on the Sandbox; no field-level merge is performed. If the caller only wants to enable `OnIngressTraffic` while retaining the pool's idle/cron rules, it must write the complete policy in the Claim.
4. **Paired deadline write.** If the Claim specifies either `pauseTime` or `shutdownTime`, set both corresponding Sandbox fields to the Claim's current values: the side that is nil on the Claim is cleared on the Sandbox. If neither is specified, do not modify the Sandbox deadlines.
5. **Do not derive deletion time.** Expiration of `pauseTime` only pauses; deletion is still controlled solely by `shutdownTime`. To “pause and then delete,” the caller must provide both absolute times.
6. **Batch consistency.** Multiple replicas successfully claimed by the same Claim in one reconcile carry the same overlay.
7. **Merge probes by name.** When claim `probes` is non-nil, merge it by name with the candidate Sandbox's `spec.probes`: a same-name item is replaced by the Claim version (the pool position is preserved), and a new name is appended after the pool sequence; when the Claim does not include probes, leave them unchanged. The merged set for each candidate Sandbox must not exceed the Sandbox probe limit (16); see “Validation and Failure” for Claim-level merge limits and candidate compatibility checks. Unlike the single-value `autoPausePolicy`, which is replaced in full, probes are collection configuration and are merged by key like labels/annotations; see “Claim-Supplied Probes” for the trade-off.

### Validation and Failure

The Claim validates `autoPausePolicy` and `probes` when building the claim options:

- Use the same validation rules as SandboxSet admission
- The Claim's `probes` list itself must pass `ValidateProbes` (unique names, exec-only, and names usable as condition types, as with SandboxSet)
- Referenced probe names are evaluated against the set obtained by merging the current `SandboxSet.spec.probes` and `claim.probes` by name (template probes are ineffective for running pool members, consistent with the existing copy rules); when the Claim has no policy, the pool policy does not need to be revalidated—the merge can only add probe names, so references in the pool policy cannot become invalid
- When the set obtained by merging the current `SandboxSet.spec.probes` and `claim.probes` by name exceeds 16, complete the Claim with `InvalidClaimSpec`: provide a clear error early instead of waiting for the apiserver to reject the Sandbox update
- After Claim-level validation passes, recheck each candidate Sandbox's own `spec.probes` at claim time, excluding names supplied by the Claim: a Claim-supplied probe need not be declared by the candidate in advance and is merged in at claim time; an old candidate during a rolling update that lacks another referenced probe remains in the pool and waits for a compatible candidate, rather than receiving a policy it cannot execute
- If merging a candidate Sandbox's existing probes with the Claim probes exceeds 16, skip that candidate and retry through the no-available-Sandbox path; the `createOnNoStock` creation path also treats this as a retryable `NoAvailableError`, rather than classifying a candidate-level limit violation as `InvalidClaimSpec`
- A policy containing only `OnIngressTraffic` and no probe rules is valid
- An empty policy (with no pause/resume rules), a `messageRegex` that cannot be compiled, and a reference to a nonexistent probe are all invalid

The observable results for invalid input are the same as for the existing reserved identity-key validation:

- Do not continue claiming
- Complete the Claim
- Set `status.conditions[type=Completed].reason` to `InvalidClaimSpec`
- Emit a Warning event, `InvalidClaimSpec`
- Sandboxes successfully claimed before this reconcile retain their written state; they are not rolled back

A candidate that lacks a probe referenced by the current valid policy, or whose probes exceed 16 after merging with the Claim probes, represents temporarily incompatible pool capacity, not an invalid Claim spec: do not claim that candidate in this round and retry through the existing no-available-Sandbox path; when `createOnNoStock` is enabled, a compatible instance can be created from the current SandboxSet if it can yield a compatible probe set.

The apiserver still accepts objects whose field shapes are valid; cross-field rules do not rely on a new webhook. If an invalid policy is written to a Sandbox by bypassing validation, the existing Sandbox-side `ProbeValid=False` still rejects probe-driven pause, but that is not the primary Claim path.

### Relationship to Existing Pause Mechanisms

After the write completes, the Sandbox behaves as it does today when its spec is edited directly:

- `pauseTime` is unconditionally executed by `checkTimers` and does not depend on `AutoPauseController`
- Probe-driven pause/resume still requires that gate to be enabled, and the probe referenced by the rule must actually exist in the Sandbox spec (under this design, in the merged probes; the sandbox controller patches `kruise.io/podprobe` on the running Pod, and changes to `spec.probes` take effect while it is Running)
- `OnIngressTraffic` is still executed by the gateway and does not enable the probe decision loop
- `pauseTime` and `autoPausePolicy` coexist: whichever expires first pauses the Sandbox; even while a probe reports active, `pauseTime` still pauses it. Callers that need the probe to be the final authority should not set `pauseTime`

Writing a policy from a Claim does not enable the feature gate. When the gate is disabled, the policy still appears in the Sandbox spec, but probe injection and probe decisions do not run; `pauseTime` still takes effect.

### Compatibility and Upgrade

- Existing Claims that do not write these two fields behave exactly as they do now
- A claimed Sandbox for which the caller did not overlay a policy continues to use the pool policy
- After recycle, a pool member is restored to the SandboxSet's `probes` and `autoPausePolicy`, and `pauseTime` is cleared; the next Claim applies its new overlay
- E2B HTTP is unchanged; E2B continues to use a bool plus relative seconds, while Claim uses absolute times

### Observable Examples

**Inherit the pool policy.** The Claim omits the two new fields. After claiming, the Sandbox has the SandboxSet's `autoPausePolicy`, and `pauseTime` is empty. If the pool policy contains an idle probe and the gate is enabled, idle timing starts at `claim-timestamp` and does not immediately pause because of idle time accumulated during warm-up.

**Scheduled pause only.** The Claim sets only `pauseTime`, without `autoPausePolicy` or `shutdownTime`. The Sandbox receives that `pauseTime`, retains the pool policy, and still has an empty `shutdownTime`. It pauses on expiration and is not deleted.

**Policy override only.** The pool has an idle rule. The Claim provides an `autoPausePolicy` containing only `resume.onIngressTraffic`. The idle rule on the Sandbox is replaced, leaving only traffic wake-up. If the caller also wants the idle rule, it must include it in the Claim policy.

**Invalid reference.** The SandboxSet has no probe named `Active`, while the Claim's pause rule references `Active`. The Claim completes with reason=`InvalidClaimSpec`, and no new Sandbox is claimed by this failed options build.

**Probe as the final authority.** The Claim sets only `autoPausePolicy.pause.whenProbedIdleState`, without `pauseTime`. Expiration behavior is determined only by the probe.

**Both triggers set.** The Claim sets both `pauseTime` and an idle policy. If `pauseTime` has passed while the probe still reports active, the Sandbox still pauses.

## Alternatives and Trade-offs

- **Use E2B's `autoPause` bool on Claim.** This is inconsistent with the existing absolute `shutdownTime` and would also move relative seconds and retention arithmetic into Claim. Rejected.
- **Merge `OnIngressTraffic` at the field level.** This is more convenient for “enable wake-up only,” but a declarative CR would not reveal the final policy, and it would be inconsistent with whole-value copying from SandboxSet. Rejected; the caller writes the complete policy.
- **Claim-supplied `probes`.** Implemented; the merge semantics and trade-offs are described in “Claim-Supplied Probes.”
- **Create a Claim webhook.** Sandbox itself has no webhook either; cross-validation depends on SandboxSet and is suitable for the existing Claim controller failure path.

## Risks

- Whole-value replacement removes pool rules that the caller did not copy. This is deliberate visibility, rather than a silent merge.
- When `pauseTime` and a probe policy coexist, the timer can interrupt an Agent that is still working. This is consistent with the current Sandbox proposal; the caller chooses not to set both.
- `AutoPauseController` is disabled by default: overlaying only the policy without enabling the gate means the probe path does not run, which can make it appear that “the Claim did not take effect.” The probe path checks the gate; `pauseTime` does not.

## Claim-Supplied Probes

Implemented as part of this design: `SandboxClaim.spec.probes` is optional and is merged by name into the candidate Sandbox's `spec.probes`.

### Motivation

A probe-driven policy requires referenced probes to be declared in advance on the target SandboxSet. When different tenants need different probing logic (different exec commands or probe intervals), the SandboxSet must enumerate all probe variants (`MaxItems=16`), and every pool member executes all probes indiscriminately—even though most Claims do not use them and still consume resources. Claim-supplied probes allow each Claim to declare and execute only the probes it needs.

### Technical Foundation (Already Supported by the Current Code)

- The sandbox controller's `EnsureProbe` patches the `kruise.io/podprobe` annotation on a running Pod, so changes to `spec.probes` take effect while it is Running; probe results are added and removed in sync with `spec.probes`, and a probe condition left by the previous Claim is removed
- Recycle's `resetSandboxForPool` already restores `spec.probes` and `spec.autoPausePolicy` from the SandboxSet verbatim, so probes written by a Claim do not leak to the next tenant

### Implementation: Merge by Name, Not Whole-Value Replacement

Probes copied from the SandboxSet to a candidate are **merged by name** (`+listType=map`'s natural semantics):

- A Claim probe with the same name replaces the pool version, allowing the tenant to customize probe parameters (command and interval), while preserving the pool position
- A Claim probe with a new name is appended after the pool sequence
- Claim-supplied probes must pass `ValidateProbes`; SandboxSet probes have already been validated against the same rules at admission, and the merged set is then checked against `MaxItems=16`

Final self-consistency validation (the controller holds the SandboxSet during claim):

- `finalProbes = merge(sandboxSet.spec.probes, claim.probes)` (the Claim side wins for same names)
- `ValidateProbes(claim.probes)` + `ValidateAutoPausePolicy(claim.autoPausePolicy, finalProbes)`
- Complete with `InvalidClaimSpec` when the merged set exceeds the limit

Candidate filtering retains the existing mechanism and derives required probes from the policy while subtracting Claim-supplied names: `required = PolicyProbeNames(finalPolicy) − names(claim.probes)`. In other words, a Claim-supplied probe need not be declared by a candidate in advance. Before persistence, `modifyPickedSandbox` deep-copies the merged result into `spec.probes` (including the `createOnNoStock` creation path); there is no persisted intermediate state, and the merged result does not alias the Claim or pool spec.

### Merge vs. Replace Trade-off

The alternative whole-value replacement semantics (when claim.probes is non-nil, replace all Sandbox probes) was rejected:

- **Administrator preconfiguration plus caller addition is the expected shape.** SandboxSet probes are declared by platform administrators (platform-level security audits and compliance checks), and the Claim caller adds its own probes on top. Whole-value replacement would let a tenant's Claim remove platform probes until recycle restores them—creating a detection blind spot during the Claim and effectively letting the caller decide the fate of platform configuration
- **A probe is a general mechanism, not an attachment of the policy.** The `Probe` contract is that “the probe itself defines no semantics; the consumer does”; a second consumer already exists (mirroring probe results to `SandboxStatus.Conditions` for observability), and more may appear in the future. Whole-value replacement's “the probe set switches together with the policy” only works when the policy is the sole consumer
- **Consistent with Claim metadata semantics.** Claim labels/annotations are merged by key (`MergePodLabels`/`MergePodAnnotations`) rather than replaced wholesale because the object already has data from other sources; probes are also collection configuration. `autoPausePolicy` has single-value semantics and is therefore replaced in full—the merge rules are naturally different for a single value and a collection

Costs of merging and mitigations:

- **The final probe set can be inferred only after merging:** the Claim alone does not show it. Current validation errors and `InvalidClaimSpec` events report only the specific validation error or limit, not the complete merged result; moreover, “referencing a pool probe” is already a runtime dependency (if a probe name referenced by the Claim policy is absent from the merged set of the current SandboxSet and Claim, the Claim completes with `InvalidClaimSpec`; retries apply only when a candidate is incompatible)
- **Same-name replacement can obscure a pool probe:** current Claim validation does not emit a separate event identifying overridden pool probe names; overrides must be inferred from the Claim and pool configurations
- **Accumulated merges can exceed the limit:** both Claim-level and candidate-level checks enforce `MaxItems=16` on the merged result

### Closed Open Questions

- When a Claim completes before a new probe has produced its first result, `thresholdDuration` starts from the first result: this is consistent with existing Sandbox probe semantics, and WaitReady does not additionally cover probe readiness
- `createOnNoStock` path: merging completes in memory before the Claim is persisted (`modifyPickedSandbox`), so there is no persisted intermediate state in which pool probes are copied and then merged
- E2B HTTP: does not expose probe configuration, keeping this capability exclusive to SandboxClaim
