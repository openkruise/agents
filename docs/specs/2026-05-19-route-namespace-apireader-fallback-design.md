# Route Namespace and Cache-Hit APIReader Fallback Design

## Background

`GetClaimedSandbox` reads from the informer cache first. The cache lookup uses the claimed-sandbox index, so cache hits preserve the contract that only Sandboxes with `agents.kruise.io/sandbox-claimed=true` are returned.

In a multi-replica sandbox-manager deployment, the informer cache on one replica may lag behind another replica that has just created or updated a Sandbox. The proxy route table may be synchronized between replicas faster than the informer cache can observe the apiserver update, but route state must not turn a cache miss into an APIReader read.

`GetClaimedSandbox` should use APIReader fallback only after the cache returns a Sandbox. This keeps the claimed-sandbox index as the gate for lookup results and gives fallback code an unambiguous namespace/name key from the cached object.

Fallback is allowed for two cache-hit cases:

- the local resource-version expectation is not satisfied;
- the route resourceVersion is newer than the cached Sandbox resourceVersion.

Cache misses are handled by waiting for the informer cache until the caller's context is canceled or expires. Admin requests may have `opts.Namespace == ""`, and Sandbox IDs use `<namespace>--<name>`, which is ambiguous when namespaces contain `--`; requiring a cache hit before APIReader fallback avoids key reconstruction from ambiguous request or route data.

## Goals

- In the lookup phase, always retry cache misses until the cache hits or the caller's context is canceled or expires.
- Do not use APIReader fallback for `cache.ErrSandboxNotFound`, even when the proxy route table already contains the sandbox route.
- Preserve APIReader fallback for cache-hit results that are locally below resource-version expectations or behind route resourceVersion.
- Derive APIReader fallback keys from the cached Sandbox object, not from `opts.SandboxID`, route namespace, or legacy parsing.
- Keep old route data compatible during rolling upgrades.
- Keep gateway routing semantics unchanged.
- Refactor `GetClaimedSandbox` so lookup, fallback decisions, metrics, logging, APIReader reads, NotFound wrapping, and claimed-label validation are easier to read and test.

## Non-Goals

- Do not change the external Sandbox ID format.
- Do not redesign `ParseSandboxID`.
- Do not require a migration for existing routes.
- Do not change sandbox-gateway request routing or registry key semantics.
- Do not use route namespace or `ParseSandboxID` to recover from cache misses in `GetClaimedSandbox`.
- Do not solve the theoretical full ambiguity of `<namespace>--<name>` when both namespace and name may contain `--`; the fallback path avoids that ambiguity by requiring a cache hit before APIReader fallback.

## Proposed Route Change

`proxy.Route` may include `Namespace`:

```go
type Route struct {
    Namespace       string    `json:"namespace,omitempty"`
    IP              string    `json:"ip"`
    ID              string    `json:"id"`
    UID             types.UID `json:"uid"`
    Owner           string    `json:"owner"`
    State           string    `json:"state"`
    ResourceVersion string    `json:"resourceVersion"`
}
```

Populate it in `sandboxutils.GetRouteFromSandbox` when building route entries:

```go
return proxy.Route{
    Namespace:       s.Namespace,
    IP:              s.Status.PodInfo.PodIP,
    ID:              GetSandboxID(s),
    UID:             s.GetUID(),
    Owner:           s.GetAnnotations()[agentsv1alpha1.AnnotationOwner],
    State:           state,
    ResourceVersion: s.GetResourceVersion(),
}
```

The JSON tag uses `omitempty` so routes produced by older binaries remain valid and routes sent to older binaries do not break JSON decoding. The `Namespace` field is useful route metadata. `GetClaimedSandbox` uses the cached Sandbox object for APIReader key derivation.

## GetClaimedSandbox Refactor

Refactor the function around a small lookup result and focused helpers. The lookup result only represents cache-hit outcomes:

```go
type claimedSandboxLookup struct {
    sandbox  *v1alpha1.Sandbox
    route    proxy.Route
    hasRoute bool
}

func (i *Infra) lookupClaimedSandbox(ctx context.Context, opts infra.GetClaimedSandboxOptions) (claimedSandboxLookup, error)
func decideAPIReaderFallback(lookup claimedSandboxLookup) string
func (i *Infra) getClaimedSandboxFromAPIReader(ctx context.Context, key client.ObjectKey, sandboxID string) (*v1alpha1.Sandbox, error)
```

The top-level flow should become linear:

```go
lookup, err := i.lookupClaimedSandbox(ctx, opts)
if err != nil {
    return nil, err
}

fallbackReason := decideAPIReaderFallback(lookup)
if fallbackReason == "" {
    return AsSandbox(lookup.sandbox, i.Cache), nil
}

key := client.ObjectKey{Namespace: lookup.sandbox.Namespace, Name: lookup.sandbox.Name}

fresh, err := i.getClaimedSandboxFromAPIReader(ctx, key, opts.SandboxID)
if err != nil {
    return nil, err
}
return AsSandbox(fresh, i.Cache), nil
```

`lookupClaimedSandbox` behavior:

- call `Cache.GetClaimedSandbox` with `opts.Namespace` and `opts.SandboxID`;
- on success, load the route for `opts.SandboxID` and return the cache-hit lookup result;
- on `cache.ErrSandboxNotFound`, keep polling at `RetryInterval`;
- stop waiting only when the cache hits, a non-NotFound cache error occurs, or `ctx.Done()` is closed;
- return `context.Canceled` or `context.DeadlineExceeded` unchanged when the caller's context ends.

`decideAPIReaderFallback` behavior:

- return `fallbackReasonRVExpectation` when `ResourceVersionExpectationSatisfied(lookup.sandbox)` is false;
- return `fallbackReasonCacheLagging` when a route exists, the route resourceVersion is non-empty, and the route resourceVersion is strictly newer than the cached Sandbox resourceVersion;
- return an empty reason when the cache-hit object can be served directly.

`getClaimedSandboxFromAPIReader` must preserve the cache contract:

- wrap APIReader NotFound as `cache.ErrSandboxNotFound`;
- reject fresh objects whose `LabelSandboxIsClaimed` is not `v1alpha1.True`;
- return non-NotFound APIReader errors unchanged.

## APIReader Key Resolution

APIReader fallback only occurs after a cache hit, so the key is always:

```go
client.ObjectKey{Namespace: lookup.sandbox.Namespace, Name: lookup.sandbox.Name}
```

`GetClaimedSandbox` should not derive an APIReader key from `opts.Namespace`, `opts.SandboxID`, `lookup.route.Namespace`, or `sandboxutils.ParseSandboxID`. This avoids admin-path ambiguity for namespaces containing `--` in the fallback path.

## Sandbox Gateway Impact

The runtime impact on `pkg/sandbox-gateway` is intentionally small:

- `gateway/controller` builds routes through `DefaultGetRouteFunc`, so routes generated from watched Sandboxes include `Namespace`.
- `gateway/registry` stores routes by an external string key and does not parse `Route.ID`.
- `gateway/filter` obtains the sandbox ID from the request adapter, looks it up in the registry, and uses only route `IP` and `State` for forwarding.
- `gateway/server` decodes route JSON on refresh. New fields are accepted by new binaries, ignored by old binaries, and absent in old route payloads.

Expected gateway code changes are limited to tests that compare full `proxy.Route` values.

## Compatibility

Rolling upgrades are supported:

- New sender to old receiver: old receiver ignores the `namespace` JSON field.
- Old sender to new receiver: new receiver sees an empty route namespace, which is acceptable because `GetClaimedSandbox` uses the cached Sandbox object for fallback key resolution.
- New sender to new receiver: route namespace is available as metadata, but APIReader fallback still uses the cached Sandbox key.

For cache misses, `GetClaimedSandbox` waits until the informer cache observes the claimed Sandbox or the caller's context ends. It does not call APIReader on route-present cache misses. This avoids serving objects that have not satisfied the local cache contract and avoids ambiguous key reconstruction from the admin path.

## Testing Plan

Add focused table-driven tests:

- `GetRouteFromSandbox` includes `Route.Namespace`.
- Cache miss, route present, cache later hits: `GetClaimedSandbox` waits for the cache and does not call APIReader.
- Cache miss, route present, context expires before cache hit: `GetClaimedSandbox` returns the context error and does not call APIReader.
- Cache miss, no route, context expires before cache hit: `GetClaimedSandbox` returns the context error and does not call APIReader.
- Cache hit, route resourceVersion newer than cache resourceVersion: APIReader fallback still occurs.
- Cache hit, local resource-version expectation unsatisfied: APIReader fallback still occurs.
- Cache hit, route resourceVersion equal to cache resourceVersion: no APIReader fallback.
- APIReader fallback still rejects objects that are not claimed.

Run:

```bash
go test ./pkg/sandbox-manager/infra/sandboxcr -count=1
go test ./pkg/sandbox-gateway/... -count=1
go test ./pkg/proxy -count=1
```

## Implementation Notes

- Keep `Route.ID` as the registry and lookup key.
- Do not use `ParseSandboxID` for `GetClaimedSandbox` APIReader fallback.
- Do not emit the `cache_not_found_route_present` fallback metric reason from `GetClaimedSandbox`.
- Do not keep an unsupported fallback path for key derivation failures in `GetClaimedSandbox`; key derivation cannot fail after a cache hit.
- Use context-bound polling rather than a fixed-step cache retry for cache misses.
- Avoid broad route protocol changes beyond the optional `namespace` field.
