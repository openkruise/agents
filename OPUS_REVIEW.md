# 分支 `feature/stable-timeout-pause-resume-260506` 代码审查报告

审查范围聚焦在分支末尾三次提交（`22d802a2` / `dbf70474` / `8ce6b33a`，以 `MEMO.md` 为引导），同时核对了 `pkg/utils/timeout`、`pkg/sandbox-manager/infra/sandboxcr/sandbox.go`、`pkg/controller/sandbox/sandbox_controller.go`、`pkg/servers/e2b/{pause_resume,timeout}.go`、`pkg/sandbox-manager/infra/interface.go` 等最终代码版本。

---

## 一、严重问题（建议修复后再合并）

### 1. `Resume` 在 `runtime.InitRuntime` 调用处未使用 `postCtx`
**位置**：`pkg/sandbox-manager/infra/sandboxcr/sandbox.go:450`

```go
postCtx := ctx
if ctx.Err() != nil {
    postCtx, postCancel = context.WithTimeout(context.Background(), postResumeOperationTimeout)
    ...
    log.Info("original context expired after wait, using fresh context for post-resume operations")
}
if err = s.InplaceRefresh(postCtx, false); err != nil { ... }      // ✓ postCtx
...
if initRuntimeOpts != nil {
    if _, err := runtime.InitRuntime(ctx, s.Sandbox, *initRuntimeOpts, s.refreshFunc()); err != nil { ... }  // ✗ 用了 ctx
}
...
mountConfigs, resolveErr := resolveCSIMountConfigs(postCtx, ...)   // ✓ postCtx
if _, mountErr := runtime.ProcessCSIMounts(postCtx, ...); ...      // ✓ postCtx
```

注释明确说明 `postCtx` 用于 "post-resume operations (ReInit, CSI mount, inplace refresh)"，但 `InitRuntime` 仍使用原 `ctx`。当 `ctx.Err() != nil` 触发的正是创建 `postCtx` 的条件（`Wait` 在 deadline 边界返回时），此时把已过期的 `ctx` 传给 `InitRuntime` 会让它立刻返回 context 错误：`InitRuntime` 内部使用 `commonutils.RetryIfContextNotCanceled(ctx)`，过期 ctx 不会重试。

测试 `TestSandbox_Resume_ContextExpiredAfterWait` 不能覆盖这条路径，因为它没有设置 `AnnotationInitRuntimeRequest`，导致 `initRuntimeOpts == nil`、跳过该分支。

**修复**：把 `runtime.InitRuntime(ctx, ...)` 改为 `runtime.InitRuntime(postCtx, ...)`，并在测试里增加 `withInitRuntime=true` 且使用短 ctx 的用例。

---

### 2. `Pause` 的 snapshot 捕获被 `opts.Timeout != nil` 包裹
**位置**：`pkg/sandbox-manager/infra/sandboxcr/sandbox.go:336-346`

```go
if opts.Timeout != nil {
    ...
    if opts.CaptureTimeoutSnapshot {
        if err := timeout.SetTimeoutSnapshot(sbx); err != nil { ... }
    }
}
```

如果调用方希望「不修改 timeout、只捕获当前 timeout 的 snapshot」（这是合理的需求），必须先读出当前 timeout 再当成 `opts.Timeout` 传回来才行，否则被静默忽略。当前 `e2b.PauseSandbox` 总是构造 timeout（`buildPauseTimeoutOptions` 把 paused 的 timeout 推到 3026 年），所以现在没有触发；但 `TestSandbox_PauseCaptureSnapshotWithoutTimeoutDoesNotCreateSnapshot` 已经把这个反直觉行为固化进了测试。

**建议**：把 `if opts.CaptureTimeoutSnapshot { ... }` 提到 `if opts.Timeout != nil` 块外（或文档化 `PauseOptions` 字段间的依赖语义）。

---

### 3. 失败后无法重入：Resume 赢家路径中的非幂等步骤失败会让 sandbox 卡死
**位置**：`pkg/sandbox-manager/infra/sandboxcr/sandbox.go:368-485`

`Resume` 已经把 `spec.Paused` 翻成 `false` 并完成 `Wait`，但 `InitRuntime` 或 CSI re-mount 失败后会返回错误。然而 sandbox 现在状态是 `Running`，再次 `Resume` 会被开头 `state != SandboxStatePaused` 的前置检查拒绝（"resuming is only available for paused state"）。也就是说：

- 调用方收到错误 → 重试 → 永远拒绝；
- 调用方不重试 → sandbox 处于「Running 但 runtime/CSI 未恢复」的破损状态。

`MEMO.md` 把这一点列为已知约束（待 agent-runtime 接管），但分支里已经显式做了首写者过滤（loser 跳过 InitRuntime/CSI），所以**赢家失败再无补偿者**。建议至少：

- `Resume` 在赢家失败时把 `spec.Paused` 翻回 `true`，或写一个标记位让下一次 reconcile 拉起；
- 或者在前置检查里允许「state 是 Running 但运行时初始化条件未达成」的重入。

---

## 二、设计层面的隐患（建议明确语义/补单元测试）

### 4. 控制器 `ensurePauseTimeoutSnapshot` 会清掉 `SaveTimeoutWithPolicySnapshotAware` 故意制造的 drift
**位置**：`pkg/controller/sandbox/sandbox_controller.go:425-450`

```go
if ok, _ := timeout.IsTimeoutMatchedSnapshot(box); ok {
    return nil
}
return retry.RetryOnConflict(retry.DefaultRetry, func() error {
    ...
    if !latest.Spec.Paused { return nil }
    modified := latest.DeepCopy()
    if err := timeout.SetTimeoutSnapshot(modified); err != nil { return err }
    ...
})
```

`SnapshotAware` 策略的核心是：第一个 writer 看到 snapshot==current 走 Always，并**保留旧 snapshot 不动**，从而让后续 writer 在 snapshot!=current 上落入 ExtendOnly。这一切只有在 `spec.Paused==true` 期间发生 `SaveTimeoutWithPolicy` 时才有意义。

控制器只在 `latest.Spec.Paused==true` 时同步，而当前 e2b 调用流里 `SnapshotAware` 都发生在 `Resume` 翻 `spec.Paused=false` **之后**——因此今天没有触发 race。但：

1. 这是隐式契约，未来任何在 `spec.Paused=true` 期间走 `SnapshotAware` 的代码都会被控制器无声抹掉 drift；
2. **auto-pause 的 race window**（控制器还没来得及补上 snapshot 时，并发 Connect 到来）会让两路 Connect 都看到 `snapshotExists=false`、双双走 Always，丧失 drift 保护。

**建议**：

- 把控制器的 `ensurePauseTimeoutSnapshot` 改成「只在 snapshot 缺失时写入」，不要在 mismatch 时覆盖；
- 或者在 `SaveTimeoutWithPolicySnapshotAware` 的胜出分支同步写新 snapshot（让 snapshot 始终等于上一次成功设定的值），依靠 RV 冲突保证序列化；
- 在 AGENTS.md 记录上述不变式，便于以后维护者不犯回归。

### 5. Resume 不清理 snapshot，造成长期残留
**位置**：`sandbox.go:Resume`

`Resume` 不会 `ClearPauseTimeoutSnapshot`，导致一个 `Running` 的 sandbox 长期带着上一次 pause cycle 写下的 `agents.kruise.io/pause-timeout-snapshot` 注解。`MEMO.md` 解释了「保留以便并发 Connect 的 ExtendOnly 行为」，但：

- 注解会一直留到下一次 Pause（API Pause 才会重写），auto-pause 时旧值则会在第 4 项的 race window 中影响判断；
- 观测/排障时容易误读：`Running` 的 sandbox 有 `pause-timeout-snapshot` 是反直觉的。

**建议**：在 `Resume` 的 modifier 末尾，若 `EnsureTimeoutSnapshotIfMissing == false`（即不需要给后续并发兜底），就 `ClearPauseTimeoutSnapshot`；或者在 `Resume` 的 winner 路径完整结束后再清理。

### 6. `Pause` 已经 paused 时的语义不一致
- `s.Status.Phase != SandboxRunning` 直接报错 "sandbox is not in running phase"；
- 而 race 场景下进入 `retryUpdate` 看到 `latest.Spec.Paused=true` 是「静默跳过、视为成功」。

也就是说：**确定的 already-paused 报错，竞态的 already-paused 成功**。`Resume` 里有专门的 idempotent 短路（`localState==Paused, latest=Running, !Spec.Paused`），但 `Pause` 没有对称处理。建议二选一：要么 `Pause` 也接受「latest 已 paused 就视为成功」；要么 `retryUpdate` 的 already-paused skip 应被外层翻译成同样的错误。

### 7. `buildPauseTimeoutOptions` 用 `now.AddDate(1000, 0, 0)`（约公元 3026 年）当作「无限」
**位置**：`pkg/servers/e2b/pause_resume.go:65-77`

把 `Spec.ShutdownTime` 设成 3026 年会让 `Reconcile` 里 `requeueAfter = box.Spec.ShutdownTime.Sub(now.Time)` 得到一个超大 `time.Duration`。controller-runtime 通常会做 clamp，但依赖未文档化的行为。语义上更合理的做法是 `time.Time{}`（`setTimeout` 会把对应字段设为 `nil`，符合「never timeout」的约定）。

---

## 三、次要问题

### 8. `TestTimeEqual` 用例命名误导
`pkg/utils/timeout/timeout_test.go:582-586`

```go
{
    name: "Close but normalized same second",
    a:    time.Date(2026, 1, 2, 3, 4, 5, 900_000_000, time.UTC),
    b:    time.Date(2026, 1, 2, 3, 4, 6, 100_000_000, time.UTC),
    want: false,
},
```

实际两个时间 truncate 到秒后是 5 与 6，不是「same second」。测试本身正确（`want: false`），但名字会让人误解为期望相等。

### 9. `IsTimeoutMatchedSnapshot` 的错误在控制器里被丢弃
`sandbox_controller.go:429`

```go
if ok, _ := timeout.IsTimeoutMatchedSnapshot(box); ok { return nil }
```

注解格式损坏会让 `_` 吃掉错误，落入下一步重写覆盖。当前行为正确（坏 snapshot 会被修好），但应该至少 `klog.WarningS` 一下，便于发现外部错误写入。

### 10. `retryUpdate` 跳过更新时不会调用 `ResourceVersionExpectationExpect`
非赢家路径 `s.Sandbox = latest` 后直接返回，依赖 `Resume` 在外层 `InplaceRefresh` 后再补 `Expect`。逻辑上没问题，但流程依赖比较隐式，建议在 `retryUpdate` 注释里写清楚「skip 路径不影响期望」，避免别人复用时漏调用。

### 11. `TestSandbox_PauseSkipsSideEffectsWhenLatestAlreadyPaused` 依赖 APIReader 调用计数
该测试通过 `mutatingAPIReader` 的「第二次 Get 时插入 paused」来构造 race。如果将来 `Pause` 增加一次 APIReader 读取（例如多读一次 latest），断言会失效，但行为正确性其实没变。属于脆性测试。

### 12. `e2b.ResumeSandbox`（deprecated）和 `ConnectSandbox` 重复使用 `EnsureTimeoutSnapshotIfMissing: true`
两处行为一致，可抽出常量或 helper，避免一处改了另一处忘改。

---

## 四、其他扫到的杂项（与本次 pause/resume/timeout 主题无关，但属于本分支）

- `pkg/utils/webhookutils/writer/fs.go`：把证书私钥从 0666 改到 0600、目录从 0777 改到 0755，方向正确。建议同时收紧 `dirExists` 路径下检查到「过宽权限」时的处理（目前只创建用 0755）。
- `sandbox_controller.go` 大量从 `logf.FromContext` 切到 `klog.InfoS`：与上下文绑定的 `sandbox` 字段需要每条手动加，已注意到都加了，但容易随着新增日志漏掉，建议加 lint 规则或保留一个 `logger := klog.FromContext(ctx).WithValues(...)` 收敛。
- `Reconcile` 里新增的 Upgrading 错误处理：`if newStatus.Phase == agentsv1alpha1.SandboxUpgrading { ... persist upgrade status }` —— `retErr` 被仅记录、未与原始 `err` 合并返回，`updateSandboxStatus` 失败的细节会被吞掉，建议合并到返回错误中。

---

## 总结

最关键、必须修的是 **#1 Resume 把 ctx 传给 InitRuntime**：直接破坏了上面 5 行注释承诺的语义，并在测试里有缺口。
**#3 Resume winner 失败后不可恢复** 是设计层最大的隐患，建议优先讨论补偿方案。
**#4 控制器抹掉 SnapshotAware drift** 与 **#5 Resume 不清 snapshot** 是 timeout 协同设计目前最容易踩坑的两个隐式契约，建议在 AGENTS.md/接口注释里明确写下。
其余条目按优先级排序处理即可。