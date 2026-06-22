# Traffix Extension

`traffic-extension` is an **Envoy ext-proc (external processing) service** that
enforces the `SecurityProfile` CRD on HTTP egress traffic. It runs as an
independent component within the OpenKruise Agents project and provides
application-layer policy enforcement (currently terminal `Block` and `Bypass`
actions) for AI agent workloads.

> Origin: this component was migrated from the standalone *Zeus* project.
> The CRD has been renamed `SandboxSecurityProfile → SecurityProfile` and the
> API group has been unified with the OpenKruise Agents group
> (`agents.kruise.io/v1alpha1`).

## How It Works

```
Client → Envoy (ext_proc filter) → traffic-extension (gRPC ext-proc server)
  └── matches request against SecurityProfile rules
  └── runs registered plugins (currently Bypass, Block)
  └── forwards / rewrites / rejects per the matched action
```

## Features

- **CRD-driven**: `SecurityProfile` (`agents.kruise.io/v1alpha1`) defines how
  to match incoming requests and what action to take.
- **Flexible matching**: domains, paths (Exact / Prefix / Regex), HTTP
  methods, ports, headers and query parameters with wildcard support.
- **Application-layer blocking**: terminal `Block` action returns a
  configured HTTP status and response body without forwarding upstream.
- **Trusted-path bypass**: terminal `Bypass` action forwards the request
  unmodified and short-circuits the rule chain.
- **Kubernetes native**: runs as a Deployment with controller-runtime
  reconcilers watching `SecurityProfile` events.

> Other action types listed in the original design (TokenTransformation,
> IdentityInjection, SecurityCheck, Mirroring, RateLimit, Forwarding) are
> deferred until their plugin implementations land. The CRD intentionally
> only exposes the actions the data plane can enforce today.

## Architecture

```
┌─────────────┐     ┌─────────┐     ┌────────────────────────────────────┐
│   Client    │────▶│  Envoy  │────▶│  traffic-extension (ext-proc)      │
└─────────────┘     └─────────┘     │  ┌──────────────────────────────┐  │
                                    │  │ Request Handler              │  │
                                    │  │  1. Extract pod info         │  │
                                    │  │  2. Match profiles           │  │
                                    │  │  3. Walk rules / plugins     │  │
                                    │  │  4. Forward / Block / Bypass │  │
                                    │  └──────────────────────────────┘  │
                                    │  ┌──────────────────────────────┐  │
                                    │  │ Config Store                 │  │
                                    │  │  - Profile index             │  │
                                    │  │  - Dynamic label matching    │  │
                                    │  └──────────────────────────────┘  │
                                    └────────────────────────────────────┘
                                            │
                                            ▼
                                ┌─────────────────────┐
                                │  SecurityProfile    │
                                │  CRD                │
                                └─────────────────────┘
```

## Build

```bash
# Generate code (deepcopy, clientset, CRD manifests)
make generate manifests

# Build binary (output: bin/traffic-extension)
make build-traffic-extension

# Build Docker image (default tag: traffic-extension:latest)
make docker-build-traffic-extension
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
| `--audit-log-buffer-size` | `4096` | Channel capacity for the async audit logger; entries dropped when full |

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
  # Trusted internal domains — forward without further processing.
  - name: trust-internal
    match:
    - domains: ["internal.local"]
    actions:
      bypass: true
  # Block any request to internal admin endpoints. Terminal — short-circuits
  # the request with the configured status code and body.
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
```

### Action Precedence

The handler walks the rule chain in order across all profiles matching the
Pod's labels. For each rule whose `match` clause matches the request:

1. If the rule has `bypass: true` — forward the request unmodified and
   stop walking. **Terminal**; no further rules or plugins run. Useful for
   trusted internal domains where future non-terminal action plugins should
   explicitly NOT execute.
2. Otherwise, if the rule has a `block` action — return an Envoy
   `ImmediateResponse` with the configured status code and body.
   **Terminal**; no further rules are evaluated.

Plugin registration order is `Bypass → Block`, so a single rule that
pathologically declares both actions resolves as Bypass.

Notes on `block`:

- `statusCode` defaults to `403` when omitted.
- `body` is sent verbatim. Envoy applies a default `text/plain`
  content-type — set the desired type via headers in the upstream
  Envoy filter chain if a different content-type is needed.

## Request Processing Flow

1. **Extract** pod namespace, name and sandbox labels from Envoy
   `filter_state` (with E2E test fallbacks if absent).
2. **Lookup** matching `SecurityProfile` resources from the in-memory store
   using dynamic label matching.
3. **Walk** the rule chain. For each rule whose `match` succeeds the
   orchestrator dispatches the rule to every registered plugin in
   registration order. Bypass short-circuits with a passthrough; Block
   short-circuits with an `ImmediateResponse`. If no plugin acts, the
   request is forwarded unmodified.

## Audit Logging

Every `RequestHeaders` invocation emits exactly one INFO-level audit record
through an asynchronous, non-blocking pipeline. Operators can grep a single
line per egress to answer "which SecurityProfile / rule / action handled
this request, and what was the outcome".

### Fields

| Key | Meaning |
|-----|---------|
| `pod` | `namespace/name` of the source pod |
| `method` / `host` / `path` | HTTP request identity |
| `profiles` | Number of SecurityProfiles whose selector matched the pod |
| `outcome` | One of `passthrough`, `mutated`, `blocked`, `bypassed`, `error` (precedence: `error > bypassed > blocked > mutated > passthrough`) |
| `actions` | List of `<plugin>:<profile-namespace>/<profile-name>/<rule>` entries for every plugin that materially acted |
| `skipped` | Map of plugin → count of times the plugin claimed a rule but a later terminal action / error preempted it |
| `error` | Non-empty only when `outcome=error`, or when a permissive-mode plugin swallowed a failure that is still worth surfacing |

### Delivery

- A single worker goroutine (registered as a `manager.Runnable`) drains a
  buffered channel and writes records via the standard logr logger.
- The request path performs a non-blocking channel send; when the buffer is
  full the entry is dropped and the
  `traffic_extension_audit_log_dropped_total` counter increments.
- Default buffer size is `4096`; tune via `--audit-log-buffer-size` and
  watch the dropped counter to decide whether to raise it.

### Sample

```
INFO audit egress request handled
     pod=default/agent-7d9 method=POST host=api.openai.com path=/v1/admin/keys
     profiles=1 outcome=blocked
     actions=["block:default/p1/deny-admin"] skipped={}
```

## Layout

```
api/v1alpha1/securityprofile_types.go    SecurityProfile CRD types
cmd/traffic-extension/                    Binary entrypoint (wires plugins)
pkg/traffic-extension/
  framework/                              Foundational infrastructure
    configstore/                            In-memory SecurityProfile cache
    controller/                             SecurityProfile reconciler
  util/                                   Stateless helpers
    matcher/                                Request matching engine
    podlabels/                              Sandbox label decoder
    logging/                                Log level constants
    auditlog/                               Buffered audit log worker
  plugins/                                Independent request-handling plugins
    plugin.go                               Plugin interface + Result/RequestContext
    bypass/                                 Terminal Bypass action plugin (passthrough)
    block/                                  Terminal Block action plugin
  handlers/                               Envoy ext-proc handler (orchestrator)
  server/                                 gRPC ext-proc server runner
  runnable/                               manager.Runnable wrappers (gRPC, leader election)
  tls/                                    Self-signed TLS helpers
  version/                                Build metadata (CommitSHA, BuildRef)
dockerfiles/traffic-extension.Dockerfile  Container image build
```

### Plugin Model

`HandleRequestHeaders` is an orchestrator: it resolves the matching profiles
and rules, then dispatches each matching rule to every registered plugin in
order. Each plugin returns one of four actions:

- `ActionContinue` — the rule's actions are not relevant to this plugin.
- `ActionImmediate` — terminal response (e.g. Block); the orchestrator
  returns it immediately and skips remaining rules and plugins.
- `ActionMutate` — header mutation that should be applied. The orchestrator
  accumulates mutations and continues, but invokes the same plugin at most
  once per request (first matching rule wins).
- `ActionRecord` — the plugin claims the rule for a deferred upstream call
  and waits for the scan to complete. The orchestrator only invokes the
  plugin's `Finalize` method if no terminal `Immediate` fired in the
  meantime, so later Block/Bypass rules suppress unnecessary work.

To add a new feature, implement `plugins.Plugin` in a new sub-package and
register it in the binary's `cmd/traffic-extension/main.go` plugin list. The
handler does not need to be modified.
