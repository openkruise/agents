---
title: Request Header Matching for Wake-on-Traffic
authors:
  - "@zmberg"
reviewers: []
creation-date: 2026-11-03
last-updated: 2026-11-03
status: implementable
see-also:
  - "/docs/proposals/20260627-wake-on-traffic-via-spec-patch.md"
  - "/docs/proposals/20260626-sandbox-auto-pause.md"
---

# Request Header Matching for Wake-on-Traffic

## Summary

Add `requestHeaders` to `spec.autoPausePolicy.resume.onIngressTraffic`. When
set, the sandbox-gateway wakes a paused sandbox only for requests whose headers
match every entry. Other requests to the paused sandbox get the same `502
sandbox_not_running` reply as a sandbox without wake-on-traffic, and the
sandbox stays paused. Requests to a Running sandbox are never filtered.

## Motivation

Wake-on-traffic currently wakes a paused sandbox on any authenticated request.
Web applications served from a sandbox often keep a browser tab connected to
it. When the sandbox pauses, the page detects the dead connection and
reconnects automatically, without any user action. For example, the OpenClaw
control UI forces a WebSocket reconnect when no message arrives for two tick
intervals (60s), and also reconnects when the tab returns to the foreground.
Each reconnect wakes the sandbox, so a tab left open produces a
pause-then-wake cycle about every minute, and auto-pause never saves resources.

The gateway cannot tell a reconnect from a first connection by the protocol
alone: both are WebSocket upgrades. Browsers, however, attach Fetch Metadata
request headers that page scripts cannot set or change. `Sec-Fetch-Mode` is
`navigate` for a page load or reload, and `websocket` / `cors` / `same-origin`
for script-initiated requests. Matching on it lets the sandbox owner choose
"wake only when the user opens or reloads the page".

### Goals

- Let a sandbox restrict which requests wake it, using request headers.
- Keep the default unchanged: without `requestHeaders`, every request wakes.

### Non-Goals

- Path, method, or port matching. The rule struct leaves room for them.
- Filtering traffic to a Running sandbox.
- Changing the E2B `autoResume` API; it keeps arming a rule without matchers.

## API

```go
type IngressTrafficRule struct {
    PauseTimeout *metav1.Duration `json:"pauseTimeout,omitempty"`

    // RequestHeaders restricts which inbound requests may wake the paused
    // sandbox. Empty means every request wakes; otherwise a request must
    // match every entry.
    // +optional
    // +listType=map
    // +listMapKey=name
    // +kubebuilder:validation:MaxItems=8
    RequestHeaders []IngressTrafficHeaderMatch `json:"requestHeaders,omitempty"`
}

type IngressTrafficHeaderMatch struct {
    // Name is matched case-insensitively. Pseudo-headers are not supported.
    Name string `json:"name"`
    // Values lists accepted values, compared exactly. The entry matches when
    // any occurrence of the header equals one of them; a missing header
    // does not match.
    Values []string `json:"values"`
}
```

Example: wake only on browser page navigation.

```yaml
spec:
  autoPausePolicy:
    resume:
      onIngressTraffic:
        pauseTimeout: 30m
        requestHeaders:
        - name: sec-fetch-mode
          values: ["navigate"]
```

Semantics: entries are ANDed, values within one entry are ORed. Exact value
matching is enough for Fetch Metadata and custom wake headers; prefix and regex
matching can be added later without breaking this shape.

### Validation

The CRD schema limits names to RFC 7230 token characters, which also rejects
pseudo-headers such as `:authority`. `autopause.ValidateAutoPausePolicy`, shared
by the SandboxSet and SandboxTemplate webhooks, the SandboxClaim controller, and
the Sandbox `ProbeValid` condition, additionally rejects:

- names that are not valid HTTP header names,
- names that differ only in case (the list-map key is case-sensitive, but the
  gateway matches names case-insensitively),
- entries without values.

## Gateway behavior

`shouldWakeSandbox` keeps its existing checks (Paused state, gateway switch,
waker, ObjectKey, UID fence, rule enabled). After they pass, it reads the
Sandbox from the informer cache and evaluates `requestHeaders` against the
request header map. A mismatch falls through to the existing non-Running
reply.

The matchers are not projected into `sandboxroute.Route`. The route registry is
synchronized to peer gateways, and adding the matchers there would grow every
route and its peer protocol for a check that runs only on the paused path. The
informer read already happens on that path for the UID fence, so this adds no
new dependency. If the Sandbox cannot be read, the request does not wake it.

## Risks and limitations

- **Open pages need a manual reload.** With `sec-fetch-mode: navigate`, an open
  tab does not wake the sandbox by itself; the user reloads the page. This is
  the intended trade-off and is opt-in.
- **Non-browser clients.** CLI tools and SDKs usually do not send Fetch
  Metadata headers and cannot wake a sandbox restricted to `navigate`. Use a
  custom header that those clients send instead, or leave `requestHeaders`
  empty.
- **Service Workers.** A page whose Service Worker answers navigations from
  cache never sends the navigation to the gateway. Such applications must
  bypass the Service Worker for navigations, or match a different header.
- **Intermediaries.** Proxies between the browser and the gateway must forward
  `Sec-Fetch-*` headers unchanged.

## Compatibility

The field is optional and absent by default, so existing sandboxes and E2B
`autoResume` keep waking on every request. Older gateways ignore the field and
keep waking on every request; upgrade the gateway before relying on it.
