# [Observability] Add Prometheus Metrics for E2B API Operations

## Summary
This PR enhances the observability of the `sandbox-manager` by instrumenting core E2B API operations with Prometheus metrics. It provides visibility into API latency and success/error rates for sandbox lifecycle events.

## Problem
Currently, the `sandbox-manager` only tracks snapshot operations. Core lifecycle events such as creating, deleting, and updating timeouts for sandboxes lack duration and request count tracking at the API layer. This makes it difficult to monitor the performance and reliability of the E2B-compatible API gateway.

## Solution
Implemented a unified metrics pattern in `pkg/servers/e2b/metrics.go` and instrumented the corresponding handlers:

1.  **Unified Metrics**:
    -   `sandbox_api_operation_duration_seconds` (Histogram): Tracks latency by operation (`create`, `delete`, `describe`, `timeout`, `snapshot`) and result (`success`, `error`).
    -   `sandbox_api_operation_total` (Counter): Tracks total requests by operation and result.
2.  **Instrumentation**:
    -   Instrumented `CreateSandbox` in `create.go`.
    -   Instrumented `DeleteSandbox` and `DescribeSandbox` in `services.go`.
    -   Instrumented `SetSandboxTimeout` in `timeout.go`.
    -   Refactored `CreateSnapshot` in `snapshot.go` to use the new unified pattern, removing legacy snapshot-specific metrics.
3.  **Code Quality**:
    -   Used named return parameters and `defer` blocks in handlers for robust metric collection.
    -   Ensured consistent bucket sizes (20ms to ~41s) for all API histograms.

## Verification
- **Unit Tests**: Ran `go test -v ./pkg/servers/e2b/...` and confirmed all 26.2s of tests pass.
- **Metric Registration**: Verified that new metrics are correctly registered in the global Prometheus registry.
- **Go Version**: Verified with **Go 1.25.0**.

## Checklist
- [x] Define new Prometheus metrics in `pkg/servers/e2b/metrics.go`.
- [x] Implement instrumentation in API handlers.
- [x] Migrate existing snapshot metrics to the new pattern.
- [x] Verify with unit tests.
