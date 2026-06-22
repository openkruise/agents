# Deploying sandbox-manager Test Environment Using Kustomize

This directory contains a pre-configured sandbox-manager deployment for functional testing scenarios. You can quickly
deploy a test environment in your cluster using these files.

## 1. Build

Build the latest sandbox-manager image from source code using
the [Dockerfile](../../dockerfiles/sandbox-manager.Dockerfile)

```shell
docker build -t sandbox-manager:latest .
```

If deploying to a real K8s cluster, please modify to an appropriate tag and push to your remote image repository.

## 2. Deployment

1. Edit the two patch files for some basic customizations:
    1. [deployment_patch.yaml](configuration_patch.yaml)
    2. [ingress_patch.yaml](ingress_patch.yaml)
2. Generate the complete YAML file and complete the deployment with the following command:
    ```shell
    kustomize build config/sandbox-manager > bin/sandbox-manager.yaml
    kubectl apply -f bin/sandbox-manager.yaml
    ```
