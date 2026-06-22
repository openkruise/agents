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

## API Key Sandbox Count Quota

API keys may carry a static `sandbox.count` quota. Dynamic enforcement uses Redis only. If `--e2b-quota-redis-addr`
is empty, or Redis is configured but unavailable, sandbox-manager intentionally fails open: limited keys are accepted
and stored, but create requests are temporarily unenforced. Metrics and logs expose fail-open events.

When Redis requires authentication, inject `E2B_QUOTA_REDIS_USERNAME` and `E2B_QUOTA_REDIS_PASSWORD` from a Kubernetes
Secret rather than writing credentials directly into deployment patches.

When using MySQL key storage with `--e2b-key-storage-disable-schema-auto-update=true`, the startup schema check requires
the `team_api_keys.quota` column to exist. Apply the manual migration from `hack/mysql-schema.sql` before starting:

```sql
ALTER TABLE team_api_keys ADD COLUMN quota JSON DEFAULT NULL AFTER created_by_uid;
```
