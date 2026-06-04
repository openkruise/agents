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

### Team Identity
- API-key teams are identified only by team name. The team name maps to a Kubernetes namespace, whose uniqueness is the
  isolation boundary.
- Team UUIDs in API models, MySQL `teams.uid`, Secret payloads, and `/teams` responses are display-only compatibility
  metadata. They must not be used for storage lookup, authorization, namespace selection, or team equality checks.

### Namespace Naming Constraint
- Sandbox IDs are encoded as `<namespace>--<name>` (see `sandboxutils.GetSandboxID`). The `--` is a reserved separator.
- Team / namespace names in the E2B path **must not contain `--`**. 
- The constraint is enforced at API key creation in `validateTeamNamespace` via `sandboxutils.ValidateNamespaceForSandboxID`.
  Admin-team callers creating keys for other teams pass through the same check.
- The admin team itself (`models.AdminTeamName = "admin"`) has no `--`, never maps to a namespace
  (`getNamespaceOfUser` returns `""` for admin), and is therefore not affected by this rule.

### List And Delete Authorization
- Any resource returned by a List endpoint should be deletable by the same caller unless deletion is explicitly unsupported
  or blocked by a documented safety rule.
- `ListSandboxes` and `DeleteSandbox` are key-owner scoped. Both must use the current `user.ID` as the sandbox owner.
- `ListSnapshots` returns Checkpoint-backed snapshots scoped to the current `user.ID`. Deleting a snapshot through the
  E2B-compatible delete path should keep the current idempotent behavior, including silent success for not found or not
  allowed cases.
- `ListTemplates` returns SandboxSet-backed templates scoped by the caller's team namespace. Admin-team keys may list
  SandboxSets across namespaces. SandboxSet-backed templates are not deletable through the E2B delete endpoint; return an
  explicit unsupported-template-delete error instead of treating the ID as a Checkpoint.
- The shared E2B delete path must distinguish SandboxSet-backed templates from Checkpoint-backed snapshots before calling
  checkpoint deletion. For admin callers, this SandboxSet check must cover the same cross-namespace visibility used by
  `ListTemplates`.
- `ListAPIKeys` is team-scoped. A caller may delete listed keys in the same team, except when blocked by storage-level
  safety rules.

### Timeout Semantics
- **Pause**: sets timeout far into the future (1000 years) so paused sandboxes are kept indefinitely.
- **Resume**: for timed sandboxes, writes an effective timeout while flipping `Spec.Paused=false`; the effective timeout is the request value after the resume floor. Never-timeout sandboxes keep nil timeout fields.
- **Connect (Running)**: extend-only — never shortens the effective deadline. If the requested deadline is earlier than the current one, the update is silently skipped.
- **Connect (Paused → Resume)**: resumes with an effective timeout placeholder, then applies the post-resume timeout with `UpdatePolicyExtendOnly`.
- **SetTimeout**: only applies to running sandboxes; conflicts return `409`.

### Connect Timeout Race Handling
Concurrent `ConnectSandbox` calls intentionally converge by deadline rather than trying to strictly serialize all callers.

For a timed paused sandbox, `ConnectSandbox` first applies the resume floor only when the sandbox is paused and has an existing deadline. `Resume` then updates `Spec.Paused=false` and writes the placeholder timeout in the same Sandbox update, so the controller cannot observe an unpaused sandbox with an already-expired pause deadline. If several callers race, only the first update that flips `Spec.Paused` wins; losing resume callers must not overwrite the winner's placeholder.

After resume, every caller runs `updateConnectTimeout` with `UpdatePolicyExtendOnly`. A later deadline extends the sandbox, while an earlier or stale deadline is skipped. This makes concurrent Connect timeout writes monotonic: final state is the longest effective deadline among the racing requests, and a shorter request cannot shrink a longer timeout even if it finishes later. Running-sandbox Connect also uses `ExtendOnly`, but the resume floor must not apply there.

### Create Sandbox
- If `templateID` matches a SandboxSet → claim path (`ClaimSandbox`).
- If `templateID` matches a Checkpoint → clone path (`CloneSandbox`).
- Otherwise → `400 Template or Checkpoint not found`.

#### Server-Side Timeouts (Claim / Clone / WaitReady)
- These are the server-side deadlines for the synchronous create operation, distinct from the sandbox lifecycle `timeout` field (auto-shutdown / auto-pause).
- **Default is unlimited**: when no timeout extension is supplied, `create.go` passes `noServerTimeout` (a far-future duration, ~100 years) for `ClaimTimeout` / `CloneTimeout` / `WaitReadyTimeout`. The operation is then bounded only by the client request context, so it ends on success or client cancellation. A far-future value is used (not a negative sentinel) so downstream `ValidateAndInit*Options`, `context.WithTimeout`, `retrySteps`, and `pkg/cache/utils/wait.go` keep working unchanged — this mirrors the "far-future time" convention used by Pause.
- **Explicit override**: the extension keys `e2b.agents.kruise.io/claim-timeout-seconds` and `e2b.agents.kruise.io/wait-ready-timeout-seconds` still apply a finite timeout when set to a positive value. A non-positive or absent value is treated as unlimited.
- All four call sites go through the `resolveServerTimeout(seconds)` helper in `create.go`.
- This unlimited default applies only to the E2B `CreateSandbox` HTTP path. The `SandboxClaim` controller builds its own options and calls `TryClaimSandbox` directly, keeping its CRD-level timeout semantics and finite defaults.

## Rules For Modification

1. **Check E2B spec first**: Read https://github.com/e2b-dev/E2B/blob/main/spec/openapi.yml before changing any endpoint behavior, status codes, or request/response shapes.
2. **Preserve status code compatibility**: Some E2B endpoints use non-standard codes (e.g. `SetTimeout` returns `500` instead of `400` for validation errors). Do not "fix" these without verifying the E2B spec.
3. **Keep dual-path registration**: New endpoints must use `RegisterE2BRoute`, not raw `mux.HandleFunc`.
4. **Timeout rules**: Resume and Connect(Paused→Resume) use the effective timeout after the resume floor, and post-resume Connect timeout writes are extend-only. Connect(Running) is also extend-only and must not apply the resume floor. Do not change this unless the product contract changes.
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
