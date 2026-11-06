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

Add `requestMatches` to `spec.autoPausePolicy.resume.onIngressTraffic`. When
set, the sandbox-gateway wakes a paused sandbox only for requests that satisfy
at least one entry; an entry is satisfied when all of its header conditions
match. Other requests to the paused sandbox get the same `502
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

The same sandbox is usually reached by more than one kind of client. Browsers
send Fetch Metadata, while SDKs, CLIs, and IM webhooks do not. A single AND-ed
header list cannot say "a browser navigation, or a client that explicitly asks
to wake", so the rule accepts a list of alternative matches. This keeps the
mechanism independent of any particular agent harness: every web UI gets the
navigation rule, and every non-browser client opts in with a wake header.

### Goals

- Let a sandbox restrict which requests wake it, using request headers.
- Allow several kinds of client to wake the same sandbox.
- Keep the default unchanged: without `requestMatches`, every request wakes.

### Non-Goals

- Path, method, or port matching. The rule struct leaves room for them.
- Filtering traffic to a Running sandbox.
- Changing the E2B `autoResume` API; it keeps arming a rule without matchers.

## API

```go
type IngressTrafficRule struct {
    PauseTimeout *metav1.Duration `json:"pauseTimeout,omitempty"`

    // RequestMatches restricts which inbound requests may wake the paused
    // sandbox. Empty means every request wakes; otherwise a request must
    // satisfy any entry.
    // +optional
    // +listType=atomic
    // +kubebuilder:validation:MaxItems=8
    RequestMatches []IngressTrafficRequestMatch `json:"requestMatches,omitempty"`
}

type IngressTrafficRequestMatch struct {
    // Headers lists header conditions that must all match.
    // +kubebuilder:validation:MinItems=1
    // +kubebuilder:validation:MaxItems=8
    // +listType=map
    // +listMapKey=name
    Headers []IngressTrafficHeaderMatch `json:"headers"`
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

Example: wake on browser page navigation, or on any request carrying
`X-Sandbox-Wake: true` (for SDK, CLI, or webhook clients).

```yaml
spec:
  autoPausePolicy:
    resume:
      onIngressTraffic:
        pauseTimeout: 30m
        requestMatches:
        - headers:
          - name: sec-fetch-mode
            values: ["navigate"]
        - headers:
          - name: x-sandbox-wake
            values: ["true"]
```

Semantics: `requestMatches` entries are ORed, header conditions within one
entry are ANDed, and values within one condition are ORed. Exact value
matching is enough for Fetch Metadata and custom wake headers; prefix and regex
matching, or path and method conditions, can be added to
`IngressTrafficRequestMatch` later without breaking this shape.

### Validation

The CRD schema limits names to RFC 7230 token characters, which also rejects
pseudo-headers such as `:authority`. `autopause.ValidateAutoPausePolicy`, shared
by the SandboxSet and SandboxTemplate webhooks, the SandboxClaim controller, and
the Sandbox `ProbeValid` condition, additionally rejects:

- `requestMatches` entries without headers, which would match every request
  and silently disable the other entries,
- names that are not valid HTTP header names,
- names within one entry that differ only in case (the list-map key is
  case-sensitive, but the gateway matches names case-insensitively),
- header conditions without values.

## Gateway behavior

`shouldWakeSandbox` keeps its existing checks (Paused state, gateway switch,
waker, ObjectKey, UID fence, rule enabled). After they pass, it reads the
Sandbox from the informer cache and evaluates `requestMatches` against the
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
- **Non-browser clients.** CLI tools, SDKs, and webhooks usually do not send
  Fetch Metadata headers and cannot wake a sandbox restricted to `navigate`.
  Add a second entry matching a custom header that those clients send, or
  leave `requestMatches` empty.
- **Not an access control.** Non-browser clients can forge any header,
  including `Sec-Fetch-*`. The matches only filter well-behaved clients;
  authentication still decides who may reach the sandbox.
- **Service Workers.** A page whose Service Worker answers navigations from
  cache never sends the navigation to the gateway. Such applications must
  bypass the Service Worker for navigations, or match a different header.
- **Intermediaries.** Proxies between the browser and the gateway must forward
  `Sec-Fetch-*` headers unchanged.

## Compatibility

The field is optional and absent by default, so existing sandboxes and E2B
`autoResume` keep waking on every request. Older gateways ignore the field and
keep waking on every request; upgrade the gateway before relying on it.
