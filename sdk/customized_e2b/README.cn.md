# Customized E2B SDK patch

这个 Python 库通过 patch E2B 客户端，将原生的 E2B 协议转换为 OpenKruise Agents 私有协议，从而简化 sandbox-manager 的部署。

## 问题

E2B SDK 通过以下协议请求后端：

| 协议                      | 说明   | 示例                                     |
|-------------------------|------|----------------------------------------|
| api.E2B_DOMAIN          | 管控接口 | api.e2b.dev                            |
| <port>-<sid>.E2B_DOMAIN | 沙箱接口 | 49999-i37sc83s52e2cv85h636jjgs.e2b.dev |

同时，E2B 在 SDK 中硬编码，强制使用 HTTPS。

在我们的实践中，发现在 K8s 场景，这种协议存在以下问题：

1. 需要配置泛域名解析到管控服务（sandbox-manager），无法使用 hosts 等方法解析。
2. 需要使用昂贵的通配符证书。

上述问题同时使得部署一套兼容 E2B 的后端服务具有相当高的门槛：不仅提高了用户使用成本，还难以自动化地搭建一个 E2E 测试环境。

## 使用方法

要求：

- e2b >= 2.8.0

```python
from kruise_agents.e2b import patch_e2b
from e2b_code_interpreter import Sandbox

patch_e2b() # patch sdk

if __name__ == "__main__":
    with Sandbox.create() as sbx:
        sbx.run_code("print('hello world')")
```

## sandbox-manager 的几种推荐接入方式

私有协议与原生协议的对比：

> 假设您配置的 E2B_DOMAIN 为 your.domain

| 原生协议                     | 私有协议                            | 
|--------------------------|---------------------------------|
| api.your.domain          | your.domain/kruise/api          | 
| <port>-<sid>.your.domain | your.domain/kruise/<sid>/<port> |

### 1. 使用原生协议接入
> 这是最标准、原生的接入方式，配置门槛也最高，一般需要手动部署。

1. 客户端配置环境变量：
    ```bash
    export E2B_DOMAIN=your.domain
    export E2B_API_KEY=<your-api-key>
    ```
2. 将泛域名 `*.your.domain` 解析到 sandbox-manager ingress 接入点
3. 安装泛域名证书 `*.your.domain`

### 2. 使用私有协议集群外 HTTPS 接入
> 这种方式可以降低部署门槛，能够结合 cert-manager 等组件能够半自动部署。

1. 客户端配置环境变量：
    ```bash
    export E2B_DOMAIN=your.domain
    export E2B_API_KEY=<your-api-key>
    ```
2. patch 客户端：
    ```python
    from kruise_agents.e2b import patch_e2b
    patch_e2b()
    ```
3. 将单域名 `your.domain` 解析到 sandbox-manager ingress 接入点
4. 安装单域名证书 `your.domain`

### 3. 使用私有协议集群内接入
> 这种方式可以快速自动部署，不需要配置域名与证书，推荐仅 E2E 测试等场景或严谨评估后使用。

1. 确保客户端与 sandbox-manager 在同一个集群内。
2. 客户端配置环境变量：
    ```bash
    export E2B_DOMAIN=sandbox-manager.sandbox-system.svc.cluster.local
    export E2B_API_KEY=<your-api-key>
    ```
3. patch 客户端并关闭 https：
    ```python
    from kruise_agents.e2b import patch_e2b
    patch_e2b(False)
    ```