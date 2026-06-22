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
last-updated: 2026-04-22
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

This proposal introduces comprehensive Prometheus metrics observability for the OpenKruise Agents Sandbox ecosystem. Following the [kube-state-metrics](https://github.com/kubernetes/kube-state-metrics) design patterns, we expose rich lifecycle, status, and operational metrics across four components:

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

As an operator, I want to monitor Sandbox Manager API metrics to detect unusual spikes in pause or resume operation failures, so that I can quickly identify and troubleshoot platform issues. I can track `rate(sandbox_pause_responses{result="failure"}[5m])` for anomaly detection.

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
| Sandbox Controller | `sandbox_creation_duration_seconds` | Histogram | ConstLabels: `source="k8s"` | Duration from sandbox creation to Ready condition (seconds). |
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
| SandboxClaim Controller | `sandboxclaim_info` | Gauge | `namespace`, `name`, `template_name`, `uid` | SandboxClaim metadata info (always 1). |
| SandboxClaim Controller | `sandboxclaim_created` | Gauge | `namespace`, `name` | Unix creation timestamp of the SandboxClaim. |
| SandboxClaim Controller | `sandboxclaim_status_phase` | Gauge | `namespace`, `name`, `phase` | Current phase of the claim (1 for active phase). |
| SandboxClaim Controller | `sandboxclaim_claim_start_time` | Gauge | `namespace`, `name` | Unix timestamp when claiming started. |
| SandboxClaim Controller | `sandboxclaim_completion_time` | Gauge | `namespace`, `name` | Unix timestamp when claim reached Completed. |
| SandboxClaim Controller | `sandboxclaim_claimed_replicas` | Gauge | `namespace`, `name` | Current number of successfully claimed replicas. |
| SandboxClaim Controller | `sandboxclaim_desired_replicas` | Gauge | `namespace`, `name` | Desired number of replicas to claim. |
| SandboxClaim Controller | `sandboxclaim_claim_duration_seconds` | Histogram | — | Duration of claim from start to completion (seconds). |
| Sandbox Manager | `sandbox_creation_duration_seconds` | Histogram | ConstLabels: `source="e2b"` | Duration of sandbox creation via E2B API (seconds). |
| Sandbox Manager | `sandbox_creation_responses` | Counter | `result` | Total number of sandbox creation responses. |
| Sandbox Manager | `sandbox_pause_duration_seconds` | Histogram | ConstLabels: `source="e2b"` | Duration of sandbox pause operations (seconds). |
| Sandbox Manager | `sandbox_pause_responses` | Counter | `result` | Total number of sandbox pause responses. |
| Sandbox Manager | `sandbox_resume_duration_seconds` | Histogram | ConstLabels: `source="e2b"` | Duration of sandbox resume operations (seconds). |
| Sandbox Manager | `sandbox_resume_responses` | Counter | `result` | Total number of sandbox resume responses. |
| Sandbox Manager | `sandbox_delete_duration_seconds` | Histogram | ConstLabels: `source="e2b"` | Duration of sandbox delete operations (seconds). |
| Sandbox Manager | `sandbox_delete_responses` | Counter | `result` | Total number of sandbox delete responses. |
| Sandbox Manager | `sandbox_claim_duration_seconds` | Histogram | — | Claim operation total duration (seconds). |
| Sandbox Manager | `sandbox_claim_total` | Counter | `result`, `lock_type` | Total number of claim operations. |
| Sandbox Manager | `sandbox_claim_retries` | Histogram | — | Distribution of retry counts per claim operation. |
| Sandbox Manager | `sandbox_clone_duration_seconds` | Histogram | — | Clone operation total duration (seconds). |
| Sandbox Manager | `sandbox_clone_total` | Counter | `result` | Total number of clone operations. |
| Sandbox Manager | `sandbox_route_sync_duration_seconds` | Histogram | `type` | Route sync operation duration (seconds). |
| Sandbox Manager | `sandbox_route_sync_total` | Counter | `type`, `result` | Total number of route sync operations. |
| Proxy | `sandbox_routes_total` | Gauge | — | Current number of routes in the routing table. |
| Proxy | `sandbox_peers_total` | Gauge | — | Current number of connected peer nodes. |
| E2B Server | `sandbox_snapshot_duration_seconds` | Histogram | — | Duration of snapshot creation operations (seconds). |
| E2B Server | `sandbox_snapshot_total` | Counter | `result` | Total number of snapshot operations. |

**Summary**: 48 metrics total — 26 Gauge, 13 Histogram, 9 Counter — across 6 components.

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
| `sandbox_creation_duration_seconds` | Histogram | ConstLabels: `source="k8s"` | Duration from sandbox creation to Ready condition in seconds. Buckets: 1, 2, 5, 10, 20, 30, 60, 120, 300, 600. Observed once per sandbox when first reaching Ready. |
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
  - Controller metrics use `source="k8s"` to identify the Kubernetes native controller path.
  - Manager metrics use `source="e2b"` to identify the E2B-compatible API path.
  - This follows the spirit of kube-state-metrics' job-based differentiation but provides a more explicit source identifier within the metric itself.
  - Example PromQL: `histogram_quantile(0.99, sum(rate(sandbox_creation_duration_seconds_bucket[5m])) by (le, source))`

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
| `sandboxclaim_claim_duration_seconds` | Histogram | — | Duration of sandbox claim from start to completion in seconds. Buckets: 1, 2, 5, 10, 20, 30, 60, 120, 300, 600. Observed once per claim when reaching Completed phase. |

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
| `sandbox_creation_duration_seconds` | Histogram | ConstLabels: `source="e2b"` | Duration of sandbox creation operations in seconds. |
| `sandbox_creation_responses` | Counter | `result` | Total number of sandbox creation responses. Label `result` is `"success"` or `"failure"`. |
| `sandbox_pause_duration_seconds` | Histogram | ConstLabels: `source="e2b"` | Duration of sandbox pause operations in seconds. |
| `sandbox_pause_responses` | Counter | `result` | Total number of sandbox pause responses. |
| `sandbox_resume_duration_seconds` | Histogram | ConstLabels: `source="e2b"` | Duration of sandbox resume operations in seconds. |
| `sandbox_resume_responses` | Counter | `result` | Total number of sandbox resume responses. |
| `sandbox_delete_responses` | Counter | `result` | Total number of sandbox delete responses. |
| `sandbox_delete_duration_seconds` | Histogram | ConstLabels: `source="e2b"` | Duration of sandbox delete operations in seconds. |
| `sandbox_claim_duration_seconds` | Histogram | — | Claim operation total duration in seconds. |
| `sandbox_claim_total` | Counter | `result`, `lock_type` | Total number of claim operations. Label `result` is `"success"` or `"failure"`. Label `lock_type` is `"create"`, `"update"`, `"speculate"`, or `"unknown"`. |
| `sandbox_claim_retries` | Histogram | — | Distribution of retry counts per claim operation. |
| `sandbox_clone_duration_seconds` | Histogram | — | Clone operation total duration in seconds. |
| `sandbox_clone_total` | Counter | `result` | Total number of clone operations. Label `result` is `"success"` or `"failure"`. |
| `sandbox_route_sync_duration_seconds` | Histogram | `type` | Route sync operation duration in seconds. |
| `sandbox_route_sync_total` | Counter | `type`, `result` | Total number of route sync operations. |

**Histogram Bucket Configuration**

All duration histograms use `prometheus.ExponentialBuckets(0.01, 2, 10)`, which generates the following bucket boundaries (in seconds):

```
0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12
```

This covers the range from 10ms to ~5.12 seconds with exponential distribution, which is well-suited for API operations that typically complete in hundreds of milliseconds but may occasionally spike to several seconds.

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
4. **Manager metrics**: Latency histograms are observed at the API handler level using `time.Since(start)`. Counters are incremented based on operation result (`"success"` or `"failure"`).

#### Useful PromQL Examples

```promql
# Sandbox creation to ready latency (seconds)
sandbox_status_ready_time - sandbox_created

# SandboxSet pool utilization ratio (higher = more consumed)
1 - (sandboxset_available_replicas / sandboxset_desired_replicas)

# SandboxClaim completion duration (seconds)
sandboxclaim_completion_time - sandboxclaim_claim_start_time

# Sandbox creation success rate (last 5 minutes)
rate(sandbox_creation_responses{result="success"}[5m])
  / rate(sandbox_creation_responses[5m])

# Number of sandboxes in each phase
count by (phase) (sandbox_status_phase == 1)

# Sandbox pause operation P99 latency (seconds)
histogram_quantile(0.99, rate(sandbox_pause_duration_seconds_bucket[5m]))

# Average sandbox creation duration (seconds, last 5m)
rate(sandbox_creation_duration_seconds_sum{source="e2b"}[5m])
  / rate(sandbox_creation_duration_seconds_count{source="e2b"}[5m])

# SandboxSet rolling update progress
sandboxset_updated_replicas / sandboxset_replicas

# Sandboxes currently not ready
count(sandbox_status_ready == 0)

# SandboxClaim claim fulfillment ratio
sandboxclaim_claimed_replicas / sandboxclaim_desired_replicas

# SandboxClaim duration P99 (seconds)
histogram_quantile(0.99, rate(sandboxclaim_claim_duration_seconds_bucket[5m]))

# SandboxClaim average duration (seconds)
rate(sandboxclaim_claim_duration_seconds_sum[5m]) / rate(sandboxclaim_claim_duration_seconds_count[5m])

# Sandbox resume failure rate (last 5m)
rate(sandbox_resume_responses{result="failure"}[5m])
  / rate(sandbox_resume_responses[5m])

# Claim total duration P99 (seconds)
histogram_quantile(0.99, rate(sandbox_claim_duration_seconds_bucket[5m]))

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
histogram_quantile(0.99, rate(sandbox_creation_duration_seconds_bucket{source="k8s"}[5m]))

# Sandbox creation-to-ready average latency (seconds, k8s source)
rate(sandbox_creation_duration_seconds_sum{source="k8s"}[5m]) / rate(sandbox_creation_duration_seconds_count{source="k8s"}[5m])

# Compare creation duration across sources (P99)
histogram_quantile(0.99, sum(rate(sandbox_creation_duration_seconds_bucket[5m])) by (le, source))

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
# Pending 阶段时长（从创建到就绪）
sandbox_status_ready_time - sandbox_created

# Running 阶段时长（从就绪到被暂停，首次运行周期）
sandbox_status_paused_time - sandbox_status_ready_time

# Paused 阶段时长（从暂停完成到恢复完成）
sandbox_status_resumed_time - sandbox_status_paused_time

# 完整生命周期时长（从创建到删除）
sandbox_deletion_timestamp - sandbox_created

# Running 阶段时长（恢复后到再次暂停或删除）
# 需结合 sandbox_status_resumed_time 和后续事件时间戳
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

## Implementation History

- 04/10/2026: PR [#258](https://github.com/openkruise/agents/pull/258) merged — Initial Sandbox lifecycle metrics with bidirectional condition timestamps
- 04/22/2026: Extended to full observability coverage including SandboxSet, SandboxClaim, and Sandbox Manager API metrics
- 04/22/2026: Added claim/clone stage-level metrics, delete latency, route sync metrics, proxy topology metrics, and E2B snapshot metrics
