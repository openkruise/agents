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
    1. [deployment_patch.yaml](configuration_patch.yaml)
    2. [ingress_patch.yaml](ingress_patch.yaml)
2. 通过以下命令生成完整的 yaml 文件并完成部署：
    ```shell
    kustomize build config/sandbox-manager > bin/sandbox-manager.yaml
    kubectl apply -f bin/sandbox-manager.yaml
    ```

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
