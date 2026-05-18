# [Observability] Hardening Context Propagation for Request Tracing

## Summary
This PR implements end-to-end context propagation for the `sandbox-manager`. It ensures that the `X-Request-ID` generated at the E2B API gateway is correctly propagated to downstream components, specifically the `agent-runtime` sidecar, via both HTTP and Connect gRPC calls.

## Problem
Previously, while the `sandbox-manager` generated or extracted a `RequestID` for incoming requests, this ID was only stored in the local logger context. Downstream calls to the sandbox sidecar (e.g., during initialization or command execution) did not include this ID. This made it difficult to correlate sidecar logs with the originating user request in distributed environments.

## Solution
1.  **Context Utility Enhancements**: Added `WithRequestID` and `GetRequestID` helpers in `pkg/sandbox-manager/logs/context.go` to manage `RequestID` as a first-class context value.
2.  **Middleware Update**: Updated the `RegisterRoute` middleware in `pkg/servers/web/framework.go` to inject the `RequestID` into the context values.
3.  **HTTP Propagation**: Updated `InitRuntime` in `pkg/utils/runtime/runtime.go` to extract the `RequestID` from the context and set the `X-Request-ID` header on outgoing HTTP requests.
4.  **gRPC Propagation**: Updated `RunCommandWithRuntime` in `pkg/utils/runtime/runtime.go` to inject the `RequestID` into the Connect RPC request headers.

## Impact
-   **Improved Debugging**: Engineers can now trace a single user request across multiple microservices using a single `RequestID`.
-   **Auditability**: Provides a clear link between external API calls and internal sidecar operations.

## Checklist
- [x] RequestID context helpers implemented.
- [x] Web middleware updated for context injection.
- [x] HTTP propagation added to `InitRuntime`.
- [x] gRPC propagation added to `RunCommandWithRuntime`.
- [x] All unit tests passed.
