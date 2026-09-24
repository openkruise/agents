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
| `PEER_KEY_SECRET` | empty | `namespace/name` of the Secret holding the 32-byte memberlist key under the fixed data key `key`; empty keeps plaintext memberlist |
| `PEER_TLS_SERVER_SECRET` | empty | `namespace/name` of the Secret holding the agent-runtime server TLS bundle, used to receive peer HTTPS; must be set together with `PEER_TLS_CLIENT_SECRET` |
| `PEER_TLS_CLIENT_SECRET` | empty | `namespace/name` of the Secret holding this gateway's own runtime client TLS bundle, used to send peer HTTPS; must be set together with `PEER_TLS_SERVER_SECRET` |
| `PEER_ALLOWED_CLIENT_CNS` | empty | Comma-separated client identities allowed on inbound peer mTLS, matched against the client certificate CN or its DNS SANs. Empty allows any client trusted by the peer server CA. A non-empty value requires `PEER_TLS_SERVER_SECRET` and `PEER_TLS_CLIENT_SECRET` |

Notes:

- TLS Secret data keys are fixed: `ca.crt` supplies the CA bundle. For certificates and private keys, `tls.crt` plus `tls.key` take precedence; `client.crt` plus `client.key` are considered only when both `tls.*` entries are absent or empty. A partially present preferred pair, malformed PEM, or failed certificate-purpose check is an error and does not fall back. Values from the two pairs are never combined.
- Peer TLS requires certificate and key material as well as the CA bundle. It keeps CA-chain validation, `ServerAuth` and `ClientAuth` purpose checks, runtime SNI-based server-name verification, and the check that an inbound server certificate cannot be used for peer client authentication.
- Use separate Secrets for inbound server and outbound client credentials when one Secret contains a server pair under `tls.*` and a client pair under `client.*`. The shared loader selects `tls.*` first for both roles, so the old mixed layout now selects the server certificate for outbound client authentication and fails its `ClientAuth` check.
- Peer TLS and memberlist data-key override flags and environment variables have been removed and are not compatible. The memberlist Secret always uses the data key `key`.
- Both TLS references empty keeps plaintext peer HTTP; setting only one of them fails startup.
- Names in `PEER_ALLOWED_CLIENT_CNS` are matched as raw comma-separated strings: entries are not trimmed or case-folded. A value of `,` enables the restriction but matches no identity.
- The remaining variables mirror the sandbox-manager flags for the memberlist Secret, peer TLS Secret references, and allowed client identities.
- Startup self-check only proves this process's inbound and outbound material is locally consistent. It does not prove cluster-wide compatibility: every participant's server certificate must verify against every participant's clientSecret `ca.crt`, and every client certificate must verify against every participant's serverSecret `ca.crt`.

## 4. Customization

To customize the deployment, you can create a patch file and reference it in the kustomization.yaml, or modify the resource files directly.

Common customizations include:
- Image repository and tag
- Resource limits and requests
- Replica count
- Envoy configuration (timeouts, circuit breakers, etc.)
