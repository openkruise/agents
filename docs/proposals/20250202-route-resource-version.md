---
title: Add ResourceVersion field to Route
authors:
  - "@tbd"
reviewers:
  - "@tbd"
creation-date: 2025-02-02
last-updated: 2025-02-02
status: provisional
---

# Add ResourceVersion field to Route

## Summary

Add a `ResourceVersion` field to the proxy `Route` struct and use it to implement conditional updates. This prevents a synced latest route from being overwritten by a slow or out-of-order sandbox update event, ensuring traffic routing stays consistent with the most recent sandbox state.

## Project Context: OpenKruise Agents

**OpenKruise Agents** is a CNCF sub-project under OpenKruise that manages AI agent workloads on Kubernetes. It provides:

- **Isolated sandboxes** for AI agents to safely execute untrusted code (code interpretation, web browsing, data analysis)
- **Resource pooling** with rapid provisioning, hibernation, and checkpoint support
- **Traffic routing** for user identity and session management, with proxy-based routing to sandbox Pod IPs
- **E2B-compatible API** and Kubernetes CRD APIs (Sandbox, SandboxSet, SandboxClaim)
- **Compatibility** with [Sig Agent-Sandbox](https://agent-sandbox.sigs.k8s.io/) when available

The proxy layer maintains a `Route` per sandbox (ID, IP, Owner, State, ExtraHeaders) used for request routing. Routes are updated from multiple sources: local informer events and peer sync via HTTP.

## Motivation

### The Problem

Routes can be updated from two main flows:

1. **Informer events** (`onSandboxAdd`, `onSandboxUpdate` in `pkg/sandbox-manager/infra/sandboxcr/infra.go`): Watch events from the Kubernetes API trigger `refreshRoute`, which calls `SetRoute`.
2. **Explicit sync** (e.g., after claim/pause/resume in `pkg/sandbox-manager/api.go`): The manager fetches the latest sandbox state, calls `SetRoute`, and `SyncRouteWithPeers` to push the route to other proxy instances.

Because event delivery is asynchronous and can be reordered or delayed, a **slow informer event** (or an event processed late) can overwrite a **newer route** that was already synced. For example:

- T1: User claims sandbox → API fetches RV=100, `SetRoute` + `SyncRouteWithPeers` (IP=X, State=Running).
- T2: Delayed informer event with RV=95 (pre-claim state) triggers `refreshRoute` → `SetRoute` overwrites with stale IP/State.
- Result: Routing uses outdated data; traffic may be misrouted or fail.

`refreshRoute` today only compares `State` and `IP`; it does not know whether the new data is actually newer than what is stored.

### Goals

- Ensure route updates are ordered by sandbox `ResourceVersion`; never overwrite a newer route with an older one.
- Preserve backward compatibility for JSON serialization (e.g., `SyncRouteWithPeers`, `handleRefresh`).
- Reuse existing `ResourceVersion` comparison logic where possible (e.g., `pkg/utils/expectations`).

### Non-Goals

- Changing the semantics of Route beyond adding versioning.
- Handling cross-sandbox routing or multi-version APIs.

## Proposal

### 1. Add ResourceVersion to Route

Extend the `Route` struct in `pkg/proxy/routes.go`:

```go
// Route represents an internal sandbox routing rule
type Route struct {
	IP              string            `json:"ip"`
	ID              string            `json:"id"`
	Owner           string            `json:"owner"`
	State           string            `json:"state"`
	ExtraHeaders    map[string]string `json:"extra_headers"`
	ResourceVersion string            `json:"resource_version,omitempty"`
}
```

### 2. Populate ResourceVersion when building Route

In `pkg/utils/sandbox-manager/proxyutils/default.go`, include the Sandbox's `ResourceVersion` in the returned `Route`:

```go
func getRouteFromSandbox(s *agentsv1alpha1.Sandbox) proxy.Route {
	// ... existing logic ...
	return proxy.Route{
		IP:              s.Status.PodInfo.PodIP,
		ID:              stateutils.GetSandboxID(s),
		Owner:           s.GetAnnotations()[agentsv1alpha1.AnnotationOwner],
		State:           state,
		ResourceVersion: s.GetResourceVersion(),
	}
}
```

### 3. Conditional SetRoute (compare ResourceVersion)

Change `SetRoute` to only overwrite when the incoming route is newer or when no existing route exists:

```go
func (s *Server) SetRoute(route Route) {
	key := route.ID
	raw, ok := s.routes.Load(key)
	if !ok {
		s.routes.Store(key, route)
		return
	}
	existing := raw.(Route)
	if !isResourceVersionNewer(existing.ResourceVersion, route.ResourceVersion) {
		return // existing is newer or equal; skip update
	}
	s.routes.Store(key, route)
}
```

Use `isResourceVersionNewer` from `pkg/utils/expectations/resource_version_expectation.go` or a shared helper to compare Kubernetes `ResourceVersion` strings (numeric comparison where possible).

### 4. Update call sites

- **Informer flow** (`infra.go`): `onSandboxAdd` and `onSandboxUpdate` already use `GetRoute()` from the sandbox; no change needed if `getRouteFromSandbox` populates `ResourceVersion`.
- **API flow** (`api.go`): `syncRoute` uses `sbx.GetRoute()`; no change needed.
- **handleRefresh** (`server.go`): Peers receive JSON with `resource_version`; decode and pass through. `SetRoute` will now apply the conditional logic.
- **DeleteRoute**: When a sandbox is deleted, `DeleteRoute` removes the route; no version check needed.

### 5. Backward compatibility

- `ResourceVersion` is optional in JSON (`omitempty`). Old clients sending routes without it will have `ResourceVersion == ""`.
- `isResourceVersionNewer("", X)` should return `true` (empty means “no existing version”), so a new route always overwrites when the stored route has no version.
- New routes with `ResourceVersion` set will not overwrite older stored routes that have a higher `ResourceVersion`.

## Implementation Details

### ResourceVersion comparison

Reuse the logic from `pkg/utils/expectations/resource_version_expectation.go`:

- Empty `old` → treat `new` as newer (accept update).
- Parse as `uint64`; `new >= old` means newer.
- On parse error for `old`, treat as older (accept).
- On parse error for `new`, treat as invalid (reject).

A small shared helper (e.g., in `pkg/utils` or `pkg/proxy`) can avoid circular dependencies.

### Informer vs sync ordering

| Source        | When                         | Has ResourceVersion? |
|---------------|------------------------------|----------------------|
| Informer add  | Sandbox created              | Yes (from cache)     |
| Informer update | Sandbox updated            | Yes (from cache)     |
| syncRoute     | After claim/pause/resume     | Yes (from API)       |
| handleRefresh | Peer pushes route            | Yes (from JSON)      |

All paths that build a `Route` from a `Sandbox` should set `ResourceVersion` via `getRouteFromSandbox`. The HTTP `handleRefresh` path receives a `Route` that was built on another node and already includes `ResourceVersion`.

## Risks and Mitigations

| Risk                         | Mitigation |
|-----------------------------|------------|
| Non-numeric ResourceVersion | Use existing `isResourceVersionNewer` behavior; fallback to string comparison or reject if needed. |
| Rolling upgrade: old binary receives Route with ResourceVersion | Old `SetRoute` ignores the field and overwrites unconditionally; acceptable during transition. New binaries sending to old binaries: old code overwrites, but informer on the same node will eventually correct. |
| Empty ResourceVersion in some edge cases | Treat empty as “oldest”; any non-empty version overwrites. |

## Alternatives

1. **Timestamp-based**: Use a timestamp instead of ResourceVersion. Rejected because it is less precise and can conflict across nodes.
2. **Sequence number**: Maintain a separate monotonic counter. Rejected because it adds state and does not align with the source of truth (Sandbox CR).
3. **Only update when State/IP change**: Current `refreshRoute` behavior. Rejected because it does not prevent overwriting a newer route with the same State/IP but different metadata (e.g., Owner, ExtraHeaders) or future fields.

## Upgrade Strategy

- **No API or CRD changes**: Only internal Route struct and proxy logic.
- **Deployment**: Deploy new proxy binary; routes will start carrying `ResourceVersion`.
- **Mixed versions**: Old proxies will ignore `resource_version` in JSON and overwrite unconditionally. This is acceptable; the main protection is on the node that runs the informer and receives sync from peers.

## Test Plan

- Unit tests for `SetRoute` with ResourceVersion: newer overwrites, older does not overwrite, empty existing accepts all.
- Unit tests for `getRouteFromSandbox` ensuring `ResourceVersion` is set.
- Integration test: claim sandbox → sync route → inject older informer event → verify route is not overwritten.
- Verify `SyncRouteWithPeers` and `handleRefresh` round-trip `ResourceVersion` correctly.

## Implementation History

- [ ] 2025-02-02: Initial proposal
