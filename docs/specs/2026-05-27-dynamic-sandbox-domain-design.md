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

## Architecture Decision Update

The initial implementation kept domain resolution in `Controller` and left
`pkg/servers/e2b/adapters` unchanged. This decision is superseded: interpreting
native versus customized request shapes is adapter protocol knowledge and must
not leak into HTTP handlers. The unified `E2BAdapter` selects the protocol shape,
while `NativeE2BAdapter` and `CustomizedE2BAdapter` own their respective domain
resolution and sandbox-address formatting rules.

The shared `E2BMapper` interface gains domain resolution and sandbox-address
formatting methods so the unified adapter can delegate all shape-specific work
through `ChooseAdapter`. The data-plane `proxy.RequestAdapter` contract remains
unchanged, and sandbox-gateway continues to call only `Map` and
`IsSandboxRequest`.

Static configuration precedence is not adapter protocol knowledge. The
Controller short-circuits on a non-empty `--e2b-domain`; otherwise it asks the
adapter to resolve the request authority. `BrowserUse` then passes the resolved
domain back to the adapter for shape-specific address formatting.

## Goals

- Return a `domain` derived from the current HTTP request when no static
  `--e2b-domain` is configured.
- Preserve `--e2b-domain` as a static override that bypasses dynamic
  resolution. Deployments that explicitly set the flag must keep working
  bit-for-bit.
- Cover both deployment shapes (native and customized) for both the response
  `domain` field and the `BrowserUse` websocket URL.
- Keep native/customized request-shape knowledge inside the adapter package;
  handlers receive only resolved strings from the unified `E2BAdapter` facade.
- Make the default `make deploy-sandbox-manager` path produce a deployment
  that uses dynamic resolution out of the box; manifests must not contradict
  the new default.

## Non-Goals

- Do not trust `X-Forwarded-Host`. Reverse-proxy support requires an explicit
  trust / allowlist config and is left for a follow-up PR.
- Do not persist `domain` anywhere on the Sandbox CR, annotations, labels,
  or runtime state.
- Do not change the `proxy.RequestAdapter` interface or data-plane routing
  behavior.

## Resolution Rules

When a handler is about to return a `models.Sandbox`, the Controller and unified
`E2BAdapter` derive the domain as follows:

1. If the configured domain (set from `--e2b-domain`) is non-empty, return it
   as is.
2. If the request authority is empty, return an error with
   `cannot resolve sandbox domain: empty host`.
3. If the request path starts with `adapters.CustomPrefix` (`/kruise`), the
   customized adapter is in use:
   1. Split `host[:port]` with the shared authority helper.
   2. Preserve host case and any leading `api.` segment because customized
      routing is path-based.
   3. Strip one trailing dot from the host.
   4. If the resulting host is empty, return HTTP 400 with
      `cannot resolve sandbox domain: empty host`.
   5. Rejoin `host` and `port` (omitting `:` when port is empty).
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
| `gateway.example.com:8443` | `/kruise/api/sandboxes` | `gateway.example.com:8443` |
| `api.gateway.example.com` | `/kruise/api/sandboxes` | `api.gateway.example.com` |
| `Gateway.example.com` | `/kruise/api/sandboxes` | `Gateway.example.com` |
| `gateway.example.com.` | `/kruise/api/sandboxes` | `gateway.example.com` |
| `gateway.example.com.:8443` | `/kruise/api/sandboxes` | `gateway.example.com:8443` |
| `:8443` | `/kruise/api/sandboxes` | HTTP 400 |
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

- `resolveSandboxDomain` returns the configured domain as-is when it is
  non-empty; otherwise it calls `E2BAdapter.GetDomain(authority, path)`.
- `E2BAdapter.GetSandboxAddress` accepts the resolved domain, path, sandbox ID,
  and port, selects the shape, and returns the final address as a string.
- Native and customized adapters implement their own address formatters; the
  Controller does not branch on request shape.
- The websocket URL replacer matches both `ws://` and `wss://` upstream
  `webSocketDebuggerUrl` values.

Without this split, customized deployments today silently return a
non-routable native-style URL — a latent bug that this design fixes.

## Code Changes

### `pkg/servers/e2b/core.go`

`Controller` stores the unified adapter created by `NewController`. `Init()`
passes the same instance to the sandbox manager so both the HTTP handlers and
data plane use the same adapter configuration:

```go
func (sc *Controller) Init() error {
    sandboxManager, err := sandboxmanager.NewSandboxManagerBuilder(sc.sandboxManagerOptions()).
        WithSandboxInfra().
        WithMemberlistPeers().
        WithRequestAdapter(sc.adapter).
        Build()
}
```

### `pkg/servers/e2b/sandbox.go`

- Keep `resolveSandboxDomain(r *http.Request) (string, *web.ApiError)` as a
  thin HTTP boundary. It returns `sc.domain` immediately when configured;
  otherwise it delegates to `sc.adapter.GetDomain` and maps adapter errors to
  HTTP 400.
- Change `convertToE2BSandbox` to
  `func (sc *Controller) convertToE2BSandbox(sbx infra.Sandbox, accessToken, domain string) *models.Sandbox`.
  The body uses the `domain` parameter instead of `sc.domain`; no other change.

### `pkg/servers/e2b/services.go`

`BrowserUse` calls `resolveSandboxDomain` before proxying to the sandbox, then
passes the resolved domain to `sc.adapter.GetSandboxAddress`. The previous
package-level native/customized address helpers are removed.

### `pkg/servers/e2b/adapters`

- Add `GetDomain(authority, path string) (string, error)` to the concrete
  unified adapter. It delegates through `ChooseAdapter(path)`.
- Add `GetSandboxAddress(domain, path, sandboxID string, port int32) string` to
  the unified adapter. The domain is already resolved, so formatting cannot
  fail.
- Extend `E2BMapper` with path-free `GetDomain(authority string)` and
  `GetSandboxAddress(domain, sandboxID, port)` methods implemented by
  `NativeE2BAdapter` and `CustomizedE2BAdapter`.
- Keep `proxy.RequestAdapter` unchanged. Sandbox-gateway does not call the new
  `E2BMapper` methods.
- Move optional-port and IPv6-aware authority splitting from the Controller to
  `NativeE2BAdapter`, the only protocol shape that uses it.

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
| `services.go` | `BrowserUse` | Parse `cdpPort` and get the sandbox first, resolve the domain, then ask the adapter to format the final sandbox address before proxying. |
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

### `pkg/proxy`

No changes. `proxy.RequestAdapter` remains unchanged; sandbox-gateway continues
using only `Map` and `IsSandboxRequest` on the concrete unified adapter.

## Error Handling

- Empty or unresolvable `r.Host` (no fallback static domain): HTTP 400 with
  `cannot resolve sandbox domain: empty host`. The 400 is returned before
  any state-mutating operation runs.
- `X-Forwarded-Host` is not consulted. Reverse proxies that need to
  propagate the inbound host must rewrite the upstream `Host` header.

## Testing

### Unit Tests — `pkg/servers/e2b/adapters/domain_test.go`

Add table-driven `TestNativeE2BAdapter_GetDomain` and
`TestCustomizedE2BAdapter_GetDomain` tests. Together their cases mirror the
resolution tables and native case-insensitivity. Static override behavior is a
Controller policy and is tested at the Controller boundary, not in adapters.

The `expectError` column follows the project convention (empty string = no
error, non-empty = `assert.Contains(err.Error(), expectError)`).

Add table-driven `TestNativeE2BAdapter_GetSandboxAddress` and
`TestCustomizedE2BAdapter_GetSandboxAddress` tests covering formatting of an
already resolved domain, including preservation of case and trailing dots. Keep
small unified-adapter tests for native/customized path dispatch only. Expected
addresses are literal values rather than values built by production helpers.

### Adapter Package

Path-shape detection is private to `adapters`. Concrete-adapter tests own the
protocol-specific behavior matrix, while unified-adapter tests cover only
dispatch outcomes instead of directly testing the internal selector.
Controller-level resolver tests own static override precedence and HTTP error
mapping.

### Integration Tests (minimum contract)

| Test file | Handler | Cases |
|---|---|---|
| `sandbox_test.go` | `resolveSandboxDomain` | configured domain bypasses an empty Host and is returned as-is; dynamic native/customized resolution; dynamic empty Host → 400 |
| `services_test.go` | `CreateSandbox` | empty host → 400 **and** pooled sandbox is not claimed; subsequent create with a valid host succeeds and `Domain` matches the resolved value |
| `services_test.go` | `BrowserUse` | configured native/customized domains bypass an empty Host and remain unchanged; dynamic native/customized URL shapes; dynamic empty Host → 400 **and** no upstream request was sent to the sandbox |

Each test uses `httptest.NewRequest` with an explicit `req.Host` and an
`r.URL.Path` consistent with the shape under test. The "no state mutation
on 400" assertions are the central regression guard for P2.

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
- `sandbox-gateway` continues using only `Map` and `IsSandboxRequest`; its
  configuration and request-routing behavior are unchanged.
- No CRD, annotation, label, or runtime state is added or read.
