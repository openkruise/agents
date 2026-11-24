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
    1. [configuration_patch.yaml](configuration_patch.yaml)
    2. [ingress_patch.yaml](ingress_patch.yaml)
2. Generate the complete YAML file and complete the deployment with the following command:
    ```shell
    kustomize build config/sandbox-manager > bin/sandbox-manager.yaml
    kubectl apply -f bin/sandbox-manager.yaml
    ```

## E2B network policy with Agentio

By default, sandbox-manager stores E2B outbound allow and deny lists in a `TrafficPolicy`.
To enable annotation mode, upgrade all sandbox-manager replicas, then start them with `--e2b-network-annotation=true`.
Enable Sandbox policy discovery in Agentio and route managed outbound traffic through its egress gateway.
New pool claims and snapshot clones store E2B network settings in the `agents.kruise.io/e2b-network` annotation.
Existing sandboxes continue to use the backend selected at creation.
In annotation mode, a client can send both `network.rules` and the `e2b.agents.kruise.io/security-profile` metadata key, and later network updates preserve the native profile.
In the default mode, HTTP header transforms in `network.rules` use the legacy `security-rules` annotation and return HTTP 400 when combined with a native profile.

For a sandbox created with `allow_internet_access` set to `false` and `network.allowOut` set to `["192.0.2.10"]`, read the annotation using the Sandbox's Kubernetes namespace and object name.

```shell
kubectl get sandboxes.agents.kruise.io "$SANDBOX_NAME" -n "$NAMESPACE" \
  -o go-template='{{index .metadata.annotations "agents.kruise.io/e2b-network"}}{{"\n"}}'
```

In annotation mode, the command prints the following JSON.

```json
{"allowInternetAccess":false,"network":{"allowOut":["192.0.2.10"]}}
```

The proposal includes a create request, the resulting Sandbox YAML, and the fields to send when updating a policy. [E2B network annotation](../../docs/proposals/20261121-e2b-inline-security-profile.md#e2b-network-annotation)

## API Key Quota

API keys may carry static quotas across `sandbox.count`, `limits.cpu`, and `limits.memory`, scoped by `running` or
`all`. Public API payloads, Kubernetes Secrets, and MySQL storage all use the canonical `QuotaSpec` limits shape,
such as
`{"limits":[{"dimension":"sandbox.count","scope":"running","limit":10},{"dimension":"limits.cpu","scope":"running","limit":8000},{"dimension":"limits.memory","scope":"running","limit":16384},{"dimension":"sandbox.count","scope":"all","limit":50}]}`.
Dynamic enforcement uses Redis only. If `--quota-redis-addr`
is empty, or Redis is configured but unavailable, sandbox-manager intentionally fails open: limited keys are accepted
and stored, but create requests are temporarily unenforced. Metrics and logs expose fail-open events.

When Redis requires authentication, inject `QUOTA_REDIS_USERNAME` and `QUOTA_REDIS_PASSWORD` from a Kubernetes
Secret rather than writing credentials directly into deployment patches.

When using MySQL key storage with `--e2b-key-storage-disable-schema-auto-update=true`, the startup schema check requires
the `team_api_keys.quota` column to exist. Apply the manual migration from `hack/mysql-schema.sql` before starting:

```sql
ALTER TABLE team_api_keys ADD COLUMN quota JSON DEFAULT NULL AFTER created_by_uid;
```
