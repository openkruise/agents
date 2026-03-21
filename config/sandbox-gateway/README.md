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

## 4. Customization

To customize the deployment, you can create a patch file and reference it in the kustomization.yaml, or modify the resource files directly.

Common customizations include:
- Image repository and tag
- Resource limits and requests
- Replica count
- Envoy configuration (timeouts, circuit breakers, etc.)
