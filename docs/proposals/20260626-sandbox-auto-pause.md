---
title: Sandbox Auto-Pause and Resume
authors:
  - "@zhaomingshan"
reviewers:
  - "@TBD"
creation-date: 2026-06-26
last-updated: 2026-07-09
status: provisional
see-also:
  - "/docs/proposals/20251218-sandbox-inplace-update.md"
  - "https://openkruise.io/docs/user-manuals/podprobemarker"
---

# Sandbox Auto-Pause and Resume

## Table of Contents

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
  - [Design Overview](#design-overview)
  - [Mode 1: Probe-Driven Decisions](#mode-1-probe-driven-decisions)
  - [Mode 2: Probe-Only Reporting](#mode-2-probe-only-reporting)
  - [Architecture Overview](#architecture-overview)
  - [Interaction with Existing E2B Timeout Mechanism](#interaction-with-existing-e2b-timeout-mechanism)
  - [API Design](#api-design)
  - [Reconcile Logic](#reconcile-logic)
  - [Probe Contract](#probe-contract)
  - [State Machine](#state-machine)
  - [Interaction with SandboxSet](#interaction-with-sandboxset)
- [Alternatives](#alternatives)
- [Risks and Mitigations](#risks-and-mitigations)
- [Upgrade Strategy](#upgrade-strategy)
- [Test Plan](#test-plan)
- [Implementation History](#implementation-history)

## Summary

This proposal introduces two fields on the `Sandbox` CRD: `Spec.Probes` (lifecycle probes) and `AutoPausePolicy` (pause/resume strategy), providing two composable mechanisms:

1. **Probing**: Users define generic named probes in `Spec.Probes`. The controller executes them periodically and writes results to `SandboxStatus.Conditions`. Probes are generic — they do not distinguish between "active" or "cron" types; semantics are assigned by the decision layer.
2. **Decisions**: Optional `AutoPausePolicy.Pause`/`Resume` reference probe names and define when to pause/resume the sandbox based on probe Conditions. If not configured, the controller only reports probe results (Mode 2).

This design targets AI Agent workloads (e.g., OpenClaw), reclaiming compute resources during idle periods while preserving the ability to resume before scheduled tasks trigger. The policy is embedded directly in `Sandbox` — no additional CRD is needed.

## Motivation

AI Agent sandboxes (OpenClaw, code-interpreter, etc.) often sit idle for long periods. Keeping them in the Running state wastes compute resources. On the other hand, Agents with scheduled tasks (e.g., "turn on the AC every day at 18:00") must be resumed **before** the task fires.

The existing mechanism (`Spec.PauseTime`) is a one-shot absolute-time trigger and cannot express activity detection via probes or scheduled-task-aware resume.

### Goals

- **Generic named probes**: Users define any number of probes, each writing results to a Sandbox Condition without preset semantics.
- **Decoupling of probing and decision-making**: Probes always run and report Conditions; decision rules are optional and reference probe names to define semantics.
- **Probe-driven auto-pause/resume**: The controller automatically pauses idle sandboxes and resumes them before scheduled tasks, based on probe results.
- **Probe-only reporting mode**: The controller reports probe results to Conditions without making decisions, allowing upper-layer platforms to implement their own strategies.

### Non-Goals

- **Passive resume triggered by IM messages** is not implemented. For OpenClaw, its Gateway runs inside the sandbox and keeps online status via outbound persistent connections to the IM platform. Once the sandbox is Paused, these connections drop, and IM platform messages cannot reach the Gateway. This scenario depends on the IM platform's offline message queue, webhook retry mechanism, or external resume triggers — all higher-layer solutions outside this proposal.
- **WebSocket / push-based traffic detection** is out of scope. Only exec-based probe contracts are defined.
- **Standalone CRD** (e.g., `SandboxCron`) is intentionally avoided — see [Alternatives](#alternatives).
- **Schedule-driven mode** (cron-based time windows) is not implemented in this version. The API fields for scheduling may be added in a future proposal.
- **Do not modify the existing pause/resume execution path of the Sandbox controller.** The auto-pause logic only manages `Spec.Paused`; actual Pod pause/resume is handled by the existing Sandbox controller.

## Proposal

### User Stories

| Scenario | Role | Requirement | Key Benefit |
|------|------|------|----------|
| **Agent activity protection** | Platform operator | Probe checks Agent before pausing; if busy, pause is delayed | Does not interrupt work the Agent is currently executing |
| **Probe-only reporting** | Upper-layer platform | Controller only reports probe results to Conditions and does not auto-pause; users read Conditions to decide | Decouples probing from decisions; users can implement their own pause policies flexibly |

### Design Overview

This proposal supports two modes:

| Mode | Configuration | Suitable Scenario | Decision Maker |
|------|------|----------|--------|
| **Probe-driven decisions** | `probes` + `autoPausePolicy.pause`/`resume` | Agents with scheduled tasks (e.g., OpenClaw) | Controller (by probe results) |
| **Probe-only reporting** | Only `probes` | Need to implement your own pause/resume strategy | User (reads Conditions) |

> **Gradual adoption**: Start with Mode 2 (probe-only) to verify probe Condition results are correct, then add `AutoPausePolicy.Pause`/`Resume` to enable Mode 1 (auto-pause).

### Mode 1: Probe-Driven Decisions

Configure `probes` + `autoPausePolicy.pause`/`resume`. Probes detect the Agent's actual state, and the controller automatically decides to pause/resume. Suitable for Agents with scheduled tasks, such as OpenClaw — pause when idle, resume before tasks.

Core idea:
1. **Active probe** (every 30s): Detects whether the Agent is active (active sessions + cron tasks running), outputting `"active"`/`"inactive"`
2. **Cron probe** (every 60s): Extracts the next scheduled task timestamp, outputting a Unix timestamp or `"none"`
3. **Pause strategy** (`pause`): Active continuously outputs `"inactive"` matching the regex for `thresholdDuration` (e.g., 15 minutes, measured from the Condition's `lastTransitionTime`) → pause
4. **Resume strategy** (`resume`): Cron outputs a timestamp → automatically resume `leadTime` (5 minutes) before the task

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: Sandbox
metadata:
  name: openclaw-cron
spec:
  probes:
    - name: Active
      exec:
        command:
          - sh
          - -c
          - |
            if openclaw sessions list --active 900 2>/dev/null | grep -q . \
              || openclaw cron list --json 2>/dev/null \
                | jq -e '[.[] | select(.enabled != false and .status == "running")] | length > 0' >/dev/null 2>&1; then
              echo "active"
            else
              echo "inactive"
            fi
      periodSeconds: 30
      timeoutSeconds: 10
    - name: Cron
      exec:
        command:
          - sh
          - -c
          - |
            NEXT_MS=$(openclaw cron list --json 2>/dev/null \
              | jq -r '[.[] | select(.enabled != false) | .nextRunAtMs] | map(select(. != null)) | sort | .[0]')
            if [ -n "$NEXT_MS" ] && [ "$NEXT_MS" != "null" ]; then
              echo $((NEXT_MS / 1000))
            else
              echo "none"
            fi
      periodSeconds: 60
      timeoutSeconds: 10
  autoPausePolicy:
    pause:
      whenIdleProbeFires:
        probe: Active
        messageRegex: "^inactive$"
        thresholdDuration: 15m   # Pause after condition matches for 15 minutes
    resume:
      whenProbedScheduleTime:
        probe: Cron
        timeFormat: unix           # Parse probe message as Unix timestamp
        leadTime: 5m              # Resume 5 minutes before the next scheduled task
```

After pausing, the controller writes the upcoming schedule to `Status.Schedules`:

```yaml
status:
  conditions:
    - type: agents.kruise.io/Active
      status: "True"
      reason: Succeeded
      message: "inactive"
    - type: agents.kruise.io/Cron
      status: "True"
      reason: Succeeded
      message: "1751373600"      # Next scheduled task time
  schedules:
    - reason: probedSchedule
      nextResumeTime: "2026-07-01T17:55:00Z"   # task time - leadTime(5m)
```

> **Combining with traffic-driven wake-up**: For passive wake-up via sandbox-gateway L7 access, sandbox-gateway is adding support to automatically resume paused sandboxes when requests arrive — the gateway detects the target sandbox is Paused and triggers resume. Combined with this proposal's `AutoPausePolicy`, this achieves a full loop of "idle auto-pause + traffic auto-resume".

### Mode 2: Probe-Only Reporting

Configure only `probes` without `autoPausePolicy`. The controller periodically executes probes and writes results to `SandboxStatus.Conditions`, but **does not manage `Spec.Paused`**. Upper-layer platforms read Conditions via informer or `kubectl get` and implement their own pause/resume strategies.

Probe names are arbitrary strings; detection logic is fully user-defined. The controller writes probe results to Conditions but does not perform any pause/resume actions based on them.

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: Sandbox
metadata:
  name: openclaw-probe-only
spec:
  probes:
    - name: Active
      exec:
        command:
          - sh
          - -c
          - |
            if openclaw sessions list --active 900 2>/dev/null | grep -q .; then
              echo "active"
            else
              echo "inactive"
            fi
      periodSeconds: 30
      timeoutSeconds: 10
    - name: Cron
      exec:
        command:
          - sh
          - -c
          - |
            NEXT_MS=$(openclaw cron list --json 2>/dev/null \
              | jq -r '[.[] | select(.enabled != false) | .nextRunAtMs] | map(select(. != null)) | sort | .[0]')
            if [ -n "$NEXT_MS" ] && [ "$NEXT_MS" != "null" ]; then
              echo $((NEXT_MS / 1000))
            else
              echo "none"
            fi
      periodSeconds: 60
      timeoutSeconds: 10
  # autoPausePolicy not configured — controller only reports probe results to Conditions
```

The controller writes probe results to `SandboxStatus.Conditions` but does not set `Schedules`:

```yaml
status:
  conditions:
    - type: agents.kruise.io/Active
      status: "True"
      reason: Succeeded
      message: "active"
    - type: agents.kruise.io/Cron
      status: "True"
      reason: Succeeded
      message: "none"
  # schedules not set — pause/resume not configured
```

Upper-layer platform reads probe results:

```bash
kubectl get sandbox openclaw-probe-only -o jsonpath='{.status.conditions}'
# [{"type":"agents.kruise.io/Active","status":"True","reason":"Succeeded","message":"active",...},
#  {"type":"agents.kruise.io/Cron","status":"True","reason":"Succeeded","message":"none",...}]
```

### Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                      Sandbox CR                                      │
│                                                                      │
│  Spec.Probes                                                         │
│  └── []Probe                      ← Named probes (Name + v1.Probe)    │
│      ├── { name: "Active", exec: {...}, periodSeconds: 30 }         │
│      └── { name: "Cron",   exec: {...}, periodSeconds: 60 }         │
│                                                                      │
│  Spec.AutoPausePolicy                                                │
│  ├── pause: *PausePolicy    ← Pause strategy (optional)   │
│  └── resume: *ResumePolicy  ← Resume strategy (optional)  │
│                                                                      │
│  Status.Conditions                  ← Probe results (standard K8s   │
│  ├── { type: "agents.kruise.io/Active", status: True,               │
│  │     reason: "Succeeded", message: "active", ... }                │
│  └── { type: "agents.kruise.io/Cron", status: True,                 │
│        reason: "Succeeded", message: "1746018000", ... }            │
│                                                                      │
│  Status.Schedules                   ← Upcoming scheduled events       │
│  └── []Schedule                     (only when pause/resume          │
│                                      configured)                     │
└─────────────────────────────────────────────────────────────────────┘
```

The auto-pause logic is integrated into the existing sandbox controller's `Reconcile` loop (via `handleAutoPause`, called after `checkTimers` and before `calculateStatus`). The logic is split into two phases:

1. **Probe phase** (executed when `Spec.Probes` is configured): While the sandbox is Running, the controller executes each probe at its configured `PeriodSeconds` and writes results to `SandboxStatus.Conditions` (standard K8s Conditions with type = `agents.kruise.io/<probe-name>`).
2. **Decision phase** (executed only when `AutoPausePolicy.Pause`/`Resume` is configured): The controller reads probe results from Conditions, evaluates pause/resume rules, and manages `Spec.Paused`. If `Pause`/`Resume` is not configured, the controller only updates Conditions and does not auto-pause — upper-layer platforms read Conditions to decide.

> **Why integrate into the existing controller?** Probe execution is delegated to the agent-runtime sidecar via the PodProbeMarker Serverless protocol. The controller reads results from `Pod.Status.Conditions` without executing probes inline — so there is no latency impact on the core reconcile path.

### Interaction with Existing E2B Timeout Mechanism

The existing E2B-compatible API provides a timeout mechanism via two `SandboxSpec` fields:

- **`Spec.PauseTime`** — absolute timestamp; when reached, the Sandbox controller's `checkTimers` sets `Spec.Paused = true` (one-shot auto-pause). This conflicts with `AutoPausePolicy` (see below).
- **`Spec.ShutdownTime`** — absolute timestamp; when reached, `checkTimers` deletes the Sandbox. This does not conflict with `AutoPausePolicy` — it represents the user's intended deletion time.

When a client creates a sandbox with `autoPause=true`, the sandbox-manager sets both `PauseTime` and `ShutdownTime`. The `checkTimers` function runs **unconditionally** on every Reconcile — it does not check whether `AutoPausePolicy` is configured.

#### Conflict

If a Sandbox was created via the E2B API (which sets `PauseTime`) and later has `AutoPausePolicy` added, the two mechanisms will conflict over `Spec.Paused`:

1. **`PauseTime` re-pauses after AutoPausePolicy resumes.** When AutoPausePolicy's `Resume` sets `Spec.Paused = false`, a stale `PauseTime` (already in the past) causes `checkTimers` to immediately re-pause the Sandbox on the next Reconcile — undoing the resume.
2. **`PauseTime` overrides "Agent active" decision.** Even if probes report the Agent is active and AutoPausePolicy decides to keep Running, `checkTimers` will still pause the Sandbox when `PauseTime` arrives.

#### Solution: `checkTimers` Awareness of `AutoPausePolicy`

The existing Sandbox controller's `checkTimers` must skip the `PauseTime`-based auto-pause when `AutoPausePolicy` is active with `Pause` / `Resume` configured. The modification is minimal and does not alter the pause/resume *execution* path (Pod-level pause/resume remains unchanged):

```go
// In checkTimers, before the PauseTime auto-pause block:
if box.Spec.PauseTime != nil && !box.Spec.Paused {
    if hasActiveAutoPausePolicy(box) {
        // AutoPausePolicy with Pause/Resume takes over
        // pause decisions; skip the one-shot PauseTime timer.
        klog.V(4).InfoS("skipping PauseTime timer; AutoPausePolicy is active",
            "sandbox", klog.KObj(box))
    } else if pauseTimeReached(box.Spec.PauseTime, now) {
        // ... existing auto-pause logic ...
    }
}
```

Where `hasActiveAutoPausePolicy` returns `true` when `Spec.AutoPausePolicy` is non-nil and at least one of `Pause` / `Resume` is configured.

> **Note on Non-Goal scope.** The Non-Goal "Do not modify the existing pause/resume execution path of the Sandbox controller" refers to the Pod-level pause/resume *execution* (cgroups freeze, volume snapshot, etc.). Adding a guard clause to `checkTimers` that skips the `PauseTime` trigger is a *decision-layer* change, not an *execution-path* change. The actual Pod pause/resume is still performed by the existing controller's `EnsureSandboxPaused` / `EnsureSandboxResumed` functions.

### API Design

All new types are added to `api/v1alpha1/sandbox_types.go`.

#### 1. `AutoPausePolicy` (on `SandboxSpec`)

```go
// AutoPausePolicy defines pause/resume decision rules based on probe
// Conditions. Probes are defined separately in Spec.Probes.
// When set, the sandbox controller evaluates pause/resume rules.
// Probe results (from Spec.Probes) are read via Pod.Status.Conditions
// and mirrored to SandboxStatus.Conditions.
// +optional
type AutoPausePolicy struct {
    // Pause defines the pause policy for the sandbox.
    // +optional
    Pause *PausePolicy `json:"pause,omitempty"`

    // Resume defines the resume policy for the sandbox.
    // +optional
    Resume *ResumePolicy `json:"resume,omitempty"`
}

// PausePolicy defines when to pause the sandbox based on probe results.
type PausePolicy struct {
    // WhenIdleProbeFires pauses the sandbox when a probe's Condition message
    // matches MessageRegex for at least ThresholdDuration.
    // +optional
    WhenIdleProbeFires *IdleProbeFireRule `json:"whenIdleProbeFires,omitempty"`
}

// ResumePolicy defines when to resume the sandbox based on probe results.
type ResumePolicy struct {
    // WhenProbedScheduleTime resumes the sandbox before a scheduled task
    // by parsing the probe's Condition message as a timestamp.
    // +optional
    WhenProbedScheduleTime *ProbedScheduleTimeRule `json:"whenProbedScheduleTime,omitempty"`
}
```

Added to `SandboxSpec`:

```go
type SandboxSpec struct {
    // ... existing fields ...

    // Lifecycle defines lifecycle hooks for the sandbox.
    // +optional
    Lifecycle *SandboxLifecycle `json:"lifecycle,omitempty"`

    // Probes defines a list of named probes that run periodically while the sandbox
    // is Running. Each probe writes its result to a Pod Status Condition with
    // type "agents.kruise.io/<name>". Probes are generic — their semantics (e.g.,
    // "activity detection" vs "cron task detection") are defined by
    // AutoPausePolicy.Pause/Resume, not by the probe itself.
    //
    // Probe execution is delegated to the agent-runtime sidecar via the
    // PodProbeMarker Serverless protocol (kruise.io/podprobe annotation).
    // The controller reads results from Pod.Status.Conditions and mirrors
    // them to SandboxStatus.Conditions for observability.
    // +optional
    Probes []Probe `json:"probes,omitempty"`

    // AutoPausePolicy defines pause/resume decision rules based on probe
    // Conditions. Probes are defined in Spec.Probes.
    // +optional
    AutoPausePolicy *AutoPausePolicy `json:"autoPausePolicy,omitempty"`

    EmbeddedSandboxTemplate `json:",inline"`
}
```

#### 2. `SandboxLifecycle` (existing type, unchanged)

`SandboxLifecycle` retains its existing `PreUpgrade`/`PostUpgrade` hooks. `Probes` has been moved to `SandboxSpec` as a sibling field.

```go
// SandboxLifecycle defines lifecycle hooks for sandbox.
type SandboxLifecycle struct {
    // PreUpgrade is the action executed before the upgrade.
    // +optional
    PreUpgrade *UpgradeAction `json:"preUpgrade,omitempty"`

    // PostUpgrade is the action executed after the upgrade.
    // +optional
    PostUpgrade *UpgradeAction `json:"postUpgrade,omitempty"`
}
```

#### 3. `Probe`

Wraps the native `corev1.Probe` with a `Name` field for identification, inlining `corev1.Probe` so that its fields (exec handler, periodSeconds, timeoutSeconds, etc.) are directly accessible. Currently only `exec`, `periodSeconds`, `timeoutSeconds`, and `failureThreshold` are actively used; other `corev1.Probe` fields may be supported in the future as needed.

```go
// Probe defines a named probe that writes its result to a Pod Condition.
// Embeds corev1.Probe inline so that exec/periodSeconds/timeoutSeconds/etc.
// are directly accessible. Currently only exec probes are supported;
// other corev1.Probe fields (httpGet, tcpSocket, grpc) may be supported
// in the future as needed.
type Probe struct {
    // Name is the unique identifier for this probe within the sandbox.
    // Probe results are written to a Condition with type "agents.kruise.io/<Name>".
    // +kubebuilder:validation:Required
    Name string `json:"name"`

    // ContainerName specifies which container to execute the probe in.
    // If empty, defaults to the first container in the pod spec.
    // +optional
    ContainerName string `json:"containerName,omitempty"`

    // Probe embeds corev1.Probe inline. Currently only exec, periodSeconds,
    // timeoutSeconds, and failureThreshold are actively used.
    // +optional
    // +kubebuilder:pruning:PreserveUnknownFields
    // +kubebuilder:validation:Schemaless
    v1.Probe `json:",inline"`
}
```

YAML example:

```yaml
probes:
  - name: Active
    exec:
      command: ["sh", "-c", "echo active"]
    periodSeconds: 30
    timeoutSeconds: 10
```

#### 4. `IdleProbeFireRule` and `ProbedScheduleTimeRule`

Each references a probe by name and matches the Condition's `message` field — not relying on exit codes, but parsing the probe's stdout text.

```go
// IdleProbeFireRule defines the rule for pausing when an idle probe fires.
// The controller reads the referenced probe's Condition and matches its
// message against MessageRegex. When the match persists for at least
// ThresholdDuration (measured from the Condition's lastTransitionTime),
// the sandbox is paused.
type IdleProbeFireRule struct {
    // Probe is the name of the probe to read.
    // The controller reads the Condition "agents.kruise.io/<Probe>".
    // +required
    Probe string `json:"probe"`

    // MessageRegex is a regular expression matched against the Condition's
    // message field (i.e., the probe's stdout). When the regex matches,
    // the probe indicates that the Agent is inactive.
    //
    // Probes should always exit 0 and output semantic text to stdout.
    // The decision is based on message content, not exit codes.
    //
    // Example: "^inactive$" matches when the probe outputs "inactive".
    // +required
    MessageRegex string `json:"messageRegex"`

    // ThresholdDuration defines how long the probe condition must continuously
    // match MessageRegex before the sandbox is paused. The controller uses
    // the Condition's lastTransitionTime to determine elapsed time — no
    // separate tracking field is needed in SandboxStatus.
    // Example: "15m" means pause only after the condition has matched for 15 minutes.
    // Default: nil (pause immediately when condition matches).
    // +optional
    ThresholdDuration *metav1.Duration `json:"thresholdDuration,omitempty"`
}

// ProbedScheduleTimeRule defines the rule for resuming based on a probed
// schedule time. The controller reads the referenced probe's Condition and,
// when TimeFormat is "unix", parses its message as a Unix timestamp
// (next event time). The sandbox is resumed LeadTime before the parsed timestamp.
type ProbedScheduleTimeRule struct {
    // Probe is the name of the probe to read.
    // The controller reads the Condition "agents.kruise.io/<Probe>".
    // +required
    Probe string `json:"probe"`

    // TimeFormat indicates the format of the probe's Condition message for
    // parsing as a timestamp. When set to "unix", the controller parses the
    // message as a Unix timestamp (seconds since epoch) and uses it as the
    // next event time for resume scheduling.
    // +optional
    // +kubebuilder:validation:Enum=unix
    TimeFormat string `json:"timeFormat,omitempty"`

    // LeadTime is how early before the parsed timestamp to resume the sandbox.
    // Default: 5m.
    // +optional
    LeadTime *metav1.Duration `json:"leadTime,omitempty"`
}
```

**Evaluation logic**:

1. **Pause check** (when `Pause` is set): Read the referenced probe's Condition. If `status == True` and `message` matches `Pause.WhenIdleProbeFires.MessageRegex` → the Agent is inactive. Compute `elapsed = now - Condition.lastTransitionTime`. If `ThresholdDuration` is nil OR `elapsed >= ThresholdDuration` → proceed to resume check (step 2). Otherwise, keep Running, Reason = "InactivePending", RequeueAfter = `ThresholdDuration - elapsed`. If the message does not match (e.g., probe outputs `"active"`) → the Agent is active, stay Running.
2. **Resume check** (when `Resume` is set): Read the referenced probe's Condition. If `status == True` and `Resume.WhenProbedScheduleTime.TimeFormat == "unix"` → parse `message` as a Unix timestamp. Add a Schedule with `NextResumeTime = timestamp - LeadTime`, then pause the sandbox. The controller resumes the sandbox when `NextResumeTime` is reached.
3. If `Pause` is not set → the controller does not auto-pause based on probes. If `Resume` is not set → the controller does not auto-resume based on probes.

> The probe's responsibility is to detect "when to pause". When the message does not match `Pause.WhenIdleProbeFires.MessageRegex` (e.g., `"active"`), the Agent naturally stays running. The `Resume` rule complements this by detecting scheduled tasks and ensuring the sandbox wakes up before they fire.

#### 5. `Schedule` (on `SandboxStatus`)

Probe results are reported through standard `SandboxStatus.Conditions` (consistent with PodProbeMarker). `Schedules` stores upcoming pause/resume events directly on `SandboxStatus`.

```go
// Schedule represents a single upcoming scheduled pause or resume event.
// written to SandboxStatus.Schedules.
type Schedule struct {
    // Reason describes why this schedule exists.
    // Examples: "probedSchedule" (resume based on probe timestamp),
    // "startDeadline" (pause by deadline).
    // +optional
    Reason string `json:"reason,omitempty"`

    // NextResumeTime is when the sandbox should be resumed.
    // +optional
    NextResumeTime *metav1.Time `json:"nextResumeTime,omitempty"`

    // NextPauseTime is when the sandbox should be paused.
    // +optional
    NextPauseTime *metav1.Time `json:"nextPauseTime,omitempty"`
}
```

Added to `SandboxStatus`:

```go
type SandboxStatus struct {
    // ... existing fields ...

    // Conditions contains probe results and other auto-pause conditions.
    // Each configured probe in Spec.Probes writes a Condition with
    // type "agents.kruise.io/<probe-name>".
    // +optional
    // +patchMergeKey=type
    // +patchStrategy=merge
    // +listType=map
    // +listMapKey=type
    Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

    // Schedules contains upcoming scheduled pause/resume events.
    // Only populated when Spec.AutoPausePolicy.Pause/Resume is configured.
    // +optional
    Schedules []Schedule `json:"schedules,omitempty"`
}
```

**Probe Condition format** (written to `SandboxStatus.Conditions`):

Probes should always exit 0 and output semantic information to stdout (Condition `message`). The decision layer uses `Pause.WhenIdleProbeFires.MessageRegex` (regex match) or `Resume.WhenProbedScheduleTime.TimeFormat` (parse as Unix timestamp) to match message content and decide behavior, not relying on exit codes. On timeout or execution error, status is Unknown (fail-closed).

```yaml
status:
  conditions:
    - type: agents.kruise.io/Active
      status: "True"              # probe exit 0 → True; timeout/error → Unknown
      reason: "Succeeded"          # Succeeded | Timeout | Error | Unhealthy
      message: "active"           # stdout = semantic text, matched by Pause.WhenIdleProbeFires.MessageRegex
      lastTransitionTime: "2026-07-01T10:00:00Z"  # updated when status changes
    - type: agents.kruise.io/Cron
      status: "True"              # probe exit 0 → True (always succeeds)
      reason: "Succeeded"
      message: "1746018000"       # stdout = Unix timestamp, parsed when Resume.WhenProbedScheduleTime.TimeFormat="unix"
      lastTransitionTime: "2026-07-01T09:59:00Z"
```

#### 6. Reason Constants

```go
const (
    // ProbeConditionPrefix is the prefix for probe Conditions written to
    // SandboxStatus.Conditions. The full type is "agents.kruise.io/<probe-name>".
    ProbeConditionPrefix = "agents.kruise.io/"

    // Condition reasons (written to SandboxStatus.Conditions[].reason)
    ProbeReasonSucceeded = "Succeeded" // Probe succeeded (exit 0)
    ProbeReasonTimeout   = "Timeout"   // Probe timed out
    ProbeReasonError     = "Error"     // Probe execution error
    ProbeReasonUnhealthy = "Unhealthy" // Consecutive failures reached FailureThreshold

    // Auto-pause decision reason constants (used for logging and events).
    PauseReasonScheduledResume = "ScheduledResume" // Reached probe-calculated resume time, auto-resumed
    PauseReasonProbePaused     = "ProbePaused"      // Paused by Pause
    PauseReasonAgentActive     = "AgentActive"      // Agent is active, pause delayed
    PauseReasonInactivePending = "InactivePending"  // Agent is inactive but thresholdDuration not yet reached
    PauseReasonProbeFailed     = "ProbeFailed"      // Probe failed but not yet thresholded, treated as active (fail-closed)
    PauseReasonProbeUnhealthy  = "ProbeUnhealthy"   // Probe consecutive failures reached threshold, skip probe

    // Schedule reason constants (written to SandboxStatus.Schedules).
    ScheduleReasonProbedSchedule = "probedSchedule"
    ScheduleReasonStartDeadline  = "startDeadline"
)
```

### Reconcile Logic

The auto-pause logic (within the sandbox controller's `Reconcile`) is split into two phases:

#### Phase 1: Probing (executed when Spec.Probes is configured)

When the sandbox is Running, the controller executes each probe at its configured `PeriodSeconds` and writes results to `SandboxStatus.Conditions`.

```
1. If Spec.AutoPausePolicy is nil and Spec.Probes is empty → return (not managed)

2. If the sandbox is Running, iterate Spec.Probes:
   For each Probe:
   a. Execute the probe (subject to TimeoutSeconds)
   b. Update Condition based on execution result:
      - Success (exit 0, recommended that probes always exit 0):
        Condition status = True, reason = "Succeeded"
        message = stdout (semantic text, matched by Pause.WhenIdleProbeFires.MessageRegex)
        Reset consecutive failure count
      - Timeout:
        Condition status = Unknown, reason = "Timeout"
        message = "probe timed out after Xs"
        Increment consecutive failure count
      - Execution error:
        Condition status = Unknown, reason = "Error"
        message = error message
        Increment consecutive failure count
   c. When consecutive failure count >= FailureThreshold:
      - Condition reason changed to "Unhealthy", status = Unknown
      - Emit Kubernetes Warning Event: "Probe <name> is unhealthy"
   d. Update Condition lastTransitionTime (only when status changes)
   e. Write all Condition updates to SandboxStatus

3. If AutoPausePolicy is nil or Pause/Resume is not configured → return (only report Conditions, no decisions)
```

> **Condition update strategy**: Probes should always exit 0 and output semantic information to stdout (Condition `message`). `status=True` means probe execution succeeded; `status=Unknown` means timeout or error. The decision layer matches message content via `Pause.WhenIdleProbeFires.MessageRegex` (regex match) or parses it as a Unix timestamp via `Resume.WhenProbedScheduleTime.TimeFormat` (when `TimeFormat="unix"`), not exit codes. The controller uses the Condition's `lastTransitionTime` to determine how long the matching state has persisted, comparing against `Pause.WhenIdleProbeFires.ThresholdDuration`.

#### Phase 2: Decision-Making (executed only when AutoPausePolicy.Pause/Resume is configured)

```
4. If sandbox is Running:
   a. Read Pause probe Condition:
      - If status == Unknown and reason == "Unhealthy":
        Skip pause rule, keep Running (log: ProbeUnhealthy)
        RequeueAfter = PeriodSeconds, return
      - If status == Unknown (timeout/error, not Unhealthy):
        Fail-closed, treat as active, keep Running (log: ProbeFailed)
        RequeueAfter = PeriodSeconds, return
      - If status == True → match message with Pause.WhenIdleProbeFires.MessageRegex:
        - If match fails → Agent active:
          Keep Running (log: AgentActive)
          RequeueAfter = PeriodSeconds, return
        - If match succeeds → Agent inactive:
          elapsed = now - Condition.lastTransitionTime
          If ThresholdDuration is nil OR elapsed >= ThresholdDuration → continue to step 4b
          Otherwise → keep Running (log: InactivePending)
            RequeueAfter = ThresholdDuration - elapsed, return

   b. Read Resume probe Condition (if Resume is set):
      - If status == True → parse message as Unix timestamp (nextFireTime):
        - If parse succeeds → add Schedule with NextResumeTime = nextFireTime - Resume.WhenProbedScheduleTime.LeadTime
          Set Paused = true, write Schedules to SandboxStatus
          RequeueAfter = NextResumeTime - now, return
        - If parse fails → log warning, continue to step 4c
      - If status == Unknown → fail-closed, treat as having a scheduled task, keep Running (log: ProbeFailed)
        RequeueAfter = PeriodSeconds, return

   c. No upcoming scheduled task → pause:
      Set Paused = true, clear Schedules
      RequeueAfter = default interval, return

5. If sandbox is Paused:
   a. If NextResumeTime is set and now >= NextResumeTime:
      - Resume sandbox: set Paused = false
      - Clear Schedules
      - RequeueAfter = PeriodSeconds (re-evaluate probe in next round)
      - Return
   b. Else → RequeueAfter = NextResumeTime - now (or default interval if NextResumeTime is nil)
```

### Probe Contract

> Currently only `exec` probes are supported. `httpGet`, `tcpSocket`, and `grpc` are rejected by webhook validation and can be extended later as needed.

Probes are generic — they have no preset semantics. **Probes should always exit 0** and output semantic information to stdout (Condition `message`). The decision layer uses `Pause.WhenIdleProbeFires.MessageRegex` to match message content or `Resume.WhenProbedScheduleTime.TimeFormat` to parse it as a Unix timestamp, not relying on exit codes.

#### Design Rationale: Decoupling Probe and Decision

Decoupling Probe and Decision is the core concept of this proposal. Users write shell scripts to detect the Agent's actual state (session activity, scheduled tasks, custom metrics, etc.), and script output is written to the Condition `message`. Users can flexibly customize their detection logic without modifying the API or controller code.

- **Without `AutoPausePolicy`** (Mode 2): The controller only periodically executes probes and reports Condition results. Upper-layer platforms read Conditions and implement their own pause/resume strategies.
- **With `AutoPausePolicy.Pause`/`Resume`** (Mode 1): The controller executes probes and makes decisions simultaneously, automatically managing `Spec.Paused`.

> **Message stability requirement**: Each probe execution writes stdout to the Condition `message`, and Condition updates trigger PATCH requests to Sandbox Status. To reduce unnecessary API server pressure, **probe scripts should output the same message when semantics have not changed**. For example, when the Active probe continuously detects an active Agent, it should always output `"active"` rather than dynamic text containing timestamps or counters. The controller skips Condition updates when the message has not changed, avoiding meaningless Status writes.

#### Condition States

| Execution Result | Condition | Meaning |
|----------|-----------|------|
| exit 0 (success) | status=True, reason="Succeeded" | Probe execution succeeded, message = stdout (semantic text) |
| Timeout | status=Unknown, reason="Timeout" | Probe failed (fail-closed: decision layer treats as active, no pause) |
| Execution error | status=Unknown, reason="Error" | Probe failed (fail-closed: decision layer treats as active, no pause) |
| Consecutive failures >= FailureThreshold | status=Unknown, reason="Unhealthy" | Probe unhealthy; decision layer skips this probe |

#### Message Content and Regex Matching

Probe stdout is written to the Condition's `message` field. `Pause` and `Resume` match message content as follows:

| Rule | Matches message | Example |
|------|-------------|------|
| `Pause.WhenIdleProbeFires.MessageRegex: "^inactive$"` | Regex match | Matches `"inactive"` |
| `Resume.WhenProbedScheduleTime.TimeFormat: "unix"` | Parse as Unix timestamp | Parse `"1751373600"` as timestamp |

| Rule | Condition | action after match |
|------|------|----------|
| `Pause` | `MessageRegex` matches | Check elapsed time vs thresholdDuration; proceed to resume check when threshold reached |
| `Resume` | `TimeFormat="unix"` and message parses as Unix timestamp | Parse timestamp, add Schedule with NextResumeTime, then pause |

When the message does not match `Pause.WhenIdleProbeFires.MessageRegex` (e.g., probe outputs `"active"` which does not match `"^inactive$"`), the Agent is treated as active and stays Running.

> **Probe health mechanism**: `v1.Probe` has built-in `FailureThreshold` (default 3). The controller tracks consecutive failures (timeouts/errors). A single failure remains fail-closed (status=Unknown, treated as active during decision-making); after consecutive failures reach the threshold, the Condition reason changes to "Unhealthy", the controller skips this probe, and emits a Kubernetes Warning Event. The first successful probe execution resets the failure count.

### State Machine

The diagram below illustrates state transitions in Mode 1 (probe-driven decisions):

```
                    ┌──────────────────────────────┐
                    │  Sandbox Running              │
                    │  Probe phase: execute probes  │
                    └──────────────┬───────────────┘
                                   │
                     ┌─────────────▼──────────────┐
                     │  Pause              │
                     │  MessageRegex matches?      │
                     └─────────────┬──────────────┘
                       not match   │   match
                    ┌────────────────┘ └──────────────────────┐
                    ▼                                        ▼
          ┌──────────────────────┐              ┌──────────────────┐
          │ AgentActive          │              │ thresholdDuration │
          │ (or ProbeFailed)     │              │ reached?          │
          │ Requeue=PeriodSeconds│              └────────┬─────────┘
                                                yes  │  no (pending)
                                        ┌─────────┘    └──────────┐
                                        ▼                         ▼
                                ┌────────────────┐  ┌──────────────────────┐
                                │ Resume │  │ InactivePending    │
                                │ matches?       │  │ Requeue=PeriodSeconds │
                                └────────┬─────────┘  └──────────────────────┘
                    yes  │  no
              ┌─────────┘    └──────────┐
              ▼                         ▼
      ┌────────────────────────┐        ┌────────────────────┐
      │ Set Schedules          │        │ Paused=true        │
      │ Paused=true            │        │ Clear Schedules    │
      └────────┬───────────────┘        └────────────────────┘
              │ time reaches NextResumeTime
              ▼
      ┌───────────────────────┐
      │ Resume sandbox          │
      │ Paused=false            │
      │ Clear Schedules         │
      └───────┬───────────────┘
              │ next Reconcile
              ▼
      ┌────────────────────────────┐
      │ Re-evaluate probes:        │
      │ Pause matches?     │─── no  ──→ keep running (AgentActive)
      │ Resume matches?    │─── yes ──→ set new Schedules, pause
      │                            │─── no  ──→ pause
      └────────────────────────────┘
```

### Interaction with SandboxSet

- Sandboxes managed by SandboxSet with `claimed=false` are **excluded** from auto-pause management. The controller checks the `agents.kruise.io/sandbox-claimed` label; if it is `false`, Reconcile is skipped.
- Batch configuration: Use `SandboxUpdateOps` label selector to patch `AutoPausePolicy` to multiple Sandboxes at once.
- `SandboxUpdateOps` already supports rolling/partitioned strategies, suitable for gradually rolling out pause policies.

## Alternatives

### Alternative 1: Standalone `SandboxCron` CRD

A standalone CRD that references Sandboxes via label selector and patches `Spec.Paused` on their behalf.

**Rejected because:** Pause state is best expressed directly on the Sandbox itself. A standalone CRD requires cross-CRD state synchronization, introducing race conditions. Embedding the policy in Sandbox means each Sandbox has exactly one policy, with no multi-rule conflicts.

### Alternative 2: External Script / K8s CronJob

A script or CronJob that polls Agent state and patches `Spec.Paused` via `kubectl`.

**Rejected because:** Not declarative, no Reconcile loop, cannot express probe-based activity detection, and creates API server load with one job per Sandbox.

### Alternative 3: Fixed Probe Fields

Hard-coded `activeProbe` and `cronProbe` fields with probe semantics embedded in the API.

**Rejected because:** Inflexible — cannot support other detection needs. Results stored in custom structs are less observable than standard K8s Conditions. Adding new probe types requires modifying the API.

## Risks and Mitigations

| Risk | Impact | Mitigation |
|------|------|----------|
| **Probe latency blocking Reconcile** | Controller slows down; other Sandboxes starve | Probes execute asynchronously in the agent-runtime sidecar via PodProbeMarker; the controller reads results from `Pod.Status.Conditions` without blocking; probe timeout (`TimeoutSeconds`) is enforced by the sidecar |
| **Probe command hangs or times out** | Controller waits indefinitely | Each probe call has `TimeoutSeconds`; **single failure sets Condition status=Unknown (fail-closed, treated as active, no pause)**; after consecutive failures reach `FailureThreshold`, reason="Unhealthy", skip probe, and emit Warning Event |
| **Probe script environment issues blur idle vs failure** | Probe timeout/error misclassified as idle, causing mistaken pause | Probe failures set Condition status=Unknown (not True); decision layer treats Unknown as active (fail-closed); after consecutive failures reach threshold, reason="Unhealthy" and probe is skipped |
| **Conflict with E2B timeout mechanism** | Stale `PauseTime`/`ShutdownTime` (set by E2B API at create time) fire unconditionally in `checkTimers`, overriding `AutoPausePolicy` decisions | `checkTimers` skips `PauseTime` auto-pause when `AutoPausePolicy` is active with `Pause`/`Resume` — see [Interaction with Existing E2B Timeout Mechanism](#interaction-with-existing-e2b-timeout-mechanism). `ShutdownTime` is not skipped (remains the ultimate safety net). Future work: webhook validation rejects manual `Spec.Paused` modifications while `AutoPausePolicy` is active |
| **Probe exec requires Pod Running** | Probe fails when Pod is Paused | Probes only execute while sandbox is Running. Paused sandboxes do not need probes |

## Upgrade Strategy

- **API compatibility.** `AutoPausePolicy` and `Spec.Probes` are new optional fields. Existing Sandboxes without these fields are completely unaffected.
- **Controller deployment.** The auto-pause logic is integrated into the existing sandbox controller within the agent-sandbox-controller binary. No new deployment is needed — just upgrade the image.
- **Feature gate.** Feature gate `AutoPauseController` (default: `false`) controls whether the controller is activated. Supports gradual rollout and quick rollback.
- **Status fields.** `Conditions` and `Schedules` are additive fields; old clients that ignore them are unaffected.
- **No breaking changes.** No existing fields are modified or deleted. When `AutoPausePolicy` is not set, `Spec.Paused` continues to work as usual.
- **Gradual adoption.** Start with Mode 2 (probe-only) to verify probe Condition results, then add `Pause`/`Resume` to enable Mode 1 (auto-pause).

## Test Plan

### Unit Tests

- **Probe execution and Condition writing:** exit 0 → status=True, reason="Succeeded", message=stdout; timeout → status=Unknown, reason="Timeout"; execution error → status=Unknown, reason="Error"; Pause.WhenIdleProbeFires.MessageRegex matches message (e.g., `^inactive$`); Resume.WhenProbedScheduleTime.TimeFormat="unix" parses message as Unix timestamp; invalid message → treat as no scheduled task + warning log.
- **Probe health:** single failure → status=Unknown (fail-closed); consecutive failures < FailureThreshold → reason="Timeout"; consecutive failures >= FailureThreshold → reason="Unhealthy" + Warning Event; first success resets count and reason.
- **Condition lastTransitionTime:** verify it only updates when status changes, used for thresholdDuration elapsed-time comparison.
- **Condition update optimization:** verify Status patch is skipped when message has not changed (reducing API server pressure); patch normally when message changes.
- **ThresholdDuration elapsed-time comparison:** verify elapsed time is correctly computed from Condition.lastTransitionTime; pause only triggers when elapsed >= ThresholdDuration; InactivePending state while waiting; immediate pause when ThresholdDuration is nil.
- **Decision tree:** Pause.WhenIdleProbeFires.MessageRegex match (inactive) / mismatch (active), status=Unknown fail-closed, thresholdDuration reached/not reached, Resume match/mismatch, probe Unhealthy fallback.
- **Probe-only reporting mode:** When AutoPausePolicy is not configured, probe results are written to Conditions, Schedules is nil, and Spec.Paused is not modified.
- **checkTimers guard:** When `AutoPausePolicy` is active with `Pause`/`Resume`, `checkTimers` skips the `PauseTime` auto-pause even if `PauseTime` is in the past; `ShutdownTime` deletion is not skipped. When `AutoPausePolicy` is nil or has no strategies, `checkTimers` behaves as before.
- **RequeueAfter calculation:** verify correct requeue time for each branch.

### Integration Tests

- **End-to-end pause/resume loop:** Create a Sandbox with probes + Pause/Resume, verify it pauses when idle and resumes before scheduled tasks.
- **ScheduledResume flow:** Mock Cron probe output timestamp (message), verify Resume.WhenProbedScheduleTime.TimeFormat="unix" causes the sandbox to pause and resume at NextResumeTime, and the next Reconcile re-evaluates probes.
- **thresholdDuration smoothing:** Mock Active probe alternating `"active"`/`"inactive"`, verify no pause within thresholdDuration window and pause only after elapsed time exceeds thresholdDuration.
- **Pause strategy mismatch delay:** Mock Active probe message output `"active"` (does not match `"^inactive$"`), verify pause is delayed and retried after PeriodSeconds.
- **Probe unhealthy fallback:** Mock Active probe failing consecutively up to FailureThreshold, verify Condition reason="Unhealthy", controller skips the probe, and emits Warning Event.
- **Probe-only reporting mode:** Without AutoPausePolicy configured, verify probe results are written to Conditions but Spec.Paused is not modified.
- **checkTimers + AutoPausePolicy coexistence:** Create a Sandbox with E2B `autoPause=true` (sets `PauseTime` in the past), then add `AutoPausePolicy` with `Pause`. Verify `checkTimers` does not re-pause when AutoPausePolicy says "Agent active". Verify `ShutdownTime` still triggers deletion when reached.

### E2E Tests

- Deploy an OpenClaw sandbox with pause policy on a kind cluster.
- Verify nightly pause and morning resume.
- Verify scheduled-task-aware resume: create an OpenClaw cron job and check that the sandbox resumes before the task triggers.
- Verify probe-only reporting mode: do not configure AutoPausePolicy, and confirm probe results are correctly reported via kubectl reading Conditions.

## Implementation History

- [x] 2026-06-26: Initial proposal draft (SandboxCron CRD + embedded AutoPausePolicy).
- [x] 2026-06-30: Reuse `corev1.Probe`; narrow to Exec only; add probe health mechanism (FailureThreshold + fail-closed + Warning Event).
- [x] 2026-07-01: Decoupling of probing and decision-making. Generic named probes writing to standard `SandboxStatus.Conditions`. Decision layer uses regex matching on message (`Pause.WhenIdleProbeFires.MessageRegex`) and Unix timestamp parsing (`Resume.WhenProbedScheduleTime.TimeFormat`). Move `Probes` to `SandboxLifecycle`.
- [x] 2026-07-13: Move `Probes` from `SandboxLifecycle` to `SandboxSpec` (sibling of `Lifecycle`). Integrate auto-pause logic into the existing sandbox controller instead of a standalone controller. Reuse PodProbeMarker Serverless protocol for probe execution.
- [x] 2026-07-13: Restructure `AutoPausePolicy` API — replace flat `PauseStrategy`/`ResumeStrategy` with nested `Pause.WhenIdleProbeFires` (`IdleProbeFireRule`) / `Resume.WhenProbedScheduleTime` (`ProbedScheduleTimeRule`). Replace `MessageUnix bool` with `TimeFormat string` enum (`unix`) for extensibility.
- [x] 2026-07-13: Replace `AutoPauseStatus.NextResumeTime` with `Schedules []Schedule` array for extensibility. Each Schedule entry has `Reason`, `NextResumeTime`, and `NextPauseTime` fields.
- [x] 2026-07-14: Remove `AutoPauseStatus` struct (including `CurrentState` and `Reason`). Move `Schedules` directly to `SandboxStatus`. Decision state is reflected by `Spec.Paused` and `Status.Phase`; reasons are logged via klog.
- [x] 2026-07-08: `Pause`/`Resume` as independent fields (not array). `ThresholdDuration` (time-based, `*metav1.Duration`) using Condition's `lastTransitionTime` — no tracking field in `SandboxStatus`.
- [x] 2026-07-09: Add "Interaction with Existing E2B Timeout Mechanism" — `checkTimers` skips `PauseTime` when `AutoPausePolicy` is active. Restore `TimeFormat` field on `Resume`. Rewrite document to focus on Mode 1 (probe-driven) and Mode 2 (probe-only); remove schedule-driven mode from scope.
- [ ] TODO: Community review and feedback.
- [x] API type implementation + `make generate manifests`.
- [x] Auto-pause controller implementation.
- [ ] TODO: Unit tests + integration tests.
