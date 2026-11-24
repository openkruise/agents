# 使用 Kustomize 部署测试 sandbox-manager

这个目录包含了针对功能测试场景预配置好的 sandbox-manager 部署，您可以通过这些文件在集群中快速部署一个测试环境。

## 1. 构建

通过 [Dockerfile](../../dockerfiles/sandbox-manager.Dockerfile) 从源码构建最新的 sandbox-manager 镜像

```shell
docker build -t sandbox-manager:latest .
```

如果需要部署到一个真实的 K8s 集群，请修改为合适的 tag 并推送到您的远程镜像仓库中。

## 2. 部署

1. 编辑两个 patch 文件，进行一些基础的自定义：
    1. [configuration_patch.yaml](configuration_patch.yaml)
    2. [ingress_patch.yaml](ingress_patch.yaml)
2. 通过以下命令生成完整的 yaml 文件并完成部署：
    ```shell
    kustomize build config/sandbox-manager > bin/sandbox-manager.yaml
    kubectl apply -f bin/sandbox-manager.yaml
    ```

## 使用 Agentio 管理 E2B 出口策略

默认情况下，sandbox-manager 使用 `TrafficPolicy` 保存 E2B 出口网络的允许和拒绝列表。
要启用注解模式，请先升级所有 sandbox-manager 副本，再使用 `--e2b-network-annotation=true` 启动。
在 Agentio 中启用 Sandbox 策略发现，并将受管出站流量路由到其出口网关。
此后，从池中领取或从快照克隆的新沙箱将 E2B 网络配置保存在 `agents.kruise.io/e2b-network` 注解中。
已有沙箱继续使用创建时选定的策略存储方式。
注解模式下，客户端可以同时传入 `network.rules` 和 `e2b.agents.kruise.io/security-profile` 元数据，后续更新 E2B 网络配置时会保留原生 profile。
默认模式使用旧 `security-rules` 注解保存 `network.rules` 中的 HTTP 请求头改写规则，与原生 profile 同时配置时会返回 HTTP 400。

按 SDK 示例创建沙箱后，用 Sandbox 在 Kubernetes 中的命名空间和资源名称读取两种策略。[SDK 配置示例](../../sdk/customized_e2b/README.md#outbound-network-policy)

```shell
kubectl get sandboxes.agents.kruise.io "$SANDBOX_NAME" -n "$NAMESPACE" \
  -o go-template='e2b-network={{index .metadata.annotations "agents.kruise.io/e2b-network"}}{{"\n"}}security-profile={{index .metadata.annotations "agents.kruise.io/security-profile"}}{{"\n"}}'
```

注解模式下，输出的每一行对应一个注解的 JSON 值。

```text
e2b-network={"allowInternetAccess":false,"network":{"allowOut":["192.0.2.10"]}}
security-profile={"tcpRules":[{"name":"deny-ssh","match":[{"ports":[22]}],"action":"Deny"}]}
```

注解模式下，`e2b-network` 中的 `allowInternetAccess: false` 会拒绝未命中前序规则的 TCP 流量，Sandbox 和 Pod 上的 `agents.kruise.io/allow-internet-access` 标签仍为 `"true"`。
网络更新时各字段的替换、清空和保留行为见 proposal。[读取和更新网络策略](../../docs/proposals/20261121-e2b-inline-security-profile.md#reading-and-updating-the-network-policy)

## API Key Quota

API key 可以携带静态配额，维度包括 `sandbox.count`、`limits.cpu`、`limits.memory`，作用域为 `running` 或 `all`。
公开 API、Kubernetes Secret、MySQL 存储层都使用 canonical `QuotaSpec` limits 形状，例如
`{"limits":[{"dimension":"sandbox.count","scope":"running","limit":10},{"dimension":"limits.cpu","scope":"running","limit":8000},{"dimension":"limits.memory","scope":"running","limit":16384},{"dimension":"sandbox.count","scope":"all","limit":50}]}`。
动态限额只依赖 Redis。如果
`--quota-redis-addr` 为空，或者 Redis 已配置但暂时不可用，sandbox-manager 会有意 fail-open：受限 key
仍然可以创建和存储，但 create 请求会暂时不执行动态限额。相关 fail-open 事件会通过 metrics 和日志暴露出来。

如果 Redis 需要认证，请通过 Kubernetes Secret 注入 `QUOTA_REDIS_USERNAME` 和
`QUOTA_REDIS_PASSWORD`，不要把凭据直接写进 deployment patch。

使用 MySQL key storage 且设置 `--e2b-key-storage-disable-schema-auto-update=true` 时，启动期 schema check 会要求
`team_api_keys.quota` 列已经存在。启动前请先应用 `hack/mysql-schema.sql` 中的手动迁移：

```sql
ALTER TABLE team_api_keys ADD COLUMN quota JSON DEFAULT NULL AFTER created_by_uid;
```
