# Dynamic Sandbox Domain Resolution Design

## Context

The E2B Sandbox response body carries a `domain` field that clients use to
construct sandbox-side URLs such as `wss://{port}-{sandboxID}.{domain}`. Today,
`sandbox-manager` fills this field from a single `--e2b-domain` startup flag
(default `localhost`). A single `sandbox-manager` fronting multiple user-facing
hostnames (e.g., `api.foo.com` and `api.bar.com`) cannot return the host the
client actually used.

This design adds per-request dynamic domain resolution while keeping
`--e2b-domain` as a static override, so single-domain deployments are
unaffected.

The two deployment shapes the resolver must cover:

| Shape | API host / path | Sandbox URL pattern |
|---|---|---|
| Native | `api.X` (subdomain) | `<port>-<sid>.X` |
| Customized | `X/kruise/api/...` (path prefix) | `X/kruise/<sid>/<port>` |

`BrowserUse` returns a websocket URL that points at the sandbox, so its URL
shape must follow the same native vs. customized split.

## Goals

- Return a `domain` derived from the current HTTP request when no static
  `--e2b-domain` is configured.
- Preserve `--e2b-domain` as a static override that bypasses dynamic
  resolution. Deployments that explicitly set the flag must keep working
  bit-for-bit.
- Cover both deployment shapes (native and customized) for both the response
  `domain` field and the `BrowserUse` websocket URL.
- Keep `pkg/proxy` and `pkg/servers/e2b/adapters` unchanged. Domain
  resolution is a Controller-layer concern and uses the inbound request
  `Host` and path directly.
- Make the default `make deploy-sandbox-manager` path produce a deployment
  that uses dynamic resolution out of the box; manifests must not contradict
  the new default.

## Non-Goals

- Do not trust `X-Forwarded-Host`. Reverse-proxy support requires an explicit
  trust / allowlist config and is left for a follow-up PR.
- Do not persist `domain` anywhere on the Sandbox CR, annotations, labels,
  or runtime state.
- Do not change the `proxy.RequestAdapter` interface or the
  `adapters.E2BAdapter` public surface.

## Resolution Rules

When a handler is about to return a `models.Sandbox` (or wires a sandbox URL
into a response, as `BrowserUse` does), the Controller derives the domain as
follows:

1. If `sc.domain` (set from `--e2b-domain`) is non-empty, return it as is.
2. If `r.Host` is empty, return HTTP 400 with
   `cannot resolve sandbox domain: empty host`.
3. If `r.URL.Path` starts with `adapters.CustomPrefix` (`/kruise`), the
   customized adapter is in use. Return `r.Host` unchanged — host
   case is preserved because customized routing is path-based, and the
   response must echo the client's request host verbatim.
4. Otherwise (native adapter shape):
   1. Split `host[:port]` while preserving bracketed IPv6. Inputs without an
      explicit port, including bracketed IPv6 such as `[::1]`, are valid.
   2. Lowercase `host` (DNS labels are case-insensitive, RFC 4343). The port
      is preserved as written.
   3. Strip a leading `api.` segment from the lowercased `host`. Only a
      literal `api.` segment is stripped, because the dot is part of the
      prefix; `apiserver.example.com` does not match.
   4. Strip one trailing dot from the native host.
   5. If the resulting host is empty, return HTTP 400 with
      `cannot resolve sandbox domain: empty host`.
   6. Rejoin `host` and `port` (omitting `:` when port is empty).

Without case normalization, a client request to `API.example.com` would
yield response `API.example.com`. The constructed sandbox subdomain
`<port>-<sid>.API.example.com` would then diverge from the actual
(lowercased) DNS name; this confuses TLS SNI validation and some clients.

### Native Examples

| Input `r.Host` | Output |
|---|---|
| `api.example.com` | `example.com` |
| `api.example.com:8443` | `example.com:8443` |
| `API.example.com` | `example.com` |
| `API.example.com:8443` | `example.com:8443` |
| `api.example.com.` | `example.com` |
| `api.example.com.:8443` | `example.com:8443` |
| `[::1]` | `[::1]` |
| `[::1]:8443` | `[::1]:8443` |
| `example.com` | `example.com` |
| `localhost:7788` | `localhost:7788` |
| `apiserver.example.com` | `apiserver.example.com` |
| `api.` | HTTP 400 |
| `api.:8443` | HTTP 400 |
| `""` | HTTP 400 |

### Customized Examples

| Input `r.Host` | `r.URL.Path` | Output |
|---|---|---|
| `gateway.example.com` | `/kruise/api/sandboxes` | `gateway.example.com` |
| `api.gateway.example.com` | `/kruise/api/sandboxes` | `api.gateway.example.com` |
| `Gateway.example.com` | `/kruise/api/sandboxes` | `Gateway.example.com` |
| `gateway.example.com.` | `/kruise/api/sandboxes` | `gateway.example.com.` |
| `""` | `/kruise/api/sandboxes` | HTTP 400 |

## BrowserUse Websocket Shape

`BrowserUse` (`pkg/servers/e2b/services.go`) returns a websocket debugger
URL that points at the sandbox. The URL shape depends on the deployment
style of the inbound request:

- Native request (path does not start with `/kruise`):
  `wss://<port>-<sid>.<domain>`
- Customized request (path starts with `/kruise`):
  `wss://<domain>/kruise/<sid>/<port>`

This requires:

- A second URL builder for the customized shape. Add
  `GetCustomizedSandboxAddress(sandboxID, domain string, port int32) string`
  in `pkg/utils/sandbox-manager/e2b.go`. The existing `GetSandboxAddress`
  keeps the native shape unchanged.
- A small Controller helper `isCustomizedRequest(r *http.Request) bool` that
  returns `strings.HasPrefix(r.URL.Path, adapters.CustomPrefix)` — the same
  check used inside `resolveSandboxDomain`.
- `BrowserUse` picks the address builder based on `isCustomizedRequest(r)`.
- The websocket URL replacer matches both `ws://` and `wss://` upstream
  `webSocketDebuggerUrl` values.

Without this split, customized deployments today silently return a
non-routable native-style URL — a latent bug that this design fixes.

## Code Changes

### `pkg/servers/e2b/core.go`

`Controller` does not store the request adapter. `Init()` constructs the
adapter locally for the sandbox manager:

```go
func (sc *Controller) Init() error {
    // ...
    adapter := adapters.DefaultAdapterFactory(sc.port)
    sandboxManager, err := sandboxmanager.NewSandboxManagerBuilder(sc.sandboxManagerOptions()).
        WithSandboxInfra().
        WithMemberlistPeers().
        WithRequestAdapter(adapter).
        Build()
    // ...
}
```

### `pkg/servers/e2b/sandbox.go`

- Add `resolveSandboxDomain(r *http.Request) (string, *web.ApiError)`
  implementing the rules above. Empty-host surfaces as `*web.ApiError`
  with `Code: http.StatusBadRequest`.
- Add `splitHostPort(authority string) (host, port string)` as a local
  helper: split on the last `:`; return `(authority, "")` when no colon is
  present. `net.SplitHostPort` is rejected because it errors on inputs like
  `example.com` (no port), which is a normal case here.
- Add `isCustomizedRequest(r *http.Request) bool` returning
  `strings.HasPrefix(r.URL.Path, adapters.CustomPrefix)`.
- Change `convertToE2BSandbox` to
  `func (sc *Controller) convertToE2BSandbox(sbx infra.Sandbox, accessToken, domain string) *models.Sandbox`.
  The body uses the `domain` parameter instead of `sc.domain`; no other
  change.

### `pkg/utils/sandbox-manager/e2b.go`

Add a customized-shape builder alongside the existing native builder:

```go
// GetSandboxAddress returns the native E2B subdomain address:
// "<port>-<sid>.<domain>".
func GetSandboxAddress(sandboxId, domain string, port int32) string {
    return fmt.Sprintf("%d-%s.%s", port, sandboxId, domain)
}

// GetCustomizedSandboxAddress returns the customized path-style address:
// "<domain>/kruise/<sid>/<port>".
func GetCustomizedSandboxAddress(sandboxId, domain string, port int32) string {
    return fmt.Sprintf("%s/kruise/%s/%d", domain, sandboxId, port)
}
```

### Handler Call Sites

For every handler that produces a `models.Sandbox` or a sandbox URL,
`resolveSandboxDomain` is invoked before any state mutation (claim, clone,
resume, timeout update, or upstream proxy request). Handlers may first run
side-effect-free request validation or sandbox existence checks when that
preserves the API's error precedence.

| File | Handler / Helper | Change |
|---|---|---|
| `create.go` | `CreateSandbox` | Parse and validate the request body, then resolve domain; bail with 400 before `ClaimSandbox` / `CloneSandbox` runs. Pass `domain` to `createSandboxWithClaim` / `createSandboxWithClone`. |
| `create.go` | `createSandboxWithClaim` | Accept new `domain string` parameter; forward to `convertToE2BSandbox`. |
| `create.go` | `createSandboxWithClone` | Accept new `domain string` parameter; forward to `convertToE2BSandbox`. |
| `services.go` | `DescribeSandbox` | Get the sandbox first so missing sandbox remains 404, then resolve domain and pass it to `convertToE2BSandbox`. |
| `services.go` | `BrowserUse` | Parse `cdpPort` and get the sandbox first, then resolve domain before proxying to the sandbox. Branch on `isCustomizedRequest(r)` to pick `GetSandboxAddress` (native) or `GetCustomizedSandboxAddress`. |
| `list.go` | `ListSandboxes` | Parse and validate query parameters first, then resolve once before manager list and reuse for every entry. |
| `pause_resume.go` | `ConnectSandbox` | Parse and validate the timeout request, then resolve domain before `ResumeSandbox` / `updateConnectTimeout` runs; bail with 400 on empty host. Pass to `convertToE2BSandbox`. |

### `cmd/sandbox-manager/main.go`

Flip the flag default and update the help string:

```go
pflag.StringVar(&domain, "e2b-domain", "",
    "Static E2B domain. When empty, the domain is resolved per-request from "+
        "the HTTP Host header (api. prefix stripped for native paths; host "+
        "preserved for /kruise/* customized paths).")
```

### Manifest Updates

Without these, the default `make deploy-sandbox-manager` path would still
inject a non-empty `--e2b-domain` and short-circuit the new logic.

**`config/sandbox-manager/deployment.yaml`** — remove the
`--e2b-domain=replace.with.your.domain` argument (currently at args
index 6). Every argument after it shifts left by one. The new args layout:

```yaml
args:
  - -v=7                              # 0
  - --zap-log-level=7                 # 1
  - --system-namespace=sandbox-system # 2
  - --peer-selector=...               # 3
  - --kube-client-qps=10000           # 4
  - --kube-client-burst=30000         # 5
  - --e2b-admin-key=some-api-key      # 6 (was 7)
  - --e2b-enable-auth=true            # 7 (was 8)
  - --e2b-max-timeout=2592000         # 8 (was 9)
```

**`config/sandbox-manager/configuration_patch.yaml`** — remove the
`--e2b-domain` patch entry entirely. Update the `--e2b-admin-key` patch
path to match the shifted index:

```yaml
# E2B API Key (now configured via command line args)
- op: replace
  path: /spec/template/spec/containers/0/args/6
  value: --e2b-admin-key=some-api-key
```

Operators who still want the previous `localhost` behavior can add their
own overlay setting `--e2b-domain=localhost`.

**`config/sandbox-manager/ingress_patch.yaml`** — unchanged. It configures
ingress host names (a deployment-level concern), not the sandbox-manager
process.

### `pkg/proxy` and `pkg/servers/e2b/adapters`

No changes.

## Error Handling

- Empty or unresolvable `r.Host` (no fallback static domain): HTTP 400 with
  `cannot resolve sandbox domain: empty host`. The 400 is returned before
  any state-mutating operation runs.
- `X-Forwarded-Host` is not consulted. Reverse proxies that need to
  propagate the inbound host must rewrite the upstream `Host` header.

## Testing

### Unit Tests — `pkg/servers/e2b/sandbox_test.go`

Add `TestResolveSandboxDomain`, table-driven. Cases mirror the resolution
tables plus the static override and case-insensitivity:

| name | sc.domain | r.Host | r.URL.Path | expect (string) | expectError |
|---|---|---|---|---|---|
| static configured wins | `example.com` | `api.foo.com` | `/sandboxes` | `example.com` | `""` |
| native strip api with port | `""` | `api.example.com:8443` | `/sandboxes` | `example.com:8443` | `""` |
| native strip api no port | `""` | `api.example.com` | `/sandboxes` | `example.com` | `""` |
| native strip uppercase api | `""` | `API.example.com` | `/sandboxes` | `example.com` | `""` |
| native strip uppercase with port | `""` | `API.example.com:8443` | `/sandboxes` | `example.com:8443` | `""` |
| native no api prefix | `""` | `example.com` | `/sandboxes` | `example.com` | `""` |
| native trailing dot | `""` | `api.example.com.` | `/sandboxes` | `example.com` | `""` |
| native bracketed ipv6 without port | `""` | `[::1]` | `/sandboxes` | `[::1]` | `""` |
| native bracketed ipv6 with port | `""` | `[::1]:8443` | `/sandboxes` | `[::1]:8443` | `""` |
| native localhost with port | `""` | `localhost:7788` | `/sandboxes` | `localhost:7788` | `""` |
| native apiserver not stripped | `""` | `apiserver.example.com` | `/sandboxes` | `apiserver.example.com` | `""` |
| customized host as-is | `""` | `gateway.example.com` | `/kruise/api/sandboxes` | `gateway.example.com` | `""` |
| customized api host not stripped | `""` | `api.gateway.example.com` | `/kruise/api/sandboxes` | `api.gateway.example.com` | `""` |
| customized case preserved | `""` | `Gateway.example.com` | `/kruise/api/sandboxes` | `Gateway.example.com` | `""` |
| customized trailing dot preserved | `""` | `gateway.example.com.` | `/kruise/api/sandboxes` | `gateway.example.com.` | `""` |
| empty host returns 400 | `""` | `""` | `/sandboxes` | — | `empty host` |
| native api dot returns 400 | `""` | `api.` | `/sandboxes` | — | `empty host` |

The `expectError` column follows the project convention (empty string = no
error, non-empty = `assert.Contains(err.Message, expectError)`).

Add `TestIsCustomizedRequest`, table-driven: native path returns `false`,
`/kruise/...` returns `true`, empty path returns `false`.

### Integration Tests

Every response-affecting handler gets coverage for: static configured,
dynamic resolution success, and dynamic resolution failure. The failure
rows additionally assert that no state mutation occurred where the handler
would otherwise mutate sandbox state.

| Test file | Handler | Added cases |
|---|---|---|
| `services_test.go` | `DescribeSandbox` | static → body `Domain` matches configured; dynamic success → body `Domain` matches resolved; empty host → 400; missing sandbox plus empty host → 404 |
| `create_test.go` | `CreateSandbox` (claim path) | dynamic success → body `Domain` matches; empty host → 400 **and** `ClaimSandbox` was not invoked |
| `pause_resume_test.go` | `ConnectSandbox` | dynamic success → body `Domain` matches; empty host → 400 **and** `ResumeSandbox` / sandbox timeout writes were not invoked; invalid timeout plus empty host preserves timeout validation 400 |
| `list_test.go` | `ListSandboxes` | dynamic success → every returned entry's `Domain` matches the resolved value; invalid query plus empty host → query validation 400 |
| `services_test.go` | `BrowserUse` | native path → URL is `wss://<port>-<sid>.<domain>`; customized path (`/kruise/api/browser/...`) → URL is `wss://<domain>/kruise/<sid>/<port>`; empty host → 400 **and** no upstream request was sent to the sandbox; invalid `cdpPort` or missing sandbox plus empty host preserve their earlier errors |

Each test uses `httptest.NewRequest` with an explicit `req.Host` and an
`r.URL.Path` consistent with the shape under test. The "no state mutation
on 400" assertions are the central regression guard for P2.

### Adapter Package

Unchanged. Controller routing shape detection uses the same path prefix as
the adapter.

## Compatibility

- Deployments that explicitly set `--e2b-domain=<value>` keep the previous
  behavior bit-for-bit.
- The flag default flips from `localhost` to `""`, and the standard
  manifests no longer inject a hard-coded value. Fresh
  `make deploy-sandbox-manager` deployments default to dynamic resolution.
- Operators currently relying on the patched `--e2b-domain=localhost`
  default keep working by explicitly setting `--e2b-domain=localhost` in
  their own overlay.
- Customized-shape `BrowserUse` was previously returning a native-style URL
  even in customized deployments — a latent bug. After this change the
  returned URL matches the actually reachable address. Captured in the
  changelog under behavioral fixes.
- No CRD, annotation, label, or runtime state is added or read.

## Implementation Order

1. Add `GetCustomizedSandboxAddress` to `pkg/utils/sandbox-manager/e2b.go`.
2. Change `convertToE2BSandbox` signature; thread `domain` through every
   handler call site (still using `sc.domain` as the value at this step);
   add `isCustomizedRequest` and split `BrowserUse` URL builder per shape.
   Tests continue to pass.
3. Keep adapter construction local to `Init()` for the sandbox-manager
   builder; domain resolution does not depend on it.
4. Implement `resolveSandboxDomain` and `splitHostPort`; switch handlers
   from `sc.domain` to `resolveSandboxDomain(r)`, resolving before any
   mutating call or upstream request while preserving handler-specific
   validation precedence.
5. Flip the `--e2b-domain` default to `""` in `main.go`.
6. Update `config/sandbox-manager/deployment.yaml` (drop the args entry)
   and `configuration_patch.yaml` (drop the domain patch, renumber the
   admin-key patch path).
7. Add `TestResolveSandboxDomain` and `TestIsCustomizedRequest` in
   `pkg/servers/e2b/sandbox_test.go`.
8. Add integration test rows in `services_test.go`, `create_test.go`,
   `pause_resume_test.go`, and `list_test.go` per the table above.
