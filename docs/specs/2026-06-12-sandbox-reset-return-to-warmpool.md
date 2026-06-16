# Sandbox Reset & 回池复用 — 设计文档

- 日期: 2026-06-12
- 分支: `sandbox-reset-docs`
- 涉及范围: `api/v1alpha1/`, `pkg/controller/sandbox/`, `pkg/controller/sandboxset/`, `pkg/sandbox-manager/`

## 1. 问题背景

当前，SandboxClaim 完成后用户使用完 sandbox，sandbox 会被销毁（通过 `shutdownTime` 或手动删除）。这是一种浪费：sandbox Pod 已经完成了创建、调度、初始化和预热。销毁后重新创建会带来不必要的开销：

- Pod 调度延迟
- 容器镜像拉取耗时
- 运行时初始化耗时（agent-runtime、CSI 挂载、sidecar 注入）
- IP 地址分配开销

在许多场景下（如短期 AI agent 任务、代码执行环境），sandbox 环境本质上是无状态的——用户数据可以被清理，sandbox 可以归还到池中供下一个用户使用。

## 2. 目标与非目标

### 目标

- 允许已被 claim 的 sandbox 进行 **reset**（清理用户状态）并**归还**到 SandboxSet 池中复用。
- 使用 **annotation** 作为唯一触发方式——不新增 CRD spec 字段，不引入 ReclaimPolicy。
- 新增 `Resetting` phase 到 Sandbox 生命周期，表示正在清理中、等待回池。
- 在 reset 开始时**立即恢复 OwnerReference** 到 SandboxSet，使 SandboxSet 将 resetting 中的 sandbox 计入 Replicas，避免不必要的补货。
- 确保 reset 后的 sandbox 从 SandboxSet 视角看与新创建的 Available sandbox 无异。

### 非目标

- 定义具体的清理动作（文件系统清除、进程终止等）——这些由 `SandboxResetter` 接口的具体实现负责，本设计仅编排生命周期并定义接口契约。
- 支持有持久卷（PVC）的 sandbox reset——初版仅针对无状态 sandbox。
- reset 后的自动健康检查——复用现有的 readiness probe 机制。

## 3. 当前生命周期

```
SandboxSet 创建 Sandbox
    → Sandbox Phase: Pending → Running（Ready condition 为 True）
    → SandboxSet 将其归类为: Available

SandboxClaim / E2B API claim Sandbox
    → 移除 OwnerReference（SandboxSet 不再拥有该 sandbox）
    → Label sandbox-claimed = "true"
    → Label claim-name = "<claim-name>"
    → Annotation owner = "<user>"
    → SandboxSet: sandbox 从列表消失 → 补货创建新 sandbox

用户使用完毕 → Sandbox 被删除
    → Pod 被删除，资源释放
```

关键观察：claim 流程会**移除 OwnerReference**，导致 SandboxSet 认为该 sandbox "不再属于自己" 并触发补货。reset 流程需要逆转这一操作。

## 4. 新生命周期

```
用户使用完毕 → 在 sandbox 上设置 annotation agents.kruise.io/reset（触发器）
    → Sandbox controller 检测到 annotation
    → 非 Running phase:
        → 设置 Resetting condition 为 ResetRejected + Warning Event
        → 不做任何变更（OwnerRef、labels、annotations、phase 全部保持原样）
        → 上层决定后续操作（详见 5.2、5.3）
    → Running phase:
        → 立即恢复 OwnerReference 到原始 SandboxSet
        → 移除 claim 相关的 labels/annotations
        → 清除 shutdownTime / pauseTime
        → Sandbox Phase: Running → Resetting
        → SandboxSet 将其归入 Resetting 组（计入 Replicas，不计入 Available）
        → SandboxSet 不会为这个 sandbox 补货
        → 通过 SandboxResetter 接口执行清理（清理用户数据）
        → 成功:
            → Sandbox Phase: Resetting → Running（Ready）
            → SandboxSet 将其归类为: Available
            → 如果 unclaimed 总数 > replicas → SandboxSet 缩容删除多余的
        → 失败 / 超时:
            → 设置 Resetting condition（ResetFailed / ResetTimeout）+ Warning Event
            → SandboxSet 将其归入 Dead 组 → 删除 → 按需补货
```

## 5. 触发机制

### 5.1 基于 Annotation 的触发

reset 流程通过在 Sandbox 上设置一个 annotation 来触发：

```go
const (
    // AnnotationReset 触发 sandbox reset 回池流程。
    // 设置为 "true" 时，sandbox controller 将 reset 该 sandbox 并归还到原始 SandboxSet 池。
    AnnotationReset = InternalPrefix + "reset"
)
```

不引入新的 spec 字段，不引入 ReclaimPolicy。annotation 是唯一触发方式。

### 5.2 SandboxClaim 场景

通过 SandboxClaim 管理 sandbox 的用户，自行在 sandbox 上设置 annotation：

```bash
kubectl annotate sandbox <name> agents.kruise.io/reset=true
```

如果 sandbox 当前 phase 不是 `Running`，sandbox controller 会设置 `ResetRejected` condition。用户的上层 controller 需要自行监听该 condition，并决定降级策略（如直接删除 sandbox）。

### 5.3 E2B API 场景

sandbox-manager 通过环境变量控制行为：

```
SANDBOX_RESET_ON_DELETE=true
```

开启后，`SandboxManager.DeleteSandbox()` 会检查 sandbox 是否有 SandboxSet 来源标签（`LabelSandboxPool`）且当前 phase 为 `Running`。满足条件时设置 `AnnotationReset` annotation 代替真正删除，由 sandbox controller 接管后续流程。如果 phase 不是 `Running`（如 `Paused`），直接走删除链路。

```go
func (m *SandboxManager) DeleteSandbox(ctx context.Context, sbx infra.Sandbox) error {
    if m.resetOnDelete && hasSandboxPool(sbx) && sbx.Phase() == SandboxRunning {
        return sbx.TriggerReset(ctx)  // 设置 annotation，不真删
    }
    // 默认行为：删除（包括非 Running phase 的降级）
    return m.doDeleteSandbox(ctx, sbx)
}
```

## 6. API 变更

### 6.1 Sandbox: Resetting Phase

新增 `SandboxPhase`：

```go
const (
    // SandboxResetting 表示 sandbox 正在被 reset（清理用户状态），准备回池。
    SandboxResetting SandboxPhase = "Resetting"
)
```

新增 condition 类型：

```go
const (
    // SandboxConditionResetting 表示 sandbox reset 的进度。
    SandboxConditionResetting SandboxConditionType = "Resetting"
)

const (
    SandboxResettingReasonRejected  = "ResetRejected"
    SandboxResettingReasonStarted   = "ResetStarted"
    SandboxResettingReasonSucceeded = "ResetSucceeded"
    SandboxResettingReasonFailed    = "ResetFailed"
    SandboxResettingReasonTimeout   = "ResetTimeout"
)
```

### 6.2 Sandbox Status: ResetCount

```go
type SandboxStatus struct {
    // ...现有字段...

    // ResetCount 记录该 sandbox 被 reset 复用的次数。
    // +optional
    ResetCount int32 `json:"resetCount,omitempty"`
}
```

### 6.3 Labels 与 Annotations

复用现有的 `LabelSandboxPool`（`agents.kruise.io/sandbox-pool`）来标识 sandbox 的来源 SandboxSet。该 label 在 SandboxSet 创建 sandbox 时就已设置，值始终为 SandboxSet 名称，claim 时保留不变，无需新增 label。

> **注意**: `LabelSandboxPool` 原本标记为 deprecated，但 reset 回池场景赋予了它明确的用途——标识来源 SandboxSet。移除其 deprecated 标记。

```go
const (
    // AnnotationReset 触发 sandbox reset 回池流程。
    AnnotationReset = InternalPrefix + "reset"
)
```

### 6.4 Sandbox State 常量

```go
const (
    SandboxStateResetting = "resetting"
)
```

## 7. SandboxSet Controller 变更

### 7.1 GroupedSandboxes

新增 Resetting 组：

```go
type GroupedSandboxes struct {
    Creating   []*agentsv1alpha1.Sandbox
    Available  []*agentsv1alpha1.Sandbox
    Used       []*agentsv1alpha1.Sandbox
    Resetting  []*agentsv1alpha1.Sandbox // 正在 reset；失败/超时的归入 Dead
    Dead       []*agentsv1alpha1.Sandbox
}
```

### 7.2 状态机: GetSandboxState

更新 `pkg/utils/utils.go` 中的 `GetSandboxState`：

```go
func GetSandboxState(sbx *agentsv1alpha1.Sandbox) (state string, reason string) {
    // ...现有的 deletion/shutdown 检查...

    // 处理 Resetting phase
    if sbx.Status.Phase == agentsv1alpha1.SandboxResetting {
        // 失败或超时的 Resetting sandbox 视为 Dead，由 SandboxSet 删除
        if cond := getSandboxCondition(sbx, agentsv1alpha1.SandboxConditionResetting); cond != nil &&
            (cond.Reason == agentsv1alpha1.SandboxResettingReasonFailed ||
             cond.Reason == agentsv1alpha1.SandboxResettingReasonTimeout) {
            return agentsv1alpha1.SandboxStateDead, cond.Reason
        }
        return agentsv1alpha1.SandboxStateResetting, "ResourceResetting"
    }

    // ...后续现有逻辑...
}
```

### 7.3 Status 计算

Resetting sandbox 计入 `Replicas` 但**不**计入 `AvailableReplicas`：

```go
func calculateSandboxSetStatusFromGroup(...) {
    newStatus.AvailableReplicas = int32(len(groups.Available))
    newStatus.Replicas = int32(len(groups.Creating)) + int32(len(groups.Available)) +
        int32(len(groups.Resetting)) + int32(len(dirtyScaleUp[expectations.Create]))
}
```

### 7.4 Scale Delta

由于 Resetting sandbox 计入 `Replicas`，scale delta 自然会考虑它们：

```
delta = spec.replicas - status.replicas
```

- 如果 1 个 sandbox 正在 resetting，且 SandboxSet 已经补了 1 个 → `Replicas` 包含两者 → delta 可能为负 → SandboxSet 缩容删掉多余的（删 Creating 状态的新 sandbox）。
- 如果 sandbox 刚被 claim 且替代品还没创建完，Resetting sandbox 回来后阻止了不必要的补货。

### 7.5 缩容优先级

当 `delta < 0`（需要缩容）时，现有 `scaleDown` 函数选择要删除的 sandbox。优先级：

1. **Creating sandbox**（最新的优先）——还没完成初始化，丢弃成本最低。
2. **Available sandbox**（最新的优先）——已就绪但未被 claim，刚 reset 回来的通常是更老更热的。

这基本就是现有行为，不需要特殊处理。

## 8. Sandbox Controller 变更

### 8.1 Reset 流程

Sandbox controller 检测到 `AnnotationReset` annotation 后执行 reset：

1. **检测触发条件**: Sandbox 有 annotation `agents.kruise.io/reset=true`。
   - **仅允许 `Running` phase**。如果 sandbox 处于其他 phase（如 `Paused`、`Pending`、`Terminating`），拒绝 reset：
     - 设置 `Resetting` condition 为 `ResetRejected`，message 说明当前 phase 不允许 reset。
     - 发出 Warning Event（reason: `ResetRejected`）。
     - 保留 `AnnotationReset` 不移除。
   - 上层组件负责处理拒绝后的降级策略，详见 5.2、5.3 节。
2. **提前恢复 OwnerRef 并清理 claim 状态**:
   - 通过 `LabelSandboxPool` 查找 SandboxSet，立即恢复 OwnerReference。这是防止 SandboxSet 过度补货的关键步骤。
   - 移除 claim 相关 labels: `sandbox-claimed` → `"false"`，删除 `claim-name`。
   - 移除 claim 相关 annotations: `owner`、`claim-timestamp`、`init-runtime-request`、`runtime-access-token`、`reset`。
   - 清除 `spec.shutdownTime` 和 `spec.pauseTime`（防止 reset 期间被 shutdown 控制器终止）。
3. **转入 Resetting**: 设置 `status.phase = Resetting`，设置 `Resetting` condition 为 `ResetStarted`。
4. **执行 Reset**:
   - 通过 `SandboxResetter.Reset()` 清理用户数据。
   - 超时通过 `SandboxControlArgs.ResetTimeout` 配置（对应 controller flag `--reset-timeout`，默认 60s）。
5. **成功时**:
   - 设置 `status.phase = Running`，Ready condition 为 True。
   - `status.resetCount` 加 1。
6. **失败 / 超时时**:
   - 设置 `Resetting` condition：明确错误设为 `ResetFailed`，超时设为 `ResetTimeout`。
   - 发出对应 Warning Event。
   - SandboxSet controller 在 `groupAllSandboxes` 中将带有 `ResetFailed` 或 `ResetTimeout` condition 的 Resetting sandbox 归入 Dead 组，走正常的删除 + 补货流程。

### 8.2 OwnerRef 恢复细节

```go
func (c *control) restoreOwnerRef(ctx context.Context, sbx *agentsv1alpha1.Sandbox) error {
    poolName := sbx.Labels[agentsv1alpha1.LabelSandboxPool]
    if poolName == "" {
        return fmt.Errorf("sandbox %s has no sandbox-pool label", sbx.Name)
    }

    sbs := &agentsv1alpha1.SandboxSet{}
    if err := c.Get(ctx, client.ObjectKey{Namespace: sbx.Namespace, Name: poolName}, sbs); err != nil {
        return fmt.Errorf("origin SandboxSet %s not found: %w", poolName, err)
    }

    sbx.OwnerReferences = []metav1.OwnerReference{
        *metav1.NewControllerRef(sbs, agentsv1alpha1.SandboxSetControllerKind),
    }
    return nil
}
```

## 9. Claim 流程变更

### 9.1 LabelSandboxPool 已自然保留

Claim 流程无需额外修改。`LabelSandboxPool` 在 SandboxSet 创建 sandbox 时已设置，claim 时不会被移除，因此 reset 流程可以直接读取该 label 找到来源 SandboxSet。

## 10. SandboxResetter 接口

Sandbox controller 通过 `SandboxResetter` 接口调用清理逻辑，不绑定具体实现组件。不同场景可以提供不同的实现（如通过 Pod 内 sidecar API、exec 命令、或外部服务）。

```go
// SandboxResetter handles the actual cleanup of user state inside a sandbox.
// Implementations are injected into the sandbox controller at startup.
type SandboxResetter interface {
    // Reset triggers the cleanup of user state in the sandbox.
    // This may be asynchronous — use IsResetComplete to poll for the result.
    Reset(ctx context.Context, sandbox *agentsv1alpha1.Sandbox) error

    // IsResetComplete checks whether a previously triggered reset has finished.
    // Returns (true, nil) on success, (true, err) on failure, (false, nil) if still in progress.
    IsResetComplete(ctx context.Context, sandbox *agentsv1alpha1.Sandbox) (complete bool, err error)
}
```

Sandbox controller 的 reconcile 流程：
1. 首次进入 Resetting phase → 调用 `Reset()` 触发清理
2. 后续 reconcile → 调用 `IsResetComplete()` 轮询结果
3. 返回 `(true, nil)` → 成功，回到 Running
4. 返回 `(true, err)` → 失败，设 `ResetFailed` condition
5. 返回 `(false, nil)` → 进行中，requeue 等待下次检查（受 `--reset-timeout` 约束）

实现应执行以下清理（具体由实现方决定）：

1. **终止用户进程** — kill 所有用户启动的进程。
2. **清理文件系统** — 删除用户创建的文件，恢复初始文件系统状态。
3. **重置环境变量** — 清除 claim 期间设置的环境变量。
4. **重置网络状态** — 清除用户创建的网络规则或连接。
5. **健康检查** — 验证 sandbox 处于干净、可用的状态。

## 11. Metrics & Events

### 12.1 Prometheus Metrics

| Metric | 类型 | Labels | 说明 |
|--------|------|--------|------|
| `sandbox_reset_total` | Counter | `namespace`, `result` (success/failure) | sandbox reset 操作总次数 |
| `sandbox_reset_duration_seconds` | Histogram | `namespace` | sandbox reset 操作耗时 |
| `sandbox_reuse_total` | Counter | `namespace`, `sandboxset` | sandbox 回池复用总次数 |

### 12.2 Kubernetes Events

Sandbox controller 在 reset 生命周期的关键节点发出 K8s Event，复用 condition reason 常量：

| 时机 | EventType | Reason | Message 示例 |
|------|-----------|--------|-------------|
| phase 不允许 reset（非 Running） | Warning | `ResetRejected` | `Reset rejected: sandbox is in %s phase, only Running is allowed` |
| 开始 reset（恢复 OwnerRef、清理 claim 状态） | Normal | `ResetStarted` | `Reset started, restored OwnerRef to SandboxSet %s` |
| reset 成功（sandbox 回到 Available） | Normal | `ResetSucceeded` | `Reset succeeded, sandbox returned to pool (resetCount: %d)` |
| reset 失败 | Warning | `ResetFailed` | `Reset failed: %s` |
| reset 超时 | Warning | `ResetTimeout` | `Reset timed out after %s` |

所有 reason 常量已在 6.1 节统一定义。

## 13. 边界情况与失败处理

1. **Reset 期间 SandboxSet 被删除**: sandbox 无法恢复 OwnerReference，标记为 Failed 并被垃圾回收。

2. **Reset 期间 SandboxSet 模板变更**: 见待讨论第 6 点。

3. **Reset 超时**: 通过 controller flag `--reset-timeout` 配置（默认 60s）。超时后 sandbox controller 设置 `ResetTimeout` condition 并发出 Warning Event，SandboxSet 将其归入 Dead 组删除并按需补货。

4. **Reset 和 Delete 并发**: 如果用户在 reset 期间显式删除 sandbox，delete 优先（K8s 原生删除语义）。

5. **多次 Reset**: sandbox 可以被多次 reset 复用。`status.resetCount` 记录次数，无硬性上限，运营方可据此设置策略。

6. **In-place 更新后的 Reset**: 如果 sandbox 在 claim 期间做了 image/资源变更，reset 流程不做回退。回池后 SandboxSet 的 rolling update 机制会自然处理模板不匹配的 sandbox。

7. **过度补货竞争**: 如果 SandboxSet 在 reset annotation 设置之前已经补了货，sandbox 回来后 total > replicas。SandboxSet 现有的缩容逻辑会删除多余的 Creating 或 Available sandbox。

8. **无 SandboxPool Label**: 在没有通过 SandboxSet 创建的 sandbox 上不存在 `LabelSandboxPool`。这些 sandbox 的 reset annotation 会被忽略，sandbox-manager 回退到 delete。

## 14. 向后兼容

- 不新增 CRD spec 字段——完全向后兼容。
- `LabelSandboxPool` 是现有 label，无需额外设置，所有通过 SandboxSet 创建的 sandbox 天然具备。
- 现有的 sandbox 和 claim 继续正常工作。
- sandbox-manager `SANDBOX_RESET_ON_DELETE` 默认 `false`。

## 15. 实现顺序

1. **Phase 1 — API 类型**: 新增 `SandboxResetting` phase、`SandboxStateResetting`、`AnnotationReset` 常量、`status.resetCount`。移除 `LabelSandboxPool` 的 deprecated 标记。
2. **Phase 2 — Sandbox controller**: 实现 reset reconciliation（检测 annotation → 通过 `LabelSandboxPool` 恢复 OwnerRef → Resetting phase → 调用 `SandboxResetter.Reset()` → 清理 → 回到 Available）。
3. **Phase 3 — SandboxSet controller**: 在 `groupAllSandboxes` 中处理 `Resetting` 组，更新 `calculateSandboxSetStatusFromGroup` 和 scale delta 逻辑。
4. **Phase 4 — Sandbox-manager**: 新增 `SANDBOX_RESET_ON_DELETE` 环境变量，更新 `DeleteSandbox` 设置 annotation 代替删除。
5. **Phase 5 — Agent-runtime reset 端点**: 实现清理 API（单独 spec）。
6. **Phase 6 — Metrics 与可观测性**: 新增 Prometheus metrics。

## 16. 待讨论问题

1. ~~**Reset 是否需要回退 in-place 更新？**~~ — **已决定：不需要**。回池后如果 sandbox 与当前 SandboxSet 模板不匹配，由 SandboxSet 的 rolling update 机制自然处理。

2. ~~**Reset 超时配置**~~ — **已决定：全局 controller flag `--reset-timeout`**，通过 `SandboxControlArgs.ResetTimeout` 传入，默认 60s。后续按需增加 per-SandboxSet annotation 覆盖。

3. **最大 Reset 次数** — 是否需要上限？超过 N 次后改为删除，防止状态泄漏积累。可以先用全局 flag。

4. ~~**LabelSandboxPool 复用**~~ — **已决定：复用 `LabelSandboxPool`**。该 label 的值始终为 SandboxSet 名称（不受 `TemplateRef` 影响，那是 `LabelSandboxTemplate` 的语义），claim 时自然保留，无需新增 `LabelSandboxSetOrigin`。同时移除 `LabelSandboxPool` 的 deprecated 标记。

5. **CSI 动态挂载的清理** — reset 时 CSI 挂载 annotations 是否需要移除？动态挂载可能涉及 unmount 等实际操作（不仅是删 annotation），需要明确清理流程和执行时机。

6. **Reset 期间 SandboxSet 模板变更** — reset 过程中如果 SandboxSet 模板发生变更，回池的 sandbox 持有旧模板。是直接删除该 sandbox（避免回池一个马上要被 rolling update 的 sandbox），还是让 rolling update 自然处理？
