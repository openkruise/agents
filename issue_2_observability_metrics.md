# [Observability] Add Prometheus Metrics for E2B API Operations

## Description
Following the recent enhancement of controller-level metrics (#292), there is still a gap in observability for the `sandbox-manager` E2B-compatible APIs. Currently, only snapshot operations are instrumented. Core lifecycle operations like creating, deleting, and updating timeouts for sandboxes lack duration and success/failure tracking at the API layer.

To provide a complete observability picture, we should add Prometheus metrics to track these core E2B API operations.

## Proposed Changes
Enhance `pkg/servers/e2b/metrics.go` and instrument the following handlers:

1. **New Metrics**:
    - `sandbox_create_duration_seconds` (Histogram): Tracks the latency of sandbox creation requests.
    - `sandbox_delete_duration_seconds` (Histogram): Tracks the latency of sandbox deletion requests.
    - `sandbox_timeout_update_duration_seconds` (Histogram): Tracks the latency of timeout update requests.
    - `sandbox_api_request_total` (Counter): Tracks total requests partitioned by `operation` (create/delete/timeout/list) and `result` (success/error).

2. **Instrumentation Points**:
    - `CreateSandbox` in `pkg/servers/e2b/create.go`.
    - `DeleteSandbox` in `pkg/servers/e2b/services.go`.
    - `UpdateSandboxTimeout` in `pkg/servers/e2b/services.go`.

## Rationale
While controller metrics track K8s-level readiness, API-level metrics are essential for identifying bottlenecks in the `sandbox-manager` itself (e.g., slow template lookups, auth latency, or communication overhead with the API server).

## Checklist
- [x] Verify no overlap with existing PR #292 (Done: PR #292 focused on controllers and snapshots).
- [x] Define new metrics in `pkg/servers/e2b/metrics.go`.
- [x] Implement instrumentation in the corresponding E2B handlers.
- [x] Add unit tests in `pkg/servers/e2b/metrics_test.go` (if applicable) or handler tests.
