# 资源访问隔离（Resource Access Isolation）使用指南

本文面向两类读者：

- **平台管理员**：负责部署 sandbox-manager / agent-sandbox-controller，决定是否启用隔离模式；
- **API 用户**：通过 E2B 兼容 API / SDK 创建、快照、克隆沙箱并挂载存储的租户。

设计契约见 [`docs/proposals/20261029-resource-access-modes.md`](../proposals/20261029-resource-access-modes.md)。本特性已在真实 ACK 集群完成兼容性回归与 12 项隔离攻击用例验证。

## 1. 特性概述

资源访问隔离是一组**可选启用（opt-in）**的安全加固，封堵三类跨租户攻击路径：

| 风险 | 未加固时的攻击方式 | 加固后的行为 |
|------|--------------------|--------------|
| 快照越权克隆 | 用他人 checkpoint ID 克隆出含其数据的沙箱 | 克隆前校验 checkpoint 属主，非属主一律拒绝 |
| 快照 ID 碰撞 | 构造重复 checkpoint ID，诱导系统克隆到错误的快照 | ID 歧义时 fail-closed 拒绝 |
| 存储挂载劫持 | 通过挂载 attributes 覆盖 PV 的 bucket / 凭证提供方等，把挂载指向攻击者控制的存储或凭证 | 整批预检：仅允许受限的身份选择器，其余属性必须与 PV 模板完全一致 |
| 生命周期策略绕过 | 利用 refresh/reinit/recovery 路径按进程默认策略处理，绕过交付时的隔离约束 | 每个交付持久化策略标记，生命周期路径只读持久化标记，非法标记 fail-closed |

**默认不启用**。不配置开关时，所有行为与历史版本完全一致（兼容性已在测试集群回归验证）。

### 1.1 安全边界：加固与 IdP 的分工

存储租户权限的**裁决方始终是外部 IdP/存储服务**，加固不替代它。IdP 对未注册身份、越权 scope 的拒绝在加固前就存在；加固的增量在于封堵**两类 IdP 拦不住的 legacy 攻击**：

| 攻击 | 为什么 IdP 拦不住 | 加固后 |
|------|--------------------|--------|
| `authType` 降级为静态凭证 | 挂载走共享 Secret 分支，**根本不经过 IdP**，无 token 签发 | isolated 强制 agent-identity，静态分支不存在 |
| `url` 等路由属性劫持 | 身份/bucket/provider 都合法，IdP **正确签发**限定凭证，但凭证被投递到攻击者控制的 endpoint | 非身份选择器属性必须与 PV 模板逐字一致，改不了 |

因此：预检通过 ≠ 授权通过（IdP 仍可能拒绝）；IdP 拒绝也不代表预检多余——两者是纵深防御，不是替代关系。

## 2. 开关与启用方式（平台管理员）

### 2.1 开关名称

```
--resource-access-mode=<trusted-domain|isolated>
```

| 取值 | 含义 |
|------|------|
| `trusted-domain`（默认） | 信任域模式，保持旧行为，不做属主隔离 |
| `isolated` | 隔离模式，启用上文全部加固 |

sandbox-manager 与 agent-sandbox-controller 均提供该 flag，建议两者配置一致。

约束：

- `isolated` **必须**搭配 `--e2b-enable-auth=true`（E2B API Key 认证）。未开启认证时 manager 启动直接失败，不会静默降级。
- 未知取值会导致启动失败（fail-closed），不存在"配错就回退兼容模式"的行为。

### 2.2 启用示例

以 Deployment 方式部署的 sandbox-manager 为例：

```bash
# 在容器 args 中追加开关并滚动更新
kubectl patch deploy sandbox-manager -n sandbox-system --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--resource-access-mode=isolated"}]'
kubectl rollout status deploy/sandbox-manager -n sandbox-system
```

controller 同理追加 `--resource-access-mode=isolated`。

### 2.3 关闭 / 回退

移除 `--resource-access-mode=isolated` 参数并滚动即可回到默认信任域模式。
注意：**已按 isolated 交付的沙箱不会因此降级**（见 §6 策略标记）。

## 3. 两种模式的行为差异（API 用户）

| 操作 | trusted-domain（默认） | isolated |
|------|------------------------|----------|
| 创建 / pause / resume / 超时续期 | 不变 | 属主本人操作不变 |
| 从 checkpoint 克隆 | 不校验属主 | 仅属主可克隆；跨用户访问返回 404 |
| checkpoint ID 重复 | 取第一个匹配 | 拒绝（404） |
| CSI 动态挂载 | 允许 attributes 透传（保留键除外） | 仅允许身份选择器类属性，且 PV 必须为 agent-identity 认证 |
| metadata 携带平台保留键 | 按既有黑名单拦截 | 额外拦截存储授权标记伪造 |
| 跨用户访问他人沙箱（pause 等） | 依赖命名空间/路由隔离 | 同样不可见（404） |

对**合规使用的属主用户**，isolated 模式下常规 E2B SDK 用法（创建、连接、pause、resume、本人快照克隆）无需任何改动。

## 4. 快照与克隆

### 4.1 属主规则

- checkpoint 的属主 = 创建它的 API Key 对应的用户 ID（记录在 checkpoint 的 `agents.kruise.io/owner` annotation）。
- isolated 模式下（或 checkpoint 本身以 isolated 交付时），克隆请求的用户必须与属主一致，否则拒绝。
- 拒绝统一表现为 **404 "Resource not found" / "Template or Checkpoint not found"**，不泄露资源是否存在、属主是谁。

### 4.2 用法（与默认模式相同）

```bash
# 1) 暂停沙箱生成快照
curl -X POST -H "X-API-Key: $KEY" $API/sandboxes/$SANDBOX_ID/pause

# 2) 用快照 ID（Checkpoint status.checkpointId）克隆新沙箱 —— 必须使用同一属主的 Key
curl -X POST -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d '{"templateID":"<checkpointId>","timeout":300}' $API/sandboxes
```

### 4.3 注意事项

- 快照 ID 必须全局唯一（同命名空间内）。平台侧若出现重复 ID，克隆会被拒绝而不是随机选一个——这是有意的 fail-closed 设计。
- 跨团队共享快照不属于本特性支持范围；isolated 的语义就是"谁创建谁使用"。

## 5. CSI 存储挂载

### 5.1 isolated 模式的硬性要求

1. **PV 认证方式必须是 `agent-identity`**（PV `spec.csi.volumeAttributes.authType=agent-identity`）。静态 Secret 凭证的 PV 一律拒绝——共享静态凭证无法建立按租户的授权。
2. **仅支持 OSS / NAS 两类 driver**：`ossplugin.csi.alibabacloud.com`、`nasplugin.csi.alibabacloud.com`。
3. PV 必须处于正常状态（CSI 类型、未在删除中）。
4. 挂载请求必须来自已认证交付的属主身份（namespace + owner），不接受请求属性自证身份。
5. **必须请求 agent 身份**：metadata 携带 `security.agents.kruise.io/agent-name`，且该 agent 身份**已在 IdP 侧注册并授权**。未注册的身份会在 token 签发阶段被 IdP 拒绝（如 404），整个创建请求回滚、沙箱被删除——这是实测验证过的行为，不是平台预检报错。

### 5.2 attributes 规则

保留键在任何模式下都禁止传入：`path`、`authType`、`csi.storage.k8s.io/` 前缀。

isolated 模式下，其余属性分两类：

| 类别 | 允许的属性 | 规则 |
|------|-----------|------|
| 身份选择器（可按请求选择） | OSS：`bucket`、`bucketSpace`、`bucketSpacePrefix`、`region`、`kmsKeyId`；NAS：`server`；两者：`sandboxCredProviderName` / `credentialProviderName` | 取值必须是合法的单值标识（禁止通配符、路径分隔符、空白等）；NAS server 必须是完整的 AP 域名 `ap-xxx.*.nas.aliyuncs.com` |
| 其他所有属性 | 无 | 必须与 PV 模板**完全一致**（包括键是否存在）；不允许新增、修改或删除 |

其他约束：

- `subPath` 会并入 PV 模板路径做校验，禁止 `.` / `..` 路径逃逸。
- OSS 必须提供 `bucket` 或 `bucketSpace`+`bucketSpacePrefix` 之一；选择 `kmsKeyId` 即隐含 KMS 加密，不允许清除模板已有的 KMS 约束。
- `credentialProviderName` 与 `sandboxCredProviderName` 同时出现时必须一致。
- **整批预检**：一次请求中的多个挂载要么全部通过，要么全部拒绝，不会出现部分挂载生效的中间态。

> 重要：平台预检只保证"挂载上下文与模板一致、身份选择器合法"。**租户对所选 bucket / 凭证提供方是否有权限，由下游 IdP 与存储服务独立鉴权**，预检通过不等于授权通过。

### 5.3 用法示例

普通挂载（推荐，SDK `volumeMounts` 字段）：

```bash
curl -X POST -H "X-API-Key: $KEY" -H "Content-Type: application/json" -d '{
  "templateID": "my-template",
  "timeout": 300,
  "metadata": {"security.agents.kruise.io/agent-name": "my-registered-agent"},
  "volumeMounts": [{"name": "pv-oss-shared", "path": "/mnt/data"}]
}' $API/sandboxes
```

带身份选择器的挂载（metadata 扩展键 `e2b.agents.kruise.io/csi-volume-config`）：

```json
{
  "templateID": "my-template",
  "metadata": {
    "security.agents.kruise.io/agent-name": "my-registered-agent",
    "e2b.agents.kruise.io/csi-volume-config": "[{\"pvName\":\"pv-oss-shared\",\"mountPath\":\"/mnt/data\",\"subPath\":\"tenant-a/workspace\",\"attributes\":{\"bucket\":\"tenant-a-bucket\",\"credentialProviderName\":\"tenant-a-provider\"}}]"
  }
}
```

上例中 `bucket` / `credentialProviderName` 属于身份选择器，允许按租户选择（前提是 IdP 侧已为该租户授权）；若试图修改 `url`、`otherOpts` 等模板属性，整个请求会被拒绝。

### 5.4 平台生成的 storage-auth 格式（仅供查看，不可伪造）

准入成功后，平台在 **Sandbox CR** 上写入投影结果。请求字段会被映射为 IdP 契约字段：

```yaml
# kubectl get sandbox <name> -n <ns> -o yaml 中的 metadata.annotations
security.agents.kruise.io/agent-name: my-registered-agent
security.agents.kruise.io/storage-auth: '[{"credentialProviderName":"tenant-a-provider","attributes":{"bucket-name":"tenant-a-bucket","sub-path":"tenant-a/workspace"}}]'
```

注意 `bucket` → `bucket-name`、`subPath` → `sub-path` 的映射。该 annotation 是平台对 IdP 的**授权范围声明**：无身份挂载时不存在（合法状态）；有 isolated 挂载时必然存在；每次 token 签发前平台会删除旧值并重新投影，手工篡改无效。

## 6. 策略标记与模式切换语义

每个沙箱交付在创建时持久化策略标记（annotation `agents.kruise.io/resource-access-policy`）：

| 标记值 | 含义 |
|--------|------|
| `trusted-domain-v1` | 按信任域策略交付 |
| `isolated-v1` | 按隔离策略交付 |
| 无标记 | 历史存量交付，按信任域语义对待 |
| 其他/空值 | **非法，fail-closed**：相关生命周期操作拒绝执行 |

关键语义：

- 生命周期路径（初始化、token 刷新、reinit、recovery）**只读交付上的持久化标记**，不看进程当前开关。
- 因此：管理员把开关从 isolated 切回 trusted-domain，**不会**让已隔离的存量沙箱降级；反之，切到 isolated 后，存量无标记交付仍按信任域语义运行，但克隆它们时若进程处于 isolated，会强制属主校验（跨用户克隆被拒，属主不受影响）。
- 标记是平台保留 annotation，API 用户无法通过 metadata 伪造（见 §7）。
- 如需将存量交付全部收紧为隔离语义，可由平台侧批量补打 `isolated-v1` 标记（属主信息需同时完备）。

### 6.1 在哪里查看交付元数据（重要）

安全元数据**只在 Sandbox CR 上，不在 Pod 上**：

```bash
# 正确：策略标记、owner、storage-auth 都在这里
kubectl get sandbox <name> -n <ns> -o jsonpath='{.metadata.annotations}'

# Pod 上只有 owner label（供路由/归属识别）与基础设施元数据，
# 看不到 resource-access-policy / storage-auth 是设计使然，不是丢失
kubectl get pod <name> -n <ns> -o jsonpath='{.metadata.labels}'
```

原因：策略与 scope 的消费者（manager/controller/identity provider）全部从 Sandbox CR 读取；CR 是跨 pause/resume、Pod 重建的持久交付记录，Pod 只是可替换的执行产物；不下发也减少了策略状态的暴露面。凭证本身通过 runtime 初始化与凭证文件投递进沙箱，不走 Pod 元数据。

## 7. 错误信息与排查

| 现象 | 含义 | 处理建议 |
|------|------|----------|
| 404 `Resource not found`（克隆/访问他人资源时） | 属主校验拒绝或资源不可见（安全设计，不区分两者） | 确认使用资源属主的 API Key；跨租户共享请改用平台侧授权流程 |
| 400 `Template or Checkpoint not found` | 当前用户命名空间内无此模板/快照 | 检查 Key 所属团队与快照所在命名空间是否一致 |
| 400 `Forbidden metadata key [agents.kruise.io/...]` | metadata 使用了平台保留前缀（`e2b.agents.kruise.io/`、`agents.kruise.io/`、`alibabacloud.com`） | 移除保留键；策略标记与存储授权标记只能由平台写入 |
| 400 `Forbidden metadata key [security.agents.kruise.io/storage-auth]` | 试图伪造存储授权标记 | 同上 |
| 400 `isolated storage authentication requires an agent identity` | isolated 挂载遇到非 agent-identity 的 PV 或凭证提供方配置不合法 | 改用 agent-identity PV；检查 credentialProviderName 配置 |
| 404/拒绝且日志为 `resource access denied` | CSI 属性覆盖、非法身份选择器、静态凭证等预检拒绝 | 对照 §5.2 检查 attributes；服务端日志含 request_id 可关联定位 |
| manager 启动失败 `isolated resource access requires E2B authentication` | isolated 未搭配 API Key 认证 | 增加 `--e2b-enable-auth=true` |
| manager/controller 启动失败 `unsupported resource access mode` | 开关取值拼写错误 | 仅接受 `trusted-domain` / `isolated` |

## 8. 上线建议与已知限制

1. **灰度顺序**：先在测试环境以 isolated 跑通业务回归（覆盖快照/克隆属主准入、CSI 预检、策略标记持久化等用例），再在生产启用。
2. **启用前确认**：E2B 认证已开启；存储 PV 已改造为 agent-identity；下游 IdP 已按租户配置凭证提供方授权。
3. **组件覆盖**：sandbox-manager、agent-sandbox-controller 已包含加固；sandbox-gateway 的重建验证在测试轮次中跳过，生产全量上线前需补齐。
4. **端到端依赖**：快照克隆的数据面恢复依赖 ACS 快照后端，存储 token 刷新依赖 identity provider；在不具备这些组件的环境中，相关路径的安全边界由准入/预检层与单元测试保证。

## 9. FAQ

**Q：启用 isolated 后，现有 SDK 代码要改吗？**
A：属主本人的常规用法（创建/连接/pause/resume/本人快照克隆/普通 volumeMounts）不需要改。只有依赖"跨用户克隆他人快照"或"挂载时覆盖 PV 存储属性"的用法会被拒绝——这两类正是被加固封堵的攻击路径。

**Q：为什么拒绝时返回 404 而不是 403？**
A：避免向攻击者泄露"资源存在但无权访问"的信息，统一表现为不可见。

**Q：开关切回 trusted-domain 后，之前的隔离沙箱还安全吗？**
A：安全。策略随交付持久化，生命周期路径不受进程开关切换影响（§6）。

**Q：多个租户想读同一份数据怎么办？**
A：使用平台预置的共享 agent-identity PV 模板，各租户通过身份选择器（bucket/subPath/凭证提供方）选择自己的数据域，由 IdP 按租户鉴权；不要尝试共享快照或覆盖 PV 属性。

**Q：为什么 Pod 上看不到策略标记和 storage-auth？**
A：设计使然，见 §6.1。安全元数据只在 Sandbox CR 上；Pod 只有 owner label。查加固状态请 `kubectl get sandbox`。

**Q：isolated 挂载创建成功但沙箱随后被删除？**
A：典型原因是 agent-name 未在 IdP 注册或所选 scope 未获授权：平台预检通过，但 token 签发被 IdP 拒绝（如 404），创建整体回滚。请先在 IdP 侧完成身份注册与授权（§8），再查 manager 日志中的 `GetIdentityToken` 状态码。

**Q：IdP 本来就会拒绝越权，为什么还需要平台预检？**
A：见 §1.1。静态凭证降级和 endpoint 劫持两类攻击不依赖 IdP 点头（前者绕开 IdP，后者借 IdP 的合法签发钓凭证），只有平台侧锁死模板属性才能封堵。