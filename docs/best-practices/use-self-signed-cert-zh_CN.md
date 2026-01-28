# 最佳实践：通过自签名证书以 E2B 协议访问 sandbox-manager

本文给出了一种通过自签证书访问 sandbox-manager 的最佳实践，包括如何通过一个优化的脚本创建证书、如何将证书安装到集群中、如何配置
E2B 客户端。

## 前提条件

1. 集群中已经完成 sandbox-manager 的安装
2. 确保 sandbox-manager 的 ingress 已经生效（具备 entrypoint），您可以通过以下命令确认，Address 列应当不为空：

```shell
$ kubectl get ingress sandbox-manager -n sandbox-system
```

## 步骤一：创建证书

通过脚本 [generate-certificate.sh](../../hack/generate-certificates.sh) 创建自签名证书。您可以通过以下命令查看脚本的使用方法。

```shell
$ bash generate-certificates.sh --help

Usage: generate-certificates.sh [OPTIONS]

Options:
  -d, --domain DOMAIN     Specify certificate domain (default: your.domain.com)
  -o, --output DIR        Specify output directory (default: .)
  -D, --days DAYS         Specify certificate validity days (default: 365)
  -h, --help              Show this help message

Examples:
  generate-certificates.sh -d myapp.your.domain.com
  generate-certificates.sh --domain api.your.domain.com --days 730
```

完成证书生成后，您会得到以下文件：

- fullchain.pem：服务器证书公钥
- privkey.pem：服务器证书私钥
- ca-fullchain.pem：CA 证书公钥
- ca-privkey.pem：CA 证书私钥

该脚本会同时生成单域名（your.domain）与泛域名（*.your.domain）证书，兼容原生 E2B 协议与 OpenKruise 定制 E2B 协议。

## 步骤二：安装证书

通过以下命令将服务器证书挂载到集群的 Ingress 上：

```shell
$ kubectl create secret tls sandbox-manager-tls \
          --cert=fullchain.pem \
          --key=privkey.pem -n sandbox-system
```

证书的生效可能会有一些延迟，具体生效效率由您的 ingress 控制器决定。

## 步骤三：配置客户端信任

客户端需要设置环境变量 `SSL_CERT_FILE` 为步骤一中生成的 CA 公钥（ca-fullchain.pem）文件路径：

```shell
$ export SSL_CERT_FILE=/path/to/ca-fullchain.pem

# 如果你需要通过定制 SDK 的方式使用单域名接入，还需要配置以下环境变量
$ export REQUESTS_CA_BUNDLE=/path/to/ca-fullchain.pem
```