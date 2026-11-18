# Opt-in cold recovery after Sandbox Pod eviction

## Problem

The Sandbox controller maps every failed Pod to `SandboxFailed` and stops
reconciling terminal Sandboxes. A kubelet eviction therefore ends a delivered
Sandbox even when its PVC-backed filesystem survives. `restartPolicy: Always`
only restarts containers within a Pod; it cannot replace a terminal Pod.
Existing terminal behavior is intentional and remains the default.

Two related problems make recovery unsafe without additional changes: terminal
Sandboxes can retain `Ready=True`, and status merge patches that omit cleared
fields leave old Pod addresses and lifecycle markers in storage. At the E2B
boundary, deleting an owned failed Sandbox currently returns success without
requesting deletion because list visibility is reused as delete eligibility.

## API and scope

Enable the alpha controller feature gate `SandboxEvictionRecovery` and opt in
through the Sandbox or SandboxSet spec:

```yaml
spec:
  pauseStrategy:
    type: Stop
  recoveryPolicy:
    type: OnEvicted
    maxAttempts: 3
    backoffSeconds: 30
    timeoutSeconds: 600
    healthyResetSeconds: 600
```

Omitting the policy, selecting `Never`, or leaving the gate disabled preserves
terminal behavior. The policy is copied through pool materialization and
checkpoint/clone templates and participates in both inline and referenced
SandboxSet revisions. SessionService's embedded template also carries it.

Initial support covers delivered or standalone Sandboxes using Stop, Immediate
startup and an inline Pod template. Pool materialization already resolves a
SandboxSet templateRef into an inline template. Unclaimed SandboxSet members
continue to use pool replacement. Direct mutable Sandbox templateRefs, Deferred
startup, memory preservation and IP preservation are unsupported. Invalid cold
recovery inputs prevent recording a healthy baseline and are reported through
`Ready=False/RecoveryBaselineUnavailable`.

Only `Pod.status.phase=Failed` and `Pod.status.reason=Evicted`, with the current
Sandbox controller owner UID, start recovery. Ordinary application failure,
container OOM, successful completion, missing Pods and node disconnection do
not. Already Failed Sandboxes are not retroactively resurrected: operators must
enable the policy while a healthy Pod and its original volume identities can
still be observed.

This is cold recovery. Processes restart; memory, active commands, connections,
and streaming SDK sessions are not preserved. Only data on retained PVCs is
preserved. Root filesystem persistence still requires an appropriate storage
integration such as RootPV; the controller does not implement that integration.

## Durable state machine

Before publishing Ready, the controller records `status.recovery`: the healthy
Pod UID, source node, construction-input hash, PVC/PV names and UIDs, and
observed container image digests. Images are pinned during reconstruction and
checked again before initialization, so mutable tags do not introduce an
implicit upgrade. Operators must keep runtime injection topology and admission
configuration compatible with the recorded workload.

1. On eviction, persist `Recovering`, revoke Ready and both published Pod IP
   fields, and reset RuntimeInitialized before performing any Pod action.
2. Validate the recorded template and volume bindings. A missing, replaced,
   unbound or deleting PVC/PV blocks recovery; the PVC provisioning path is
   bypassed after a healthy baseline has been recorded. There is no fallback
   to a newly provisioned empty disk.
3. Delete only the recorded evicted Pod, with its UID as a precondition. Use
   normal graceful deletion and existing delete expectations. Wait for actual
   Pod absence even if its deletionTimestamp has satisfied the expectation.
4. Persist an attempt reservation and its backoff deadline before creation.
   Repeated API calls, AlreadyExists and controller restarts reuse the same
   reservation. A deterministic annotation associates the replacement with
   that source Pod UID and attempt; unknown same-name Pods are rejected.
5. Wait for the source node to be Ready, schedulable and free of observed
   memory/disk/PID pressure. Poll with a bounded requeue. Kubernetes scheduling
   and CSI topology still determine placement; this does not provide fencing
   for migration away from an unreachable node.
6. Create through the existing PodControl and normal admission, preserving the
   Sandbox object, credentials, claimed identity and original PVC references.
   Record the new Pod UID before initializing it.
7. Once the Pod is Running/Ready, run the existing runtime/security-token/CSI
   initializer against its new address. Initialization retries reuse that Pod
   and do not consume another attempt. Publish Running/Ready and the new IP
   together only after initialization succeeds. Do not invoke PostResume.

Repeated eviction within a recovery episode keeps its original timeout and
attempt budget. A sustained Ready interval resets the budget; momentary Ready
does not. Timeout and exhausted budgets end in Failed. Missing replacement
Pods and non-eviction replacement failures also end in Failed.

Deletion and an expired shutdown deadline take precedence over recovery.
Pausing cancels recovery, deletes any existing Pod and reaches Stop/Paused
only after actual deletion. Recovery never extends lifetime or reclaims an ID.
Disabling the gate/policy during recovery stops it with Failed; operators
should drain active Recovering instances before rollback.

Status patches are computed from old and new values so cleared fields become
explicit removals. Recovery writes include UID and resourceVersion guards,
and continue using resource-version expectations. Legacy status writes retain
their existing concurrency behavior.

## State consumers and E2B behavior

The internal phase and route state are `Recovering` / `recovering`. Route
projection preserves instance identity and withdraws its endpoint even if an
old IP is still present in a stale object. Existing gateway admission serves
only running routes, so recovery cannot trigger normal paused traffic wakeup.
Quota remains allocated until actual deletion. Token refresh only operates on
Running instances; the recovery initializer refreshes credentials when needed.

The E2B public state enum remains running/paused. GET and List project recovery
as running intent. Both connect endpoints authenticate and check ownership,
then return 503 during recovery without changing pause state or deadlines.
Callers retry connect; after recovery the original Sandbox ID and access token
remain usable. This first version does not synchronously wait inside connect
or promise transparent retry of user operations. The upstream
[E2B OpenAPI](https://github.com/e2b-dev/E2B/blob/main/spec/openapi.yml) permits
503 for both connect endpoints.

An explicit, authorized kill of Failed/Succeeded Sandboxes now requests actual
deletion and follows the existing quota-release flow. Reserved failed objects
and cross-owner lookups retain their existing protections; missing or already
terminating objects keep idempotent responses.

## Rollout and validation

Install generated CRDs first, then update Manager/Gateway consumers before
enabling the controller gate. Initially opt in only test templates and wait for
their healthy baselines. An old Manager can misclassify the new phase, so mixed
versions must not enable recovery. No RootPV code changes or new Prometheus
collectors are required.

Regression coverage includes default-off compatibility, structured eviction
classification, failed intent writes, original volume identity, image identity,
UID-preconditioned deletion, durable attempts, pressure/backoff waiting,
initialization failure, repeated eviction, deadlines, pause, route withdrawal,
SDK state projection/connect responses and real deletion of terminal objects.
Integration validation must additionally exercise a real kubelet eviction,
same-Sandbox/new-Pod identity, retained volume contents and controller restart.
Memory-pressure, disk-pressure and each runtime/storage integration require
their own validation; one successful eviction mechanism does not establish all
of them.

### Manual validation on Kubernetes 1.37

The implementation was exercised in an isolated kind/containerd cluster with
the generated CRDs and the controller gate enabled. A Stop/Immediate Sandbox
mounted a dynamically provisioned local-path PVC at `/data` and a 16 MiB
`emptyDir` at `/scratch`. After recording a healthy baseline and writing a
marker on the PVC, writing 64 MiB to `/scratch` caused a real kubelet
`Failed/Evicted` event. No Pod status was patched to simulate eviction.

The controller was stopped and restarted after persisting attempt 1, during
the configured 45-second backoff. Assertions against API objects and events
confirmed:

- The Sandbox, PVC and PV retained their UIDs; the replacement Pod had a new
  UID and IP, and the PVC marker retained its contents.
- Recovering had `Ready=False` and neither published IP field contained the
  old address.
- Recovery completed with `Running`, `Ready=True`, `RuntimeInitialized=True`
  and the same recorded image digest.
- The attempt count remained 1 across restart, and the completed episode's
  start time, retry deadline and replacement UID were actually removed from
  API status.

This smoke test used a plain container and PVC; it did not exercise RootPV,
gVisor, Kata, node-wide resource pressure or SDK runtime initialization over
the network. Runtime initialization failure/new-address handling and E2B
connect/delete behavior are additionally covered by package tests. Each
storage/runtime integration still needs its own rollout validation.
