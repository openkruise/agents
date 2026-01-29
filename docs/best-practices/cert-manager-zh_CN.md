# 最佳实践：使用 cert-manager 管理 sandbox-manager 自签证书

本文提供了一种使用 cert-manager 来管理和部署 sandbox-manager 自签证书的最佳实践。

## 前提条件

1. 集群中已安装 sandbox-manager
2. 确保具备 kubectl 命令行工具并具有相应权限

## 步骤一：安装 cert-manager

如果您还没有安装 cert-manager，请参考 [官方文档](https://cert-manager.io/docs/installation/) 进行安装。

## 步骤二：通过 cert-manager 自动管理证书

1. 将 [cert-manager.yaml](cert-manager.yaml) 中的 "*.your.domain.com" 与 "your.domain.com" 替换为你的域名。
2. 将配置添加到 Kubernetes 集群中：`kubectl apply -f cert-manager.yaml`

## 步骤三：验证证书状态

检查证书是否正确创建和颁发：

```bash
kubectl get certificates -n sandbox-system
kubectl describe certificate sandbox-manager-ingress-cert -n sandbox-system
kubectl describe secret sandbox-manager-tls -n sandbox-system
```

检查 Ingress 状态：

```bash
kubectl get ingress sandbox-manager -n sandbox-system
kubectl describe ingress sandbox-manager -n sandbox-system
```

## 步骤四：配置客户端信任

由于您使用的是自签名证书，客户端需要信任根 CA 证书。

### 4.1 获取 CA 证书

```bash
kubectl get secret sandbox-ca-key-pair -n sandbox-system -o jsonpath='{.data.tls\.crt}' | base64 -d > ca.crt
```

### 4.2 配置客户端

客户端需要设置环境变量 `SSL_CERT_FILE` 为获取的 CA 证书路径：

```bash
export SSL_CERT_FILE=/path/to/ca.crt

# 如果你需要通过定制 SDK 的方式使用单域名接入，还需要配置以下环境变量
$ export REQUESTS_CA_BUNDLE=/path/to/ca.crt
```
