---
title: Sandbox Prometheus Metrics Observability
authors:
  - "@KeyOfSpectator"
reviewers:
  - "@zmberg"
  - "@furykerry"
  - "@liangxiaoping"
  - "@BH4AWS"
creation-date: 2026-04-22
last-updated: 2026-10-28
status: implemented
see-also:
  - "https://github.com/openkruise/agents/pull/258"
---

# Sandbox Prometheus Metrics Observability

## Table of Contents

- [Sandbox Prometheus Metrics Observability](#sandbox-prometheus-metrics-observability)
  - [Table of Contents](#table-of-contents)
  - [Summary](#summary)
  - [Motivation](#motivation)
    - [Goals](#goals)
    - [Non-Goals](#non-goals)
  - [Proposal](#proposal)
    - [User Stories](#user-stories)
      - [Story 1: Platform Operator Monitors Sandbox Readiness Latency](#story-1-platform-operator-monitors-sandbox-readiness-latency)
      - [Story 2: SRE Alerts on SandboxSet Available Replica Shortage](#story-2-sre-alerts-on-sandboxset-available-replica-shortage)
      - [Story 3: Developer Analyzes SandboxClaim Efficiency](#story-3-developer-analyzes-sandboxclaim-efficiency)
      - [Story 4: Operator Detects Abnormal Pause/Resume Operations](#story-4-operator-detects-abnormal-pauseresume-operations)
    - [All Metrics Summary](#all-metrics-summary)
    - [Design Details](#design-details)
      - [Sandbox Controller Metrics](#sandbox-controller-metrics)
      - [SandboxSet Controller Metrics](#sandboxset-controller-metrics)
      - [SandboxClaim Controller Metrics](#sandboxclaim-controller-metrics)
      - [Sandbox Manager API Metrics](#sandbox-manager-api-metrics)
      - [Proxy Metrics](#proxy-metrics)
      - [E2B Server Metrics](#e2b-server-metrics)
      - [Metric Collection Architecture](#metric-collection-architecture)
      - [Useful PromQL Examples](#useful-promql-examples)
    - [Implementation Details/Notes/Constraints](#implementation-detailsnotesconstraints)
    - [Risks and Mitigations](#risks-and-mitigations)
  - [Future Work](#future-work)
  - [Test Plan](#test-plan)
  - [Implementation History](#implementation-history)

## Summary

This proposal introduces comprehensive Prometheus metrics observability for the OpenKruise Agents Sandbox ecosystem. Following the [kube-state-metrics](https://github.com/kubernetes/kube-state-metrics) design patterns, we expose rich lifecycle, status, and operational metrics across six components:

- **Sandbox Controller** — sandbox instance lifecycle, phase, and condition metrics
- **SandboxSet Controller** — resource pool replica tracking metrics
- **SandboxClaim Controller** — claim operation phase and timing metrics
- **Sandbox Manager** — API operation latency and success/failure counters, claim/clone stage-level metrics, route sync metrics
- **Proxy** — route table and peer topology metrics
- **E2B Server** — snapshot operation metrics

All metrics are registered through controller-runtime's metrics registry and exposed via the standard `/metrics` HTTP endpoint, ready for Prometheus scraping and Grafana dashboard visualization.

## Motivation

OpenKruise Agents is a cloud-native platform for managing AI agent sandbox workloads on Kubernetes. As production deployments grow in scale and complexity, operators and SREs need deep observability into the platform to ensure reliability and performance.

Without proper metrics, teams cannot:
- Monitor sandbox creation-to-ready latency for SLA compliance
- Detect resource pool exhaustion in SandboxSet before it impacts users
- Measure SandboxClaim operation efficiency and success rates
- Identify abnormal spikes in pause/resume/delete operations
- Perform data-driven capacity planning and autoscaler tuning

Prometheus is the de facto standard for Kubernetes observability. By following kube-state-metrics conventions, we ensure familiarity for existing Kubernetes operators and seamless integration with the broader cloud-native monitoring ecosystem.

### Goals

1. **Full CRD lifecycle coverage**: Expose metrics for every phase and condition of Sandbox, SandboxSet, and SandboxClaim resources.
2. **kube-state-metrics compatibility**: Follow established naming conventions (`_info`, `_created`, `_status_phase`, single-direction condition timestamps) so users can apply existing Kubernetes monitoring knowledge.
3. **API operation observability**: Track latency histograms and success/failure counters for all Sandbox Manager REST API operations.
4. **Grafana-ready**: All metrics are designed to support common Grafana dashboard patterns (heatmaps, gauges, time series, tables).
5. **Zero performance impact**: Metrics recording uses lightweight O(1) atomic operations within the Reconcile loop, adding negligible overhead.
6. **Clean metric lifecycle**: Metrics for deleted resources are properly cleaned up via `DeleteLabelValues`/`DeletePartialMatch` to prevent time series leaks.

### Non-Goals

- **Application-level metrics**: Metrics from within the sandbox containers (e.g., user workload CPU/memory) are out of scope.
- **Distributed tracing**: OpenTelemetry tracing integration is not part of this proposal.
- **Checkpoint Controller metrics**: The Checkpoint CRD is defined but the controller does not yet have a metrics layer; this will be addressed when the Checkpoint controller is fully implemented.
- **Grafana dashboard JSON templates**: While the metrics are designed to be Grafana-ready, shipping pre-built dashboard JSON is deferred to future work.

## Proposal

### User Stories

#### Story 1: Platform Operator Monitors Sandbox Readiness Latency

As a platform operator, I want to monitor the time from sandbox creation to Ready state via Grafana, so that I can ensure sandboxes are provisioned within the expected SLA. I can compute this using `sandbox_status_ready_time - sandbox_created` and set alerts when P99 latency exceeds thresholds.

#### Story 2: SRE Alerts on SandboxSet Available Replica Shortage

As an SRE, I want to receive alerts when a SandboxSet's available replicas fall below the desired count, so that I can proactively scale the pool before user-facing sandbox creation starts failing. I can use `sandboxset_available_replicas / sandboxset_desired_replicas` to compute pool utilization and alert when it drops below a threshold.

#### Story 3: Developer Analyzes SandboxClaim Efficiency

As a developer integrating with the E2B SDK, I want to analyze how long SandboxClaim operations take to complete and what their success rate is, so that I can optimize my claim parameters (timeout, batch size). I can compute claim duration via `sandboxclaim_completion_time - sandboxclaim_claim_start_time`.

#### Story 4: Operator Detects Abnormal Pause/Resume Operations

As an operator, I want to monitor Sandbox Manager API metrics to detect unusual spikes in pause or resume operation failures, so that I can quickly identify and troubleshoot platform issues. I can track `rate(sandbox_pause_total{source="e2b",result="failure"}[5m])` for anomaly detection.

### All Metrics Summary

The following table provides a comprehensive overview of all Prometheus metrics exposed by the OpenKruise Agents platform.

| Component | Metric Name | Type | Labels | Description |
|---|---|---|---|---|
| Sandbox Controller | `sandbox_info` | Gauge | `namespace`, `name`, `created_by_kind`, `created_by_name`, `node`, `pod_uid`, `sandbox_template` | Information about the sandbox (always 1). |
| Sandbox Controller | `sandbox_created` | Gauge | `namespace`, `name` | Unix creation timestamp of the sandbox. |
| Sandbox Controller | `sandbox_deletion_timestamp` | Gauge | `namespace`, `name` | Unix deletion timestamp of the sandbox. |
| Sandbox Controller | `sandbox_status_phase` | Gauge | `namespace`, `name`, `phase` | Current phase of the sandbox (1 for active phase). |
| Sandbox Controller | `sandbox_status_ready` | Gauge | `namespace`, `name` | Whether the Ready condition is True (1) or not (0). |
| Sandbox Controller | `sandbox_status_ready_time` | Gauge | `namespace`, `name` | Unix timestamp of last transition to Ready=True. |
| Sandbox Controller | `sandbox_status_paused_time` | Gauge | `namespace`, `name` | Unix timestamp of last transition to SandboxPaused=True. |
| Sandbox Controller | `sandbox_status_resumed_time` | Gauge | `namespace`, `name` | Unix timestamp of last transition to SandboxResumed=True. |
| Sandbox Controller | `sandbox_status_inplace_update_done` | Gauge | `namespace`, `name` | Whether the InplaceUpdate condition is True (1) or not (0). |
| Sandbox Controller | `sandbox_status_inplace_update_done_time` | Gauge | `namespace`, `name` | Unix timestamp of last transition to InplaceUpdate=True. |
| Sandbox Controller | `sandbox_create_total` | Counter | `namespace`, `result`; ConstLabels: `source="k8s"` | One mutually exclusive result per new Sandbox UID observed by the controller: pool initialization, scale-out and replenishment, Claim cold-start, direct creation and Clone. `success` on first Ready, `failure` when Failed is observed before first Ready. Claiming an existing pooled sandbox, Resume, repeated Ready and pod rebuilds are not counted. |
| Sandbox Controller | `sandbox_create_duration_seconds` | Histogram | `namespace`; ConstLabels: `source="k8s"` | Successful creations only, covering the same scenarios as `sandbox_create_total{source="k8s"}`: trusted duration from CreationTimestamp to the first Ready transition. |
| Sandbox Controller | `sandbox_inplace_update_duration_seconds` | Histogram | — | Duration of in-place update operations (seconds). |
| Sandbox Controller | `sandbox_pause_duration_seconds` | Histogram | ConstLabels: `source="k8s"` | Duration of sandbox pause operations (seconds). |
| Sandbox Controller | `sandbox_resume_duration_seconds` | Histogram | ConstLabels: `source="k8s"` | Duration of sandbox resume operations (seconds). |
| Sandbox Controller | `sandbox_labels` | Gauge | `namespace`, `name`, `label_<key>` (dynamic, opt-in) | Sandbox labels as Prometheus labels. Controlled via `--metric-labels-allowlist`. |
| SandboxSet Controller | `sandboxset_created` | Gauge | `namespace`, `name` | Unix creation timestamp of the SandboxSet. |
| SandboxSet Controller | `sandboxset_replicas` | Gauge | `namespace`, `name` | Current total number of replicas. |
| SandboxSet Controller | `sandboxset_available_replicas` | Gauge | `namespace`, `name` | Number of available replicas ready to be claimed. |
| SandboxSet Controller | `sandboxset_desired_replicas` | Gauge | `namespace`, `name` | Desired replica count from spec.replicas. |
| SandboxSet Controller | `sandboxset_updated_replicas` | Gauge | `namespace`, `name` | Number of replicas updated to the latest revision. |
| SandboxSet Controller | `sandboxset_updated_available_replicas` | Gauge | `namespace`, `name` | Number of updated replicas that are also available. |
| Sandbox Controller | `sandbox_claim_total` | Counter | `namespace,result,type`; ConstLabels: `source="k8s"` | One per completed instance claim cycle published in the claim-observation annotation, covering SandboxClaim and E2B claims. `type="hot-start"`: a pooled sandbox is claimed, including one still creating; `type="cold-start"`: a new sandbox is created and claimed because the pool is short. `failure` means post-claim processing failed after binding; failures before any sandbox is bound are not counted. |
| Sandbox Controller | `sandbox_claim_duration_seconds` | Histogram | `namespace,result,type`; ConstLabels: `source="k8s"` | Wall-clock duration of the same claim cycles, from claim start to the end of post-claim processing, recorded for both success and failure. |
| SandboxClaim Controller | `sandboxclaim_info` | Gauge | `namespace`, `name`, `template_name`, `uid` | SandboxClaim metadata info (always 1). |
| SandboxClaim Controller | `sandboxclaim_created` | Gauge | `namespace`, `name` | Unix creation timestamp of the SandboxClaim. |
| SandboxClaim Controller | `sandboxclaim_status_phase` | Gauge | `namespace`, `name`, `phase` | Current phase of the claim (1 for active phase). |
| SandboxClaim Controller | `sandboxclaim_claim_start_time` | Gauge | `namespace`, `name` | Unix timestamp when claiming started. |
| SandboxClaim Controller | `sandboxclaim_completion_time` | Gauge | `namespace`, `name` | Unix timestamp when claim reached Completed. |
| SandboxClaim Controller | `sandboxclaim_claimed_replicas` | Gauge | `namespace`, `name` | Current number of successfully claimed replicas. |
| SandboxClaim Controller | `sandboxclaim_desired_replicas` | Gauge | `namespace`, `name` | Desired number of replicas to claim. |
| Sandbox Manager | `sandbox_create_duration_seconds` | Histogram | `namespace`; ConstLabels: `source="e2b"` | Successful creations only, for new sandboxes from Claim cold-start or Clone: trusted duration from CreationTimestamp to first Ready, not API call latency. |
| Sandbox Manager | `sandbox_create_total` | Counter | `namespace`, `result`; ConstLabels: `source="e2b"` | One mutually exclusive result per new Sandbox UID created by sandbox-manager: Claim cold-start on pool shortage and Clone. `success` on first Ready, `failure` when Failed is observed before first Ready. Pool hits, Resume and pod rebuilds are not counted; delivery failures after Ready do not revoke success. Not a request count. |
| Sandbox Manager | `sandbox_claim_creation_responses` | Counter | `namespace`, `result`; ConstLabels: `source="e2b"` | Deprecated. One final result per sandbox-manager Claim call, including failures before any sandbox is bound; not an alias of the instance-level `sandbox_claim_total`, and recorded only when the compatibility switch is enabled. |
| Sandbox Manager | `sandbox_pause_duration_seconds` | Histogram | ConstLabels: `source="e2b"` | Duration of sandbox pause operations (seconds). |
| Sandbox Manager | `sandbox_pause_total` | Counter | `namespace`, `result`; ConstLabels: `source="e2b"` | Total number of sandbox pause requests. |
| Sandbox Manager | `sandbox_resume_duration_seconds` | Histogram | ConstLabels: `source="e2b"` | Duration of sandbox resume operations (seconds). |
| Sandbox Manager | `sandbox_resume_total` | Counter | `namespace`, `result`; ConstLabels: `source="e2b"` | Total number of sandbox resume requests. |
| Sandbox Manager | `sandbox_delete_duration_seconds` | Histogram | ConstLabels: `source="e2b"` | Duration of sandbox delete operations (seconds). |
| Sandbox Manager | `sandbox_delete_total` | Counter | `namespace`, `result`; ConstLabels: `source="e2b"` | Total number of sandbox delete requests. |
| Sandbox Manager | `sandbox_claim_duration_seconds` | Histogram | `namespace,result,type`; ConstLabels: `source="e2b"` | Wall-clock duration of each bound instance claim process, from attempt start to the end of post-claim processing, recorded for both success and failure; `type` has the same meaning as in `sandbox_claim_total`. |
| Sandbox Manager | `sandbox_claim_total` | Counter | `namespace,result,type,lock_type`; ConstLabels: `source="e2b"` | One per sandbox bound by a sandbox-manager claim; batch claims and retries that bind different sandboxes count each sandbox. `type="hot-start"` (`lock_type` `update`/`speculate`): a pooled sandbox is claimed, including one still creating; `type="cold-start"` (`lock_type="create"`): a new sandbox is created and claimed because the pool is short. `failure` means post-claim processing failed after binding; invalid parameters, missing template, admission and lock conflicts before binding are not counted. |
| Sandbox Manager | `sandbox_claim_retries` | Histogram | — | Distribution of retry counts per claim operation. |
| Sandbox Manager | `sandbox_clone_duration_seconds` | Histogram | — | Clone operation total duration (seconds). |
| Sandbox Manager | `sandbox_clone_total` | Counter | `result` | Total number of clone operations. |
| Sandbox Manager | `sandbox_route_sync_duration_seconds` | Histogram | `type` | Route sync operation duration (seconds). |
| Sandbox Manager | `sandbox_route_sync_total` | Counter | `type`, `result` | Total number of route sync operations. |
| Proxy | `sandbox_routes_total` | Gauge | — | Current number of routes in the routing table. |
| Proxy | `sandbox_peers_total` | Gauge | — | Current number of connected peer nodes. |
| E2B Server | `sandbox_snapshot_duration_seconds` | Histogram | — | Duration of snapshot creation operations (seconds). |
| E2B Server | `sandbox_snapshot_total` | Counter | `result` | Total number of snapshot operations. |

**Summary**: Gauges describe resource state, histograms describe operation durations, and counters describe request or instance outcomes across the six components. Deprecated aliases are optional.

### Design Details

#### Sandbox Controller Metrics

Source: `pkg/controller/sandbox/metrics.go`

The Sandbox controller exposes the following metrics for each Sandbox resource:

| Metric Name | Type | Labels | Description |
|---|---|---|---|
| `sandbox_info` | Gauge | `namespace`, `name`, `created_by_kind`, `created_by_name`, `node`, `pod_uid`, `sandbox_template` | Sandbox metadata info metric (always 1). Includes owner reference labels for identifying which SandboxSet or SandboxClaim created the sandbox. `node` is the node name from `status.nodeName`. `pod_uid` is the underlying Pod UID from `status.podInfo.podUID`, used for precise Pod correlation. `sandbox_template` is the SandboxTemplate name from label `agents.kruise.io/sandbox-template`. |
| `sandbox_created` | Gauge | `namespace`, `name` | Unix creation timestamp of the sandbox (`metadata.creationTimestamp`). |
| `sandbox_deletion_timestamp` | Gauge | `namespace`, `name` | Unix deletion timestamp of the sandbox (`metadata.deletionTimestamp`). Only set when the sandbox is being deleted. |
| `sandbox_status_phase` | Gauge | `namespace`, `name`, `phase` | Current phase of the sandbox. Following the `kube_pod_status_phase` pattern, only the active phase is emitted with value `1`; stale phase series are deleted to reduce cardinality. |
| `sandbox_status_ready` | Gauge | `namespace`, `name` | Whether the Ready condition is True (`1`) or not (`0`). |
| `sandbox_status_ready_time` | Gauge | `namespace`, `name` | Unix timestamp of the last transition to Ready=True (`condition.lastTransitionTime`). |
| `sandbox_status_paused_time` | Gauge | `namespace`, `name` | Unix timestamp of the last transition to SandboxPaused=True. |
| `sandbox_status_resumed_time` | Gauge | `namespace`, `name` | Unix timestamp of the last transition to SandboxResumed=True. |
| `sandbox_status_inplace_update_done` | Gauge | `namespace`, `name` | Whether the InplaceUpdate condition is True (`1`) or not (`0`). |
| `sandbox_status_inplace_update_done_time` | Gauge | `namespace`, `name` | Unix timestamp of the last transition to InplaceUpdate=True. |
| `sandbox_create_total` | Counter | `namespace`, `result`; ConstLabels: `source="k8s"` | One mutually exclusive result per new Sandbox UID observed by the controller. Applies to pool initialization, scale-out and replenishment, Claim cold-start, direct creation through the Kubernetes API, and Clone. `success` at first Ready, `failure` when Failed is observed before first Ready. Claiming an existing pooled sandbox, Resume, repeated Ready and pod rebuilds of the same CR are not counted. |
| `sandbox_create_duration_seconds` | Histogram | `namespace`; ConstLabels: `source="k8s"` | Same scenarios as `sandbox_create_total{source="k8s"}`, successful creations only: recorded once per UID from CreationTimestamp to the first Ready transition when both timestamps are trusted; otherwise only the Counter is incremented. Buckets: `ExponentialBuckets(0.02, 2, 12)`. |
| `sandbox_claim_total` | Counter | `namespace,result,type`; ConstLabels: `source="k8s"` | One per completed instance claim cycle published in the `agents.kruise.io/claim-observation` annotation. Applies to SandboxClaim and E2B claims; a batch claim counts each claimed sandbox. `type="hot-start"`: a pooled sandbox is claimed, including one still creating; `type="cold-start"`: a new sandbox is created and claimed because the pool is short. `failure` means post-claim processing failed after binding; failures before any sandbox is bound are not counted. |
| `sandbox_claim_duration_seconds` | Histogram | `namespace,result,type`; ConstLabels: `source="k8s"` | Same claim cycles as `sandbox_claim_total{source="k8s"}`: wall-clock duration from claim start to the end of post-claim processing, recorded for both success and failure. |
| `sandbox_inplace_update_duration_seconds` | Histogram | — | Duration of in-place update operations from start (InplaceUpdate=False) to completion (InplaceUpdate=True) in seconds. Buckets: 1, 2, 5, 10, 20, 30, 60, 120, 300, 600. Observed once per update cycle. |
| `sandbox_pause_duration_seconds` | Histogram | ConstLabels: `source="k8s"` | Duration of sandbox pause operations from start (SandboxPaused=False) to completion (SandboxPaused=True) in seconds. Buckets: 1, 2, 5, 10, 20, 30, 60, 120, 300, 600. Observed once per pause cycle. |
| `sandbox_resume_duration_seconds` | Histogram | ConstLabels: `source="k8s"` | Duration of sandbox resume operations from start (SandboxResumed=False) to completion (SandboxResumed=True) in seconds. Buckets: 1, 2, 5, 10, 20, 30, 60, 120, 300, 600. Observed once per resume cycle. |
| `sandbox_labels` | Gauge | `namespace`, `name`, `label_<key>` | Sandbox labels converted to Prometheus labels. Opt-in: disabled by default, enabled via `--metric-labels-allowlist`. |

**Design Patterns**

- **Phase metrics** follow the `kube_pod_status_phase` pattern: only the currently active phase is emitted as a time series with gauge value `1`; stale phase series are deleted to reduce cardinality. This enables queries like `count by (phase) (sandbox_status_phase == 1)`.

- **Condition metrics** use a lean recording approach:
  - **Ready and InplaceUpdate** conditions: a boolean gauge (`sandbox_status_ready`, `sandbox_status_inplace_update_done`) is `1` when True, `0` otherwise. A companion `_time` gauge records the `lastTransitionTime` Unix timestamp when the condition is True.
  - **Paused and Resumed** conditions: only the `_time` gauge is recorded (`sandbox_status_paused_time`, `sandbox_status_resumed_time`), without a boolean gauge. The paused/resumed state can be inferred from the sandbox phase (`sandbox_status_phase`) or the presence of the timestamp. This avoids redundant time series.
  - Additionally, **pause and resume duration Histograms** (`sandbox_pause_duration_seconds`, `sandbox_resume_duration_seconds`) track the operation latency from condition False→True transition, with `source="k8s"` ConstLabel to differentiate from the Manager's `source="e2b"` counterparts.

- This single-direction approach keeps queries simple while significantly reducing metric cardinality. Alerting on the False direction uses `metric == 0` instead of a dedicated gauge.

- **Source label for cross-component CRUD metrics**: CRUD duration metrics that have equivalents across Controller and Manager (e.g., sandbox creation) use `ConstLabels` to add a `source` label for explicit origin differentiation:
  - `source="k8s"` 标识 controller 观测组件，也包含 E2B 触发的实例领取和创建。
  - `source="e2b"` 标识 manager 观测组件，两侧观测范围可能重叠，不跨 source 相加作为唯一数量。
  - This follows the spirit of kube-state-metrics' job-based differentiation but provides a more explicit source identifier within the metric itself.
  - Example PromQL: `histogram_quantile(0.99, sum(rate(sandbox_create_duration_seconds_bucket[5m])) by (le, source))`

- **Opt-in label metrics** follow the [kube-state-metrics](https://github.com/kubernetes/kube-state-metrics) `kube_pod_labels` pattern. The `sandbox_labels` metric is **not enabled by default** to avoid unbounded cardinality. Operators opt in by specifying which label keys to export via the `--metric-labels-allowlist` flag:
  - Usage: `--metric-labels-allowlist=key1,key2,key3`
  - Label keys are sanitized: non-alphanumeric characters are replaced with underscores (`_`), and each key is prefixed with `label_`.
  - Example: `--metric-labels-allowlist=app,env,version` exports:
    ```
    sandbox_labels{namespace="ns",name="sbx",label_app="myapp",label_env="prod",label_version="v1"} 1
    ```
  - When the allowlist is empty (default), `sandbox_labels` is not registered and produces no time series.

**Sandbox Phase State Machine**

```
Pending ──────► Running ──────► Paused ──────► Resuming ──────► Running
                  │                                                │
                  ▼                                                ▼
                Failed                                         Succeeded
                  │
                  ▼
              Terminating
```

- **Pending**: Sandbox accepted but Pod not yet scheduled or started.
- **Running**: Pod is running and all containers have been started.
- **Paused**: Sandbox has been paused (containers frozen via CRIU or pod deleted with persistent state).
- **Resuming**: Sandbox is being resumed from a paused state.
- **Succeeded**: All containers exited successfully (exit code 0).
- **Failed**: At least one container terminated with a non-zero exit code.
- **Terminating**: Sandbox is being cleaned up after deletion.

**Sandbox Conditions**

| Condition Type | Description |
|---|---|
| `Ready` | True when the sandbox pod is fully ready to serve requests. False during creation, in-place update, or failure. |
| `SandboxPaused` | True when the sandbox has been successfully paused. False when unpaused or never paused. |
| `SandboxResumed` | True when the sandbox has been successfully resumed. False during resume or before first resume. |
| `InplaceUpdate` | True when an in-place update has completed successfully. False when an in-place update is in progress. |

#### SandboxSet Controller Metrics

Source: `pkg/controller/sandboxset/metrics.go`

The SandboxSet controller exposes the following metrics for each SandboxSet resource:

| Metric Name | Type | Labels | Description |
|---|---|---|---|
| `sandboxset_created` | Gauge | `namespace`, `name` | Unix creation timestamp of the SandboxSet. |
| `sandboxset_replicas` | Gauge | `namespace`, `name` | Current total number of replicas (`status.replicas`), including creating, available, running, and paused sandboxes. |
| `sandboxset_available_replicas` | Gauge | `namespace`, `name` | Number of available replicas (`status.availableReplicas`) that are ready to be claimed. |
| `sandboxset_desired_replicas` | Gauge | `namespace`, `name` | Desired replica count from `spec.replicas`. |
| `sandboxset_updated_replicas` | Gauge | `namespace`, `name` | Number of sandboxes that have been updated to the latest `UpdateRevision` (`status.updatedReplicas`). |
| `sandboxset_updated_available_replicas` | Gauge | `namespace`, `name` | Number of updated sandboxes that are also available (`status.updatedAvailableReplicas`). |

**Capacity Planning and Autoscaler Integration**

These replica metrics are essential for:

- **Pool utilization monitoring**: `sandboxset_available_replicas / sandboxset_desired_replicas` gives the real-time pool availability ratio. Alerts can fire when this drops below a threshold (e.g., 20%).
- **Capacity planning**: Tracking `sandboxset_replicas` over time reveals usage patterns for right-sizing the pool.
- **Rolling update progress**: Comparing `sandboxset_updated_replicas` with `sandboxset_replicas` shows how far a rolling update has progressed. `sandboxset_updated_available_replicas` confirms how many updated sandboxes are actually ready.
- **HPA/custom autoscaler integration**: External autoscalers can use `sandboxset_available_replicas` as a scaling signal to maintain a minimum pool buffer.

#### SandboxClaim Controller Metrics

Source: `pkg/controller/sandboxclaim/metrics.go`

The SandboxClaim controller exposes the following metrics for each SandboxClaim resource:

| Metric Name | Type | Labels | Description |
|---|---|---|---|
| `sandboxclaim_info` | Gauge | `namespace`, `name`, `template_name`, `uid` | SandboxClaim metadata info metric (always 1). Includes `template_name` label identifying which SandboxSet pool is being claimed from. `uid` is the SandboxClaim resource UID from `metadata.uid`, used for precise deduplication and correlation. |
| `sandboxclaim_created` | Gauge | `namespace`, `name` | Unix creation timestamp of the SandboxClaim. |
| `sandboxclaim_status_phase` | Gauge | `namespace`, `name`, `phase` | Current phase of the claim. Uses the same compact pattern as `sandbox_status_phase`: only the active phase is emitted with value `1`; stale phase series are deleted to reduce cardinality. |
| `sandboxclaim_claim_start_time` | Gauge | `namespace`, `name` | Unix timestamp when the claiming process started (`status.claimStartTime`). |
| `sandboxclaim_completion_time` | Gauge | `namespace`, `name` | Unix timestamp when the claim reached Completed phase (`status.completionTime`). |
| `sandboxclaim_claimed_replicas` | Gauge | `namespace`, `name` | Current number of successfully claimed replicas (`status.claimedReplicas`). |
| `sandboxclaim_desired_replicas` | Gauge | `namespace`, `name` | Desired number of replicas to claim (`spec.replicas`). |

**SandboxClaim Phase State Machine**

```
"" (empty) ──────► Claiming ──────► Completed
```

- **"" (empty)**: Initial state before the controller starts processing.
- **Claiming**: The controller is actively claiming sandboxes from the target SandboxSet pool.
- **Completed**: The claim process has finished — either all desired replicas were claimed, or the `claimTimeout` was reached.

The claim duration can be computed as `sandboxclaim_completion_time - sandboxclaim_claim_start_time`, and claim success rate as `sandboxclaim_claimed_replicas / sandboxclaim_desired_replicas`.

#### Sandbox Manager API Metrics

Source: `pkg/sandbox-manager/metrics.go`

The Sandbox Manager exposes operational metrics for its REST API endpoints:

| Metric Name | Type | Labels | Description |
|---|---|---|---|
| `sandbox_create_duration_seconds` | Histogram | `namespace`; ConstLabels: `source="e2b"` | Same scenarios as `sandbox_create_total{source="e2b"}`, successful creations only: trusted duration from CreationTimestamp to first Ready; if a timestamp is missing or reversed, only the Counter is incremented. Not API call latency. |
| `sandbox_create_total` | Counter | `namespace`, `result`; ConstLabels: `source="e2b"` | One mutually exclusive result per new Sandbox UID created by sandbox-manager. Applies to Claim cold-start when the pool is short and to Clone. `success` at first Ready, `failure` when Failed is observed before first Ready. Pool hits (hot-start), Resume and pod rebuilds are not counted; delivery failures after Ready do not revoke success; unknown timeouts and rejected Create calls are not inferred as failures. |
| `sandbox_claim_creation_responses` | Counter | `namespace`, `result`; ConstLabels: `source="e2b"` | Deprecated request-level result: one per sandbox-manager Claim call, including failures before any sandbox is bound. Recorded only when the compatibility switch is enabled; it is not equivalent to the instance-level `sandbox_claim_total`. |
| `sandbox_pause_duration_seconds` | Histogram | ConstLabels: `source="e2b"` | Duration of sandbox pause operations in seconds. |
| `sandbox_pause_total` | Counter | `namespace`, `result`; ConstLabels: `source="e2b"` | Total number of sandbox pause requests. |
| `sandbox_resume_duration_seconds` | Histogram | ConstLabels: `source="e2b"` | Duration of sandbox resume operations in seconds. |
| `sandbox_resume_total` | Counter | `namespace`, `result`; ConstLabels: `source="e2b"` | Total number of sandbox resume requests. |
| `sandbox_delete_total` | Counter | `namespace`, `result`; ConstLabels: `source="e2b"` | Total number of sandbox delete requests. |
| `sandbox_delete_duration_seconds` | Histogram | ConstLabels: `source="e2b"` | Duration of sandbox delete operations in seconds. |
| `sandbox_claim_duration_seconds` | Histogram | `namespace,result,type`; ConstLabels: `source="e2b"` | Same claim processes as `sandbox_claim_total{source="e2b"}`: wall-clock duration from attempt start to the end of post-claim processing, recorded for both success and failure; not the duration of the outer batch call or a sum of stage durations. |
| `sandbox_claim_total` | Counter | `namespace,result,type,lock_type`; ConstLabels: `source="e2b"` | One per sandbox bound by a sandbox-manager claim. `type="hot-start"` (`lock_type` `update`/`speculate`): a pooled sandbox is claimed, including one still creating; `type="cold-start"` (`lock_type="create"`): a new sandbox is created and claimed because the pool is short. `success` after post-claim processing completes, `failure` when it fails after binding. Retries that bind different sandboxes count each sandbox; invalid parameters, missing template, admission and lock conflicts before binding are not counted, and there is no `unknown` type. |
| `sandbox_claim_retries` | Histogram | — | Distribution of retry counts per claim operation. |
| `sandbox_clone_duration_seconds` | Histogram | — | Clone operation total duration in seconds. |
| `sandbox_clone_total` | Counter | `result` | Total number of clone operations. Label `result` is `"success"` or `"failure"`. |
| `sandbox_route_sync_duration_seconds` | Histogram | `type` | Route sync operation duration in seconds. |
| `sandbox_route_sync_total` | Counter | `type`, `result` | Total number of route sync operations. |

**Deprecated `*_responses` counters**

The claim/pause/resume/delete result counters follow the Prometheus `_total` naming convention and share
their names with the Sandbox Controller counters (distinguished by `source="e2b"` / `source="k8s"`).
Their former names are deprecated and kept only for migration:

| Deprecated Name | Replacement |
|---|---|
| `sandbox_claim_creation_responses` | `sandbox_claim_total{source="e2b"}`（实例单位，不是旧请求数量的等值替换） |
| `sandbox_pause_responses` | `sandbox_pause_total{source="e2b"}` |
| `sandbox_resume_responses` | `sandbox_resume_total{source="e2b"}` |
| `sandbox_delete_responses` | `sandbox_delete_total{source="e2b"}` |

旧 Claim Counter 每个 manager 调用记录一次最终结果，包括绑定前失败；新 Counter 每个实际绑定实例领取周期一次。单位不同，聚合掉 `type` / `lock_type` 也不是等值别名。pause/resume/delete 的旧新名称仍保持相同标签及数值。

All four legacy counters are registered and incremented only with the process startup flag
`--enable-deprecated-response-metrics=true` (the default). Setting it to `false` at startup skips
registration, increments, and child-series creation for the old names. This is not a runtime toggle.
Canonical `_total` counters always record outcomes, so disabling aliases does not lose operation counts.
Migrate dashboards and alerts before disabling compatibility; the aliases and flag will be removed in a future release.

**实例领取与新建 CR 的指标契约**

Claim 的单位是已绑定具体 Sandbox CR 的一次实例领取过程，两个组件保持相同单位。`source="e2b"` 表示 manager，`source="k8s"` 表示 controller，不是互斥的入口协议。同一次 E2B 领取可由两侧观测；不能跨 source 相加作为唯一领取数或创建数。

| 指标 | 变量标签 | 口径 |
|---|---|---|
| `sandbox_claim_total` | `namespace,result,type`；manager 另有 `lock_type` | 每个已绑定领取周期一次，success/failure 互斥 |
| `sandbox_claim_duration_seconds` | `namespace,result,type` | 该次领取开始至全部后处理成功或失败结束的墙钟时间 |
| `sandbox_create_total` | `namespace,result` | 每个新 UID 的首次 Ready 或 Ready 前观测到的启动终态 Failed |
| `sandbox_create_duration_seconds` | `namespace` | 仅成功，从 CreationTimestamp 至首次 Ready 的可信转换时间 |

所有指标均有固定 `source` 标签。`type` 直接复用 `agents.kruise.io/claim-method` 的值：`hot-start` 包含 `lock_type="update"` 和 `"speculate"`，即已有池实例，无论是否 Ready；`cold-start` 对应 `lock_type="create"`，池不足时脱离池新建并领取。没有 `hot`、`cold`、`code` 或 `unknown` 类型。

参数、模板、准入和绑定前锁竞争失败不进入实例 Claim。绑定后 runtime、token、CSI 等全部后处理成功才计 success，失败计 failure；绑定前重试不计，实际绑定不同实例的重试分别记录。Histogram 不使用批量请求总耗时，也不以阶段耗时之和代替墙钟时间。

| 场景 | Claim | Create |
|---|---|---|
| 池初始化、扩容、领取后补货 | 不计 | controller 每个新 UID 一次 |
| 领取已有池实例，包括仍在启动的实例 | hot-start 每实例过程一次；controller 也观测 E2B 领取 | 领取动作不增加，池实例自身首次创建另行统计 |
| 池不足，新建并领取 | cold-start 每实例过程一次 | 两侧按实际观测每个新 UID 一次 |
| 用户直接通过 K8s API 创建 | 不计 | 只有 controller 每个新 UID 一次 |
| Clone 产出新 CR | 不计；保留 Clone 请求指标 | 两侧按实际观测每个新 UID 一次 |
| 一份 K8s SandboxClaim 领取 2 hot-start、1 cold-start | 分别 +2、+1，不对整个 Completed 请求 +1 | cold-start 新 CR 及池补货分别计数 |
| 新 UID 启动 Failed，重试另一 UID Ready | 两次实例 Claim 的 failure/success | 两个 UID 的 failure/success |

Create success 早于 runtime、token、CSI 等交付处理；Ready 后交付 failure 不撤销 Create success。Create API 拒绝、未知超时和取消不推断创建失败。Resume、重复 Ready、同 CR Pod 重建不增加 Create。manager 不监听池补货或直接 K8s 创建；Claim/Clone 累积创建事实后按 UID 去重。Counter 与成功 Histogram 使用同一 UID 去重入口；缺少可信时间只保留 Counter，不伪造耗时。

**跨组件领取观测协议**

`agents.kruise.io/claim-observation` 保存一条最新周期的 JSON：

```json
{"id":"cycle-id","method":"hot-start","startedAt":"2026-10-28T10:00:00.123456789Z","completedAt":"2026-10-28T10:00:00.234567890Z","result":"success"}
```

- `method` 复用现有分类；`claim-method` label 本身不表示交付成功。
- TryClaimSandbox 与绑定 Create/Update 原子写入新周期 ID、method、RFC3339Nano 开始时间，清除旧结果；同 UID 再领取使用新 ID。
- 后处理结束发布完成时间及结果，先于失败清理。JSON Patch 同时校验 UID 和初始周期值，旧周期不能覆盖新周期。
- 使用已有 informer-backed client，不新增 APIReader Get/List。发布遵守请求取消，最多 5 秒、瞬态错误最多 3 次尝试；发布错误日志记录并返回为 Infra 观测诊断，不改变业务结果、重试分类或清理职责。
- 用户 metadata 不能覆盖观测字段，也不能将其纳入回收 key list。新池实例、cold-start 创建和 Clone 不继承旧观测；Clone 不发布 Claim。回收可保留最近历史，下次绑定原子覆盖。
- Infra 不注册 collector，只返回逐实例事实。manager 按 UID+周期去重；Sandbox controller 在同一入口记录 Counter/Histogram，保存每 UID 最新周期及 RV，拒绝重复及旧版本。SandboxClaim controller 保留 info、phase、replicas、时间 gauge，不再计 canonical Claim。
- controller 只计缓存确实观测到的完成周期；发布失败、快速删除、周期覆盖、缓存遗漏或重启可造成两侧差异，不承诺严格等量或跨重启审计。历史无观测的对象不补造事件，启动期已有新格式完成观测可记录一次。
- 删除只清理对应实例内存去重，不清零 namespace 累计值；异步 gauge 清理不清除同名新 UID 结果。UID、名称和周期 ID 不增加为 Counter/Histogram label。

**迁移与查询**

`sandbox_creation_total` 改为 `sandbox_create_total`，`sandbox_creation_duration_seconds` 改为 `sandbox_create_duration_seconds`；不提供 creation 双写别名，旧查询必须改名。Claim 新增 `type`，Histogram 新增 `result`；manager 保留 `lock_type` 用于 update/speculate 细分。旧请求级 Claim 与新实例级 Claim 不能等值替换。

```promql
# 实例领取增量及成功率；保留 source/type，不跨 source 合并
sum by (source, type) (increase(sandbox_claim_total[5m]))
sum by (source, type) (rate(sandbox_claim_total{result="success"}[5m]))
  / sum by (source, type) (rate(sandbox_claim_total[5m]))

# cold-start 领取成功 P99，hot-start 查询替换 type 值
histogram_quantile(0.99, sum by (le, source, type) (rate(sandbox_claim_duration_seconds_bucket{result="success",type="cold-start"}[5m])))

# 创建增量、成功率及首次 Ready 耗时 P99；不跨 source 合并
sum by (source, result) (increase(sandbox_create_total[5m]))
sum by (source) (rate(sandbox_create_total{result="success"}[5m]))
  / sum by (source) (rate(sandbox_create_total[5m]))
histogram_quantile(0.99, sum by (le, source) (rate(sandbox_create_duration_seconds_bucket[5m])))
```

**Delete observation boundaries**

`source` identifies the component that emits a metric; it is not a mutually exclusive request-protocol
split. The manager's `sandbox_delete_total{source="e2b"}` counts `Kill` call results
(success/failure), and `sandbox_delete_duration_seconds{source="e2b"}` records the call duration only
on success. The controller's `sandbox_delete_total{source="k8s"}` counts observed CR deletion
completions (today `result` is always success); its histogram of the same name measures the time from
`DeletionTimestamp` to the informer Delete callback and is recorded only when the deletion start was
observed. A CR deletion triggered through E2B is also observed by the controller, so queries must
filter on `source` instead of summing both sides as independent user operations.

**Histogram Bucket Configuration**

两侧 Claim 使用 `append(prometheus.ExponentialBuckets(0.01, 2, 16), 600, 1200)`，覆盖 hot-start 和长 cold-start。两侧 Create 使用 `prometheus.ExponentialBuckets(0.02, 2, 12)`，仅成功耗时。其他操作沿用各自既有 bucket，不随本轮修订调整。

#### Proxy Metrics

Source: `pkg/proxy/metrics.go`

The Proxy component exposes topology metrics for routing and peer discovery:

| Metric Name | Type | Labels | Description |
|---|---|---|---|
| `sandbox_routes_total` | Gauge | — | Current number of routes in the routing table. |
| `sandbox_peers_total` | Gauge | — | Current number of connected peer nodes. |

These gauges provide real-time visibility into the proxy's routing table size and cluster membership, useful for diagnosing routing issues and monitoring cluster health.

#### E2B Server Metrics

Source: `pkg/servers/e2b/metrics.go`

The E2B Server exposes metrics for snapshot operations:

| Metric Name | Type | Labels | Description |
|---|---|---|---|
| `sandbox_snapshot_duration_seconds` | Histogram | — | Duration of snapshot creation operations in seconds. |
| `sandbox_snapshot_total` | Counter | `result` | Total number of snapshot operations. Label `result` is `"success"` or `"failure"`. |

#### Metric Collection Architecture

```
┌─────────────────────────────────────────────────────────┐
│                  agent-sandbox-controller                │
│                                                         │
│  ┌──────────────┐  ┌──────────────┐  ┌───────────────┐  │
│  │   Sandbox    │  │  SandboxSet  │  │ SandboxClaim  │  │
│  │  Controller  │  │  Controller  │  │  Controller   │  │
│  │              │  │              │  │               │  │
│  │ Reconcile()  │  │ Reconcile()  │  │  Reconcile()  │  │
│  │  ├─record    │  │  ├─record    │  │  ├─record     │  │
│  │  └─delete    │  │  └─delete    │  │  └─delete     │  │
│  └──────┬───────┘  └──────┬───────┘  └───────┬───────┘  │
│         │                 │                  │          │
│         └────────┬────────┴──────────────────┘          │
│                  ▼                                       │
│     controller-runtime metrics.Registry                 │
│                  │                                       │
│                  ▼                                       │
│           /metrics endpoint ◄──── Prometheus scrape      │
└─────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────┐
│                    sandbox-manager                       │
│                                                         │
│  ┌──────────────────────────────┐                       │
│  │       REST API Handlers      │                       │
│  │  create / pause / resume /   │                       │
│  │  delete / claim / clone      │                       │
│  │  ├─ Observe(latency)         │                       │
│  │  └─ Inc(result)              │                       │
│  └──────────────┬───────────────┘                       │
│                 ▼                                        │
│     controller-runtime metrics.Registry                 │
│                 │                                        │
│                 ▼                                        │
│          /metrics endpoint ◄──── Prometheus scrape       │
└─────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────┐
│                       proxy                             │
│                                                         │
│  ┌──────────────────────────────┐                       │
│  │   Route Table & Peer Mgmt   │                       │
│  │  ├─ Set(routes_total)        │                       │
│  │  └─ Set(peers_total)         │                       │
│  └──────────────┬───────────────┘                       │
│                 ▼                                        │
│     controller-runtime metrics.Registry                 │
│                 │                                        │
│                 ▼                                        │
│          /metrics endpoint ◄──── Prometheus scrape       │
└─────────────────────────────────────────────────────────┘
```

Key design decisions:

1. **Registration**: All metrics are registered via `metrics.Registry.MustRegister()` in package `init()` functions, leveraging controller-runtime's built-in metrics infrastructure.
2. **Recording**: Metrics are updated within each `Reconcile()` call after the resource state is resolved. This ensures metrics always reflect the latest observed state.
3. **Cleanup**: When a resource is deleted (detected via `errors.IsNotFound` in Reconcile or via finalizer logic), `deleteSandboxMetrics(namespace, name)` (and equivalents for other resources) removes all associated time series using `DeleteLabelValues` and `DeletePartialMatch`. This prevents stale metric accumulation.
4. **Manager metrics**：Claim/Create 消费 Infra 返回的逐实例事实：Claim 使用单次领取墙钟耗时，Create 使用首次 Ready 的可信转换时间；不是外层 API 请求耗时。其余请求指标保留既有操作结果和耗时口径，旧 Claim 请求指标单独记录。

#### Useful PromQL Examples

```promql
# Sandbox creation to ready latency (seconds)
sandbox_status_ready_time - sandbox_created

# SandboxSet pool utilization ratio (higher = more consumed)
1 - (sandboxset_available_replicas / sandboxset_desired_replicas)

# SandboxClaim completion duration (seconds)
sandboxclaim_completion_time - sandboxclaim_claim_start_time

# manager 实例领取成功率（非请求成功率，聚合 lock_type）
sum(rate(sandbox_claim_total{source="e2b",result="success"}[5m]))
  / sum(rate(sandbox_claim_total{source="e2b"}[5m]))

# Number of sandboxes in each phase
count by (phase) (sandbox_status_phase == 1)

# Sandbox pause operation P99 latency (seconds)
histogram_quantile(0.99, rate(sandbox_pause_duration_seconds_bucket[5m]))

# Average sandbox creation duration (seconds, last 5m)
rate(sandbox_create_duration_seconds_sum{source="e2b"}[5m])
  / rate(sandbox_create_duration_seconds_count{source="e2b"}[5m])

# SandboxSet rolling update progress
sandboxset_updated_replicas / sandboxset_replicas

# Sandboxes currently not ready
count(sandbox_status_ready == 0)

# SandboxClaim claim fulfillment ratio
sandboxclaim_claimed_replicas / sandboxclaim_desired_replicas

# controller 实例领取成功 P99（保留 type）
histogram_quantile(0.99, sum by (le, type) (rate(sandbox_claim_duration_seconds_bucket{source="k8s",result="success"}[5m])))

# controller 实例领取平均耗时（保留 type/result）
sum by (type, result) (rate(sandbox_claim_duration_seconds_sum{source="k8s"}[5m]))
  / sum by (type, result) (rate(sandbox_claim_duration_seconds_count{source="k8s"}[5m]))

# Sandbox resume failure rate (last 5m)
rate(sandbox_resume_total{source="e2b",result="failure"}[5m])
  / rate(sandbox_resume_total{source="e2b"}[5m])

# 实例领取耗时 P99（保留 source/type/result）
histogram_quantile(0.99, sum by (le, source, type, result) (rate(sandbox_claim_duration_seconds_bucket[5m])))

# Clone failure rate (last 5m)
rate(sandbox_clone_total{result="failure"}[5m])

# Current routing table size
sandbox_routes_total

# Current connected peers
sandbox_peers_total

# Route sync P99 duration (seconds)
histogram_quantile(0.99, rate(sandbox_route_sync_duration_seconds_bucket[5m]))

# Route sync failure rate (last 5m)
rate(sandbox_route_sync_total{result="failure"}[5m])

# Snapshot creation P99 duration (seconds)
histogram_quantile(0.99, rate(sandbox_snapshot_duration_seconds_bucket[5m]))

# Snapshot failure rate (last 5m)
rate(sandbox_snapshot_total{result="failure"}[5m])

# Average claim retry count (last 5m)
rate(sandbox_claim_retries_sum[5m])
  / rate(sandbox_claim_retries_count[5m])

# Delete operation P99 duration (seconds)
histogram_quantile(0.99, rate(sandbox_delete_duration_seconds_bucket[5m]))

# Sandbox creation-to-ready P99 latency (seconds, k8s source)
histogram_quantile(0.99, rate(sandbox_create_duration_seconds_bucket{source="k8s"}[5m]))

# Sandbox creation-to-ready average latency (seconds, k8s source)
rate(sandbox_create_duration_seconds_sum{source="k8s"}[5m]) / rate(sandbox_create_duration_seconds_count{source="k8s"}[5m])

# Compare creation duration across sources (P99)
histogram_quantile(0.99, sum(rate(sandbox_create_duration_seconds_bucket[5m])) by (le, source))

# Sandbox pause P99 duration (seconds, k8s source)
histogram_quantile(0.99, rate(sandbox_pause_duration_seconds_bucket{source="k8s"}[5m]))

# Sandbox resume P99 duration (seconds, k8s source)
histogram_quantile(0.99, rate(sandbox_resume_duration_seconds_bucket{source="k8s"}[5m]))

# Compare pause duration across sources (P99)
histogram_quantile(0.99, sum(rate(sandbox_pause_duration_seconds_bucket[5m])) by (le, source))

# InplaceUpdate P99 duration (seconds)
histogram_quantile(0.99, rate(sandbox_inplace_update_duration_seconds_bucket[5m]))
```

##### Phase Duration Analysis (via Timestamp Subtraction)

By subtracting existing timestamp metrics, you can calculate how long a Sandbox stays in each phase without any additional metrics:

```promql
# Pending duration (creation to readiness)
sandbox_status_ready_time - sandbox_created

# Running duration (readiness to pause, first running cycle)
sandbox_status_paused_time - sandbox_status_ready_time

# Paused duration (pause completion to resume completion)
sandbox_status_resumed_time - sandbox_status_paused_time

# Full lifecycle duration (creation to deletion)
sandbox_deletion_timestamp - sandbox_created

# Running duration (resume to the next pause or deletion)
# Combine sandbox_status_resumed_time with the subsequent event timestamp
```

> **Notes**:
>
> - The queries above are based on the timestamp chain: `sandbox_created` → `sandbox_status_ready_time` → `sandbox_status_paused_time` → `sandbox_status_resumed_time` → `sandbox_deletion_timestamp`.
> - Timestamp metrics only record the most recent transition value; in multiple pause/resume cycles, earlier timestamps are overwritten.
> - The per-operation latency distribution for each pause/resume can be analyzed via the `sandbox_pause_duration_seconds` and `sandbox_resume_duration_seconds` Histograms.
> - Paused duration (`resumed_time - paused_time`) includes both the waiting time and the Resuming operation time. To isolate the Resuming operation latency alone, use the `sandbox_resume_duration_seconds` Histogram.

### Implementation Details/Notes/Constraints

1. **Metric lifecycle management**: Every metrics file implements a paired `record*Metrics()` / `delete*Metrics()` function set. The record function is called on each successful Reconcile. The delete function is called when the resource is confirmed deleted. This ensures no orphaned time series remain after resource deletion.

2. **Label cardinality**: The primary label dimensions are `namespace` and `name`, whose cardinality is proportional to the number of live resources. The `phase` label has a fixed set of values (7 for Sandbox, 2 for SandboxClaim). The `created_by_kind`, `created_by_name`, `node`, `pod_uid`, and `sandbox_template` labels on `sandbox_info` are cleaned up via `DeletePartialMatch` since they have variable values. The `uid` label on `sandboxclaim_info` has 1:1 cardinality with the resource itself. Overall cardinality is well-controlled.

3. **Thread safety**: All metric operations use `prometheus.GaugeVec`, `prometheus.CounterVec`, and `prometheus.Histogram` from the Prometheus client library, which are inherently concurrent-safe. The `WithLabelValues().Set()` / `.Inc()` / `.Observe()` operations use internal sync mechanisms (atomic operations and mutexes).

4. **Performance characteristics**: Prometheus metric recording involves an O(1) hash map lookup to find the metric series, followed by an atomic float64 set/increment. This adds sub-microsecond overhead per metric operation. With ~12 metrics per Sandbox (including opt-in `sandbox_labels`), ~6 per SandboxSet, ~7 per SandboxClaim, ~21 for Sandbox Manager API, ~2 for Proxy, and ~2 for E2B Server, the total overhead per Reconcile is negligible (under 1μs).

5. **Condition timestamp semantics**: All condition timestamp metrics use `condition.LastTransitionTime.Unix()`. This timestamp updates only when the condition status changes (not on every Reconcile), which is consistent with Kubernetes API conventions and kube-state-metrics behavior.

6. **Helper functions**: The reusable helper function `recordConditionTrueMetric()` encapsulates the single-direction condition recording pattern, reducing code duplication and ensuring consistent behavior across all conditions.

### Risks and Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| **Time series growth in large clusters**: Total time series count is O(N × M) where N = resource count and M = metrics per resource (~13 for Sandbox). A cluster with 10,000 sandboxes would generate ~130K time series. | Medium — may increase Prometheus memory and storage usage. | `delete*Metrics()` functions actively clean up series for deleted resources. Prometheus `--storage.tsdb.retention` can be tuned. Series count is proportional to *live* resources only. |
| **Condition timestamp clock skew**: `LastTransitionTime` is set by the controller using the node's local clock. In multi-node clusters, minor clock drift is possible. | Low — timestamps are used for relative duration computation, not absolute wall-clock correlation. | Use cluster-internal NTP synchronization. This is the same approach used by kube-state-metrics and the Kubernetes API server itself. |
| **Metric scrape overhead**: If Prometheus scrape interval is very aggressive (e.g., 1s), the `/metrics` endpoint handler may add load. | Low — controller-runtime's metric handler is lightweight. | Use the standard 15–30s scrape interval recommended by Prometheus best practices. |
| **Label value explosion on `sandbox_info`**: The `created_by_name` label has cardinality equal to the number of distinct SandboxSets/SandboxClaims. | Low — bounded by the number of controller resources. | `DeletePartialMatch` ensures cleanup. This pattern matches `kube_pod_info`'s `created_by_name` label. |

## Future Work

- **Checkpoint Controller metrics**: Once the Checkpoint controller is fully implemented, add corresponding lifecycle metrics (phase, completion time, duration) following the same design patterns.
- **Reconcile performance metrics**: Leverage controller-runtime's built-in `controller_runtime_reconcile_total` and `controller_runtime_reconcile_time_seconds` metrics, and consider adding custom sub-step timing.
- **SandboxSet scale operation metrics**: Track scale-up and scale-down events with counters and latency histograms.
- ~~**Proxy/Gateway routing metrics**~~: ✅ Implemented — `sandbox_routes_total`, `sandbox_peers_total`, `sandbox_route_sync_duration_seconds`, `sandbox_route_sync_total` now expose route table size, peer topology, and route sync performance.
- ~~**Memberlist cluster metrics**~~: ✅ Partially implemented — `sandbox_peers_total` tracks connected peer node counts. Join/leave events and gossip sync status are deferred.
- **Webhook admission metrics**: Record admission request counts, rejection rates, and latency for the sandbox validation/mutation webhooks.
- **Cache hit/miss metrics**: Track sandbox-manager internal cache effectiveness for sandbox lookups.
- **Grafana dashboard JSON templates**: Provide pre-built, importable Grafana dashboard definitions covering all metrics.

## Test Plan

Each metrics file has a corresponding comprehensive unit test file:

- `pkg/controller/sandbox/metrics_test.go` — Tests for all 14 Sandbox metrics (including opt-in `sandbox_labels`)
- `pkg/controller/sandboxset/metrics_test.go` — Tests for all 6 SandboxSet metrics
- `pkg/controller/sandboxclaim/metrics_test.go` — Tests for all 7 SandboxClaim metrics
- `pkg/sandbox-manager/metrics_test.go` — Tests for Sandbox Manager API metrics (claim, clone, delete, route sync)
- `pkg/proxy/metrics_test.go` — Tests for Proxy route/peer topology metrics
- `pkg/servers/e2b/metrics_test.go` — Tests for E2B Server snapshot metrics

Testing approach:

1. **Exact value validation**: Uses `prometheus/client_golang/prometheus/testutil` to assert exact metric values after recording. Each test creates a resource with specific state, calls `record*Metrics()`, and verifies the expected metric output using `testutil.CollectAndCompare`.

2. **True-direction coverage**: For Sandbox condition metrics, tests cover the True direction, verifying that:
   - When condition is True: the True-direction gauge is `1` and the True timestamp is set.
   - When condition is False: the True-direction gauge is `0`.

3. **Deletion coverage**: Tests verify that `delete*Metrics()` properly removes all time series by checking that `testutil.CollectAndCount` returns `0` after deletion.

4. **Edge cases**: Tests cover:
   - Empty phase (no phase metrics emitted)
   - Nil timestamps (timestamp metrics not set)
   - Multiple conditions on the same resource
   - Resources with and without owner references
   - Phase transitions (old phase set to `0`, new phase set to `1`)
   - Claim：2 hot-start + 1 cold-start、绑定前/后失败、实例重试、同 UID 新周期、重复/旧 RV、回收历史及发布失败。
   - Create：池补货、直接创建、cold-start、Clone、首次 Ready/启动 Failed、可信时间缺失、Ready 后交付失败、同名新 UID 与重复 Ready/Resume/Pod 重建。
   - 观测协议：UID/周期 guarded patch、取消及有限重试、metadata 防伪和 Clone 隔离、兼容开关完全停止旧名注册及 child series。
   - 导出名称、source/type/result 标签、两组件 bucket 对齐及并发去重。

## Implementation History

- 04/10/2026: PR [#258](https://github.com/openkruise/agents/pull/258) merged — Initial Sandbox lifecycle metrics with bidirectional condition timestamps
- 04/22/2026: Extended to full observability coverage including SandboxSet, SandboxClaim, and Sandbox Manager API metrics
- 04/22/2026: Added claim/clone stage-level metrics, delete latency, route sync metrics, proxy topology metrics, and E2B snapshot metrics
