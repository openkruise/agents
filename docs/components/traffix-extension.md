# Traffix Extension

`traffix-extension` is an **Envoy ext-proc (external processing) service** that
injects security tokens into HTTP request headers based on the `SecurityProfile`
CRD. It runs as an independent component within the OpenKruise Agents project
and provides token-based request authentication for AI agent workloads.

> Origin: this component was migrated from the standalone *Zeus* project.
> The CRD has been renamed `SandboxSecurityProfile → SecurityProfile` and the
> API group has been unified with the OpenKruise Agents group
> (`agents.kruise.io/v1alpha1`).

## How It Works

```
Client → Envoy (ext_proc filter) → traffix-extension (gRPC ext-proc server)
  └── matches request against SecurityProfile rules
  └── fetches tokens from the credential provider
  └── injects tokens into request headers
  └── forwarded request proceeds to destination
```

## Features

- **CRD-driven**: `SecurityProfile` (`agents.kruise.io/v1alpha1`) defines how
  to match incoming requests and what action to take.
- **Flexible matching**: domains, paths (Exact / Prefix / Regex) and HTTP
  methods, with wildcard support.
- **Application-layer blocking**: terminal `Block` action returns a configured
  HTTP status and response body without forwarding upstream — evaluated before
  token injection and independent of sandbox identity.
- **Token injection**: `tokenTransformation` rewrites credential headers using
  Go `text/template` (e.g. `Bearer {{ .Token }}`).
- **Conditional injection**: `when` conditions using regex pattern matching
  on existing headers.
- **Fail strategies**: `Block` (reject the request) or `Allow`/`Ignore`
  (forward without token) — controls token-injection failure handling.
- **Credential integration**: pluggable credential provider client with
  optional mTLS and an in-memory LRU token cache.
- **Kubernetes native**: runs as a Deployment with controller-runtime
  reconcilers watching `SecurityProfile` events.

## Architecture

```
┌─────────────┐     ┌─────────┐     ┌────────────────────────────────────┐
│   Client    │────▶│  Envoy  │────▶│  traffix-extension (ext-proc)      │
└─────────────┘     └─────────┘     │  ┌──────────────────────────────┐  │
                                    │  │ Request Handler              │  │
                                    │  │  1. Extract pod info         │  │
                                    │  │  2. Match profiles           │  │
                                    │  │  3. Check conditions         │  │
                                    │  │  4. Fetch token (cached)     │  │
                                    │  │  5. Inject header            │  │
                                    │  └──────────────────────────────┘  │
                                    │  ┌──────────────────────────────┐  │
                                    │  │ Config Store                 │  │
                                    │  │  - Profile index             │  │
                                    │  │  - Dynamic label matching    │  │
                                    │  └──────────────────────────────┘  │
                                    │  ┌──────────────────────────────┐  │
                                    │  │ Token Cache (LRU)            │  │
                                    │  │  - TTL: 3h (configurable)    │  │
                                    │  │  - Max: 10000 entries        │  │
                                    │  └──────────────────────────────┘  │
                                    └────────────────────────────────────┘
                                            │              │
                              ┌─────────────┘              └────────────────┐
                              ▼                                             ▼
                  ┌─────────────────────┐                       ┌──────────────────────┐
                  │  SecurityProfile    │                       │  Credential Provider │
                  │  CRD                │                       │  (external HTTP API) │
                  └─────────────────────┘                       └──────────────────────┘
```

## Build

```bash
# Generate code (deepcopy, clientset, CRD manifests)
make generate manifests

# Build binary (output: bin/traffix-extension)
make build-traffix-extension

# Build Docker image (default tag: traffix-extension:latest)
make docker-build-traffix-extension
```

## Command Line Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--grpc-port` | `9002` | gRPC port for the Envoy ext-proc service |
| `--grpc-health-port` | `9003` | gRPC port for liveness / readiness probes |
| `--metrics-port` | `9090` | HTTP port for Prometheus metrics |
| `--auth-metrics` | `false` | Require authentication / authorization on the metrics endpoint |
| `--streaming` | `false` | Enable Envoy full-duplex streaming mode |
| `-v` | `2` | Log verbosity (higher → more verbose) |

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `IDENTITY_PROVIDER_URL` | Credential provider API URL | `https://identity-provider.ack-agent-identity.svc.cluster.local:8443/` |
| `DEFAULT_SANDBOX_TOKEN` | Fallback sandbox token when filter_state is empty | unset |
| `TOKEN_CACHE_TTL` | Token cache TTL (Go duration format, e.g. `3h`, `30m`) | `3h` |
| `TOKEN_CACHE_MAX_SIZE` | Max number of cached tokens (LRU eviction) | `10000` |
| `CREDENTIAL_PROVIDER_CLIENT_CERT_PATH` | Client mTLS certificate path | `/etc/traffix-extension/mtls/client.crt` |
| `CREDENTIAL_PROVIDER_CLIENT_KEY_PATH` | Client mTLS private key path | `/etc/traffix-extension/mtls/client.key` |
| `CREDENTIAL_PROVIDER_CA_CERT_PATH` | Server CA certificate path | `/etc/traffix-extension/mtls/ca.crt` |

## CRD Sample

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SecurityProfile
metadata:
  name: agent-profile
  namespace: sandbox-prod
spec:
  selector:
    matchLabels:
      app: ai-agent
  rules:
  # Block any request to internal admin endpoints. Terminal — runs before
  # token injection and short-circuits the request with the configured
  # status code and body.
  - name: deny-admin
    match:
    - domains: ["*"]
      paths:
      - type: Prefix
        value: "/admin"
      methods: ["GET", "POST", "PUT", "DELETE"]
    actions:
      block:
        statusCode: 403
        body: '{"error":"admin path is blocked"}'
  - name: openai-chat
    match:
    - domains: ["api.openai.com"]
      paths:
      - type: Prefix
        value: "/v1/chat/completions"
      methods: ["POST"]
    actions:
      tokenTransformation:
        when:
          header: Authorization
          pattern: "^Bearer __PLACEHOLDER__"
        targetHeader: Authorization
        valueTemplate: "Bearer {{ .Token }}"
        tokenProviderRef:
          apiGroup: agentidentity.alibabacloud.com
          kind: CredentialProvider
          name: llm-api-key
        failStrategy: Block
```

### Action Precedence

The handler walks the rule chain in order across all profiles matching the
Pod's labels. For each rule whose `match` clause matches the request:

1. If the rule has `bypass: true` — forward the request unmodified and
   stop walking. **Terminal**; no further rules or plugins run. Does not
   require a sandbox token. Useful for trusted internal domains where
   subsequent action plugins (token injection, future security checks)
   should explicitly NOT execute.
2. Otherwise, if the rule has a `block` action — return an Envoy
   `ImmediateResponse` with the configured status code and body.
   **Terminal**; no further rules are evaluated. Does not require a
   sandbox token.
3. Otherwise, if the rule has a `tokenTransformation` action and no Block
   has fired yet, the rule is recorded and scanning continues so a later
   `block` rule can still win.

After the scan: if Bypass fired, the request flows through unmodified
(any earlier-accumulated mutations are discarded). If a Block fired, the
request is rejected. If only a `tokenTransformation` rule matched, token
injection runs against that rule. Otherwise the request passes through
unmodified.

Notes on `block`:

- `statusCode` defaults to `403` when omitted.
- `body` is sent verbatim. Envoy applies a default `text/plain`
  content-type — set the desired type via headers in the upstream
  Envoy filter chain if a different content-type is needed.

## Request Processing Flow

1. **Extract** pod namespace, name, sandbox labels and (optionally) sandbox
   token from Envoy `filter_state` (with E2E test fallbacks if absent).
2. **Lookup** matching `SecurityProfile` resources from the in-memory store
   using dynamic label matching.
3. **Walk** the rule chain in order; for each rule whose `match` succeeds
   (host, path, method), check `block` first — a matching Block returns
   an `ImmediateResponse` with the configured status code and body and
   terminates processing. Otherwise the first matching `tokenTransformation`
   rule is recorded and scanning continues.
4. **Check** the optional `when` condition by regex-matching an existing
   request header.
5. **Fetch** a token from the credential provider, caching it by
   `(credentialProviderName, resourceId)` with TTL.
6. **Inject** the rendered header value into the request.

If token injection fails and the action's `failStrategy` is `Block`, the
request is rejected with `PermissionDenied`. Otherwise the request is
forwarded unmodified.

## Layout

```
api/v1alpha1/securityprofile_types.go    SecurityProfile CRD types
cmd/traffix-extension/                    Binary entrypoint (wires plugins)
pkg/traffix-extension/
  framework/                              Foundational infrastructure
    configstore/                            In-memory SecurityProfile cache
    controller/                             SecurityProfile reconciler
    credential/                             Credential provider HTTP client (mTLS)
    tokencache/                             Thread-safe LRU token cache
  util/                                   Stateless helpers
    matcher/                                Request matching engine
    podlabels/                              Sandbox label decoder
    logging/                                Log level constants
  plugins/                                Independent request-handling plugins
    plugin.go                               Plugin interface + Result/RequestContext
    bypass/                                 Terminal Bypass action plugin (passthrough)
    block/                                  Terminal Block action plugin
    tokeninjection/                         TokenTransformation injection plugin
  handlers/                               Envoy ext-proc handler (orchestrator)
  server/                                 gRPC ext-proc server runner
  runnable/                               manager.Runnable wrappers (gRPC, leader election)
  tls/                                    Self-signed TLS helpers
  version/                                Build metadata (CommitSHA, BuildRef)
dockerfiles/traffix-extension.Dockerfile  Container image build
```

### Plugin Model

`HandleRequestHeaders` is an orchestrator: it resolves the matching profiles
and rules, then dispatches each matching rule to every registered plugin in
order. Each plugin returns one of three actions:

- `ActionContinue` — the rule's actions are not relevant to this plugin.
- `ActionImmediate` — terminal response (e.g. Block); the orchestrator
  returns it immediately and skips remaining rules and plugins.
- `ActionMutate` — header mutation that should be applied. The orchestrator
  accumulates mutations and continues, but invokes the same plugin at most
  once per request (first matching rule wins).

To add a new feature, implement `plugins.Plugin` in a new sub-package and
register it in the binary's `cmd/traffix-extension/main.go` plugin list. The
handler does not need to be modified.
