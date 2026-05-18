# [Observability] Hardening Context Propagation for Request Tracing

## Description
Currently, the `sandbox-manager` extracts or generates an `X-Request-ID` for every incoming E2B API request. While this ID is included in local logs, it is not propagated to downstream components like the `agent-runtime` sidecar when performing operations such as `InitRuntime` (HTTP) or `RunCommand` (Connect gRPC).

Ensuring that the `RequestID` is passed along all internal calls is essential for:
-   **Distributed Tracing**: Correlating logs across multiple components.
-   **Troubleshooting**: Identifying exactly which API request triggered a specific action in the sandbox.
-   **Observability**: Providing a consistent view of request flow in production.

## Proposed Changes

### 1. Define Standard Context Propagation Helpers
In `pkg/sandbox-manager/logs/context.go`, we should add helpers to:
-   Extract the `RequestID` from a `context.Context` (if it was stored there by middleware).
-   Provide a standard header name (e.g., `X-Request-ID`).

### 2. Instrument Outgoing HTTP Calls
Update `InitRuntime` in `pkg/utils/runtime/runtime.go`:
-   Retrieve the `RequestID` from the incoming `ctx`.
-   Set the `X-Request-ID` header on the outgoing `http.Request`.

### 3. Instrument Outgoing gRPC (Connect) Calls
Update `RunCommandWithRuntime` in `pkg/utils/runtime/runtime.go`:
-   Retrieve the `RequestID` from the incoming `ctx`.
-   Inject the `X-Request-ID` into the Connect request headers.

### 4. Update Web Middleware
Ensure the `RegisterRoute` handler in `pkg/servers/web/framework.go` not only adds the `requestID` to the logger but also stores it as a value in the `context.Context` for easy extraction.

## Checklist
- [ ] Define `RequestID` context key and helper functions in `pkg/sandbox-manager/logs/context.go`.
- [ ] Update `RegisterRoute` to store `RequestID` in the context value.
- [ ] Implement propagation in `InitRuntime` (HTTP).
- [ ] Implement propagation in `RunCommandWithRuntime` (Connect RPC).
- [ ] Verify propagation with a unit test or integration test simulation.
