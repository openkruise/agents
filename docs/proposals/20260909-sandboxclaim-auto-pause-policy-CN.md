---
title: SandboxClaim 自动暂停 overlay
authors:
  - "@sophon"
reviewers:
  - "@TBD"
creation-date: 2026-09-09
last-updated: 2026-09-11
status: implementable
see-also:
  - "/docs/proposals/20260626-sandbox-auto-pause.md"
  - "/docs/proposals/20260627-wake-on-traffic-via-spec-patch.md"
  - "/docs/proposals/20251229-sandbox-claim-crd.md"
---

# SandboxClaim 自动暂停 overlay

## 摘要

Sandbox 已能按 `spec.pauseTime` 做一次性定时暂停，并按 `spec.autoPausePolicy` 做 probe 驱动暂停/恢复与入站流量唤醒。SandboxClaim 目前只能 overlay `shutdownTime`，调用方无法在 claim 时指定这两种暂停。终态：Claim 增加三个可选字段 `spec.pauseTime`、`spec.autoPausePolicy` 与 `spec.probes`，在 claim 落盘前一次性写到 Sandbox；策略与 deadline 缺省继承池配置；probes 按名合并到候选已有的探测集（同名覆盖、新名追加）；非法配置使 Claim 以 `InvalidClaimSpec` 完成且不再重试。不改暂停运行时。

## 背景

池 Sandbox 在创建时从 SandboxSet 复制 `probes` 与 `autoPausePolicy`。Claim 之后，只有已 claim 成员才进入 auto-pause 决策，未 claim 的池成员会被跳过；recycle 会清掉 `pauseTime`/`shutdownTime` 并从 SandboxSet 恢复策略。E2B create 已能用 `autoPause` 写 `PauseTime`，用 `autoResume` 局部写入 `OnIngressTraffic`。Claim CR 作为声明式对象，缺的是与 `shutdownTime` 同形态的 overlay，而不是再引入一套 E2B 相对秒数与 bool。

没有 Claim 级 overlay 时，每租户无法覆盖池默认策略，也无法声明「到点暂停」；只能改 SandboxSet（影响整池）或绕开 Claim 直接改 Sandbox。

## 设计终态

### 范围与非目标

范围内：

- `SandboxClaim.spec.pauseTime`：绝对时间，写入 `Sandbox.spec.pauseTime`
- `SandboxClaim.spec.autoPausePolicy`：复用现有 `AutoPausePolicy` 类型，整份写到 `Sandbox.spec.autoPausePolicy`
- `SandboxClaim.spec.probes`：复用现有 `Probe` 类型，按名合并到候选的 `Sandbox.spec.probes`（同名覆盖、新名追加）
- Claim 控制器在每次 claim 尝试前校验并 overlay；与现有 labels、annotations、`shutdownTime` 同一时机

范围外：

- 不增加 `autoPause` bool、相对 timeout、paused-retention 注解，也不从 `pauseTime` 推导 `shutdownTime`
- 不增加独立的 `autoResume` 字段；流量唤醒通过 `autoPausePolicy.resume.onIngressTraffic` 表达
- 不新建 SandboxClaim webhook；不改 sandbox 控制器、gateway、recycle、E2B HTTP（E2B 不暴露 probe 配置，保持 SandboxClaim 独有能力）
- 不把 Claim 变成持续控制器：Completed 之后仍不回写已 claim 的 Sandbox

### 所有权与数据流

```
SandboxClaim spec --> SandboxClaim 控制器 --claim 时 overlay--> claimed Sandbox
SandboxSet probes 与 autoPausePolicy --> 池 Sandbox --> claimed Sandbox
claimed Sandbox --> checkTimers（pauseTime）
claimed Sandbox --> handleAutoPause（autoPausePolicy）
claimed Sandbox --> gateway（OnIngressTraffic）
claimed Sandbox --recycle--> 池 Sandbox
```

- Claim 只负责把调用方意图写进 Sandbox spec
- 暂停与恢复的执行仍完全属于现有 Sandbox 控制器与 gateway
- 池默认仍由 SandboxSet 声明；recycle 仍从 SandboxSet 恢复策略并清空 deadline

### 可见接口

`SandboxClaimSpec` 增加两个可选指针字段，语义对齐已有 `shutdownTime`：

- `pauseTime *metav1.Time`：省略或 nil 表示不 overlay `Sandbox.spec.pauseTime`
- `autoPausePolicy *AutoPausePolicy`：省略或 nil 表示不 overlay，保留池上已有策略

不引入新的策略类型。`AutoPausePolicy` 的字段、默认值与跨字段规则与 Sandbox / SandboxSet 相同。

### 写入规则

对每一次成功 claim 的 Sandbox：

1. **一次性。** overlay 只发生在该 Sandbox 被 claim 成功的那一次更新里。Claim 进入 Completed 后，再改 Claim spec 不会修改已 claim 的 Sandbox。
2. **缺省继承。** 两个字段均为 nil 时，Sandbox 保留 claim 前状态：池成员的 `autoPausePolicy` 来自 SandboxSet，`pauseTime` 在池上本为空。
3. **整份替换。** `autoPausePolicy` 非 nil 时，用 Claim 中的整份策略替换 Sandbox 上的策略，不做字段级 merge。只想打开 `OnIngressTraffic` 且保留池里的 idle/cron 规则时，调用方必须在 Claim 中写出完整策略。
4. **deadline 成对写入。** 若 Claim 指定了 `pauseTime` 或 `shutdownTime` 中的任一个，则按 Claim 当前值同时设置 Sandbox 的这两个字段：Claim 上为 nil 的那一侧在 Sandbox 上为清空。两个都不指定则不改 Sandbox 的 deadline。
5. **不推导删除时间。** `pauseTime` 到期只暂停；是否删除仍只看 `shutdownTime`。需要「暂停后再删除」时，调用方同时给出两个绝对时间。
6. **批次一致。** 同一 Claim 一次 reconcile 中成功 claim 的多个副本带上相同的 overlay。
7. **probes 按名合并。** claim `probes` 非 nil 时，与候选 Sandbox 上的 `spec.probes` 按名合并：同名项以 claim 版本替换（池序位置不变），新名追加在池序之后；claim 未带 probes 则不动。每个候选 Sandbox 的合并集合不得超过 Sandbox 的 probes 上限（16）；Claim 级合并超限与候选兼容性检查见「校验与失败」。与单值字段 `autoPausePolicy` 的整份替换不同，probes 是集合类配置，与 labels/annotations 一样按 key 合并；取舍见「Claim 自带 probes」。

### 校验与失败

Claim 在构建 claim 选项时校验 `autoPausePolicy` 与 `probes`：

- 使用与 SandboxSet admission 相同的校验规则
- claim `probes` 列表自身需通过 `ValidateProbes`（名字唯一、exec-only、name 可作 condition type，与 SandboxSet 相同）
- 被引用的 probe 名以「当前 SandboxSet.spec.probes 与 claim.probes 按名合并后的集合」为准（模板上的 probes 对运行中的池成员无效，与现有复制规则一致）；claim 未带 policy 时无需重校验池策略——合并只会增加 probe 名，池策略的引用不会失效
- 当前 `SandboxSet.spec.probes` 与 `claim.probes` 按名合并后的集合超过 16 时，Claim 以 `InvalidClaimSpec` 完成：提前给出清晰错误，而不是等到 sandbox update 被 apiserver 拒绝
- 通过 Claim 级校验后，领取时仍按每个候选 Sandbox 自身的 `spec.probes` 复核，但扣除 claim 自带的 probe 名：claim 自带的 probe 不要求候选预先声明，领取时合并写入；滚动更新中的旧候选若缺少其余被引用的 probe，会留在池中并等待兼容候选，不会写入无法执行的策略
- 候选 Sandbox 自身已有的 probes 与 claim probes 合并后超过 16 时，该候选会被跳过并按无可用 Sandbox 重试；`createOnNoStock` 的新建路径也将此情况作为可重试的 `NoAvailableError`，不将候选级超限归为 `InvalidClaimSpec`
- 仅含 `OnIngressTraffic`、不含 probe 规则的策略合法
- 空策略（没有任何 pause/resume 规则）、无法编译的 `messageRegex`、引用不存在的 probe 等均非法

非法时的可观察结果与现有保留身份键校验相同：

- 不再继续 claim
- Claim 进入 Completed
- `status.conditions[type=Completed].reason` 为 `InvalidClaimSpec`
- 发出 Warning 事件 `InvalidClaimSpec`
- 本轮之前已经 claim 成功的 Sandbox 保持已写入状态，不会回滚

候选缺少当前合法策略所引用的 probe，或候选与 claim probes 合并后超过 16，都是池容量暂时不匹配，不是 Claim spec 非法：本轮不领取该候选，并按现有无可用 Sandbox 路径重试；`createOnNoStock` 开启时，若当前 SandboxSet 能形成兼容集合，可从其中创建兼容实例。

apiserver 仍接受字段形状合法的对象；交叉规则不靠新 webhook。若非法策略被绕过写到了 Sandbox，Sandbox 侧既有 `ProbeValid=False` 仍会拒绝 probe 驱动暂停，但这不是 Claim 的主路径。

### 与现有暂停机制的关系

写入完成后，Sandbox 上的行为与今天直接编辑 Sandbox spec 相同：

- `pauseTime` 由 `checkTimers` 无条件执行，不依赖 `AutoPauseController`
- probe 驱动的 pause/resume 仍要求该 gate 打开，且规则引用的 probe 实际存在于 Sandbox spec（本设计下即合并后的 probes；sandbox 控制器对运行中的 Pod patch `kruise.io/podprobe`，`spec.probes` 变化在 Running 状态即生效）
- `OnIngressTraffic` 仍由 gateway 执行，不打开 probe 决策循环
- `pauseTime` 与 `autoPausePolicy` 并存：谁先到期谁暂停；probe 仍报 active 时 `pauseTime` 仍会暂停。需要 probe 最终裁决的调用方不要设 `pauseTime`

Claim 写入策略不打开 feature gate。gate 关闭时，策略仍出现在 Sandbox spec 上，但 probe 注入与 probe 决策不运行；`pauseTime` 仍然生效。

### 兼容与升级

- 既有 Claim 不写这两个字段：行为与现在完全相同
- 已 claim、且调用方未 overlay 策略的 Sandbox 继续使用池策略
- recycle 后的池成员恢复为 SandboxSet 上的 `probes` 与 `autoPausePolicy`，`pauseTime` 被清空；下一次 claim 再按新 Claim overlay
- 不改变 E2B HTTP；E2B 继续用 bool + 相对秒数，Claim 使用绝对时间

### 可观察例子

**继承池策略。** Claim 省略两个新字段。claim 后的 Sandbox 带有 SandboxSet 的 `autoPausePolicy`，`pauseTime` 为空。若池策略含 idle probe 且 gate 打开，idle 计时从 `claim-timestamp` 起算，不会因预热期间的 idle 立刻暂停。

**仅定时暂停。** Claim 只设 `pauseTime`，不设 `autoPausePolicy` 与 `shutdownTime`。Sandbox 得到该 `pauseTime`，保留池策略，`shutdownTime` 仍为空。到期后暂停且不被删除。

**仅覆盖策略。** 池有 idle 规则。Claim 给出只含 `resume.onIngressTraffic` 的 `autoPausePolicy`。Sandbox 上的 idle 规则被替换掉，只留下流量唤醒。调用方若还要 idle 规则，必须在 Claim 策略里一并写出。

**非法引用。** SandboxSet 没有名为 `Active` 的 probe，Claim 的 pause 规则引用 `Active`。Claim 完成，reason=`InvalidClaimSpec`，没有任何新的 Sandbox 被这次失败的选项构建 claim 出来。

**需要 probe 最终裁决。** Claim 只设 `autoPausePolicy.pause.whenProbedIdleState`，不设 `pauseTime`。到期行为只由 probe 决定。

**两种触发都设。** Claim 同时设 `pauseTime` 与 idle 策略。Sandbox 在 probe 仍为 active 时若已过 `pauseTime`，仍会暂停。

## 备选与取舍

- **Claim 上使用 E2B 的 `autoPause` bool。** 与已有绝对时间 `shutdownTime` 不一致，还要把相对秒数和 retention 算术搬进 Claim。放弃。
- **对 `OnIngressTraffic` 做字段级 merge。** 对「只开唤醒」更省事，但声明式 CR 无法看出最终策略，且与 SandboxSet 整份复制不一致。放弃；完整策略由调用方写出。
- **Claim 自带 `probes`。** 已实现，按名合并语义与取舍见「Claim 自带 probes」。
- **新建 Claim webhook。** Sandbox 本身也没有 webhook；交叉校验依赖 SandboxSet，适合走已有 claim 控制器失败路径。

## 风险

- 整份替换会清掉调用方没抄上来的池规则。这是刻意的可见性，不是静默 merge。
- `pauseTime` 与 probe 策略并存时，定时器可以打断仍在工作的 Agent。与现行 Sandbox 提案一致，由调用方选择不要同时设置。
- `AutoPauseController` 默认关闭：只 overlay 策略而不开 gate 时，probe 路径不会运行，容易被理解成「Claim 没生效」。probe 路径看 gate，`pauseTime` 不看 gate。

## Claim 自带 probes

已随本设计实现：`SandboxClaim.spec.probes` 可选，按名合并到候选 Sandbox 的 `spec.probes`。

### 动机

probe 驱动策略要求被引用的 probe 预先声明在目标 SandboxSet 上。当不同租户需要不同的探测逻辑（不同 exec 命令、不同探测周期）时，SandboxSet 必须枚举全部 probe 变体（`MaxItems=16`），且所有池成员无差别执行全部 probes——绝大多数 claim 用不到也在消耗资源。Claim 自带 probes 可让每个 claim 只声明并执行自己需要的探测。

### 技术基础（当前代码已具备的支撑）

- sandbox 控制器的 `EnsureProbe` 对运行中的 Pod patch `kruise.io/podprobe` annotation，`spec.probes` 变化在 Running 状态即生效；probe 结果按 `spec.probes` 同步增删，上一个 claim 遗留的 probe condition 会被移除
- recycle 的 `resetSandboxForPool` 已将 `spec.probes` 与 `spec.autoPausePolicy` 从 SandboxSet 原样还原，claim 写入的 probes 不会泄漏给下一个租户

### 实现：按名合并（merge），不整份替换

与 SandboxSet 复制到候选上的 probes **按名合并**（`+listType=map` 的天然语义）：

- Claim 同名 probe 覆盖池版本：租户可定制探测参数（命令、周期），池序位置不变
- Claim 新名 probe 追加在池序之后
- Claim 自带的 probes 需通过 `ValidateProbes`；SandboxSet 的 probes 已在 admission 时按同一规则校验，合并后的集合再检查 `MaxItems=16`

最终组合自洽校验（控制器 claim 时已持有 SandboxSet）：

- `finalProbes = merge(sandboxSet.spec.probes, claim.probes)`（Claim 侧同名优先）
- `ValidateProbes(claim.probes)` + `ValidateAutoPausePolicy(claim.autoPausePolicy, finalProbes)`
- 合并超限以 `InvalidClaimSpec` 完成

领取过滤沿用现有机制，从策略派生并扣除 Claim 自带项：`required = PolicyProbeNames(finalPolicy) − names(claim.probes)`，即 claim 自带的 probe 不要求候选预先声明。落盘前由 `modifyPickedSandbox` 深拷贝合并到 `spec.probes`（含 `createOnNoStock` 新建路径），无持久化中间态，合并结果不 alias claim 或池的 spec。

### merge 与 replace 的取舍

备选的整份替换语义（claim.probes 非 nil 时全量覆盖 Sandbox probes）被放弃：

- **管理员预配 + 使用方追加是预期形态。** SandboxSet 的 probes 由平台管理员声明（安全审计、合规探测等平台级配置），claim 的使用方在其上追加自己的探测。整份替换会让租户的 claim 移除平台探测直到 recycle 还原——claim 期间形成检测盲区，等于使用方决定了平台配置的命运
- **probe 是通用机制而非 policy 的附属品。** `Probe` 的契约是「probe 本身不定义语义，消费者定义」；第二个消费者已存在（probe 结果 mirror 到 `SandboxStatus.Conditions` 供可观测），未来可能有更多。整份替换的「探测集随 policy 整体切换」只在「policy 是唯一消费者」时成立
- **与 claim 的 metadata 语义一致。** claim 的 labels/annotations 是按 key 合并（`MergePodLabels`/`MergePodAnnotations`）而非全量替换，因为对象上已有其他来源的数据；probes 同属集合类配置。`autoPausePolicy` 是单值语义才整份替换——单值与集合天然不同的合并规则

merge 的代价及对策：

- **最终探测集需合并后才可推断**：claim 单边不可见。当前校验错误和 `InvalidClaimSpec` 事件只报告具体校验错误或上限，不列出完整合并结果；且「引用池 probe」本来就是运行时依赖（Claim 策略引用的 probe 名不在当前 SandboxSet 与 Claim 合并后的集合中时，以 `InvalidClaimSpec` 完成；仅候选不兼容时才重试）
- **同名覆盖可能遮蔽池探测**：当前 claim 校验不会为被覆盖的池 probe 名单独发事件，覆盖关系需从 Claim 与池配置的内容推断
- **合并叠加可能超限**：Claim 级与候选级都对合并结果检查 `MaxItems=16`

### 已关闭的开放问题

- claim 完成时新 probe 尚未产出首次结果，`thresholdDuration` 从首次结果起算：与现有 Sandbox probe 语义一致，WaitReady 不额外涵盖 probe 就绪
- `createOnNoStock` 路径：合并在 claim 落盘前于内存完成（`modifyPickedSandbox`），不存在「复制池 probes 再合并」的持久化中间态
- E2B HTTP：不暴露 probe 配置，保持 SandboxClaim 独有能力
