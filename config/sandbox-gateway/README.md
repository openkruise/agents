# Deploying sandbox-gateway Using Kustomize

This directory contains a pre-configured sandbox-gateway deployment. You can quickly deploy the gateway component in your cluster using these files.

## Overview

sandbox-gateway is an Envoy-based gateway with a Golang filter for sandbox routing. It acts as a proxy layer that routes incoming requests to the appropriate sandbox pods.

## 1. Build

Build the latest sandbox-gateway image from source code using make:

```shell
make docker-build-sandbox-gateway
```

If deploying to a real K8s cluster, please modify to an appropriate tag and push to your remote image repository.

## 2. Deployment

Deploy sandbox-gateway to your cluster using kustomize:

```shell
kubectl create ns sandbox-system # create namespace if not exist.
kustomize build config/sandbox-gateway | kubectl apply -f -
```

Or use kubectl's built-in kustomize:

```shell
kubectl apply -k config/sandbox-gateway
```

## 3. Configuration

The following components are deployed:

- **Deployment**: Runs the Envoy proxy with the sandbox-gateway filter
- **Service**: Exposes the gateway on port 10000
- **ConfigMap**: Contains the Envoy configuration
- **ServiceAccount**: Used by the gateway pods
- **RBAC**: ClusterRole and ClusterRoleBinding for accessing sandbox resources

### Peer Security (optional)

The gateway filter reads its peer configuration from environment variables. Each
feature is enabled only by its own Secret reference; an empty reference keeps
that channel in plaintext.

| Environment variable | Default | Meaning |
| --- | --- | --- |
| `PEER_KEY_SECRET` | empty | `namespace/name` of the Secret holding the 32-byte memberlist key; empty keeps plaintext memberlist |
| `PEER_KEY_SECRET_KEY` | `key` | Secret data key holding the memberlist key |
| `PEER_TLS_SERVER_SECRET` | empty | `namespace/name` of the Secret holding the agent-runtime server TLS bundle, used to receive peer HTTPS; must be set together with `PEER_TLS_CLIENT_SECRET` |
| `PEER_TLS_SERVER_CA_KEY` | `ca.crt` | Secret data key for the CA trusted for inbound peer client certificates |
| `PEER_TLS_SERVER_CERT_KEY` | `tls.crt` | Secret data key for the server certificate presented on inbound peer HTTPS |
| `PEER_TLS_SERVER_KEY_KEY` | `tls.key` | Secret data key for the private key of the server certificate |
| `PEER_TLS_CLIENT_SECRET` | empty | `namespace/name` of the Secret holding this gateway's own runtime client TLS bundle, used to send peer HTTPS; must be set together with `PEER_TLS_SERVER_SECRET` |
| `PEER_TLS_CLIENT_CA_KEY` | `ca.crt` | Secret data key for the CA trusted for outbound peer server certificates |
| `PEER_TLS_CLIENT_CERT_KEY` | `client.crt` | Secret data key for the client certificate presented on outbound peer HTTPS; set `tls.crt` when the referenced Secret uses the runtime mTLS file names |
| `PEER_TLS_CLIENT_KEY_KEY` | `client.key` | Secret data key for the private key of the client certificate; set `tls.key` when the referenced Secret uses the runtime mTLS file names |

Notes:

- Data-key variables describe the Secret layout, not credentials, and are read only while the matching `*_SECRET` reference is set.
- Both TLS references empty keeps plaintext peer HTTP; setting only one of them fails startup.
- The variables mirror the sandbox-manager flags (`--peer-key-secret`, `--peer-tls-*`) with the same names, defaults, and meanings.
- Startup self-check only proves this process's inbound and outbound material is locally consistent. It does not prove cluster-wide compatibility: every participant's server certificate must verify against every participant's clientSecret `ca.crt`, and every client certificate must verify against every participant's serverSecret `ca.crt`.

## 4. Customization

To customize the deployment, you can create a patch file and reference it in the kustomization.yaml, or modify the resource files directly.

Common customizations include:
- Image repository and tag
- Resource limits and requests
- Replica count
- Envoy configuration (timeouts, circuit breakers, etc.)
