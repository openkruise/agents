---
title: Sandbox Manager Network Interface Binding and Reliable Peer Discovery
authors:
  - "@AiRanthem"
reviewers: []
creation-date: 2026-08-24
last-updated: 2026-11-23
status: implementable
---

# Sandbox Manager Network Interface Binding and Reliable Peer Discovery

## Summary

Sandbox Manager accepts an optional `--network-interface` flag. When it is set,
one validated global-unicast address from that interface, preferring IPv4 over
IPv6, becomes the process-wide sandbox-cluster address for the control API,
peer route service, memberlist, and the local return entry used by non-Sandbox
proxy requests. When it is empty, the
existing non-hosted network behavior is preserved. A second optional flag,
`--disable-envoy-ext-proc`, skips the Envoy ext-proc gRPC listener on port
`9002` for deployments whose data plane does not consume ext-proc; the peer
route service starts regardless.

Peer discovery lists selector-matching Pods through a live, uncached Kubernetes
client built from the process `rest.Config`. Sandbox Manager and Sandbox Gateway
share that bounded listing contract: namespace and selector are required at
startup, each retry cycle performs at most one List while unjoined, and listing
stops after a successful join. After memberlist begins listening, seed
discovery and joining run in the background with retry. Shutdown stops new
discovery and retries; an active join returns under its network deadlines. One
lifecycle owner runs Leave and Shutdown exactly once. This keeps temporary peer
absence from blocking the control API without creating competing cleanup paths.

## Background

A hosted Sandbox Manager is a Sandbox Manager deployed outside the sandbox
cluster, meaning the cluster that runs sandbox orchestration. That Pod has both
a platform network and a sandbox-cluster network. Endpoints that serve or
advertise sandbox-cluster traffic must consistently use the sandbox-cluster
network; selecting different local addresses can make a process reachable
through one protocol but isolated through another.

Peer Pods also start and stop independently. A one-time Kubernetes list or a
single synchronous join turns ordinary startup ordering into permanent
isolation. Reliable discovery therefore retries a namespace- and
selector-scoped live Pod list until a join succeeds, then hands membership to
memberlist. That retry is independent of API readiness.

## Target Design

### One sandbox-cluster address

`--network-interface` has the following contract:

- An empty value preserves the established non-hosted behavior. The control
  API and peer route service listen on all local addresses. Memberlist uses
  `POD_IP` when present, otherwise the first non-loopback IPv4 address. The
  non-Sandbox proxy return entry remains on loopback.
- A non-empty value is an exact operating-system interface name. The interface
  must exist, be up, and have exactly one global-unicast address in the
  preferred family: IPv4 when the interface has any global-unicast IPv4
  address, otherwise IPv6. Missing, down, addressless, or multiple addresses
  in the preferred family make startup fail. The process never falls back to
  another interface.
- The address is resolved and validated once during startup. Address changes
  take effect after a process restart.
- The same address is used by the control API listener, the peer route listener
  on port `7789`, and memberlist's bind and advertised address. The non-Sandbox
  proxy return entry uses this address as well, so return traffic does not
  silently switch to loopback after the listeners move to the sandbox-cluster
  network.
- Interface selection controls local listeners and advertised peer identity. It
  does not use `SO_BINDTODEVICE`, select outbound routes, or change DNS policy.

The ext-proc listener on port `9002`, pprof, metrics, and other observability
listeners are outside this address contract. Their exposure is a separate
concern; only ext-proc enablement is covered below.

```mermaid
flowchart LR
    Flag["--network-interface"] --> Resolve[Resolve and validate one address]
    Resolve --> API[Control API]
    Resolve --> Route[Peer route :7789]
    Resolve --> Gossip[Memberlist bind and advertise]
    Resolve --> Return[Non-Sandbox proxy return entry]
```

### Optional ext-proc listener

`--disable-envoy-ext-proc` has the following contract:

- The default keeps the established behavior: the Envoy ext-proc gRPC server
  and its gRPC health service listen on port `9002` on all local addresses.
- When set, neither the port `9002` listener nor the gRPC server is created.
  The peer route service on port `7789`, route ingestion, and route
  distribution are unaffected, so the replica still participates in route
  synchronization.
- The flag does not change the ext-proc address or protocol. An Envoy that is
  configured to call ext-proc must not target a replica started with this
  flag; that pairing is a deployment configuration error.
- Both listeners bind synchronously during startup. A bind failure of either
  enabled listener fails startup before the process serves requests.

### Controller 主动路由同步

Sandbox controller 在 Sandbox status Patch 成功后，对比更新前后
`Route.CanRouteTraffic()`。仅当该布尔值发生变化时，向 memberlist 中的
manager/gateway 推送一次更新后的 route；调用内部继续使用既有的并行发送、
超时与重试策略。投影统一使用 `sandboxroute.RouteFromSandbox`，推送携带
API server 返回对象的 UID、resourceVersion 和完整路由属性，避免发送写入前的版本。
status merge patch 显式写入 `podInfo.podIP`（包括空字符串），防止 `omitempty`
在地址清空时仍保留已存储的旧 IP；其余 PodInfo 字段沿用现有序列化规则。

- 同步是尽力加速。Patch 失败、NotFound 或 status 未改变时不发送；路由投影
  或发送失败只记录错误，不回滚 status，也不以该错误重试 reconcile。
  manager/gateway 的 Sandbox informer 仍负责最终收敛。
- 触发条件不是 phase、IP 或整个 route 的任意变化。两个不同的非空 IP、
  message 或 resourceVersion 变化，只要 `CanRouteTraffic()` 不变就不推送。
  spec、Ready condition、池归属等仍由统一投影决定流量可路由性。
- controller 用 `sc-<pod-name>` 加入 memberlist，参与 TCP/UDP gossip，
  但不启动 `:7789/refresh`。所有成员的 `GetPeers()` 排除 `sc-` 前缀，
  `GetAllMembers()` 仍保留这些节点用于观察成员关系。controller 不加入
  manager/gateway 的 seed Pod labels。
- controller 仅提供 `--peer-selector`，默认为空，关闭功能且不启动 peer listener。
  启用后固定在 `sandbox-system` 发现 peers，memberlist TCP/UDP 端口固定为 7946。
  ACK 部署的 selector 为
  `agents.kruise.io/sandbox-manager-peer-finder=ack-sandbox-manager`。
  `POD_NAME` 决定节点名（未设置时使用 hostname），`POD_IP` 决定绑定地址。
- controller 新增独立 Pod informer，仅 watch `sandbox-system` 中符合 selector 的 Pods。
  它不复用可能开启 Sandbox Pod 标签过滤的主缓存，也不使用 APIReader List。
  不加载或 watch peer Secrets，不新增 Secrets RBAC 权限。
- controller 使用明文 memberlist 和 HTTP refresh，适用于未启用 peer 加密的部署。
  manager/gateway 既有的 gossip 加密和 peer mTLS 能力不变；启用这些安全能力的
  部署不能使用当前 controller 主动同步通道。
- manager 生命周期先启动并同步 peer cache，随后启动 controllers；发送回调
  等待 outbound client 发布，避免与组件启动竞态。退出时取消发现、
  完成 memberlist Leave/Shutdown，并关闭 idle HTTP connections。

发送实现 `SyncRouteWithPeers` 和 `PeerOutbound` 从 `pkg/proxy` 迁至中立的
`pkg/sandboxroute/refresh`。proxy 保留实例方法包装，gateway 与 controller
共享同一发送实现；本次新增同步链路的依赖闭包不包含 API 或 sandbox-manager。
已有 `sandbox_peers`、`sandbox_peer_outbound_tls_errors_total` 随实现迁移，
未新增指标名称；controller 进程也会注册这些既有指标。

部署编排需提供 `POD_NAME`/`POD_IP`，允许 controller 与 peers 双向 TCP/UDP 7946，
并允许 controller 向接收方发起 TCP 7789。启用功能前必须先更新接收方的
`sc-` 过滤逻辑；旧版本接收方会把 controller 误当作 refresh 目标。
`config/manager/manager.yaml` 保留可选启用参数示例，不自动启用该能力，
无需为 controller 添加 refresh Service，也不改变既有 probe 配置。

验证覆盖：布尔值双向变化、Ready/IP/phase 组合、布尔值不变、发送失败、
Patch 失败/NotFound、回传 resourceVersion、功能关闭、配置校验、startup 取消、
以及 memberlist 发送方过滤。普通开发验证不执行集群部署或 E2E。

### Bounded peer listing

The Sandbox Manager builder constructs an uncached controller-runtime client
from the same `rest.Config` already supplied to the process. It does not add a
Pod informer to the shared sandbox cache, and it does not list through
`APIReader`. Peer code receives that client as a reader; it does not import or
read the cache, and it does not read through Infra.

Listing is restricted by both of these required inputs:

- a non-empty system namespace; and
- a non-empty, syntactically valid peer label selector.

Invalid or absent scope is a startup error rather than an unfiltered Pod list.
Sandbox Manager also requires a non-nil `rest.Config` so assembly can build the
live client.

Sandbox Manager and Sandbox Gateway share this listing contract. Gateway builds
the same kind of live client from in-cluster configuration. While unjoined,
each retry cycle performs at most one live Pod list and stops listing after a
successful join. API availability therefore affects peer convergence, not
control-API or Gateway readiness.

### Trusted seed addresses

Each listed, selector-matching Pod can contribute at most one seed:

- Without a `memberlist-url` annotation, the seed is the Pod's `status.podIP`
  plus the configured memberlist port. IPv4 and IPv6 PodIPs are both eligible;
  IPv6 seeds use the bracketed host:port form.
- With the annotation, its host must be the same valid IP as
  `status.podIP`; only the port may differ. An annotation cannot redirect peer
  traffic to another Pod, tenant, or external address.
- Pods with an empty or invalid PodIP, invalid annotated address, the local
  memberlist address, or a duplicate address are excluded.
- Pods in the terminal `Succeeded` or `Failed` phase are excluded: no container
  remains to accept memberlist traffic, so each would only cost a dial timeout
  per retry cycle.

**Warning:** A Sandbox Manager started with `--network-interface` must not be a
memberlist seed. Seed addresses are always derived from `status.podIP`, which
can differ from the selected interface address that memberlist actually binds
and advertises. If the peer selector matches that Manager Pod, other members
join `status.podIP` and never reach the listener. The peer selector must not
select those Pods.

This constraint belongs to the deployment that owns the peer selector. Sandbox
Manager deliberately adds no in-process guard: the process knows only its own
flag value and bound address, while the unreachable join happens in other
members, so a local check could pass while the topology stays wrong. A
misconfigured selector therefore keeps this process starting normally and
surfaces as the affected members repeatedly logging join failures against the
hosted Manager's PodIP. Correcting it is an orchestration change to the peer
selector, not a code change.

Those Managers still join memberlist by contacting a seed that listens on its
PodIP, typically a Sandbox Gateway or a Sandbox Manager started without
`--network-interface`. After a successful join, gossip advertises the selected
interface address, so later members discover the hosted Manager that way
rather than through Kubernetes seed listing.

The Kubernetes seed set is therefore only the selector-matching Pods that
listen on `status.podIP`. Sandbox Gateway and non-hosted Sandbox Manager
replicas remain valid seeds. Hosted Managers may appear in memberlist
membership after they join, but they are not seed sources. The local member is
excluded. A Kubernetes Service and its mirrored or selected backends are
routing objects, not peer membership or seed sources.

Pod readiness and non-terminal phases do not filter the seed set. They are not
reliable membership signals: a newly starting peer may already accept
memberlist traffic, while memberlist itself owns ongoing liveness after a join.

Seeds are sorted into a stable order, and every seed is joined, one Join call
per seed, without stopping at the first success. Per-seed calls keep each
failure attributable, because a bulk Join discards errors once any seed
succeeds, and honor cancellation between dials.

### Non-blocking join lifecycle

The peer route listener on port `7789` starts before memberlist begins
listening, so a replica never advertises itself to peers before it can accept
their route refreshes. Memberlist then begins listening before seed discovery
starts, and Sandbox Manager runs the following lifecycle in the background:

1. List eligible peer Pods from the API server and derive trusted seed
   addresses.
2. Join every seed in stable order, one Join call per seed, without stopping
   at the first success.
3. Stop discovery after a cycle in which any join reports `joined > 0`.
4. If the list fails, no seeds exist, or every join fails, wait 10 seconds
   after the attempt finishes and retry.

Joining is not a readiness condition and does not delay control API startup. A
replica may temporarily operate as a single member; successful joining hands
ongoing membership changes to memberlist, so periodic Kubernetes rediscovery is
unnecessary. A replica whose selector never yields a seed, such as a
single-replica deployment, keeps listing once per retry interval for the life
of the process; the empty seed set is logged at the default verbosity on the
first cycle and at verbosity 2 afterwards, while list failures are logged as
errors on every cycle.

Kubernetes listing and the 10-second retry wait stop immediately when the peer
lifecycle context is canceled. Memberlist's join API has no context parameter,
so an already-started join returns under memberlist's existing network
deadlines rather than being interrupted by cancellation. Its default connection
and stream deadlines are each 10 seconds, and a complete push/pull can cross
more than one deadline window. No new join or retry starts after cancellation.

Known limit: there is no single 10-second upper bound for a complete in-flight
join. If shutdown must interrupt that work immediately or enforce one overall
deadline, the upgrade path is a context-aware memberlist transport that
preserves memberlist's dynamic-port and cleanup semantics. That transport is
not part of this proposal.

```mermaid
flowchart LR
    Listener[Memberlist listening] --> Worker[Background join worker]
    Worker --> List[List peer Pods from APIServer]
    List --> Seeds[Validate, deduplicate, and sort seeds]
    Seeds --> Join[Join one seed]
    Join -->|joined > 0| Done[Memberlist maintains membership]
    Join -->|all fail| Wait[Wait 10 seconds]
    List -->|error or empty| Wait
    Wait --> List
    Stop[Stop request or parent cancellation] --> Worker
    Worker -->|active join returned| Leave[Best-effort Leave]
    Leave --> Shutdown[Mandatory Shutdown while process is alive]
```

### Startup and shutdown boundary

Startup fails before serving requests when the selected interface, resolved
address, namespace, selector, peer client, control API listener,
peer route listener, or memberlist listener is invalid. The initial seed set
and join result are deliberately not startup requirements. This startup and
shutdown contract applies to the Sandbox Manager process. Sandbox Gateway
process lifecycle is outside this proposal.

Each peer instance establishes one lifecycle owner when memberlist starts. That
owner remains until cleanup finishes, including after seed discovery succeeds.
An explicit stop request or parent-context cancellation cancels the peer
lifecycle context; the owner then runs Leave and Shutdown exactly once. The
peer's Start is called exactly once and its Stop at most once, after Start has
returned, by the component that owns it; a second Start is rejected, and
concurrent or repeated Stop calls are outside the contract. A caller's wait
deadline does not transfer cleanup ownership or create a competing close path:
when Stop returns because its context expired, the owner still completes Leave
and Shutdown. Stop on a peer whose Start never succeeded is a no-op, so a
component can run its shutdown sequence after a failed Start without special
cases.

Sandbox cache synchronization completes before the process serves requests. A
termination signal that arrives while startup is still running follows the
crash model described below rather than the graceful Stop path: the process
exits immediately without waiting for startup to finish and without
per-component cleanup, so memberlist Leave and primary lease release are
skipped. Other members remove the stale entry through failure detection and
the lease expires through its TTL; both self-heal without operator action. A
signal that arrives once startup has completed, including one observed
together with the completion, runs the graceful Stop path. Running the
graceful chain for an interrupted startup is the upgrade path if graceful
cleanup during startup ever becomes a requirement.

The lifecycle owner prevents new discovery work, allows an in-flight join to
return under the network-deadline boundary described above, and then attempts
`Leave` before `Shutdown`. `Leave` is best effort and has a five-second limit;
its failure is reported but never prevents `Shutdown` while the process remains
alive. No code path calls `Leave` after `Shutdown`, and the join worker never
races a separate stop caller for ownership of memberlist cleanup.

If the process is forcibly terminated before the active join returns or the
leave message is delivered, other members temporarily retain a stale active
member and remove it through memberlist's normal failure detection. This is the
same cluster-level failure model as a process crash, OOM, node loss, or network
partition. Peer membership is not a quorum or authorization source. A stale
peer may add only bounded per-peer fanout delay or error: it must not roll back
local route state, change the outcome of an authoritative Sandbox mutation, or
prevent parallel synchronization to other live peers.

Failure detection removes an unreachable member from the surviving members'
view. It does not promise that two isolated memberlist partitions will merge
again without an external seed after connectivity returns; successful seed
discovery intentionally stops its Kubernetes retry loop.

### Compatibility and ownership

This proposal adds two optional CLI flags, `--network-interface` and
`--disable-envoy-ext-proc`. It does not change CRDs, HTTP models, Secrets, or
the memberlist wire protocol. Both in-cluster configuration and `KUBECONFIG`
continue to supply the sandbox-cluster Kubernetes client.

Process peer discovery and peer lifecycle belong to the Manager layer because
they coordinate Sandbox Manager processes rather than implement a Sandbox
backend. Interface address resolution is a policy-neutral helper in
`pkg/utils/network`. Command entrypoints only accept the flags, resolve the
interface address once, and assemble these capabilities; the Manager builder
constructs the live peer client from the supplied `rest.Config`. API protocol
behavior and Infra backend behavior remain unchanged.

Sandbox Gateway now requires non-empty `PEER_NAMESPACE` and
`PEER_LABEL_SELECTOR`; an empty value fails peer startup instead of listing
Pods across all namespaces. The reference manifest under
`config/sandbox-gateway` already sets both. Self-authored Gateway manifests
must set them before upgrading, and the release notes for the first release
containing this change must call this out.

The following are outside this proposal:

- memberlist encryption and peer-route mTLS;
- changing the address or port of the `9002` ext-proc listener;
- metrics, pprof, or observability listener isolation;
- Deployment, RBAC, KDM, or rollout configuration;
- Sandbox Gateway process lifecycle, including SIGTERM handling and peer-route
  listener bind sequencing;
- an informer refactor for Sandbox Gateway;
- address hot reload or periodic seed reconciliation.

No RBAC change is required for Sandbox Manager: its existing Pod permissions
already include `get`, `list`, and `watch`. Peer discovery treats listed Pods as
read-only objects and never mutates them.

Deployment changes remain outside this proposal, but hosted activation depends
on them. Kubernetes TCP probes without an explicit host target the Pod IP; when
the control API and peer route bind only to a different sandbox-cluster
address, those probes cannot reach the listeners and can restart the Pod.
Hosted rollout configuration must make the `8080` and `7789` probes reach the
selected sandbox-cluster address before enabling `--network-interface`.

Hosted activation also depends on seed topology. The peer selector must not
match Sandbox Manager Pods started with `--network-interface`. At least one
PodIP-reachable seed must remain in that selector, and the selected interface
must be able to reach that seed's memberlist address. New hosted replicas
cannot join if every such seed is gone, because Kubernetes listing stops after
the first successful join.

## Risks

- Live Pod listing during retry depends on APIServer availability. Namespace and
  label filtering bound each list to the peer Pods for one Sandbox Manager
  scope, and listing stops after the first successful join. A replica that
  never finds a seed keeps issuing one bounded list per retry interval.
- A canceled process can remain in a memberlist join across multiple 10-second
  network-deadline windows. An immediate-cancellation requirement has a defined
  transport-based upgrade path.
- Forced termination, or a termination signal during startup, can skip Leave
  and temporarily retain a stale member. Memberlist failure detection removes
  it; the stale entry may add bounded per-peer fanout failure but cannot change
  authoritative Sandbox state. A primary lease acquired during startup is held
  until its TTL expires.
- An interface address can change while the process is running. Resolving once
  avoids split listener identity; operational recovery is a restart.
- The API may become ready while the replica is still a single member. This is
  intentional: background retry repairs startup races without making peer
  availability a control-plane availability dependency.
- A hosted Manager whose Pod is still selected as a seed will advertise one
  address and be joined at another. Peer selector configuration must exclude
  those Pods.
- After the first successful join, a new hosted replica cannot join unless a
  PodIP-reachable seed is still available.

## Alternatives

- A shared Pod informer on the sandbox cache is rejected because seed discovery
  lists a handful of process Pods until join succeeds, then stops. A
  process-lifetime watch expands cache filters, startup registration, and tests,
  and makes memberlist wait on informer sync.
- Listing through the manager `APIReader` is rejected. That reader is the
  cache-bypass Get fallback, not a substitute for informers. Assembly constructs
  a dedicated uncached client from `rest.Config`, matching Gateway.
- Picking the first address from a multi-address interface is rejected because
  address ordering is not a stable network identity.
- Trusting an arbitrary `memberlist-url` host is rejected because Pod metadata
  must not redirect peer traffic outside the selected Pod identity.
- Publishing the selected interface address as a Kubernetes seed is rejected.
  Hosted Managers join through a PodIP-reachable seed and then rely on
  memberlist gossip; the peer selector must not list them as seeds.
- A custom cancellable memberlist transport is deferred because this scope
  accepts memberlist's existing network-deadline behavior, and a correct
  transport must preserve more than dialing alone.
- Continuous Kubernetes seed reconciliation after a successful join is
  rejected because memberlist already owns membership convergence.
