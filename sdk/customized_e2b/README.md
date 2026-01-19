# Customized E2B SDK patch

This Python library patches the E2B client, converting the native E2B protocol to the OpenKruise Agents private
protocol, thereby simplifying sandbox-manager deployment.

## Problem Statement

The E2B SDK requests the backend using the following protocol:

| Protocol                    | Description          | Example                                |
|-----------------------------|----------------------|----------------------------------------|
| api.E2B_DOMAIN              | Management interface | api.e2b.dev                            |
| \<port\>-\<sid\>.E2B_DOMAIN | Sandbox interface    | 49999-i37sc83s52e2cv85h636jjgs.e2b.dev |

Meanwhile, E2B SDK forces the use of HTTPS.

In our practice, we found that in K8s scenarios, this protocol has the following issues:

1. Requires configuring wildcard domain resolution to the management service (sandbox-manager), unable to use methods
   like hosts for resolution.
2. Requires using expensive wildcard certificates.

The above issues simultaneously make deploying a backend service compatible with E2B have a high threshold: not only
increasing user costs, but also making it difficult to automate the setup of an E2E test environment.

## Usage

Requirements:

- e2b >= 2.8.0

```python
from kruise_agents.patch_e2b import patch_e2b
from e2b_code_interpreter import Sandbox

patch_e2b()  # patch sdk

if __name__ == "__main__":
    with Sandbox.create() as sbx:
        sbx.run_code("print('hello world')")
```

## Recommended sandbox-manager integration methods

Comparison between private protocol and native protocol:

> Assuming your configured E2B_DOMAIN is `your.domain`

| Native Protocol          | Private Protocol                | 
|--------------------------|---------------------------------|
| api.your.domain          | your.domain/kruise/api          | 
| <port>-<sid>.your.domain | your.domain/kruise/<sid>/<port> |

> **VERY IMPORTANT**: The `E2B_DOMAIN` environment variable of sandbox-manager must be set to the same as the client.
> You can edit the deployment with `kubectl edit deploy -n sandbox-system sandbox-manager`

### 1. Integration using native protocol

> This is the most standard, native integration method, but also has the highest configuration threshold, generally
> requiring manual deployment.

1. Client configuration environment variables:
    ```shell
    # The E2B_DOMAIN env of sandbox-manager container should be set to the same
    export E2B_DOMAIN=your.domain
    export E2B_API_KEY=<your-api-key>
    ```
2. Resolve wildcard domain `*.your.domain` to sandbox-manager ingress endpoint
3. Install wildcard certificate `*.your.domain`

### 2. Private protocol HTTPS access from outside cluster

> This approach can reduce deployment threshold and can be semi-automatically deployed in combination with components
> like cert-manager.

1. Client configuration environment variables:
    ```shell
    # The E2B_DOMAIN env of sandbox-manager container should be set to the same
    export E2B_DOMAIN=your.domain
    export E2B_API_KEY=<your-api-key>
    ```
2. Patch client:
    ```python
    from kruise_agents.patch_e2b import patch_e2b
    patch_e2b()
    ```
3. Resolve single domain `your.domain` to sandbox-manager ingress endpoint
4. Install single domain certificate `your.domain`

### 3. Private protocol in-cluster access

> This approach enables rapid automated deployment without requiring domain and certificate configuration. Recommended
> for E2E testing scenarios only, or after rigorous evaluation.

1. Ensure client and sandbox-manager are in the same cluster.
2. Client configuration environment variables:
    ```shell
    # The E2B_DOMAIN env of sandbox-manager container should be set to the same
    export E2B_DOMAIN=sandbox-manager.sandbox-system.svc.cluster.local
    export E2B_API_KEY=<your-api-key>
    ```
3. Patch client and disable HTTPS:
    ```python
    from kruise_agents.patch_e2b import patch_e2b
    patch_e2b(False)
    ```

### 4. Port forward sandbox-manager to local machine

1. Client configuration environment variables:
    ```shell
    # The E2B_DOMAIN env of sandbox-manager container should be set to the same
    export E2B_DOMAIN=localhost
    export E2B_API_KEY=<your-api-key>
    ```
2. Port forward sandbox-manager to local machine:
   ```shell
   kubectl port-forward services/sandbox-manager 80:7788 -n sandbox-system
   ```
3. Patch client:
    ```python
    from kruise_agents.patch_e2b import patch_e2b
    patch_e2b(False)
    ```