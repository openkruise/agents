# MEMO

Last updated: 2026-04-30

## Current Topic

设计分布式 single-flight 模块，用于跨副本序列化 Pause / Resume 并发操作，替代当前 `resumable()` + `retryUpdate` 的乐观锁竞态。

## 设计决策（已确认）

- **annotation 格式**: `singleflight.agents.kruise.io/<key>` = `"<seq>:<done>:<lastUpdate>"`
- **首次读取**: APIReader（防 informer stale）
- **precheck 语义**: 仅做硬性不可行判断（已达成目标 / Dead），中间状态全放行
- **分叉后**: Wait 返回不 re-validate；调用者根据业务状态判断 + 重试
- **锁释放**: defer + 独立 postCtx + APIReader + 严格 retry
- **抢占**: lastUpdate 超 5 分钟可抢占，阈值通过 SandboxManagerOptions 传入
- **Wait 返回**: Informer GET（Reconciler 刚确认过条件，足够新）
- **annotation 清理**: 不清理，seq 单调递增
- **Pause + Resume**: 共享 key `"pause-resume"`，各自 precheck/modifier/function

## 落地计划

### Phase 1: single-flight 核心模块 (`pkg/cache/singleflight.go`)

- `DistributedSingleFlightDo[T client.Object]` 泛型函数
- `Provider` 接口扩展（暴露 APIReader、client、waitHooks）
- Wait 机制集成（annotation checker 驱动 WaitEntry）

### Phase 2: Pause / Resume 接入

- `Sandbox.Pause / Resume` 调用 `SingleflightDo`，以 `"pause-resume"` 为 key
- precheck 重写：只做幂等检查 + Dead 检查
- 调用者增加重试循环（检测状态是否符合预期）

### Phase 3: 测试更新

- `resume when pausing` / `pause when resuming` 预期行为变更（从立即报错 → Wait + retry）
- 新增并发碰撞测试（Pause + Resume 竞态）

## Status

Plan saved to: `.agents/plans/distributed-singleflight.plan.md`
Plan audited by `plan-auditor`: 7 BLOCKERs + 5 MAJORs + 2 MINORs found and ALL resolved.
Status: **READY_FOR_IMPLEMENTATION**
