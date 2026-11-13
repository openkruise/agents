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

同一个 sandbox 通常同时接收浏览器、SDK、CLI 和 webhook 的请求。浏览器发送
Fetch Metadata，其他客户端通常不发送。因此提供两种配置方式：通过多个 OR
条目允许页面导航或携带自定义唤醒 header 的客户端；或者通过 `NotIn` 排除
浏览器后台请求，同时允许缺少该 header 的非浏览器客户端唤醒，无需修改客户端。
两种方式都不依赖特定 agent harness。

### Goals

- Let a sandbox restrict which requests wake it, using request headers.
- Allow several kinds of client to wake the same sandbox.
- 支持 `In` 和 `NotIn` header 集合匹配，默认 `In`。
- Keep the default unchanged: without `requestMatches`, every request wakes.

### Non-Goals

- Path, method, or port matching. The rule struct leaves room for them.
- Filtering traffic to a Running sandbox.
- `Exists` / `DoesNotExist` 操作符及全局否决规则。
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
    // Operator 默认为 In；NotIn 在所有 header 取值均未命中时匹配。
    // header 缺失时 In 不匹配，NotIn 匹配。
    // +optional
    // +kubebuilder:default=In
    Operator IngressTrafficHeaderMatchOperator `json:"operator,omitempty"`
    // Values 是区分大小写的精确匹配集合，In 和 NotIn 均要求非空。
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

另一个可直接使用的示例：排除指定的浏览器后台请求，同时允许页面导航和
不发送 Fetch Metadata 的 SDK、CLI、webhook 唤醒。

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

### 匹配语义

`requestMatches` 条目间为 OR，同一条目内的 header 条件为 AND。
header 名称不区分大小写，取值按区分大小写的精确比较处理。

| Operator | 匹配条件 | header 缺失 |
| --- | --- | --- |
| 省略或 `In` | 至少一个 header 取值属于 `values` | 不匹配 |
| `NotIn` | 没有任何 header 取值属于 `values` | 匹配 |

重复 header 的所有取值均参与判断：即使一个取值未命中，只要另一个取值命中，
`NotIn` 就不匹配。无效的空 `values` 或未识别的 operator 在运行时不匹配。

`NotIn` 仅是所在条目的条件，不是全局否决。如果另一个 OR 条目匹配，请求仍可
唤醒。若希望所有唤醒入口都排除某类请求，需要在每个条目中加入相应条件。

当前仅支持 `In` / `NotIn`。未来增加 `Exists` / `DoesNotExist` 时需要同步放宽
`values` 的必填校验并要求其为空；旧 gateway 遇到未知 operator 不匹配该条件，
但不影响其他 OR 条目的求值。

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
- 非空且不属于 `In` / `NotIn` 的 operator；省略 operator 按 `In` 处理。

CRD 为 operator 设置 `In` 默认值，并通过枚举约束合法取值。

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
  可以增加匹配自定义唤醒 header 的 OR 条目，改用上述 `NotIn` 配置，或不设置
  `requestMatches`。
- **缺失 Fetch Metadata。** `NotIn` 允许缺少该 header 的请求唤醒。
  浏览器在不安全的 HTTP origin 上通常不发送 `Sec-Fetch-*`；若中间代理移除该
  header，后台请求同样可能唤醒。此规则是减少唤醒噪声的配置，不是访问控制。
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

不带 operator 的已有 `requestMatches` 按 `In` 处理，行为不变。
`requestMatches` 与 operator 应一起发布；使用 `NotIn` 前必须确认 gateway 支持
operator。只支持旧版 `requestMatches` 的预发布 gateway 会忽略 operator，将
`NotIn` 错当成 `In`，因此不能与新配置混用。支持 operator 的 gateway 对未来
未知操作符仅判定对应条件不匹配，不会把它误解释为 `In`。
