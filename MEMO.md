# MEMO

Last updated: 2026-05-01T11:45:00Z

## Current Topic

Team Resource Isolation for E2B Sandbox K8s resources is implemented end-to-end. The HTTP layer now derives resource scope from the API key's Team and routes all Sandbox/SandboxSet/Checkpoint/SandboxTemplate access through namespace-aware manager APIs, with a deliberate admin-team exception.

## Latest Progress

- Background: Team name is the Kubernetes namespace for normal teams. All sandbox resources accessed through a non-admin API key must stay within that Team namespace.
- Architecture decision: internal interfaces use `XXXOptions` structs rather than bare `namespace string` parameters, so namespace scoping was added without freezing future API shape.
- Infra boundary: `infra` is namespace-aware only and does not understand E2B `user`. Owner filtering and authorization remain in `pkg/sandbox-manager`.
- Cache and manager layer: namespace-aware `WithOptions` methods were added for claimed sandbox lookup, checkpoint lookup, pool/template lookup, sandbox/checkpoint listing, and checkpoint deletion. Namespace is also part of the relevant cache singleflight keys.
- E2B layer integration is complete: create/claim/clone, list sandboxes, list snapshots, get template, list templates, delete template, and sandbox single-object lookups now flow through namespace-aware manager APIs.
- Authorization decision: user-controlled namespace selectors such as query `teamID` are not trusted for authorization. Normal team keys are constrained by the caller's team namespace regardless of request parameters.
- Admin exception: `adminTeam` intentionally preserves the original cluster-scope behavior. In the E2B layer, admin requests resolve to empty namespace and therefore keep global visibility across namespaces.
- Admin identity normalization: admin users are normalized through `keys.TeamForKey(...)` so that admin-team handling depends on the canonical `models.AdminTeam()` identity. If a key carries `team.Name == "admin"` with a mismatched ID, it is normalized back to `AdminTeamID`.
- Compatibility cleanup: old cache methods that directly accepted `user string` for list paths were removed in favor of `WithOptions` variants, and callers were migrated.
- Validation status: unit tests for `pkg/cache`, `pkg/sandbox-manager`, `pkg/sandbox-manager/infra/sandboxcr`, `pkg/controller/sandboxclaim/core`, `pkg/servers/e2b/keys`, and `pkg/servers/e2b` pass after the namespace-isolation migration.

### Review Focus

- E2B namespace derivation in `pkg/servers/e2b`: verify all resource-touching paths go through `getNamespaceOfUser` and then into `WithOptions` manager APIs rather than old global helpers.
- Admin scope exception: verify `adminTeam` remains cluster-scoped for resource access and was not accidentally narrowed to the literal `admin` namespace during cleanup or later refactors.
- Admin identity canonicalization: verify any admin key observed through `keys.TeamForKey(...)` resolves to `models.AdminTeam()` and therefore always carries `AdminTeamID`.
- Normal-team enforcement: verify non-admin requests ignore user-provided namespace/team selectors for authorization and cannot widen access beyond the caller's team namespace.
- Cross-namespace collision safety: verify same-name templates, checkpoints, pools, and sandboxes in different namespaces do not collide or leak through cache indexes, manager filtering, or E2B handlers.
- Layering rule: verify `pkg/sandbox-manager/infra` remains namespace-only and does not regain E2B `user` knowledge; owner checks belong in `pkg/sandbox-manager` or above.
- Behavioral parity: verify the refactor preserves pre-change API behavior for all touched endpoints/operations except for the intended namespace scope change, including status transitions, side effects, error mapping/codes, idempotency, and response payload structure.
- Regression checklist (pre/post behavior comparison): `ListSandboxes`, `GetSandbox`, `CreateSandbox`, `DeleteSandbox`, `CloneSandbox` in `pkg/servers/e2b/sandbox.go`; `ListSandboxTemplates`, `GetTemplate`, `DeleteTemplate` in `pkg/servers/e2b/templates.go`; `HasTemplate/HasCheckpoint` preflight checks and `ClaimSandboxWithOptions/GetClaimedSandboxWithOptions`; and corresponding manager/infra paths in `pkg/sandbox-manager` and `pkg/sandbox-manager/infra`. Validate before and after:
1. Input validation errors and returned messages remain unchanged for same invalid inputs.
2. Permission denied and not found behavior is unchanged except where namespace scope deliberately changes access scope.
3. Success response schema and field-level values stay stable.
4. Side effects (cache writes, status transitions, deletion idempotency, list ordering/pagination behavior) remain unchanged.
5. Admin behavior is unchanged for cluster-scope operations while non-admin behavior aligns with team namespace isolation.

### Affected Scope

- `pkg/cache`: namespace-aware cache/index lookup and list APIs for claimed sandboxes, checkpoints, sandbox sets, pool sandboxes, and owner-scoped lists.
- `pkg/sandbox-manager/infra` and `pkg/sandbox-manager/infra/sandboxcr`: namespace-aware infra option structs and K8s CRUD/list/get/delete implementations.
- `pkg/sandbox-manager`: manager-level owner filtering and authorization moved to namespace-aware `WithOptions` calls.
- `pkg/controller/sandboxclaim/core`: cache list call sites migrated to `WithOptions` variants affected by the cache API cleanup.
- `pkg/servers/e2b`: create sandbox, clone sandbox, list sandboxes, list snapshots, template list/get/delete, sandbox lookup, and team-aware namespace derivation.
- `pkg/servers/e2b/keys`: team resolution and admin-team normalization used by resource scoping and admin cluster-scope exception.
- Tests: `pkg/cache`, `pkg/sandbox-manager`, `pkg/sandbox-manager/infra/sandboxcr`, `pkg/controller/sandboxclaim/core`, `pkg/servers/e2b/keys`, and `pkg/servers/e2b`.

## Parallel Track: Team CRUD via E2B API Keys

- Status: API development for the Team CRUD track is complete.
- Scope delivered: Only the upstream-compatible `GET /teams` route was added for teams. No E2B-compatible `POST /teams`, `GET /teams/{teamID}`, `PATCH/PUT /teams/{teamID}`, or `DELETE /teams/{teamID}` routes were introduced.
- Lifecycle model: Teams are managed indirectly through `/api-keys`.
- API key create semantics: `POST /api-keys.name` remains the API key display name. The API accepts optional `POST /api-keys.teamName`, where `teamName` is the Kubernetes namespace and team name.
- Default target team: If `teamName` is empty, the new API key is created for the caller's own team.
- Authorization model: API key permissions are enforced through middleware in the same style as `CheckAdminKey`. Only `adminTeam` may specify or operate on another team. Non-admin callers are restricted to their own team.
- Namespace validation: If `adminTeam` targets a non-existent team, creation is allowed only when the Kubernetes namespace with the same `teamName` already exists. Missing namespace returns an E2B-compatible `400` style error.
- Team deletion: When the last active API key for a team is deleted, the team is soft deleted. Deleting the last active `adminTeam` key is forbidden.
- Storage contract: No separate Kubernetes Secret was added for team metadata. Team metadata remains derived from key associations in Secret mode and persisted in MySQL with soft deletion semantics.
- `GET /teams` semantics: A normal team key returns only its own active team. `adminTeam` returns all active teams. The response keeps the upstream `apiKey` field but returns an empty string rather than exposing key material.
- Follow-up focus: Subsequent work in this area should center on verification, bug fixes, and integration with the namespace-enforced resource path rather than additional Team CRUD surface expansion.

### Review Focus

- HTTP surface and middleware in `pkg/servers/e2b/routes.go` and `pkg/servers/e2b/api_key.go`: verify `GET /teams` is registered, `/api-keys` now relies on `CheckCreateAPIKeyPermission` and `CheckDeleteAPIKeyPermission`, and handlers do not bypass middleware-enforced authorization.
- Request and response models in `pkg/servers/e2b/models/api_key.go`: verify `NewTeamAPIKey` uses `teamName`, `ListedTeam` matches the intended upstream-compatible `GET /teams` shape, and `apiKey` is intentionally blank in team responses.
- Key storage interface and shared helpers in `pkg/servers/e2b/keys/interface.go` and `pkg/servers/e2b/keys/utils.go`: verify the `CreateKey` options, `ListTeams`, and `FindTeamByName` contracts are consistent across both storage backends.
- Secret backend changes in `pkg/servers/e2b/keys/secret.go`: verify team indexing by team name, team reuse versus new team creation, team listing deduplication, and last-admin-key deletion protection.
- MySQL backend changes in `pkg/servers/e2b/keys/mysql.go`: verify team resolution by `teams.name`, soft-delete and restore behavior, transactional create/delete flow, team listing semantics, and cache invalidation around `FindTeamByName`.
- Namespace validation path in `pkg/servers/e2b/routes.go`: verify new team creation only succeeds when the Kubernetes namespace already exists and that missing namespace is surfaced as a `400` style E2B error.
- Tests in `pkg/servers/e2b/api_key_test.go`, `pkg/servers/e2b/routes_test.go`, `pkg/servers/e2b/keys/secret_test.go`, and `pkg/servers/e2b/keys/mysql_test.go`: verify coverage for same-team versus cross-team permissions, admin-only cross-team behavior, namespace existence checks, `GET /teams`, soft delete/restore, and last-admin-key protection.
