# Proposed High-Impact Issues

After analyzing the codebase, here are three high-impact, repository-wide issues that directly address technical debt, security, and API correctness. These are excellent candidates to replace the previous "Template Filtering" and "RBAC Enhancements" tasks.

### 1. Tech Debt & Performance: Remove Redundant `SandboxUpdateOps` Concurrency Checks
- **Problem**: In `pkg/controller/sandboxupdateops/sandboxupdateops_controller.go` (line 106), the controller manually lists all ops in a namespace to prevent concurrent updates, citing a `TODO` that a webhook would be better. However, a validating webhook (`pkg/webhook/sandboxupdateops/validating/sandboxupdateops_validate.go`) **already exists** and properly enforces this at the admission layer.
- **Impact**: The controller's check is obsolete, prone to race conditions, and adds unnecessary load to the Kubernetes API server during every reconciliation loop.
- **Action**: Delete the redundant `opsList` conflict-check logic and the outdated `TODO` from the controller.

### 2. Security Hardening: Prevent Slowloris Vulnerabilities (Missing Server Timeouts)
- **Problem**: The core HTTP servers defined in `pkg/servers/e2b/core.go` (sandbox-manager) and `pkg/sandbox-gateway/server/server.go` configure `ReadHeaderTimeout` but omit `ReadTimeout`, `WriteTimeout`, and `IdleTimeout`. 
- **Impact**: This exposes the management and gateway APIs to Slowloris attacks. Malicious or poorly configured clients can open connections and slowly trickle data, holding resources open indefinitely until the server crashes or denies service to legitimate traffic.
- **Action**: Enforce strict `ReadTimeout` (e.g., 30s), `WriteTimeout` (e.g., 60s), and `IdleTimeout` (e.g., 120s) on both `http.Server` instances.

### 3. API Correctness: Fix HTTP 500 on Malformed JSON Inputs
- **Problem**: In several endpoints (`CreateSandbox`, `CreateSnapshot`, `CreateAPIKey`), when `json.NewDecoder(r.Body).Decode(&request)` fails due to malformed user input, it returns an `ApiError` without specifying a `Code`. The underlying `framework.go` (`writeJson`) defaults empty codes to **HTTP 500 (Internal Server Error)** instead of the correct **HTTP 400 (Bad Request)**.
- **Impact**: A client submitting a typo in their JSON body causes a 500 error. This triggers false-positive alerts on server dashboards and misleads developers debugging client-side errors.
- **Action**: Update the JSON decoding error handlers in `pkg/servers/e2b/` to explicitly return `http.StatusBadRequest`.
