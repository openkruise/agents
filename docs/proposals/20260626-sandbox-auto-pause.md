---
title: Sandbox Auto-Pause and Resume
authors:
  - "@zhaomingshan"
reviewers:
  - "@TBD"
creation-date: 2026-06-26
last-updated: 2026-07-06
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
  - [Architecture Overview](#architecture-overview)
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

This proposal introduces two fields on the `Sandbox` CRD: `AutoPausePolicy` (pause policy) and `Lifecycle.Probes` (lifecycle probes), providing three composable mechanisms:

1. **Scheduling**: Define time windows via cron expressions to control whether the sandbox should be Running or Paused. Optional `defaultState` specifies the default state outside windows. No probes are required; suitable for sandboxes with fixed schedules.
2. **Probing**: Users define a set of generic named probes in `Lifecycle.Probes`. The controller executes them periodically and writes results to `SandboxStatus.Conditions`. Probes are generic — they do not distinguish between "active" or "cron" types; semantics are assigned by the decision layer.
3. **Decisions**: Optional `AutoPausePolicy.Decisions` reference probe names and define when to pause/resume the sandbox based on probe Conditions. If not configured, the controller only reports probe results to Conditions, and upper-layer platforms read Conditions to make their own decisions.

This design targets AI Agent workloads (e.g., OpenClaw), reclaiming compute resources during idle periods while preserving the ability to resume before scheduled tasks trigger. The policy is embedded directly in `Sandbox` — no additional CRD is needed.

## Motivation

AI Agent sandboxes (OpenClaw, code-interpreter, etc.) often sit idle for long periods. Keeping them in the Running state wastes compute resources that could otherwise be used for offline batch workloads. On the other hand, Agents with scheduled tasks (e.g., "turn on the AC every day at 18:00") must be resumed **before** the task fires.

The existing mechanism (`Spec.PauseTime`) is a one-shot absolute-time trigger and cannot express periodic scheduling, activity detection via probes, or scheduled-task-aware resume.

### Goals

- Support cron scheduling windows that define when the sandbox should be Running or Paused.
- **Generic named probes**: Users define any number of probes, each writing results to a Sandbox Condition without preset semantics.
- **Decoupling of probing and decision-making**: Probes always run and report Conditions; decision rules are optional and reference probe names to define semantics.
- Support configurable decision rules (e.g., "pause after N consecutive inactive reports") or only reporting Conditions for upper-layer platforms to decide.

### Non-Goals

- **Passive resume triggered by IM messages** is not implemented. For OpenClaw, its Gateway runs inside the sandbox and keeps online status via **outbound persistent connections** to the IM platform. Once the sandbox is Paused, these connections drop, and IM platform messages cannot reach the Gateway. This scenario depends on the IM platform's offline message queue, webhook retry mechanism, or external resume triggers — all higher-layer solutions outside this proposal.
- **WebSocket / push-based traffic detection** is out of scope. Only exec-based probe contracts are defined.
- **Standalone CRD** (e.g., `SandboxCron`) is intentionally avoided — see [Alternatives](#alternatives).
- **Do not modify the existing pause/resume execution path of the Sandbox controller.** The auto-pause controller only manages `Spec.Paused`; actual Pod pause/resume is handled by the existing Sandbox controller.

## Proposal

### User Stories

| Scenario | Role | Requirement | Key Benefit |
|------|------|------|----------|
| **Office-hours scheduling** | Cluster admin | Sandbox pauses 23:00–06:00 and resumes at 06:00 | Releases resources to offline workloads at night |
| **Scheduled-task-aware resume** | Agent user | Agent has a daily 18:00 task; sandbox resumes at 17:55 and pauses after the task | No missed scheduled tasks; resources reclaimed between tasks |
| **Manual debug override** | Developer | User manually resumes a paused sandbox for 2 hours of debugging; scheduling rules are temporarily skipped | Interference-free debugging; scheduling automatically resumes after TTL expires |
| **Agent activity protection** | Platform operator | Probe checks Agent before pausing; if busy, pause is delayed | Does not interrupt work the Agent is currently executing |
| **Probe-only reporting** | Upper-layer platform | Controller only reports probe results to Conditions and does not auto-pause; users read Conditions to decide | Decouples probing from decisions; users can implement their own pause policies flexibly |

### Design Overview

This proposal supports three scenario modes, covering different requirement levels from simple time scheduling to intelligent probe-based decisions:

| Mode | Configuration | Suitable Scenario | Decision Maker |
|------|------|----------|--------|
| **Schedule-driven** | `schedules` + `defaultState` | Fixed-schedule sandboxes (e.g., running on workdays, paused at night) | Controller (by time window) |
| **Probe-driven decisions** | `lifecycle.probes` + `autoPausePolicy.decisions` | Agents with scheduled tasks (e.g., OpenClaw) | Controller (by probe results) |
| **Probe-only reporting** | Only `lifecycle.probes` | Need to implement pause/resume strategy yourself | User (reads Conditions) |

#### Mode 1: Schedule-Driven

Configure `schedules` + `defaultState`, declaring only running windows; outside windows the controller enforces `defaultState`. No probes are required. Suitable for sandboxes with fixed schedules. Example of a multi-window workday schedule (running in the morning, paused at lunch, running in the afternoon, paused at night):

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: Sandbox
metadata:
  name: openclaw-workhours
spec:
  autoPausePolicy:
    timezone: Asia/Shanghai
    resumeLeadTime: 5m
    defaultState: paused       # Default state outside windows
    schedules:
      - state: running
        start: "0 6 * * 1-5"     # 06:00 on workdays
        end:   "0 12 * * 1-5"    # 12:00 lunch
      - state: running
        start: "30 13 * * 1-5"   # 13:30
        end:   "0 23 * * 1-5"    # 23:00 night
    # decisions not configured — driven by schedule only
```

Conversely, `defaultState: running` suits scenarios where the sandbox runs by default and only pauses during specific windows (e.g., power saving at night):

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: Sandbox
metadata:
  name: openclaw-nightly-pause
spec:
  autoPausePolicy:
    timezone: Asia/Shanghai
    defaultState: running      # Default state outside windows
    schedules:
      - state: paused
        start: "0 23 * * *"     # 23:00 daily
        end:   "0 6 * * *"      # 06:00 next day
```

> Cron expressions use the standard 5-field format: `minute hour day month weekday`. The `timezone` field controls interpretation (IANA format). When multiple schedules overlap, they are evaluated in declaration order; first match wins. `defaultState` specifies the default state outside windows; if unset, the controller does not change `Spec.Paused` outside windows (manual control preserved).
>
> **Customer scenario note**: A customer needs the platform to automatically wake up sandboxes during a nightly maintenance window and execute maintenance scripts inside them. Although this can be expressed via `schedules`, script orchestration, error handling, and permission control are closer to upper-layer platform responsibilities. This scenario is not included in this proposal's Sandbox capabilities for now; we recommend exploring a Kubernetes CronJob that externally orchestrates "resume sandbox → run scripts → pause as needed", and decide later whether to push this down into the Sandbox controller based on feedback.

#### Mode 2: Probe-Driven Decisions

Configure `lifecycle.probes` + `autoPausePolicy.decisions`. Probes detect the Agent's actual state, and the controller automatically decides to pause/resume. Suitable for Agents with scheduled tasks, such as OpenClaw — pause when idle, resume before tasks.

Core idea:
1. **Active probe** (every 30s): Detects whether the Agent is active (active sessions + cron tasks running), outputting `"active"`/`"inactive"`
2. **Cron probe** (every 60s): Extracts the next scheduled task timestamp, outputting a Unix timestamp or `"none"`
3. **Pause rule**: Active continuously outputs `"inactive"` for `idleCount` (30 times) → pause
4. **Resume rule**: Cron outputs a timestamp → automatically resume `leadTime` (5 minutes) before the task

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: Sandbox
metadata:
  name: openclaw-cron
spec:
  lifecycle:
    probes:
      - name: Active
        probe:
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
        probe:
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
    # schedules not configured — fully probe-driven
    decisions:
      - type: pause
        probe: Active
        messageRegex: "^inactive$"
        idleCount: 30              # Pause after 30 consecutive inactive probe reports (~15 minutes)
      - type: resume
        probe: Cron
        messageUnix: true
        leadTime: 5m
```

After pausing, the controller writes the decision state to `Status.AutoPauseStatus`:

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
  autoPauseStatus:
    currentState: paused
    reason: ProbePaused
    idleCount: 30                # 30 consecutive inactive probe reports
    nextResumeTime: "2026-07-01T17:55:00Z"   # task time - leadTime(5m)
```

> **Combining with traffic-driven wake-up**: The three modes above cover "active pause + scheduled/probe-driven resume". For passive wake-up via sandbox-gateway L7 access, sandbox-gateway is adding support to automatically resume paused sandboxes when requests arrive — the gateway detects the target sandbox is Paused and triggers resume. Combined with this proposal's `AutoPausePolicy`, this achieves a full loop of "idle auto-pause + traffic auto-resume".

#### Mode 3: Probe-Only Reporting

Configure only `lifecycle.probes` without `autoPausePolicy`. The controller periodically executes probes and writes results to `SandboxStatus.Conditions`, but **does not manage `Spec.Paused`**. Upper-layer platforms read Conditions via informer or `kubectl get` and implement their own pause/resume strategies. Probe names and detection logic are fully user-defined.

Probe names are arbitrary strings; detection logic is fully user-defined. Using OpenClaw as an example, you can configure two probes without defining decision rules in `decisions`:

- **Active probe** (every 30s): Detects whether the Agent is active (active sessions), outputting `"active"`/`"inactive"`
- **Cron probe** (every 60s): Extracts the next scheduled task timestamp, outputting a Unix timestamp or `"none"`

The above are just examples — users can define any number of probes with arbitrary names according to actual needs. The controller writes probe results to Conditions but does not perform any pause/resume actions based on them. Upper-layer platforms read Conditions and combine their own scheduling logic (priority, resource quotas, business rules, etc.) to decide when to pause/resume. This mode does not require configuring `AutoPausePolicy`.

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: Sandbox
metadata:
  name: openclaw-probe-only
spec:
  lifecycle:
    probes:
      - name: Active
        probe:
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
        probe:
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

The controller writes probe results to `SandboxStatus.Conditions` but does not set `AutoPauseStatus` (because `decisions` is not configured):

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
  # autoPauseStatus not set — decisions not configured, controller does not decide
```

Upper-layer platform reads probe results:

```bash
kubectl get sandbox openclaw-probe-only -o jsonpath='{.status.conditions}'
# [{"type":"agents.kruise.io/Active","status":"True","reason":"Succeeded","message":"active",...},
#  {"type":"agents.kruise.io/Cron","status":"True","reason":"Succeeded","message":"none",...}]
```

> **Mode selection recommendation**: Evolve from simple to complex. Use Mode 1 for fixed schedules; Mode 2 for intelligent pause/resume based on Agent actual state; Mode 3 for implementing your own pause/resume strategy. Mode 3 is also suitable for gradual adoption — first verify probe Condition results are correct, then configure `AutoPausePolicy.Decisions` to enable auto-pause.

### Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                      Sandbox CR                                      │
│                                                                      │
│  Spec.Lifecycle                                                      │
│  └── probes: []NamedProbe          ← Generic named probe list       │
│      ├── { name: "Active", probe: v1.Probe{...} }                   │
│      └── { name: "Cron",   probe: v1.Probe{...} }                   │
│                                                                      │
│  Spec.AutoPausePolicy                                                │
│  ├── schedules / defaultState / timezone / resumeLeadTime            │
│  ├── decisions: []PauseDecisionRule ← Decision rules (optional,     │
│  └── manualOverride                     reference probe names)       │
│                                                                      │
│  Status.Conditions                  ← Probe results (standard K8s   │
│  ├── { type: "agents.kruise.io/Active", status: True,               │
│  │     reason: "Succeeded", message: "3 active sessions", ... }     │
│  └── { type: "agents.kruise.io/Cron", status: True,                 │
│        reason: "Succeeded", message: "1746018000", ... }            │
│                                                                      │
│  Status.AutoPauseStatus             ← Decision state (only updated  │
│  ├── currentState, reason, ...        when decisions configured)     │
│  └── idleCount, nextResumeTime, ...                                  │
└─────────────────────────────────────────────────────────────────────┘
```

The auto-pause controller is a **new standalone controller** within the agent-sandbox-controller binary. Reconcile is split into two phases:

1. **Probe phase** (executed when `Lifecycle.Probes` is configured): While the sandbox is Running, the controller executes each probe at its configured `PeriodSeconds` and writes results to `SandboxStatus.Conditions` (standard K8s Conditions with type = `agents.kruise.io/<probe-name>`).
2. **Decision phase** (executed only when `AutoPausePolicy.Decisions` is configured): The controller reads probe results from Conditions, evaluates schedules and decision rules, and manages `Spec.Paused`. If `Decisions` is not configured, the controller only updates Conditions and does not auto-pause — upper-layer platforms read Conditions to decide.

> **Why a standalone controller?** Introducing probe execution into the existing Sandbox Reconcile loop risks slowing down the core pause/resume path. A standalone controller isolates probe latency and allows independent rate-limiting and error handling.

### API Design

All new types are added to `api/v1alpha1/sandbox_types.go`.

#### 1. `AutoPausePolicy` (on `SandboxSpec`)

```go
// AutoPausePolicy defines scheduled pause/running windows and optional pause
// decision rules. Probes are defined separately in SandboxLifecycle.
// When set, the auto-pause controller evaluates schedules and decisions.
// Probe results (from Lifecycle.Probes) are read via SandboxStatus.Conditions.
// +optional
type AutoPausePolicy struct {
    // Schedules defines time windows during which the sandbox should be in a
    // specific state (running or paused). Overlapping windows are evaluated in
    // order (first match wins). Outside of any window, the controller enforces
    // DefaultState if set; otherwise it does not change Spec.Paused (manual
    // control preserved), unless Decisions is configured — in which case
    // probe-based decisions apply.
    // +optional
    Schedules []PauseSchedule `json:"schedules,omitempty"`

    // DefaultState is the sandbox state when no schedule window is active.
    // If set, the controller enforces this state outside of all schedule windows.
    // If not set, the controller does not change Spec.Paused outside of windows
    // (manual control preserved), unless Decisions is configured.
    // +optional
    DefaultState *PauseScheduleState `json:"defaultState,omitempty"`

    // Timezone is an IANA timezone name for interpreting cron expressions.
    // Default: controller's local timezone.
    // +optional
    Timezone string `json:"timezone,omitempty"`

    // ResumeLeadTime is how early before a resume event (schedule transition or cron task)
    // to start resuming the sandbox. Default: 5m.
    // +optional
    // +kubebuilder:default="5m"
    ResumeLeadTime *metav1.Duration `json:"resumeLeadTime,omitempty"`

    // Decisions defines a list of rules that determine when to pause or resume
    // the sandbox based on probe Conditions. Each rule references a probe by name
    // (defined in Lifecycle.Probes) and matches the Condition message with a
    // regex or Unix timestamp parser.
    // If not set, probes still run and results are written to Conditions,
    // but the controller does NOT manage Spec.Paused — upper-layer
    // platforms read Conditions and decide.
    // +optional
    Decisions []PauseDecisionRule `json:"decisions,omitempty"`

    // ManualOverride records a user's manual pause/resume action that overrides all
    // schedule rules. When set and not expired, the controller enforces
    // ManualOverride.State and skips all schedule evaluation and probe-based decisions.
    // When expired, the controller clears this field and returns to schedule-based control.
    // +optional
    ManualOverride *ManualOverride `json:"manualOverride,omitempty"`
}
```

Added to `SandboxSpec`:

```go
type SandboxSpec struct {
    // ... existing fields ...

    // Lifecycle defines lifecycle hooks and probes for the sandbox.
    // Probes defined here run periodically while the sandbox is Running,
    // writing results to SandboxStatus.Conditions.
    // +optional
    Lifecycle *SandboxLifecycle `json:"lifecycle,omitempty"`

    // AutoPausePolicy defines scheduled pause/running windows and optional
    // pause decision rules. Probes are defined in Lifecycle.
    // +optional
    AutoPausePolicy *AutoPausePolicy `json:"autoPausePolicy,omitempty"`

    EmbeddedSandboxTemplate `json:",inline"`
}
```

#### 2. `SandboxLifecycle` (extending existing type)

Add a `Probes` field to the existing `SandboxLifecycle`:

```go
// SandboxLifecycle defines lifecycle hooks and probes for sandbox.
type SandboxLifecycle struct {
    // PreUpgrade is the action executed before the upgrade.
    // +optional
    PreUpgrade *UpgradeAction `json:"preUpgrade,omitempty"`

    // PostUpgrade is the action executed after the upgrade.
    // +optional
    PostUpgrade *UpgradeAction `json:"postUpgrade,omitempty"`

    // Probes defines a list of named probes that run periodically while the sandbox
    // is Running. Each probe writes its result to a SandboxStatus.Condition with
    // type "agents.kruise.io/<name>". Probes are generic — their semantics (e.g.,
    // "activity detection" vs "cron task detection") are defined by AutoPausePolicy.Decisions,
    // not by the probe itself.
    //
    // Only the Exec probe handler is currently supported; HTTPGet, TCPSocket, and
    // GRPC are rejected by webhook validation.
    // +optional
    Probes []NamedProbe `json:"probes,omitempty"`
}
```

#### 3. `NamedProbe`

```go
// NamedProbe defines a named probe that writes its result to a Sandbox Condition.
// Probes are generic — semantics are defined by the decision layer
// (AutoPausePolicy.Decisions) that references them by name.
type NamedProbe struct {
    // Name is the probe name. The probe result is written to a Condition with
    // type "agents.kruise.io/<Name>". Must be unique within SandboxLifecycle.Probes.
    // +required
    Name string `json:"name"`

    // Probe is the corev1.Probe configuration (exec handler, periodSeconds, etc.).
    // Default PeriodSeconds=30, TimeoutSeconds=5, FailureThreshold=3.
    // +required
    Probe v1.Probe `json:"probe,omitempty"`
}
```

#### 4. `PauseSchedule`

```go
// PauseScheduleState represents the expected sandbox state within a time window.
// +enum
type PauseScheduleState string

const (
    PauseScheduleRunning PauseScheduleState = "running"
    PauseSchedulePaused  PauseScheduleState = "paused"
)

// PauseSchedule defines a time window during which the sandbox should be in a given state.
type PauseSchedule struct {
    // State is the expected sandbox state within this time window.
    // +required
    State PauseScheduleState `json:"state"`

    // Start is a standard 5-field cron expression defining when this window begins.
    // +required
    Start string `json:"start"`

    // End is a standard 5-field cron expression defining when this window ends.
    // +required
    End string `json:"end"`
}
```

#### 5. `PauseDecisionRule` (Decision Rule)

Each rule references a probe name and uses `messageRegex` or `unix` conditions to match the Condition's `message` field to decide the action — not relying on exit codes, but parsing the probe's stdout text.

```go
// PauseAction is the action a decision rule takes when its condition matches.
// +enum
type PauseAction string

const (
    // PauseActionResume resumes or keeps the sandbox running.
    PauseActionResume PauseAction = "resume"
    // PauseActionPause pauses the sandbox (after IdleCount or IdleDuration if set).
    PauseActionPause  PauseAction = "pause"
)

// PauseDecisionRule defines a single pause/resume decision rule.
// The controller evaluates rules in order against probe Conditions.
type PauseDecisionRule struct {
    // Type is the action to take when the condition matches.
    // - "resume": resume or keep the sandbox running
    // - "pause": pause the sandbox (after IdleCount or IdleDuration if set)
    // +required
    Type PauseAction `json:"type"`

    // Probe is the name of the probe to read.
    // The controller reads the Condition "agents.kruise.io/<Probe>".
    // +required
    Probe string `json:"probe"`

    // MessageRegex is a regular expression matched against the Condition's
    // message field (i.e., the probe's stdout). When the regex matches,
    // the rule is considered "hit".
    //
    // Probes should always exit 0 and output semantic text to stdout.
    // The decision is based on message content, not exit codes.
    //
    // Example: "^inactive$" matches when the probe outputs "inactive".
    // +optional
    MessageRegex string `json:"messageRegex,omitempty"`

    // Unix, when true, parses the Condition message as a Unix timestamp
    // (seconds). Used for time-based resume decisions (e.g., cron task
    // detection). The timestamp represents the next event time.
    // +optional
    Unix bool `json:"unix,omitempty"`

    // IdleCount is valid only for Type=pause.
    // Defines how many consecutive times the probe must report idle
    // (i.e., message keeps matching messageRegex) before the
    // controller pauses the sandbox.
    // Deprecated in favor of IdleDuration, which is more intuitive and
    // decoupled from probe frequency.
    // Default: 0 (pause immediately when condition matches).
    // +optional
    // +kubebuilder:default=0
    IdleCount int32 `json:"idleCount,omitempty"`

    // IdleDuration is valid only for Type=pause.
    // Defines how long the probe must continuously report idle
    // (i.e., message keeps matching messageRegex) before the controller
    // pauses the sandbox. If both IdleCount and IdleDuration are set,
    // IdleDuration takes precedence.
    // Example: "15m" means the sandbox pauses after being idle for 15 minutes.
    // +optional
    IdleDuration *metav1.Duration `json:"idleDuration,omitempty"`

    // LeadTime is valid only for Type=resume with Unix=true.
    // How early before the parsed timestamp to resume the sandbox.
    // +optional
    LeadTime *metav1.Duration `json:"leadTime,omitempty"`
}
```

**Rule evaluation order**:

1. Evaluate all `type=pause` + `messageRegex` rules (in declaration order): the first matching rule starts `IdleCount`/`IdleDuration` counting; once the threshold is reached, proceed to step 2. If the message does not match any pause rule (e.g., probe outputs `"active"`), the Agent is considered active and stays Running.
2. Evaluate all `type=resume` + `unix=true` rules: the first matching rule parses the timestamp, sets `NextResumeTime`, and then pauses.
3. If no rule matches → execute default behavior (by schedule).

> The probe's responsibility is to detect "when to pause". When the message does not match a pause rule (e.g., `"active"`), the Agent naturally stays running; no explicit `resume` rule is needed.

#### 6. `ManualOverride`

```go
// ManualOverride records a user's manual action that temporarily overrides all pause
// schedule rules.
type ManualOverride struct {
    // State is the user-desired sandbox state.
    // +required
    State PauseScheduleState `json:"state"`

    // ExpireTime is the absolute time when this override expires.
    // +required
    // +kubebuilder:validation:Format="date-time"
    ExpireTime metav1.Time `json:"expireTime"`
}
```

#### 7. `AutoPauseStatus` (on `SandboxStatus`)

Probe results are reported through standard `SandboxStatus.Conditions` (consistent with PodProbeMarker). `AutoPauseStatus` only stores decision state.

```go
// AutoPauseStatus reports the controller's current auto-pause decision.
// Only populated when Spec.AutoPausePolicy.Decisions is configured.
// Probe results (from Lifecycle.Probes) are always written to
// SandboxStatus.Conditions regardless.
type AutoPauseStatus struct {
    // CurrentState is the controller's current auto-pause decision.
    // +optional
    CurrentState PauseScheduleState `json:"currentState,omitempty"`

    // Reason describes why the sandbox is in this state.
    // +optional
    Reason string `json:"reason,omitempty"`

    // IdleCount tracks how many consecutive times the probe has reported idle
    // (message matching pause regex). Used for idleCount threshold comparison.
    // Cleared when the agent becomes active again (message stops matching pause regex).
    // Deprecated in favor of IdleSince + IdleDuration.
    // +optional
    IdleCount int32 `json:"idleCount,omitempty"`

    // IdleSince records when the current idle period started (i.e., when the
    // probe message first matched a pause rule). Used together with
    // PauseDecisionRule.IdleDuration to determine whether the sandbox has been
    // idle long enough to pause.
    // +optional
    IdleSince *metav1.Time `json:"idleSince,omitempty"`

    // NextResumeTime is when the sandbox will be resumed based on a probe's
    // Unix timestamp output (messageUnix). Computed as: timestamp - leadTime.
    // +optional
    NextResumeTime *metav1.Time `json:"nextResumeTime,omitempty"`

    // NextTransitionTime is when the next schedule window transition will happen.
    // Used by the controller for RequeueAfter calculation.
    // +optional
    NextTransitionTime *metav1.Time `json:"nextTransitionTime,omitempty"`
}
```

Added to `SandboxStatus`:

```go
type SandboxStatus struct {
    // ... existing fields ...

    // Conditions contains probe results and other auto-pause conditions.
    // Each configured probe in Lifecycle.Probes writes a Condition with
    // type "agents.kruise.io/<probe-name>".
    // +optional
    // +patchMergeKey=type
    // +patchStrategy=merge
    // +listType=map
    // +listMapKey=type
    Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

    // AutoPauseStatus reports the controller's current auto-pause decision.
    // Only populated when Spec.AutoPausePolicy.Decisions is configured.
    // +optional
    AutoPauseStatus *AutoPauseStatus `json:"autoPauseStatus,omitempty"`
}
```

**Probe Condition format** (written to `SandboxStatus.Conditions`):

Probes should always exit 0 and output semantic information to stdout (Condition `message`). The decision layer uses `messageRegex` or `messageUnix` in `Decisions` to match message content and decide behavior, not relying on exit codes. On timeout or execution error, status is Unknown (fail-closed).

```yaml
status:
  conditions:
    - type: agents.kruise.io/Active
      status: "True"              # probe exit 0 → True; timeout/error → Unknown
      reason: "Succeeded"          # Succeeded | Timeout | Error | Unhealthy
      message: "active"           # stdout = semantic text, matched by Decisions' messageRegex
      lastTransitionTime: "2026-07-01T10:00:00Z"  # updated when status changes
    - type: agents.kruise.io/Cron
      status: "True"              # probe exit 0 → True (always succeeds)
      reason: "Succeeded"
      message: "1746018000"       # stdout = Unix timestamp, matched by unix=true Decision
      lastTransitionTime: "2026-07-01T09:59:00Z"
```

#### 8. Reason Constants

```go
const (
    // Condition reasons (written to SandboxStatus.Conditions[].reason)
    ProbeReasonSucceeded  = "Succeeded"  // Probe succeeded (exit 0)
    ProbeReasonTimeout    = "Timeout"    // Probe timed out
    ProbeReasonUnhealthy  = "Unhealthy"  // Consecutive failures reached FailureThreshold

    // AutoPauseStatus reasons (written to AutoPauseStatus.Reason)
    PauseReasonScheduleRunning = "ScheduleRunning" // Inside a Running schedule window
    PauseReasonSchedulePaused  = "SchedulePaused"  // Inside a Paused schedule window
    PauseReasonManualOverride  = "ManualOverride"  // User manual override is active
    PauseReasonScheduledResume = "ScheduledResume" // Reached probe-calculated resume time, auto-resumed
    PauseReasonAgentActive     = "AgentActive"      // Agent is active, pause delayed
    PauseReasonInactivePending = "InactivePending"  // Agent is idle but has not reached idleDuration/idleCount
    PauseReasonProbeFailed     = "ProbeFailed"      // Probe failed but not yet thresholded, treated as active (fail-closed)
    PauseReasonProbeUnhealthy  = "ProbeUnhealthy"   // Probe consecutive failures reached threshold, falling back to schedule logic
    PauseReasonDefault         = "Default"          // No schedule match, DefaultState unset, and Decisions not configured
)
```

### Reconcile Logic

The auto-pause controller's `Reconcile` is split into two phases:

#### Phase 1: Probing (executed when Lifecycle.Probes is configured)

When the sandbox is Running, the controller executes each probe at its configured `PeriodSeconds` and writes results to `SandboxStatus.Conditions`.

```
1. If Spec.AutoPausePolicy is nil and Spec.Lifecycle.Probes is empty → return (not managed)

2. If the sandbox is Running, iterate Lifecycle.Probes:
   For each NamedProbe:
   a. Execute the probe (subject to TimeoutSeconds)
   b. Update Condition based on execution result:
      - Success (exit 0, recommended that probes always exit 0):
        Condition status = True, reason = "Succeeded"
        message = stdout (semantic text, matched by Decision's messageRegex)
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

3. If AutoPausePolicy is nil or Decisions is not configured → return (only report Conditions, no decisions)
```

> **Condition update strategy**: Probes should always exit 0 and output semantic information to stdout (Condition `message`). `status=True` means probe execution succeeded; `status=Unknown` means timeout or error. The decision layer matches message content via `messageRegex` or `messageUnix`, not exit codes. When the message first matches a pause rule, the controller increments `AutoPauseStatus.IdleCount` for `idleCount` threshold comparison.

#### Phase 2: Decision-Making (executed only when AutoPausePolicy.Decisions is configured)

```
4. If ManualOverride is set and not expired:
   a. Enforce ManualOverride.State → set Spec.Paused accordingly
   b. Set Reason = "ManualOverride"
   c. RequeueAfter = ManualOverride.ExpireTime - now
   d. Return

5. If ManualOverride is set but expired:
   a. Clear ManualOverride from Spec
   b. Continue to step 6

6. Evaluate schedule to determine desired state and NextTransitionTime
   - If current time falls within a schedule window → desired state = that window's State
   - If outside all windows:
     - If DefaultState is set → desired state = DefaultState
     - If Decisions is configured → desired state = paused (enter probe decision logic)
     - Otherwise → jump to step 9

7. If desired state is "running":
   a. Set Spec.Paused = false, clear IdleCount/IdleSince
   b. Set Reason = "ScheduleRunning"
   c. RequeueAfter = NextTransitionTime - now (or before next Paused window at ResumeLeadTime)
   d. Return

8. If desired state is "paused":
   a. If NextResumeTime is set and now >= NextResumeTime:
      - Resume sandbox: set Paused = false, clear IdleCount/IdleSince
      - Clear NextResumeTime
      - Reason = "ScheduledResume"
      - RequeueAfter = PeriodSeconds (re-evaluate probe in next round)
      - Return
   b. Iterate all type=pause + messageRegex rules in Decisions (in declaration order):
      For each rule, read the referenced probe Condition:
      - If status == Unknown and reason == "Unhealthy":
        - Skip this rule, continue to next (fall back to schedule logic)
      - If status == Unknown (timeout/error, not Unhealthy):
        - fail-closed, treat as active, keep Running, Reason = "ProbeFailed"
          RequeueAfter = PeriodSeconds, return
      - If status == True → match message with messageRegex:
        - If match succeeds → Agent idle:
          - If rule.IdleDuration is set:
            - If status.AutoPauseStatus.IdleSince is not set → set status.AutoPauseStatus.IdleSince = now
            - If now - status.AutoPauseStatus.IdleSince < rule.IdleDuration
              → keep Running, Reason = "InactivePending"
              RequeueAfter = min(PeriodSeconds, status.AutoPauseStatus.IdleSince + rule.IdleDuration - now), return
            - Otherwise (reached rule.IdleDuration) → continue to step 8c
          - Else if rule.IdleCount is set:
            - Increment status.AutoPauseStatus.IdleCount
            - If status.AutoPauseStatus.IdleCount < rule.IdleCount
              → keep Running, Reason = "InactivePending"
              RequeueAfter = PeriodSeconds, return
            - Otherwise (reached rule.IdleCount or rule.IdleCount=0) → continue to step 8c
        - If match fails → Agent active, clear status.AutoPauseStatus.IdleCount/IdleSince
          Keep Running, Reason = "AgentActive"
          RequeueAfter = PeriodSeconds, return
   c. Iterate all type=resume + messageUnix=true rules in Decisions (in declaration order):
      For each rule, read the referenced probe Condition:
      - If status == True → parse message as Unix timestamp (nextFireTime):
        - If parse succeeds → set NextResumeTime = nextFireTime - leadTime
          Set Paused = true (preserve IdleCount/IdleSince, not cleared after pause)
          Reason = "ProbePaused"
          RequeueAfter = NextResumeTime - now, return
        - If parse fails → log warning, continue to next rule
      - If status == Unknown → fail-closed, treat as having a scheduled task, keep Running, Reason = "ProbeFailed"
        RequeueAfter = PeriodSeconds, return
   d. Set Spec.Paused = true (preserve IdleCount/IdleSince, not cleared after pause)
      Reason = "SchedulePaused"
      RequeueAfter = NextTransitionTime - now
      Return

9. If no schedule matches, DefaultState is unset, and Decisions is not configured:
   - Do not change Spec.Paused
   - Reason = "Default"
   - RequeueAfter = NextTransitionTime - now (or default interval)
```

### Probe Contract

> Currently only `exec` probes are supported. `httpGet`, `tcpSocket`, and `grpc` are rejected by webhook validation and can be extended later as needed.

Probes are generic — they have no preset semantics. **Probes should always exit 0** and output semantic information to stdout (Condition `message`). The decision layer uses `messageRegex` or `messageUnix` in `Decisions` to match message content and decide behavior, not relying on exit codes.

#### Design Rationale: Decoupling Probe and Decision

Decoupling Probe and Decision is the core concept of this proposal. For customers with customization needs, we provide the probe mechanism — users write shell scripts to detect the Agent's actual state (session activity, scheduled tasks, custom metrics, etc.), and script output is written to the Condition `message`. Users can flexibly customize their detection logic based on actual situations without modifying the API or controller code.

- **Without `AutoPausePolicy`**: The controller only periodically executes probes in `Lifecycle.Probes` and reports Condition results. Upper-layer platforms read Conditions via informer or `kubectl get` and implement their own pause/resume strategies. Suitable for platforms that already have their own scheduling logic or need to combine multi-dimensional signals for comprehensive decisions.
- **With `AutoPausePolicy.Decisions`**: The controller executes probes and makes decisions simultaneously, automatically managing `Spec.Paused`. Suitable for scenarios that want out-of-the-box behavior without additional orchestration.

This layered design allows the same probe mechanism to serve different operational modes — from fully autonomous to pure reporting — as users choose.

> **Message stability requirement**: Each probe execution writes stdout to the Condition `message`, and Condition updates trigger PATCH requests to Sandbox Status. To reduce unnecessary API server pressure, **probe scripts should output the same message when semantics have not changed**. For example, when the Active probe continuously detects an active Agent, it should always output `"active"` rather than dynamic text containing timestamps or counters. The controller skips Condition updates when the message has not changed, avoiding meaningless Status writes.

#### Condition States

| Execution Result | Condition | Meaning |
|----------|-----------|------|
| exit 0 (success) | status=True, reason="Succeeded" | Probe execution succeeded, message = stdout (semantic text) |
| Timeout | status=Unknown, reason="Timeout" | Probe failed (fail-closed: decision layer treats as active, no pause) |
| Execution error | status=Unknown, reason="Error" | Probe failed (fail-closed: decision layer treats as active, no pause) |
| Consecutive failures >= FailureThreshold | status=Unknown, reason="Unhealthy" | Probe unhealthy; decision layer skips this probe and falls back to schedule logic |

#### Message Content and Regex Matching

Probe stdout is written to the Condition's `message` field. Each rule in `Decisions` matches message using conditions:

| Condition | Matches message | Example |
|------|-------------|------|
| `messageRegex: "^inactive$"` | Regex match | Matches `"inactive"` |
| `messageUnix: true` | Parse as Unix timestamp | Parse `"1751373600"` as timestamp |

Combined with the `type` field, the post-match actions are:

| type | condition | action after match |
|------|------|----------|
| `pause` | `messageRegex` | Start idleDuration/idleCount timing; proceed to next step when threshold reached |
| `resume` | `messageUnix` | Parse timestamp, set NextResumeTime, then pause |

When the message does not match any pause rule (e.g., probe outputs `"active"` which does not match `"^inactive$"`), the Agent is treated as active and stays Running.

> **Probe health mechanism**: `v1.Probe` has built-in `FailureThreshold` (default 3). The controller tracks consecutive failures (timeouts/errors). A single failure remains fail-closed (status=Unknown, treated as active during decision-making); after consecutive failures reach the threshold, the Condition reason changes to "Unhealthy", the controller skips this probe and falls back to schedule logic, and emits a Kubernetes Warning Event. The first successful probe execution resets the failure count.

### State Machine

The diagram below illustrates state transitions using the ScheduledResume scenario as an example:

```
                        ┌─────────────────────┐
                        │  Schedule: Paused    │
                        │  window active       │
                        └──────────┬──────────┘
                                   │
                     ┌─────────────▼──────────────┐
                     │  pause+messageRegex         │
                     │  matches?                   │
                     └─────────────┬──────────────┘
                       not match   │   match
                    ┌────────────────┘ └──────────────────────┐
                    ▼                                        ▼
          ┌──────────────────────┐              ┌──────────────────┐
          │ Reason=AgentActive   │              │ idleDuration/   │
          │ (or ProbeFailed)     │              │ idleCount       │
          │ Requeue=PeriodSeconds│              │ reached?        │
          └──────────────────────┘              └────────┬─────────┘
                                                yes  │  no (pending)
                                        ┌─────────┘    └──────────┐
                                        ▼                         ▼
                                ┌────────────────┐  ┌──────────────────────┐
                                │ resume+messageUnix│  │ Reason=InactivePending│
                                │ matches?        │  │ Requeue=PeriodSeconds │
                                └────────┬─────────┘  └──────────────────────┘
                    yes  │  no
              ┌─────────┘    └──────────┐
              ▼                         ▼
      ┌────────────────────────┐        ┌────────────────────┐
      │ Set NextResumeTime     │        │ Paused=true        │
      │ Paused=true            │        │ Reason=            │
      │ Reason=ProbePaused     │        │ SchedulePaused     │
      └────────┬───────────────┘        └────────────────────┘
              │ time reaches NextResumeTime
              ▼
      ┌───────────────────────┐
      │ Resume sandbox          │
      │ Paused=false            │
      │ Clear NextResumeTime    │
      │ Clear IdleCount/IdleSince│
      │ Reason=ScheduledResume  │
      └───────┬───────────────┘
              │ next Reconcile
              ▼
      ┌────────────────────────────┐
      │ Re-evaluate:               │
      │ pause+regex matches?       │─── no  ──→ keep running (AgentActive)
      │ resume+messageUnix matches?│─── yes ──→ set new NextResumeTime, pause
      │                            │─── no  ──→ pause
      └────────────────────────────┘
```

### Interaction with SandboxSet

- Sandboxes managed by SandboxSet with `claimed=false` are **excluded** from auto-pause management. The controller checks the `agents.kruise.io/sandbox-claimed` label; if it is `false`, Reconcile is skipped.
- Batch configuration: Use `SandboxUpdateOps` label selector to patch `AutoPausePolicy` to multiple Sandboxes at once.
- `SandboxUpdateOps` already supports rolling/partitioned strategies, suitable for gradually rolling out pause policies.

## Alternatives

### Alternative 1: Standalone `SandboxCron` CRD (v1 design)

The v1 design proposed a standalone `SandboxCron` CRD that references Sandboxes via label selector and patches `Spec.Paused` on their behalf.

**Rejected because:**

1. **State locality.** Pause state is best expressed directly on the Sandbox itself. A standalone CRD requires cross-CRD state synchronization, introducing race conditions.
2. **No multi-rule conflicts.** Embedding the policy in Sandbox means each Sandbox has exactly one policy.
3. **Simpler UX.** Users do not need to create and manage an additional CRD.
4. **Direct state correlation.** Pause state is adjacent to Sandbox Phase and Conditions, making debugging easier.

### Alternative 2: External Script (Demo approach)

A Python script that polls OpenClaw's `jobs.json` via `kubectl cp` and patches `Spec.Paused`.

**Rejected because:** Not declarative, no Reconcile loop, `kubectl cp` is fragile and slow.

### Alternative 3: K8s CronJob + kubectl patch

Use native `CronJob` to execute `kubectl patch sandbox` on a schedule.

**Rejected because:** Cannot express probe-based activity detection; one CronJob per Sandbox creates API server load; no state feedback.

### Alternative 4: Fixed Probe Fields (early v3 design)

The early proposal used fixed `activeProbe` and `cronProbe` fields with probe semantics hard-coded in the API.

**Rejected because:**

1. **Inflexible.** Probe semantics hard-coded as "active" and "cron" cannot support other detection needs.
2. **Results not observable.** Probe results stored in a custom `ProbeResults` structure are less intuitive than standard Conditions.
3. **Not K8s idiomatic.** The K8s and OpenKruise communities recommend using Conditions to report status rather than custom structs.
4. **Poor extensibility.** Adding new probe types requires modifying the API, whereas generic named probes only need adding an item to the `probes` list.

## Risks and Mitigations

| Risk | Impact | Mitigation |
|------|------|----------|
| **Probe latency blocking Reconcile** | Controller slows down; other Sandboxes starve | Standalone auto-pause controller has its own workqueue; enforces probe timeout (`TimeoutSeconds`); probes execute asynchronously with concurrency limits |
| **Cron expression timezone mismatch** | Sandbox resumes/pauses at wrong times | Explicit `Timezone` field (IANA name); defaults to controller local timezone; validate cron expressions in webhook admission |
| **Probe command hangs or times out** | Controller waits indefinitely | Each probe call has `TimeoutSeconds`; **single failure sets Condition status=Unknown (fail-closed, treated as active, no pause)**; after consecutive failures reach `FailureThreshold`, reason="Unhealthy", fall back to schedule logic, and emit Warning Event |
| **Probe script environment issues blur idle vs failure** | Probe timeout/error misclassified as idle, causing mistaken pause | Probe failures set Condition status=Unknown (not True); decision layer treats Unknown as active (fail-closed); after consecutive failures reach threshold, reason="Unhealthy" and fall back to schedule logic |
| **ManualOverride forgotten** | Sandbox stays in override state indefinitely | `ExpireTime` is required; controller automatically clears expired overrides; status reports `ManualOverride` reason and `NextTransitionTime` |
| **Conflict with E2B client pause/resume** | Users and controller contend for `Spec.Paused` | Document that once `AutoPausePolicy` is set, users should not directly use E2B Pause/Resume. Future work: webhook validation rejects manual `Spec.Paused` modifications while `AutoPausePolicy` is active |
| **Probe exec requires Pod Running** | Probe fails when Pod is Paused | Probes only execute while sandbox is Running. Paused sandboxes do not need probes |

## Upgrade Strategy

- **API compatibility.** `AutoPausePolicy` is a new optional field. Existing Sandboxes without this field are completely unaffected.
- **Controller deployment.** The auto-pause controller runs inside the existing agent-sandbox-controller binary. No new deployment is needed — just upgrade the image.
- **Feature gate.** Feature gate `AutoPauseController` (default: `false`) controls whether the controller is activated. Supports gradual rollout and quick rollback.
- **Status fields.** `Conditions` and `AutoPauseStatus` are additive fields; old clients that ignore them are unaffected.
- **No breaking changes.** No existing fields are modified or deleted. When `AutoPausePolicy` is not set, `Spec.Paused` continues to work as usual.
- **Gradual adoption.** You can first configure only `lifecycle.probes` without `AutoPausePolicy` (probe-only reporting mode), verify probe Condition results are correct, and then configure `AutoPausePolicy.Decisions` to enable auto-pause.

## Test Plan

### Unit Tests

- **Schedule evaluation:** cron expression parsing, timezone conversion, first-match-wins logic, NextTransitionTime calculation.
- **Probe execution and Condition writing:** exit 0 → status=True, reason="Succeeded", message=stdout (semantic text); timeout → status=Unknown, reason="Timeout"; execution error → status=Unknown, reason="Error"; messageRegex matches message (e.g., `^inactive$`); messageUnix=true parses message as Unix timestamp; invalid message → treat as no scheduled task + warning log.
- **Probe health:** single failure → status=Unknown (fail-closed); consecutive failures < FailureThreshold → reason="Timeout"; consecutive failures >= FailureThreshold → reason="Unhealthy" + Warning Event; first success resets count and reason.
- **Condition lastTransitionTime:** verify it only updates when status changes, used for idleCount calculation.
- **Condition update optimization:** verify Status patch is skipped when message has not changed (reducing API server pressure); patch normally when message changes.
- **IdleCount lifecycle:** increments when Agent idle, preserved after pause, cleared on wake (ScheduledResume / ScheduleRunning), cleared when Agent becomes active again.
- **Decision tree:** ManualOverride active/expired, Running window, Paused window, DefaultState effective, pause+messageRegex match (idle) / mismatch (active), status=Unknown fail-closed, idleCount threshold reached/not reached, resume+messageUnix match/mismatch, probe Unhealthy fallback, Default (no schedule match, no DefaultState, and no Decisions).
- **Probe-only reporting mode:** When AutoPausePolicy is not configured, probe results are written to Conditions, AutoPauseStatus is nil, and Spec.Paused is not modified.
- **RequeueAfter calculation:** verify correct requeue time for each branch.
- **ManualOverride expiration:** verify the field is cleared and schedule control resumes.

### Integration Tests

- **End-to-end pause/resume loop:** Create a Sandbox with a short schedule (e.g., 2-minute window) and verify it transitions to Paused and back to Running.
- **ScheduledResume flow:** Mock Cron probe output timestamp (message), verify resume+messageUnix match causes the sandbox to pause and resume at NextResumeTime, and the next Reconcile re-evaluates probes.
- **idleCount smoothing:** Mock Active probe alternating `"active"`/`"inactive"`, verify no pause within idleCount and pause only after idleCount is reached.
- **ManualOverride:** Set a short-TTL ManualOverride, verify it takes precedence, and schedule control resumes after expiration.
- **Pause rule mismatch delay:** Mock Active probe message output `"active"` (does not match `"^inactive$"`), verify pause is delayed and retried after PeriodSeconds.
- **Probe unhealthy fallback:** Mock Active probe failing consecutively up to FailureThreshold, verify Condition reason="Unhealthy", controller skips the probe, falls back to schedule logic, and emits Warning Event.
- **Probe-only reporting mode:** Without AutoPausePolicy configured, verify probe results are written to Conditions but Spec.Paused is not modified.

### E2E Tests

- Deploy an OpenClaw sandbox with pause policy on a kind cluster.
- Verify nightly pause and morning resume.
- Verify scheduled-task-aware resume: create an OpenClaw cron job and check that the sandbox resumes before the task triggers.
- Verify probe-only reporting mode: do not configure AutoPausePolicy, and confirm probe results are correctly reported via kubectl reading Conditions.

## Implementation History

- [x] 2026-06-26: Initial proposal draft, combining v1 (SandboxCron CRD) and v2 (embedded AutoPausePolicy) designs.
- [x] 2026-06-30: Reuse `corev1.Probe` instead of custom `PauseProbe`; narrow support to Exec only; add probe health mechanism (FailureThreshold + fail-closed + Warning Event).
- [x] 2026-07-01 (v3): Decoupling of probing and decision-making. Introduce optional `PauseDecision`; split `AutoPauseStatus` into `ProbeResults` + `DecisionState`.
- [x] 2026-07-01 (v4): Following PodProbeMarker, replace fixed `activeProbe`/`cronProbe` fields with **generic named probe list**; write probe results to standard `SandboxStatus.Conditions` (type=`agents.kruise.io/<name>`); `PauseDecision` references probe names to define semantics. More K8s-idiomatic and extensible.
- [x] 2026-07-01 (v5): Decision layer changed to **regex matching on message**, not exit codes. Introduce `ProbeMatchRule` (probe name + messageRegex); probes always exit 0 and output semantic text (e.g., `"active"`/`"inactive"`); `PauseDecision` uses `activeRule`/`inactiveRule`/`cronRule` instead of `inactiveProbe`/`cronProbe`. More flexible, supports arbitrary output formats.
- [x] 2026-07-01 (v6): Remove `PauseDecision` wrapper, directly use `decisions: []PauseDecisionRule` on `AutoPausePolicy`. Each rule is self-contained with `type` (resume/pause) + `probe` + condition (`messageRegex` or `messageUnix`) + action parameters (`idleDuration`/`leadTime`/`taskTimeout`). Simpler, rules are decoupled.
- [x] 2026-07-01 (v7): Remove `resume` + `messageRegex` rules. Probes only detect "when to pause"; when message does not match pause rules, the sandbox naturally stays running, no explicit resume rule needed. `holdDuration` renamed to `idleDuration`, semantics more intuitive (how long idle before pause).
- [x] 2026-07-01 (v8): Scenario 4 simplification — pure probe-driven mode does not need `timezone`/`resumeLeadTime`/`defaultState`. Reconcile logic adjusted: when `decisions` is configured but `schedules` and `defaultState` are not, default to probe decision logic (desired state = paused). `ResumeLeadTime` comment clarified to only apply to schedule-transition resume.
- [x] 2026-07-01 (v9): Remove `taskTimeout` and `CronResumeUntil`. After resume, no longer force Running for a fixed duration; instead rely on Active probe naturally detecting Agent activity — while the Agent executes tasks the probe outputs "active" to keep running, and after tasks finish it switches to "inactive" to start idleDuration counting. Simpler, and the "grace period" is dynamically determined by the probe rather than a fixed value.
- [x] 2026-07-01 (v10): `unix` field renamed to `messageUnix`, symmetric with `messageRegex`, clearly indicating parsing of the Condition message field. Active probe added `openclaw cron list --json` `status == "running"` detection to fix false pauses when main session cron tasks run without updating `lastInteractionAt`.
- [x] 2026-07-01 (v11): Move `Probes` from `AutoPausePolicy` to `SandboxLifecycle`, decoupling probes from pause policy at the API structural level. `AutoPausePolicy` retains only scheduling and decision rules; probes naturally belong to `SandboxLifecycle` (alongside `PreUpgrade`/`PostUpgrade`). Probe-only reporting mode does not require configuring `AutoPausePolicy`.
- [x] 2026-07-01 (v12): `idleDuration` (time) changed to `idleCount` (count), better fitting the probe periodic execution model. `AutoPauseStatus.InactiveSince` (timestamp) changed to `AutoPauseStatus.IdleCount` (int32 count), directly recording consecutive idle counts. Reconcile logic changed from time-difference comparison to count-increment comparison.
- [ ] TODO: Community review and feedback.
- [ ] TODO: API type implementation + `make generate manifests`.
- [ ] TODO: Auto-pause controller implementation.
- [ ] TODO: Unit tests + integration tests.
