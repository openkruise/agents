# SecurityProfile upstream proxy API

Add only `actions.proxy.url` to the shared SecurityProfile and
GlobalSecurityProfile rule actions object:

```yaml
actions:
  proxy:
    url: https://proxy.example:443
```

`Proxy *ProxyAction` is optional; `ProxyAction.URL string` is required.
The URL accepts http, https, and socks5 with an explicit port in 1–65535,
a DNS name, IPv4, or bracketed IPv6 host, and an optional root slash.
Userinfo, business paths, queries, fragments, and special values such as
direct/default are rejected. No proxy registry, default route, credentials,
or Sandbox inline proxy field is introduced.

Proxy is an HTTP action, incompatible in one rule with TLS actions, Block,
or enabled Bypass; bypass false remains equivalent to omission. It may
coexist with other HTTP actions. Existing HTTPCallout ordering is retained:
Bypass → Block → MCPToolPolicy → HeaderManipulation → HTTPCallout →
TokenTransformation → Proxy; Audit retains its asynchronous lifecycle.
The first actually executed Proxy selects egress without terminating rule
evaluation. Later Block, MCP, or other security rejection still prevents
connection. A later Bypass preserves an already selected proxy. If no Proxy
executes, existing forwarding and security semantics remain unchanged.

Runtime support is separate work in agentio. Authorize the real target before
connecting to a proxy. Support normal application HTTP/HTTPS only; HTTPS
requires gateway TLS termination and real URL inspection. Explicit client
proxy requests and client CONNECT chains are unsupported; legacy CONNECT
passthrough is not this action. The proxy resolves target domains. HTTP targets
use standard absolute-form forward requests through HTTP(S) proxies; HTTPS
targets use gateway-initiated CONNECT then verified target TLS, plus outer
verified proxy TLS for HTTPS proxies. SOCKS5 is no-auth only. HTTP(S) must
neither send nor forward Proxy-Authorization. Proxy failures never fall back
to direct forwarding.

Profiles select Pods through their selectors; separate destination rules can
choose separate proxies for those Pods.
Domain collections, scale, and performance are outside this proposal.
HTTP and HTTPS proxy endpoints can each forward HTTP and HTTPS targets;
the proxy URL scheme controls whether the gateway-to-proxy connection uses
TLS, independently of the target scheme. This change performs no
real-environment connectivity or E2E verification.

CRD OpenAPI patterns validate the URL, including IPv6 and port; CEL validates
action conflicts. URL parsing in per-rule CEL exceeds Kubernetes' estimated
cost budget for the existing unbounded rule list, so URL validation uses a
pattern without imposing a new rule-count limit. Both profile CRDs and DeepCopy code are generated using
`make generate manifests`. Schema tests exercise both profile kinds with
OpenAPI and CEL. No existing action's validation is widened by this change.

Deploy runtime capability before applying proxy policies; older gateways
must not silently ignore proxy actions. API generation alone does not provide
runtime enforcement. The source of truth remains agents/api; agents-api
exports it under agents/v1alpha1. Local integration can use a generated local SDK;
no publication is required for local development.
