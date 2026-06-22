# 使用 Kustomize 部署 sandbox-gateway

这个目录包含了预配置好的 sandbox-gateway 部署，您可以通过这些文件在集群中快速部署网关组件。

## 概述

sandbox-gateway 是一个基于 Envoy 的网关，使用 Golang 过滤器进行沙箱路由。它作为代理层，将传入的请求路由到相应的沙箱 Pod。

## 1. 构建

使用 make 命令从源码构建最新的 sandbox-gateway 镜像：

```shell
make docker-build-sandbox-gateway
```

如果需要部署到一个真实的 K8s 集群，请修改为合适的 tag 并推送到您的远程镜像仓库中。

## 2. 部署

使用 kustomize 将 sandbox-gateway 部署到您的集群：

```shell
kubectl create ns sandbox-system # 如果命名空间不存在
kustomize build config/sandbox-gateway | kubectl apply -f -
```

或者使用 kubectl 内置的 kustomize：

```shell
kubectl apply -k config/sandbox-gateway
```

## 3. 配置

部署的组件包括：

- **Deployment**: 运行带有 sandbox-gateway 过滤器的 Envoy 代理
- **Service**: 在 10000 端口暴露网关
- **ConfigMap**: 包含 Envoy 配置
- **ServiceAccount**: 网关 Pod 使用的服务账户
- **RBAC**: 用于访问沙箱资源的 ClusterRole 和 ClusterRoleBinding

## 4. 自定义

要自定义部署，您可以创建一个 patch 文件并在 kustomization.yaml 中引用它，或者直接修改资源文件。

常见的自定义项包括：
- 镜像仓库和标签
- 资源限制和请求
- 副本数量
- Envoy 配置（超时、熔断器等）
