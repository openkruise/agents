# `pkg/servers/e2b` Guide

This directory is the sandbox-manager's E2B-compatible HTTP API layer. It wraps the core `sandbox-manager` logic into REST endpoints that follow the [E2B OpenAPI specification](https://github.com/e2b-dev/E2B/blob/main/spec/openapi.yml). All route handlers live on the `Controller` struct and delegate real work to `sandbox-manager/infra` and related packages.

## Upstream E2B OpenAPI Spec
**IMPORTANT**: Before working on E2B APIs, you have to learn the upstream E2B OpenAPI spec with the following steps:

1. Download the spec file (openapi) from https://raw.githubusercontent.com/e2b-dev/E2B/refs/heads/main/spec/openapi.yml
2. Use your file search tool to find the part you need. **Never Read the Entire Spec, It's Extremely Large**.

## Architecture

```
Controller (core.go)
├── Init()          → builds SandboxManager, registers routes, inits KeyStorage
├── Run()           → starts HTTP server, signal handling, KeyStorage lifecycle
└── registerRoutes() (routes.go) → wires all endpoints to http.ServeMux
```

The controller depends on:
- `sandbox-manager.SandboxManager` for sandbox lifecycle operations (claim, delete, pause, resume, clone, checkpoint).
- `keys.KeyStorage` for API key authentication (optional; nil when auth is disabled).
- `adapters.E2BAdapter` for Envoy traffic routing (native vs customized E2B path mapping).

## File Responsibilities

| File | Responsibility |
|---|---|
| `core.go` | `Controller` struct, `NewController`, `Init`, `Run`, server lifecycle |
| `routes.go` | Route registration, `CheckApiKey` / `CheckAdminKey` middleware, `RegisterE2BRoute` dual-path helper |
| `create.go` | `POST /sandboxes` — create via claim (SandboxSet) or clone (Checkpoint) |
| `services.go` | `GET /sandboxes/{id}`, `DELETE /sandboxes/{id}`, `BrowserUse`, `Debug` |
| `list.go` | `GET /v2/sandboxes` (list sandboxes with pagination/filter), `GET /snapshots` (list checkpoints) |
| `pause_resume.go` | `POST .../pause`, `POST .../resume`, `POST .../connect` with timeout management |
| `timeout.go` | `POST .../timeout` — set sandbox timeout |
| `snapshot.go` | `POST .../snapshots` — create checkpoint |
| `templates.go` | `GET /templates`, `GET /templates/{id}`, `DELETE /templates/{id}` |
| `api_key.go` | `GET /api-keys`, `POST /api-keys`, `DELETE /api-keys/{id}` (admin-only) |
| `sandbox.go` | `getSandboxOfUser`, `convertToE2BSandbox`, `ParseTimeout`, metadata helpers |
| `metadata.go` | Metadata key blacklist (E2B / internal prefixes) |

## Subdirectories

- **`adapters/`** — E2B request mapping for two host/path styles: native E2B style (e.g. `api.domain.com/sandboxes`) and customized proxy style (e.g. `domain.com/api/sandboxes`). All routes are dual-registered for both adapters.
- **`keys/`** — API key persistence (`KeyStorage` interface, Secret / MySQL backends). Has its own `AGENTS.md`.
- **`models/`** — Request/response models, validation, error types, constants.

## Key Design Decisions

### Dual-Path Registration
Every E2B route is registered twice via `RegisterE2BRoute`: once for the native E2B host/path style (e.g. `api.domain.com/sandboxes`) and once for the customized proxy style (e.g. `domain.com/api/sandboxes`). Do not register routes directly on `mux` — always use `RegisterE2BRoute`.

### Authentication Flow
1. `CheckApiKey` middleware extracts `X-API-KEY` header, validates via `KeyStorage`, and injects user into context.
2. When `keys` is nil (auth disabled), all requests use `AnonymousUser` with admin privileges.
3. Sandbox ownership is verified per-request: the API key owner must match the sandbox owner.
4. Admin-only endpoints (API key management) chain `CheckAdminKey` after `CheckApiKey`.

### Timeout Semantics
- **Pause**: sets timeout far into the future (1000 years) so paused sandboxes are kept indefinitely.
- **Resume**: sets timeout strictly to the requested value (no extend-only merge).
- **Connect (Running)**: extend-only — never shortens the effective deadline. If the requested deadline is earlier than the current one, the update is silently skipped.
- **Connect (Paused → Resume)**: sets timeout strictly to the requested value (same as Resume).
- **SetTimeout**: only applies to running sandboxes; conflicts return `409`.

### Create Sandbox
- If `templateID` matches a SandboxSet → claim path (`ClaimSandbox`).
- If `templateID` matches a Checkpoint → clone path (`CloneSandbox`).
- Otherwise → `400 Template or Checkpoint not found`.

## Rules For Modification

1. **Check E2B spec first**: Read https://github.com/e2b-dev/E2B/blob/main/spec/openapi.yml before changing any endpoint behavior, status codes, or request/response shapes.
2. **Preserve status code compatibility**: Some E2B endpoints use non-standard codes (e.g. `SetTimeout` returns `500` instead of `400` for validation errors). Do not "fix" these without verifying the E2B spec.
3. **Keep dual-path registration**: New endpoints must use `RegisterE2BRoute`, not raw `mux.HandleFunc`.
4. **Timeout rules**: Resume and Connect(Paused→Resume) set timeout strictly to the request value. Connect(Running) is extend-only — cannot shorten. Do not change this unless the product contract changes.
5. **Middleware ordering**: `CheckAdminKey` must always come after `CheckApiKey`.
6. **Model changes**: Request/response types live in `models/`. Keep validation logic in `models/validation.go`.

## Tests

- Each handler file has a corresponding `*_test.go`. Update the matching test file when changing handler logic.
- Tests use `httptest` and mock `SandboxManager`. Follow table-driven style.
- Middleware tests are in `routes_test.go`.

## Review Focus: Interface Behavior Consistency with E2B

When reviewing code in this module, focus on the following areas to ensure interface behavior remains fully consistent with the upstream E2B specification:

1. **HTTP Status Codes**: Every endpoint must return status codes matching the E2B OpenAPI spec, including non-standard ones (e.g. `SetTimeout` returns `500` for validation errors). Do not "fix" these unless the E2B spec itself has changed.
2. **Request/Response Structure**: Field names, types, required/optional status, and default values in both request and response bodies must align with the E2B spec. Note that E2B uses `camelCase` (not `snake_case`) field naming.
3. **Timeout Semantics**: Pause / Resume / Connect / SetTimeout have strictly distinct timeout behaviors (see Timeout Semantics under Key Design Decisions). Review each case individually.
4. **Error Response Format**: E2B error responses follow a specific JSON structure (e.g. `error` / `message` fields). Ensure all error paths return the spec-compliant format rather than custom structures.
5. **Parameter Validation Boundaries**: Value ranges, defaults, and overflow handling for `limit`, `timeout`, `metadata`, etc. must match the E2B spec.
6. **Sandbox State Transitions**: Constraints and side effects of operations like Pause → Resume, Connect(Paused → Resume), and Delete on sandbox state must match E2B behavior.
7. **Authentication & Authorization**: `X-API-KEY` header validation, anonymous mode, and admin privilege determination must align with the E2B multi-tenant model.
