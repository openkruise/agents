---
title: Request Header Matching for Wake-on-Traffic
authors:
  - "@zmberg"
reviewers: []
creation-date: 2026-11-03
last-updated: 2026-11-13
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

The same sandbox commonly receives requests from browsers, SDKs, CLIs, and
webhooks. Browsers send Fetch Metadata headers; other clients usually do not.
Two configuration approaches are therefore available: use multiple OR entries
to allow page navigations or clients carrying a custom wake header, or use
`NotIn` to exclude browser background requests while allowing non-browser
clients without that header to wake the sandbox without client changes.
Neither approach depends on a specific agent harness.

### Goals

- Let a sandbox restrict which requests wake it, using request headers.
- Allow several kinds of client to wake the same sandbox.
- Support `In` and `NotIn` header set matching, defaulting to `In`.
- Keep the default unchanged: without `requestMatches`, every request wakes.

### Non-Goals

- Path, method, or port matching. The rule struct leaves room for them.
- Filtering traffic to a Running sandbox.
- `Exists` / `DoesNotExist` operators and global veto rules.
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
    // Operator defaults to In; NotIn matches when no header value is in Values.
    // An absent header does not match In, but matches NotIn.
    // +optional
    // +kubebuilder:default=In
    Operator IngressTrafficHeaderMatchOperator `json:"operator,omitempty"`
    // Values is an exact, case-sensitive set; In and NotIn both require it to be non-empty.
    Values []string `json:"values"`
}

// +kubebuilder:validation:Enum=In;NotIn
type IngressTrafficHeaderMatchOperator string

const (
    IngressTrafficHeaderMatchIn IngressTrafficHeaderMatchOperator = "In"
    IngressTrafficHeaderMatchNotIn IngressTrafficHeaderMatchOperator = "NotIn"
)
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

Another ready-to-use example excludes the specified browser background requests
while allowing page navigations and SDKs, CLIs, or webhooks without Fetch
Metadata headers to wake the sandbox.

```yaml
spec:
  autoPausePolicy:
    resume:
      onIngressTraffic:
        pauseTimeout: 30m
        requestMatches:
        - headers:
          - name: sec-fetch-mode
            operator: NotIn
            values: [websocket, cors, no-cors, same-origin]
```

### Matching semantics

`requestMatches` entries are OR-ed; header conditions within an entry are AND-ed.
Header names are case-insensitive; values use exact, case-sensitive comparison.

| Operator | Match condition | Header absent |
| --- | --- | --- |
| Omitted or `In` | At least one header value belongs to `values` | Does not match |
| `NotIn` | No header value belongs to `values` | Matches |

All values of a repeated header participate in matching: even if one value is
outside the set, `NotIn` does not match if another value is in the set.
Invalid empty `values` or unknown operators do not match at runtime.

`NotIn` is a condition within its entry, not a global veto. If another OR entry
matches, the request can still wake the sandbox. To exclude a request category
from every wake path, add the corresponding condition to every entry.

Only `In` / `NotIn` are currently supported. Adding `Exists` / `DoesNotExist` in
the future would require relaxing the validation that makes `values` required
and requiring it to be empty for those operators. An older gateway treats an
unknown operator as a non-matching condition without affecting the evaluation
of other OR entries.

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
- header conditions without values,
- non-empty operators other than `In` / `NotIn`; an omitted operator is treated
  as `In`.

The CRD defaults the operator to `In` and restricts valid values through an enum.

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
  Add an OR entry matching a custom wake header, use the `NotIn` configuration
  above, or leave `requestMatches` unset.
- **Missing Fetch Metadata.** `NotIn` allows requests without the header to wake
  the sandbox. Browsers usually omit `Sec-Fetch-*` on insecure HTTP origins;
  background requests can also wake the sandbox if an intermediary removes the
  header. This rule reduces wake noise; it is not an access control.
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

Existing `requestMatches` without an operator are treated as `In`, preserving
behavior. `requestMatches` and operator support should ship together; verify
that the gateway supports operators before using `NotIn`. Prerelease gateways
that support only the earlier `requestMatches` implementation ignore the
operator and incorrectly treat `NotIn` as `In`, so they must not be mixed with
the new configuration. Gateways with operator support treat future unknown
operators as non-matching conditions rather than interpreting them as `In`.
