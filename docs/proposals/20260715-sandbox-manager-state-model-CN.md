---
title: Sandbox-manager 状态模型与分层
authors:
  - "@AiRanthem"
reviewers: []
creation-date: 2026-07-15
last-updated: 2026-08-21
status: provisional
---

# Sandbox-manager 状态模型与分层

## 摘要

**范围纪律：**本提案不解决 sandbox-manager 副本之间的 State、Route 或 cache 同步。所有健康
副本继续独立处理请求，现有 primary Lease 只用于后台协调。副本间 observation 可以暂时不一致；
本提案不得让这一既有限制变得更坏，尤其不能让 stale observation 修改其他 delivery、释放其 quota
或删除其 Route。副本状态同步由后续独立设计解决。

本提案为 sandbox-manager 定义一套后端中立的结构化 State。Sandbox CR 专用映射器统一解释原生
Sandbox 事实。Infra 将得到的 State 和 Observation 返回给 Manager；Manager 不读取 Sandbox CR
的 Phase、Condition 或 reason，只依据中立事实执行生命周期和能力策略。API 认证调用方、校验
调用方与 owner 的授权关系，并把领域结果映射为公开协议。

State 包含四个互相独立的维度：

    State {
        Claimed     bool
        Release     none | due | terminal | committed
        PauseResume none | pausing | resuming
        Workload    provisioning | ready | paused | unready | completed
    }

`Release` 将可逆的截止时间、已经停止的工作负载和不可逆的释放提交分开。这些事实分别具有不同的
Kill、Route、可见性和配额语义。对象不存在是独立的 NotFound 结果，不伪造成 State。

所有依赖状态的操作都使用同一个 Observation：

    Observation {
        State         State
        Owner         string
        MutationToken opaque
    }

现有持久化 claim lock UUID 是一次 claim delivery 中不可变的 DeliveryEpoch。token 将 observation
绑定到该 DeliveryEpoch 和一个后端对象版本。token 失效后，Infra 不得针对新快照透明重试操作。
这样可以防止为一个 owner 授权的操作跨过 recycle 边界，落到同一 Sandbox CR 的下一名 owner 上。

`ShutdownTime` 是交给 Controller 的指令，不是立即生效的流量或配额边界。本地时间到达后产生
`Release=due`，但在 Controller 持久化不可逆释放事实前，Sandbox 仍对 owner 可见，Route 仍由
工作负载能力决定。due 期间只准入 List、Describe 和 Kill；其他 owner 能力不能救回 deadline。
Route wire 不新增 deadline。

claim 持久化仍然是可见性提交。因此，同一 owner 可能在 Create 或 Clone 返回前发现 Sandbox；
本提案明确不为这个请求处理窗口提供隔离保证。成功响应仍然只能在所有交付工作完成后返回。

本提案保持 provisional。它定义单次 observation 内的安全性和 delivery fence，不提供多副本
一致性，也不承诺线性一致的公开响应。

## 背景

Sandbox 状态影响池候选选择、Create 和 Clone 完成、Pause 和 Resume、流量转发、owner 可见性、
配额和释放。当不同路径分别解释 Sandbox CR 的 Phase、Condition、annotation 或本地时间时，
同一个后端事实可能产生不同的业务结果。

扁平生命周期字符串也会混淆不能互换的问题。仍在启动的池 Sandbox、暂时 unready 的已认领
Sandbox、已经停止但等待清理的工作负载、已经到达的截止时间以及已接受的删除，都不能安全地
压缩成同一个 `dead` 或 `releasing` 值。

Recycle 使 observation 边界更加重要。成功 recycle 会保留同一个 Sandbox CR 和 UID，清除旧
claim，之后允许新 owner 再次认领。如果后端 mutation 可以在静默重试时落到新 claim，那么操作前
检查 owner 和 State 并不足够。

State 不会在 Sandbox CR 旁边创建第二套后端生命周期。它建立的是依赖边界：Sandbox CR Infra
统一解释原生事实，Manager 和 API 只依赖中立含义与条件能力契约。

### 目标

- 分开表示 release due、终态工作负载和不可逆 release commit。
- Manager 和 API 的业务判断不读取 Sandbox CR Phase、Condition、annotation 或 reason。
- 所有依赖状态的能力都必须绑定到已经授权的 claim delivery。
- Infra observation 和 Route 投影共用同一个 CR mapper。
- 将工作负载能力与 owner 可见性、quota release 分开。
- 精确定义安全、类型化的 provisioning 和 ready 谓词。
- 防止 stale 的已授权操作影响另一轮 claim delivery。

### 非目标

- 不引入 SandboxRecord 或其他权威可见性存储。
- 不保证在 Sandbox CR 实际不存在时仍能恢复可见性。
- 不把 `ShutdownTime` 变成严格的墙钟流量截止点，也不向 Route 增加 deadline。
- 不在 claim 持久化到 Create 或 Clone 响应之间隔离 owner 与 Sandbox。
- 不把 Claim 或 Clone 变成可从任意 Manager 进程崩溃中恢复的事务。
- 不协调 Manager 副本之间的 observation，也不同步 Manager 与 Gateway 的 informer state；不引入
  持久化 Sandbox ID resolver 或业务 API primary gate。
- 除 claim delivery fencing 外，不改变 Checkpoint API 或 lifecycle 语义；只有创建、生产、完成和
  消费与一次 claim delivery 的绑定属于本提案范围。
- 除实现注意事项中的 release-safety 前置条件外，不描述迁移、发布、实现任务或测试过程。

## 设计终态

### 1. 总体边界

依赖方向固定为：

    Sandbox CR
        |
        | pkg/sandboxstate/sandboxcr mapper
        v
    pkg/sandboxstate State + Observation
        |                                   |
        | Infra observation/capability      | sandboxroute projection
        v                                   v
    Manager business policy -> API     manager and gateway Route stores
                                                |
                                                v
                                      gateway traffic admission

`pkg/sandboxstate` 只定义后端中立的 State、Observation、enum 校验和 opaque token 契约，不依赖
Sandbox API。`pkg/sandboxstate/sandboxcr` 负责 Sandbox CR 映射，可以依赖 Sandbox API。
`infra/sandboxcr` 和 `sandboxroute` 共同使用 CR mapper；Manager 和中立 Infra 接口只依赖中立包。

desired workload revision 是 `spec.template` 或 `spec.templateRef` 的 policy-neutral hash。Controller
和 CR mapper 使用同一个中立 revision 函数，不能因此形成 Controller 到 Manager 的反向依赖。

mapper 显式接收时钟：

    FromSandbox(sandbox, now) -> State

本地时间可能使 `Release` 在 `none` 与 `due` 之间变化。Route 投影明确忽略这个差异，因此即使
Manager 和 Gateway 的时钟不同，相同的持久化 CR 事实仍然产生相同 Route。

Sandbox Controller 继续负责原生 status 和工作负载协调。它同时负责 cleanup trigger 的不可逆
含义：某个 claim 一旦持久化该 trigger，Controller 必须 recycle 或 delete 该 claim，绝不能让它
恢复服务。

所有健康 Manager 副本可以继续处理 owner 业务 API。primary Lease 只控制后台 reconcile 任务，
不是 API admission 或 readiness gate。副本之间的 Route 和 informer 差异保持现有 eventual 行为，
不能成为可见性、authorization 或 absence 的权威。

### 2. State 和 Observation 契约

| 字段 | 取值 | 回答的问题 |
|---|---|---|
| Claimed | true / false | 本次 delivery 的用户归属是否已经持久化？ |
| Release | none / due / terminal / committed | 当前观察到了哪一种释放事实？ |
| PauseResume | none / pausing / resuming | 是否正在执行普通 Pause 或 Resume 转换？ |
| Workload | provisioning / ready / paused / unready / completed | 当前能够证明什么工作负载能力？ |

mapper 将不完整输入和 stale 的正向状态转换成有效、保守的 State，通常是 Workload=unready。未知
的输出 State enum 或非法 State 组合会使校验失败；Infra 返回中立的 internal 或 unavailable 错误，
不能把它伪装成 invisible 或 NotFound。只有通过校验的 State 才能进入 Manager 策略；Route 无法
得到有效 State 时按 dead 失败关闭。NotFound 始终位于 State 之外。

#### Observation 和 MutationToken

Infra lookup 从同一个快照返回 State、中立 owner identity 和一个 opaque MutationToken。token 至少
绑定：

- Sandbox UID 和后端对象版本；
- claimed marker 和 owner；
- DeliveryEpoch，也就是当前持久化的 claim lock UUID；
- 本次 delivery 的公开 Sandbox ID。

DeliveryEpoch 不是 secret；它与 owner 和 claimed identity 原子持久化，在一次 claim 内永不旋转，
并在成功 recycle 时清除。本提案不新增 Sandbox epoch 字段。

只有 owner、DeliveryEpoch 和可唯一解析的公开 Sandbox ID 均非空，且能够构造 MutationToken 时，
claimed observation 才有效。claimed identity 非法时返回中立 internal 或 unavailable，Route 失败
关闭；不能表现为 NotFound、owner mismatch 或 running Route。

Pause、Resume、Connect、Snapshot、Set timeout、Update network、Browser use、创建 checkpoint
以及 release commit 都必须携带该 token。它既约束 CR 写入，也约束工作负载访问。

Infra 在每项能力真正生效的后端边界校验 token：

- Sandbox CR 写入校验观察到的 resourceVersion、claimed marker、owner、公开 ID 和
  DeliveryEpoch。前置条件失败时返回 conflict，不能针对新 claim 重试。
- Browser 请求携带 delivery-scoped runtime credential，由 runtime 拒绝其他 DeliveryEpoch 的
  credential；没有可验证 credential 时 Browser 不准入。
- TrafficPolicy 及其选中的 Pod identity 携带 DeliveryEpoch；创建、更新、读取和删除都同时匹配
  Sandbox ID 与 DeliveryEpoch。
- Route 携带 DeliveryEpoch，并拒绝来自其他 delivery 的 stale event。
- Checkpoint 及其 SandboxTemplate 携带 DeliveryEpoch。producer 在执行前和完成前都校验 epoch；
  consumer 拒绝其他 delivery 的结果。
- Connect 在 Resume 后取得新的 Observation，只有 DeliveryEpoch 未变时才继续。

能力边界无法证明 DeliveryEpoch 匹配时，必须返回 conflict 或 unavailable，不能退回到只匹配
Sandbox name、UID、公开 ID 或 owner reference。

这些 fence 防止旧操作通过复用名称、legacy Sandbox ID 或相同 Sandbox UID 到达后续 delivery。
但 traffic auth 未启用且公开 Sandbox ID 被复用时，它们无法区分已经由 Gateway 转发的外部请求；
该客户端流量限制不在本提案范围内。

校验失败时，Infra 返回冲突，不得针对新快照重试业务 mutation。Manager 取得新的 Observation，
重新执行生命周期准入；API 随后重新校验调用方与 owner 的授权关系。只有刷新后的 Observation
仍属于同一个已授权 claim 且仍满足准入时，Manager 才能重试。

#### Claimed

只有持久化 sandbox-claimed marker 明确为 true 时 Claimed 才为 true；缺失、false 或无法识别的
值均映射为 false。

对于池候选，Infra 原子持久化 lock、owner、Sandbox ID 和 claimed=true；新建 Sandbox 的 create
写入直接包含这些事实。该写入是唯一的 claim commit，不表示 Create 或 Clone 已经返回，也不表示
交付工作已经完成。

成功 recycle 先持久化清除 claim-scoped 的 SandboxPaused、SandboxResumed 和 RuntimeInitialized
Condition，然后通过最终 claim-clear 写入原子清除旧 claim、owner、lock、公开 Sandbox ID 和
cleanup trigger。在该最终写入完成前，对象不能成为候选。即使 CR UID 不变，后续 claim 也必须
得到不同的 DeliveryEpoch 和 MutationToken。

#### Release

mapper 按第一条匹配规则映射：

1. DeletionTimestamp 存在、cleanup trigger 已持久化，或者 Phase 为 Recycling 或 Terminating：
   `committed`；
2. Phase 为 Succeeded 或 Failed：`terminal`；
3. ShutdownTime 存在且 `now.After(ShutdownTime)` 为 true：`due`；
4. 其他情况：`none`。

恰好等于 deadline 时尚未 due。授予正向能力前必须满足 typed serving freshness，但 status 已经
报告 terminal、Recycling 或 Terminating 时仍按保守事实处理，不能据此恢复服务。

`due` 是可逆状态。Controller 的 paused-retention 策略可以在删除提交前持久化更晚的
ShutdownTime，新的 observation 随后返回 `none`。只要仍观察到 `due`，就只准入 List、Describe
和 Kill。Pause、Resume、Connect、Snapshot、Set timeout、Update network 和 Browser use 都不能
推进 deadline 或以其他方式救回 Sandbox。Controller 只能为其观察到 deadline 的 DeliveryEpoch
条件提交 timeout release。在该 commit 持久化前，owner 可见性、公开状态、流量和 quota 仍由其他
State 维度决定。

`terminal` 只表示工作负载已经停止，不表示清理已经提交。它会隐藏 Sandbox 并拒绝流量，但 Kill
仍须提交释放，quota 继续保留。

`committed` 对一次 claim delivery 单调。cleanup trigger 一旦持久化就算 committed，因为
Controller 满足以下契约：

- 可 recycle 的 Sandbox 进入 recycle；
- paused、使用 PVC 或其他不能 recycle 的 Sandbox 转为删除；
- recycle 失败时，只能把对象保留为隐藏的 committed cleanup，并最终删除；
- 任何拒绝或失败路径都不能清除 release fact 并让同一 claim 恢复服务；
- 只有 recycle 成功时，才能在清除所有旧 claim identity 的同时清除 trigger。

因此，cleanup-trigger 写入成功已经足以让 Kill 成功并释放 quota；Manager 不需要等待第二次
recycle acknowledgement。

各项 Release 策略互相独立：

| Observation | Owner 可见性 | Kill | Quota | 流量 |
|---|---|---|---|---|
| Release=none | 根据其他 State 判断 | 提交释放 | 保留 | 根据能力判断 |
| Release=due | 根据其他 State 判断 | 提交释放 | 保留 | 根据能力判断 |
| Release=terminal | 隐藏 | 提交清理 | 保留 | 拒绝 |
| Release=committed | 隐藏 | 幂等成功 | 可释放 | 拒绝 |
| NotFound | 隐藏 | 幂等成功 | 收敛释放 | 无 Route |

#### PauseResume

PauseResume 根据类型化的 desired 和 observed pause 事实判断，不使用整个对象的 generation。
因此，只更新 timeout spec 不会抹掉进行中的 Pause 或 Resume。Claimed=false 始终映射为 `none`，
即使 recycle 后仍意外残留 claim-scoped transition Condition。

准确的 Condition Type 是 `SandboxPaused`（`SandboxConditionPaused`）、`SandboxResumed`
（`SandboxConditionResumed`）和 `RuntimeInitialized`。

按第一条匹配规则映射：

1. Claimed=false；Release 为 terminal 或 committed；或者 Phase 为 Succeeded、Failed 或
   Upgrading：`none`。
2. Phase 为 Resuming；或者 Phase 为 Paused、SandboxPaused=True 且 spec.paused=false；或者 Phase
   为 Running、SandboxResumed=True 且 RuntimeInitialized 缺失或不为 True：`resuming`。
3. Phase 为 Running 且 spec.paused=true；或者 Phase 为 Paused 且 SandboxPaused 缺失或不为 True：
   `pausing`。
4. 其他情况：`none`。

Controller 在 upgrade 中执行的内部唤醒不属于普通 Resume。

#### Workload

Workload freshness 只覆盖影响服务的事实。mapper 根据 `spec.template` 或 `spec.templateRef` 计算
desired workload revision，并与 `Status.UpdateRevision` 比较。PauseTime 和 ShutdownTime 变化不会
改变该 revision。随后按第一条匹配规则映射：

1. Phase 为 Succeeded 或 Failed：`completed`。
2. Phase 为 Paused 且 SandboxPaused=True：`paused`。
3. Phase 为 Running、desired revision 等于 `Status.UpdateRevision`、Ready=True、
   `Status.PodInfo.PodIP` 非空，并且 InplaceUpdate 不存在或具有已知安全结果：`ready`。
4. Phase 为 Pending 且 desired revision 等于 `Status.UpdateRevision`：`provisioning`。
5. 其他所有输入：`unready`。

对于 InplaceUpdate，不存在、True/Succeeded 和 False/Failed 都是 serving-safe 结果；
False/InplaceUpdating、Unknown 以及未知或非法的 status/reason 组合映射为 unready。False/Failed
表示 desired update 没有收敛，但不覆盖服务能力：如果 Ready=True 且满足其他所有 ready 谓词，
现有 workload 仍然是 ready。update convergence 与 serving capability 是两类独立事实。

Ready 缺失、False 或 Unknown 均不等于 ready。revision 不匹配、缺少 PodInfo.PodIP、Upgrading、
未知的原生 Sandbox CR Phase 或不完整 State 都映射为 unready，除非命中优先级更高的保守终态规则。
Resume 后 RuntimeInitialized 缺失、False 或 Failed 时，如果 Ready=True，并不改写 Workload；它会
保持 PauseResume=resuming，并独立阻断 Route 和能力准入。InplaceUpdate failure 绝不表示
provisioning。

SandboxSet owner 不能证明 provisioning。尤其是 SandboxSet-controlled Running+Ready!=True 一律
映射为 unready。CR CreationTimestamp 只能在 Workload 已经满足 provisioning 后作为 speculation
age 的辅助阈值；age 本身不能证明 provisioning。

后端 reason 可以保留在 Infra 诊断中，但 Manager 和 API 不得据此分支。未来后端可以把其他显式、
类型化的进度事实映射为 provisioning，但必须在自己的 mapper 中定义该事实。

#### 有效组合

| State 组合 | 含义 |
|---|---|
| Claimed=false, Release=none, Workload=ready | 池中可用的 Sandbox |
| Claimed=true, Release=none, Workload=ready | 正常用户 Sandbox |
| Claimed=true, Release=due, Workload=ready | deadline 已到，Controller 尚未提交释放 |
| Claimed=true, Release=terminal, Workload=completed | 工作负载已停止，仍需清理 |
| Claimed=true, Release=committed, Workload=ready | 工作负载停止前已经提交释放 |
| Claimed=false, Release=committed | 未认领对象正在删除或 recycle |

Claimed=false 本身不能使对象成为候选。Release 必须为 none，对象必须属于目标池、未锁定，并满足
Workload 候选策略。

### 3. 分层职责

| 层 | 负责 | 不负责 |
|---|---|---|
| API | authentication、调用方与 owner 的 authorization、协议校验、HTTP 映射、E2B 投影 | CR status 解释、用 Route 判断存在性、生命周期策略 |
| Manager | 资源可见性、候选策略、配额、生命周期和能力准入、冲突后的编排 | CR Phase、Condition、annotation、HTTP 语义 |
| Infra | Observation、CR 映射、条件后端能力、等待、冲突、claim fence | 调用方认证、HTTP 状态、配额策略 |
| Controller | 原生 CR 和工作负载协调、cleanup trigger 的不可逆结果 | Manager 或 API 策略、依赖 Manager 或 Infra 实现 |

API 获得经过认证的 owner identity，并将它作为显式查询或选择条件。Manager 根据中立 State 推导
资源可见性。API 比较 Observation.Owner 与已认证调用方，并把对象不存在、不可见和 owner mismatch
在非 Kill API 上统一映射为公开 404；Kill 使用独立的统一 HTTP 204 契约。这样既让 authentication
和 authorization 留在 API，又让后端中立的可见性策略留在 Manager。

Route 永远不是 Sandbox 存在性或 owner authorization 的权威。missing、stale 或 non-running Route
都不能在权威 Observation 之前拒绝请求。

List 在 pagination 前应用 authenticated owner 和资源可见性过滤。Describe 和所有单 Sandbox API
使用相同的 observation 与 authorization 边界。

Manager 副本继续独立处理 owner 业务 API。primary Lease 不是 API admission 或 readiness gate。
本提案既不同步各副本的 observation，也不承诺副本间响应一致；安全保证从某个副本取得
Observation 后开始。

### 4. Claim 和 Clone

Manager 使用以下候选规则：

| 候选 | 必要事实 |
|---|---|
| 普通候选 | Claimed=false、Release=none、PauseResume=none、Workload=ready、属于目标池且未锁定 |
| 投机候选 | Claimed=false、Release=none、PauseResume=none、Workload=provisioning、属于目标池、未锁定且 speculation age 已到 |
| 不可选 | Claimed=true；Release 为 due、terminal 或 committed；Workload 为 paused、unready 或 completed；池不匹配；或已锁定 |

只有 typed provisioning 谓词已经成立后，CreationTimestamp 才能作为等待时长阈值。配置时长为零时
禁用 speculative selection。

Claim 和 Clone 满足以下契约：

1. Manager 校验请求并预留 quota。
2. Infra 原子持久化或创建 owner、lock、公开 Sandbox ID 和 claimed=true。
3. Manager 等待 Workload=ready，并完成 runtime 初始化、credential、CSI、network 和其他必要交付
   能力。
4. 所有交付要求成功后，Create 或 Clone 才返回成功。

第 2 步同时是 claim commit 和 visibility commit，不存在 Delivery 维度或更晚的 visibility commit。
在第 2、3 步期间，同一 owner 的并发 List 可能发现 Sandbox ID。如果 warm Sandbox 已经满足 State
准入，同一 owner 的其他操作可能在 Create 或 Clone 返回前执行。这个请求处理窗口没有隔离保证，
调用方不得依赖其行为。

Create 或 Clone 成功响应保证交付工作已经完成。claim commit 前失败会释放预留；claim commit 后
失败可能留下 owner-visible Sandbox，quota 继续保留，直到 release committed 或观察到对象不存在。
Manager 选择保留或清理策略，Infra 使用同一个 claim fence 执行。

### 5. Owner 可见性和公开状态

Manager 推导与调用方无关的资源可见性：

    ResourceVisible =
        Sandbox 存在
        && State.Claimed
        && State.Release in {none, due}

API 随后执行 owner authorization：

    OwnerVisible = ResourceVisible && Observation.Owner == authenticated caller

| 情况 | 非 Kill owner API | Kill |
|---|---|---|
| NotFound | HTTP 404 | HTTP 204 |
| Claimed=false | HTTP 404 | HTTP 204 |
| Release=terminal | HTTP 404 | 为同一 claim 提交清理 |
| Release=committed | HTTP 404 | HTTP 204 |
| Claimed=true 且 owner mismatch | HTTP 404 | HTTP 204，无操作 |
| OwnerVisible | 应用 State 准入 | 为同一 claim 提交释放 |

Release=due 仍然 OwnerVisible，但 Kill 必须提交释放；deadline 仍为 due 时，除 List、Describe 和
Kill 外的所有 owner capability 都被拒绝。

NotFound、Claimed=false、Release=committed 和 owner mismatch 的 Kill 都返回 HTTP 204。owner
mismatch 只写入受保护的审计日志，不产生资源、Route 或 quota 副作用。正确 owner 对
Release=none、due 或 terminal 执行 Kill 时，只有 claim-fenced release commit 成功后才能返回
HTTP 204。后端失败或 claimed identity 非法时返回映射后的 unavailable 或 internal，不能假报成功。

List 只包含 OwnerVisible Sandbox，并在 pagination 前完成过滤。Describe 对每个 OwnerVisible Sandbox
返回 200，包括 paused、provisioning、unready 和 due observation。

E2B 公开状态继续保持最小集合：

| State | E2B 状态 |
|---|---|
| OwnerVisible、PauseResume=none、Workload=ready | running |
| 其他所有 OwnerVisible State | paused |

### 6. 操作准入

以下规则只在 OwnerVisible authorization 之后适用。每个已准入操作仍然必须携带有效
MutationToken 或 claim-delivery fence。

Release=due 是本节所有能力的独立拒绝条件。在 Controller 改变 deadline 或持久化 release commit
前，只准入 List、Describe 和 Kill。

| PauseResume 和 Workload | Pause | Resume | Connect |
|---|---|---|---|
| none + ready | 开始 Pause | 无操作成功 | 连接 |
| none + paused | 无操作成功 | 开始 Resume | Resume 后连接 |
| pausing + 任意 Workload | 加入并等待 | HTTP 409 | HTTP 400 |
| resuming + 任意 Workload | HTTP 409 | 加入并等待 | 等待后连接 |
| none + provisioning 或 unready | HTTP 409 | HTTP 409 | HTTP 500 |

Snapshot、Set timeout、Update network 和 Browser use 要求 PauseResume=none 且 Workload=ready。
不满足准入时的公开结果保持为：

| API | OwnerVisible 但不满足准入 |
|---|---|
| Snapshot | HTTP 400，不修改 Sandbox |
| Set timeout | HTTP 500，不修改 deadline |
| Update network | HTTP 500，不修改 policy |
| Browser use | HTTP 500 |

因此，即使 Workload=ready，Release=due 也不准入 Set timeout。Controller 持久化的
paused-retention 延长可能使 Release 随后恢复为 none，新的 Set timeout observation 再按普通规则
判断。

Manager 负责同方向操作加入、反方向冲突以及 token 冲突后的重新准入。Infra 只负责条件后端动作和
等待；API 负责状态码映射。composite Connect 在 Resume 后取得新的 Observation，不能复用原始
token 访问 workload。

### 7. Route

`sandboxroute.RouteFromSandbox` 使用与 Infra 相同的 Sandbox CR mapper，再把 State 与 route
identity、DeliveryEpoch、`Status.PodInfo.PodIP` 组合。Route 不携带完整 State 或 ShutdownTime。

DeletionTimestamp、确认的 delete event 或 tombstone 删除 Route。对于其他仍存在的对象，按第一条
匹配规则投影：

| State 和 Route 数据 | Route.State |
|---|---|
| 以下任一成立：Release=terminal 或 committed；Workload=completed；State 非法或不完整 | dead |
| Claimed=true、Release 为 none 或 due、PauseResume=none、Workload=ready 且 IP 存在 | running |
| PauseResume 为 pausing 或 resuming，或者 Workload=paused | paused |
| Workload=unready | dead |
| Workload=provisioning 或 IP 缺失 | creating |
| Claimed=false、Release=none、Workload=ready 且 IP 存在 | available |
| 其他组合 | dead |

Release=due 绝不改变 Route。如果 ShutdownTime 到达时没有 CR mutation，Gateway 可以继续转发既有
running Route。Controller 持久化 DeletionTimestamp、Terminating、Recycling 或其他 committed
事实后，才会产生拒绝或删除 Route 的事件。这就是选定的 eventual deadline 契约。

Gateway 只转发 Route.State=running。Route identity、DeliveryEpoch 和版本顺序会拒绝其他 claim
delivery 的 stale event，但 Route 是否存在以及 Route state 都不能反向决定 Sandbox 存在性、
owner authorization 或 Manager State。DeliveryEpoch 不能让已经转发的未认证客户端请求感知
delivery。

### 8. 配额和失败行为

Quota 不是 State 维度。Manager 在 claim commit 前预留 quota，并在 owner 和 claimed 事实持久化后
将其视为 active。

只有 Release=committed 或后端权威返回 NotFound 时，quota 才可释放。Release=due 和
Release=terminal 继续占用 quota。cleanup trigger 之所以算 committed，是因为 Controller 契约保证
recycle-or-delete，并禁止同一 claim 恢复服务。

release 写入失败时，Release 不会变为 committed，Kill 返回后端派生错误，owner 可见性不变，quota
继续保留。Infra 只返回中立的 NotFound、conflict、unavailable 或 operation failure；Manager 将
它与最新 Observation 组合，API 不读取 CR state，只映射领域结果。

### 9. 外部契约

| 场景 | 目标行为 | 原因 |
|---|---|---|
| Owner mismatch | 非 Kill API 返回 HTTP 404；Kill 返回 HTTP 204 且无副作用 | 保持 Kill 幂等，同时不形成存在性探针 |
| ShutdownTime 已到但 Controller 尚未提交 | 可见性、公开状态和 Route 继续由其他 State 维度决定；due 本身不改变 Route；只准入 List、Describe 和 Kill；quota 保留 | deadline 是 Controller 指令，不是 release commit |
| cleanup trigger 已持久化 | Kill 可以成功且 quota 可以释放；Controller 必须 recycle 或 delete | trigger 是当前 claim 的不可逆承诺 |
| Succeeded 或 Failed 终态 | 隐藏且 Route dead；Kill 仍需提交清理；quota 保留 | 工作负载终止不等于清理完成 |
| recycle 后重新 claim，旧 Observation 到达 | CR 写入、Browser、TrafficPolicy、Route、Checkpoint 和 composite Connect 都被 fence，不能影响新 owner | UID 不能单独标识一次 delivery |
| Create 或 Clone 处理中 | 同一 owner 可能发现并操作 Sandbox；不承诺隔离 | claim 持久化就是 visibility commit |
| Running 且 Ready 缺失、False、Unknown、revision 不匹配或 update 不安全 | Workload=unready，不能仅因 age 成为 speculative candidate | 无法证明服务能力或进展 |
| InplaceUpdate=False/Failed 且 Ready=True | 其他 serving 谓词全部满足时，Workload 可以保持 ready | update convergence 与现有 workload capability 互相独立 |
| claimed identity 缺失或冲突 | 返回 internal 或 unavailable，Route 失败关闭 | 损坏数据不能表现为 invisible 或可路由 |
| 存在多个 Manager 进程 | 副本可以暂时不一致；任何副本都不能使用 stale 的已授权 observation 修改其他 delivery | 副本状态同步属于独立设计 |

## 实现注意事项

Controller 的 recycle-or-delete 契约是硬发布前置条件。必须先升级并确认所有 Controller：cleanup
trigger 到达 paused、使用 PVC 或其他不可 recycle 的 Sandbox 时会执行删除；recycle 失败时只保留
committed cleanup 并最终删除。只有满足该前置条件后，Manager 和 Gateway 才能把持久化 cleanup
trigger 解释为 Release=committed。本提案依赖部署顺序，不增加 feature gate 或版本握手。
