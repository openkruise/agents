# Using OpenKruise Agents Sandbox via E2B SDK

The sandbox-manager component of OpenKruise Agents supports two integration methods: native E2B protocol and private
protocol.

Comparison between private protocol and native protocol:

> Assuming your configured E2B_DOMAIN is `your.domain.com`

| Native Protocol                  | Private Protocol                        | 
|----------------------------------|-----------------------------------------|
| api.your.domain.com              | your.domain.com/kruise/api              | 
| \<port\>-\<sid\>.your.domain.com | your.domain.com/kruise/\<sid\>/\<port\> |

## Important Notes on E2B_DOMAIN Environment Variable

**VERY IMPORTANT**: The `E2B_DOMAIN` environment variable of sandbox-manager must be set to the same as the client.
You can edit the deployment with `kubectl edit deploy -n sandbox-system sandbox-manager`

### How to Configure E2B_DOMAIN for Server-side (sandbox-manager)

You can configure the server-side E2B_DOMAIN by editing the following files before running
`make deploy-sandbox-manager`:

- [configuration_patch.yaml](../../config/sandbox-manager/configuration_patch.yaml)
- [ingress_patch.yaml](../../config/sandbox-manager/ingress_patch.yaml)

[//]: # (TODO: Add new deployment methods like Helm here when available)

### How to Configure E2B_DOMAIN for Client-side (E2B SDK)

You can configure the client-side E2B_DOMAIN by setting environment variables

```shell
export E2B_DOMAIN=your.domain.com
```

### Special Scenarios

#### 1. Domain with Port

For scenarios where the Ingress gateway does not use default HTTP ports (80 or 443). For example, if the domain is
`your.domain.com:8080`

- Client-side: Set environment variable `E2B_DOMAIN=your.domain.com:8080`
- Server-side
    - In [configuration_patch.yaml](../../config/sandbox-manager/configuration_patch.yaml), **keep the port**, set E2B
      Domain to
      `your.domain.com:8080`
    - In [ingress_patch.yaml](../../config/sandbox-manager/ingress_patch.yaml), **do not keep the port**, replace
      `replace.with.your.domain` with `your.domain.com`

## How to install a certificate

If you need to access sandbox-manager via HTTPS, you need to install a TLS certificate. It is recommended to use a
trusted certificate. If you don't have a trusted certificate, you can use a self-signed one, refer to the following
docs:

- [use-self-signed-cert.md](./use-self-signed-cert.md)
- [cert-manager.md](./cert-manager.md)

You can install your certificate with the following command:

```shell
# ingress-nginx example, adjust according to your ingress controller
kubectl create secret tls sandbox-manager-tls \
          --cert=fullchain.pem \
          --key=privkey.pem -n sandbox-system
```

## Recommended sandbox-manager integration methods

### 1. Integration using native protocol

> This is the most standard, native integration method, but also has the highest configuration threshold, generally
> requiring manual deployment.

1. Client configuration environment variables:
    ```shell
    # The E2B_DOMAIN env of sandbox-manager container should be set to the same
    export E2B_DOMAIN=your.domain.com
    export E2B_API_KEY=<your-api-key>
    ```
2. Resolve wildcard domain `*.your.domain.com` to sandbox-manager ingress endpoint with your DNS provider

3. Install wildcard certificate `*.your.domain.com`

### 2. Private protocol HTTPS access from outside cluster

> This approach can reduce deployment threshold and can be semi-automatically deployed.

1. Client configuration environment variables:
    ```shell
    # The E2B_DOMAIN env of sandbox-manager container should be set to the same
    export E2B_DOMAIN=your.domain.com
    export E2B_API_KEY=<your-api-key>
    ```
2. Patch client:
    ```python
    from kruise_agents.patch_e2b import patch_e2b
    patch_e2b(https=True)
    ```
3. Resolve single domain `your.domain.com` to sandbox-manager ingress endpoint with your DNS provider
4. Install single domain certificate `your.domain.com`

### 3. Private protocol in-cluster access

> This approach enables rapid automated deployment without requiring domain and certificate configuration. Recommended
> for E2E testing scenarios only, or after rigorous evaluation.

1. Ensure client(agent) and sandbox-manager are in the same cluster.
2. Client configuration environment variables:
    ```shell
    # The E2B_DOMAIN env of sandbox-manager container should be set to the same
    export E2B_DOMAIN=sandbox-manager.sandbox-system.svc.cluster.local
    export E2B_API_KEY=<your-api-key>
    ```
3. Patch client and disable HTTPS:
    ```python
    from kruise_agents.patch_e2b import patch_e2b
    patch_e2b(https=False)
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
   sudo kubectl port-forward services/sandbox-manager 80:7788 -n sandbox-system
   ```
3. Patch client:
    ```python
    from kruise_agents.patch_e2b import patch_e2b
    patch_e2b(https=False)
    ```