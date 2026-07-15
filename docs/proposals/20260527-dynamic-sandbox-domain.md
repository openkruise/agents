---
title: Dynamic Sandbox Domain Resolution
authors:
  - "@AiRanthem"
reviewers:
  - "@TBD"
creation-date: 2026-05-27
last-updated: 2026-07-15
status: implemented
see-also:
  - "/docs/specs/2026-05-27-dynamic-sandbox-domain-design.md"
---

# Dynamic Sandbox Domain Resolution

## Summary

When `--e2b-domain` is empty, `sandbox-manager` now derives each returned Sandbox domain from that request's HTTP `Host`. One process can therefore serve multiple native (`api.X` -> `<port>-<sandboxID>.X`) and customized (`X/kruise/api/...` -> `X/kruise/<sandboxID>/<port>`) hostnames. An explicitly configured domain remains an exact static override. The default deployment enables dynamic resolution, `BrowserUse` emits the matching websocket shape, and the certificate tooling supports the corresponding multi-domain TLS setup.

## Behavior

| Input | Resolved domain |
|---|---|
| Non-empty `--e2b-domain` | Returned byte-for-byte; request host is ignored. |
| Native path | Split optional port while preserving IPv6, lowercase the host, remove one leading `api.` and one trailing `.`, then reattach the port. |
| Path starting with `/kruise` | Split optional port, preserve host case and `api.`, remove one trailing `.`, then reattach the port. |
| Empty host after normalization | HTTP 400: `cannot resolve sandbox domain: empty host`. |

Only the literal `api.` prefix is removed (`apiserver.example.com` is unchanged). Ports, bracketed IPv6, and raw IPv6 without a port are accepted. `X-Forwarded-Host` is never trusted; a reverse proxy must rewrite `Host` itself.

The resolved domain is used consistently in Sandbox responses and `BrowserUse`:

| Request shape | Sandbox address |
|---|---|
| Native | `<port>-<sandboxID>.<domain>` |
| Customized | `<domain>/kruise/<sandboxID>/<port>` |

The Browser debugger URL is rewritten to `wss://<sandbox-address>` whether its upstream scheme is `ws://` or `wss://`. A static domain remains unnormalized during both response population and address formatting.

## Implementation

- `Controller` owns one unified `E2BAdapter`, passes the same instance to `sandbox-manager`, and applies static-override precedence at the HTTP boundary.
- `E2BAdapter` chooses by request path; `NativeE2BAdapter` and `CustomizedE2BAdapter` own domain resolution and address formatting. `E2BMapper` exposes those operations, while the data-plane `proxy.RequestAdapter` contract and sandbox-gateway routing remain unchanged.
- `convertToE2BSandbox` receives the already resolved domain instead of reading controller configuration.
- Domain resolution is inserted into every response-producing flow with existing validation/error precedence preserved:

  | Flow | Ordering |
  |---|---|
  | Create | Parse and validate, resolve, then claim or clone. |
  | List | Parse query, resolve once, list, then reuse for every item. |
  | Describe | Resolve only after the sandbox lookup preserves 404. |
  | Connect | Parse timeout, resolve, then resume/update timeout. |
  | BrowserUse | Parse port and look up the sandbox, resolve and format, then proxy upstream. |

  Thus an invalid dynamic host cannot claim, clone, resume, update, or proxy a sandbox. The list parser was extracted only to keep query errors ahead of domain resolution.

## Configuration and TLS

- `--e2b-domain` defaults from `localhost` to empty; its help text documents dynamic behavior.
- The base deployment and configuration patch stop injecting the flag; the admin-key patch moves from argument index 7 to 6. The ingress patch is unchanged.
- `hack/generate-certificates.sh` accepts repeated `--domain` values and emits both each base domain and `*.domain` as deduplicated SANs. It strips one input trailing dot, rejects empty/wildcard input, validates a positive lifetime, retains `your.domain.com` when no domain is supplied, uses strict shell error handling and cleans temporary OpenSSL files.
- `--ca-key` and `--ca-cert` may reuse an existing signing CA only as a pair. The script verifies readable files, a valid certificate, `CA:TRUE`, certificate-signing key usage, sufficient remaining lifetime, and matching public keys; otherwise it generates a new CA. Output key/certificate permissions and the resulting subject/SANs are explicit.
- `docs/best-practices/cert-manager-multi-domain.yaml` demonstrates one CA-backed certificate covering multiple base and wildcard domains.

## Compatibility and Scope

- Existing explicit `--e2b-domain=<value>` deployments are unchanged. To retain the former default, set `--e2b-domain=localhost` explicitly; fresh standard deployments resolve dynamically.
- Customized `BrowserUse` changes intentionally from an unreachable native-style subdomain to its routable `/kruise/<sandboxID>/<port>` form.
- No domain is persisted in CRDs, annotations, labels, or runtime state. No CRD or data-plane routing behavior changes.
- Trusted forwarded-host handling is deferred because it requires an explicit proxy trust/allowlist model.

## Verification

Table-driven tests cover native/customized normalization, ports and IPv6, empty-host errors, exact static overrides, address formatting, adapter dispatch, and both websocket schemes. Handler tests additionally prove that an empty host neither claims a pooled sandbox nor sends a BrowserUse upstream request; builder tests reuse the real unified adapter and obsolete test-only adapter scaffolding is removed.
