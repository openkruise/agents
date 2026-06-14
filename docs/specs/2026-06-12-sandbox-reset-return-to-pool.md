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
- 通过 feature gate 控制，安全灰度发布。

### 非目标

- 定义具体的清理动作（文件系统清除、进程终止等）——这些由 agent-runtime 的 `reset` 端点负责，本设计仅编排生命周期。
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
    → 立即恢复 OwnerReference 到原始 SandboxSet
    → Sandbox Phase: Running → Resetting
    → SandboxSet 将其归入 Resetting 组（计入 Replicas，不计入 Available）
    → SandboxSet 不会为这个 sandbox 补货
    → 调用 agent-runtime reset 端点（清理用户数据）
    → 成功:
        → 移除 claim 相关的 labels/annotations
        → 清除 shutdownTime / pauseTime
        → Sandbox Phase: Resetting → Running（Ready）
        → SandboxSet 将其归类为: Available
        → 如果 unclaimed 总数 > replicas → SandboxSet 缩容删除多余的
    → 失败:
        → Sandbox Phase: Resetting → Failed
        → SandboxSet 将其归类为: Dead → 垃圾回收
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

也可以在 claim 时通过 SandboxClaim 的 `spec.annotations` 字段预设该 annotation，表达释放时 reset 的意图（sandbox-manager 或上层控制器在释放时读取并执行）。

### 5.3 E2B API 场景

sandbox-manager 通过环境变量控制行为：

```
SANDBOX_RESET_ON_DELETE=true
```

开启后，`SandboxManager.DeleteSandbox()` 会检查 sandbox 是否有 SandboxSet 来源标签（`LabelSandboxSetOrigin`）。如果有，则设置 `AnnotationReset` annotation 代替真正删除，由 sandbox controller 接管后续流程。

```go
func (m *SandboxManager) DeleteSandbox(ctx context.Context, sbx infra.Sandbox) error {
    if m.resetOnDelete && hasSandboxSetOrigin(sbx) {
        return sbx.TriggerReset(ctx)  // 设置 annotation，不真删
    }
    // 默认行为：删除
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
    SandboxResettingReasonStarted   = "ResetStarted"
    SandboxResettingReasonSucceeded = "ResetSucceeded"
    SandboxResettingReasonFailed    = "ResetFailed"
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

```go
const (
    // LabelSandboxSetOrigin 记录最初创建该 sandbox 的 SandboxSet 名称。
    // 在 claim 时设置，跨 reset 周期保留，使 reset 流程知道该回哪个池。
    LabelSandboxSetOrigin = InternalPrefix + "sandbox-set-origin"

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

### 6.5 SandboxSet Status: ResettingReplicas

```go
type SandboxSetStatus struct {
    // ...现有字段...

    // ResettingReplicas 是当前正在 reset 的 sandbox 数量。
    // +optional
    ResettingReplicas int32 `json:"resettingReplicas,omitempty"`
}
```

## 7. SandboxSet Controller 变更

### 7.1 GroupedSandboxes

新增 Resetting 组：

```go
type GroupedSandboxes struct {
    Creating   []*agentsv1alpha1.Sandbox
    Available  []*agentsv1alpha1.Sandbox
    Used       []*agentsv1alpha1.Sandbox
    Resetting  []*agentsv1alpha1.Sandbox // 正在 reset，即将变为 Available
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
    newStatus.ResettingReplicas = int32(len(groups.Resetting))
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

1. **检测触发条件**: Sandbox 有 annotation `agents.kruise.io/reset=true`，当前 phase 为 `Running` 或 `Paused`，且 feature gate `SandboxReset` 已开启。
2. **提前恢复 OwnerRef**: 通过 `LabelSandboxSetOrigin` 查找 SandboxSet，立即恢复 OwnerReference。这是防止 SandboxSet 过度补货的关键步骤。
3. **转入 Resetting**: 设置 `status.phase = Resetting`，设置 `Resetting` condition 为 `ResetStarted`。
4. **执行 Reset**:
   - 调用 agent-runtime reset 端点（通过 envd API）清理用户数据。
   - 超时：可配置（默认 60s），超时后 sandbox 标记为 Failed。
5. **成功时**:
   - 移除 claim 相关 labels: `sandbox-claimed` → `"false"`，删除 `claim-name`。
   - 移除 claim 相关 annotations: `owner`、`claim-timestamp`、`init-runtime-request`、`runtime-access-token`、CSI 挂载 annotations、`reset`。
   - 清除 `spec.shutdownTime` 和 `spec.pauseTime`。
   - 如果有 in-place 更新需要回退（重新应用 SandboxSet 当前模板的 image/resources）。
   - 设置 `status.phase = Running`，Ready condition 为 True。
   - `status.resetCount` 加 1。
6. **失败时**:
   - 设置 `status.phase = Failed`。
   - 设置 `Resetting` condition 为 `ResetFailed`，附带错误信息。
   - SandboxSet 会垃圾回收该 sandbox。

### 8.2 OwnerRef 恢复细节

```go
func (c *control) restoreOwnerRef(ctx context.Context, sbx *agentsv1alpha1.Sandbox) error {
    originName := sbx.Labels[agentsv1alpha1.LabelSandboxSetOrigin]
    if originName == "" {
        return fmt.Errorf("sandbox %s has no origin SandboxSet label", sbx.Name)
    }

    sbs := &agentsv1alpha1.SandboxSet{}
    if err := c.Get(ctx, client.ObjectKey{Namespace: sbx.Namespace, Name: originName}, sbs); err != nil {
        return fmt.Errorf("origin SandboxSet %s not found: %w", originName, err)
    }

    sbx.OwnerReferences = []metav1.OwnerReference{
        *metav1.NewControllerRef(sbs, agentsv1alpha1.SandboxSetControllerKind),
    }
    return nil
}
```

## 9. Claim 流程变更

### 9.1 保留 Origin Label

在 claim sandbox 时（`modifyPickedSandbox` 中），移除 OwnerReference 之前先记录原始 SandboxSet 名称：

```go
func modifyPickedSandbox(sbx *Sandbox, lockType infra.LockType, opts infra.ClaimSandboxOptions) error {
    // ...现有逻辑...

    // 保留 SandboxSet 来源信息，以便后续 reset 回池（在移除 OwnerRef 之前）
    if controller := metav1.GetControllerOfNoCopy(sbx.Sandbox); controller != nil {
        labels[v1alpha1.LabelSandboxSetOrigin] = controller.Name
    }

    sbx.SetOwnerReferences([]metav1.OwnerReference{}) // 现有行为：移除 OwnerRef
    labels[v1alpha1.LabelSandboxIsClaimed] = v1alpha1.True
    // ...后续现有逻辑...
}
```

`LabelSandboxPool` 在 claim 时已被保留（现有行为），但 `LabelSandboxSetOrigin` 是一个独立的 label，明确表达语义，避免与 `LabelSandboxPool` 的含义混淆。

## 10. Reset 清理动作

agent-runtime `reset` 端点应执行以下清理（具体设计另出 spec）：

1. **终止用户进程** — kill 所有用户启动的进程。
2. **清理文件系统** — 删除用户创建的文件，恢复初始文件系统状态。
3. **重置环境变量** — 清除 claim 期间设置的环境变量。
4. **重置网络状态** — 清除用户创建的网络规则或连接。
5. **健康检查** — 验证 sandbox 处于干净、可用的状态。

## 11. Feature Gate

```go
const (
    SandboxResetGate featuregate.Feature = "SandboxReset"
)

// 默认关闭（Alpha）
SandboxResetGate: {Default: false, PreRelease: featuregate.Alpha},
```

feature gate 关闭时：
- sandbox controller 忽略 `AnnotationReset`。
- 不会进入 `Resetting` phase。
- sandbox-manager 的 `SANDBOX_RESET_ON_DELETE` 环境变量无效。

## 12. Metrics

| Metric | 类型 | Labels | 说明 |
|--------|------|--------|------|
| `sandbox_reset_total` | Counter | `namespace`, `result` (success/failure) | sandbox reset 操作总次数 |
| `sandbox_reset_duration_seconds` | Histogram | `namespace` | sandbox reset 操作耗时 |
| `sandbox_reuse_total` | Counter | `namespace`, `sandboxset` | sandbox 回池复用总次数 |
| `sandboxset_resetting_replicas` | Gauge | `namespace`, `sandboxset` | 当前正在 reset 的 sandbox 数量 |

## 13. 边界情况与失败处理

1. **Reset 期间 SandboxSet 被删除**: sandbox 无法恢复 OwnerReference，标记为 Failed 并被垃圾回收。

2. **Reset 期间 SandboxSet 模板变更**: 回池的 sandbox 可能持有旧模板。SandboxSet 的 rolling update 机制会处理——sandbox 会被视为 "未更新" 并最终被滚动更新。

3. **Reset 超时**: 可配置超时（默认 60s）。超时后 sandbox 标记为 Failed。

4. **Reset 和 Delete 并发**: 如果用户在 reset 期间显式删除 sandbox，delete 优先（K8s 原生删除语义）。

5. **多次 Reset**: sandbox 可以被多次 reset 复用。`status.resetCount` 记录次数，无硬性上限，运营方可据此设置策略。

6. **In-place 更新后的 Reset**: 如果 sandbox 在 claim 期间做了 image/资源变更，reset 流程会回退到 SandboxSet 当前模板，确保回池 sandbox 与池模板一致。

7. **过度补货竞争**: 如果 SandboxSet 在 reset annotation 设置之前已经补了货，sandbox 回来后 total > replicas。SandboxSet 现有的缩容逻辑会删除多余的 Creating 或 Available sandbox。

8. **无 Origin Label**: 在 feature 开启前被 claim 的 sandbox 没有 `LabelSandboxSetOrigin`。这些 sandbox 的 reset annotation 会被忽略，sandbox-manager 回退到 delete。

## 14. 向后兼容

- 不新增 CRD spec 字段——完全向后兼容。
- Feature gate 默认关闭，不开启则无行为变化。
- `LabelSandboxSetOrigin` 仅在 feature 开启后的 claim 中设置。
- 现有的 sandbox 和 claim 继续正常工作。
- sandbox-manager `SANDBOX_RESET_ON_DELETE` 默认 `false`。

## 15. 实现顺序

1. **Phase 1 — API 类型**: 新增 `SandboxResetting` phase、`SandboxStateResetting`、`AnnotationReset`、`LabelSandboxSetOrigin` 常量、`status.resetCount`、`status.resettingReplicas`、feature gate。
2. **Phase 2 — Claim 流程**: 在 `modifyPickedSandbox` 中保留 `LabelSandboxSetOrigin`。
3. **Phase 3 — Sandbox controller**: 实现 reset reconciliation（检测 annotation → 恢复 OwnerRef → Resetting phase → 调用 agent-runtime → 清理 → 回到 Available）。
4. **Phase 4 — SandboxSet controller**: 在 `groupAllSandboxes` 中处理 `Resetting` 组，更新 `calculateSandboxSetStatusFromGroup` 和 scale delta 逻辑。
5. **Phase 5 — Sandbox-manager**: 新增 `SANDBOX_RESET_ON_DELETE` 环境变量，更新 `DeleteSandbox` 设置 annotation 代替删除。
6. **Phase 6 — Agent-runtime reset 端点**: 实现清理 API（单独 spec）。
7. **Phase 7 — Metrics 与可观测性**: 新增 Prometheus metrics。

## 16. 待讨论问题

1. **Reset 是否需要回退 in-place 更新？** — 如果 sandbox 在 claim 期间做了 image/资源变更，reset 时是否回退到 SandboxSet 模板？当前建议：是，确保一致性。但这增加了复杂度。替代方案：让 SandboxSet 的 rolling update 自然处理不匹配。

2. **Reset 超时配置** — 全局 controller flag 还是 per-SandboxSet annotation？当前倾向：先用全局 flag，后续按需增加 per-SandboxSet annotation。

3. **最大 Reset 次数** — 是否需要上限？超过 N 次后改为删除，防止状态泄漏积累。可以先用全局 flag。

4. **LabelSandboxPool 复用** — `LabelSandboxPool` 在 claim 时已被保留。是否可以直接复用它代替新增 `LabelSandboxSetOrigin`？需要确认 `LabelSandboxPool` 在所有场景下（包括 templateRef 场景）都可靠指向正确的 SandboxSet 名称。
