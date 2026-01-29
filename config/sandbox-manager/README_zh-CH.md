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
