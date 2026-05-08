---
title: Sandbox Container Image Commit
authors:
  - "@Luckydog691"
reviewers:
  - "@zmberg"
  - "@furykerry"
creation-date: 2026-05-06
last-updated: 2026-05-06
status: provisional
---

# Sandbox Container Image Commit

## Summary

Introduce a `Commit` CRD that allows users to commit a running Sandbox container's filesystem changes into a new Docker image and push it to a registry. The controller creates a Kubernetes Job on the target node to perform `nerdctl commit` + `nerdctl push`, with registry authentication resolved using a DockerKeyring-based approach inspired by [OpenKruise/kruise](https://github.com/openkruise/kruise).

## Motivation

Users of OpenKruise Agents need to persist workspace state as container images for:
- Snapshotting development environments
- Creating pre-configured base images from running sandboxes
- Sharing workspace state across team members

Currently there is no declarative way to commit a container image from a running Sandbox.

### Goals

- Provide a CRD-driven workflow to commit and push container images from Sandbox pods
- Support registry authentication with a three-tier fallback strategy (explicit pushSecrets > namespace DockerKeyring matching > SA imagePullSecrets as best-effort)
- Controlled via Feature Gate (`Commit`)

### Non-Goals/Future Work

- Multi-runtime support (only containerd/nerdctl for now)
- Image layer squash optimization
- Integration with OCI artifact registries beyond Docker v2

## Proposal

### User Stories

#### Story 1: Commit a Sandbox workspace

A developer finishes configuring their Sandbox environment and wants to save it:

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: Commit
metadata:
  name: my-snapshot
  namespace: sandbox-system
spec:
  podName: my-sandbox-abc123
  containerName: workspace
  image: registry.example.com/team/my-env:v1
  pushSecrets:
    - namespace: sandbox-system
      name: my-push-secret
  ttl: 168h  # auto-cleanup after 7 days
```

#### Story 2: Dry-run validation

Before committing, a user verifies disk space is sufficient:

```yaml
spec:
  podName: my-sandbox-abc123
  containerName: workspace
  image: registry.example.com/team/my-env:v1
  dryRun: true
  diskSpaceCheck:
    enabled: true
```

### API Design

```go
// ReferenceObject is a namespace-aware reference to a Kubernetes object.
// Aligned with kruise's apis/apps/v1beta1.ReferenceObject pattern.
type ReferenceObject struct {
    Namespace string `json:"namespace,omitempty"`
    Name      string `json:"name,omitempty"`
}

type CommitSpec struct {
    PodName        string           `json:"podName"`
    ContainerName  string           `json:"containerName"`
    Image          string           `json:"image"`
    PushSecrets    []ReferenceObject `json:"pushSecrets,omitempty"`
    Ttl            *Duration        `json:"ttl,omitempty"`
    DiskSpaceCheck *DiskSpaceCheck  `json:"diskSpaceCheck,omitempty"`
    DryRun         bool             `json:"dryRun,omitempty"`
}

type DiskSpaceCheck struct {
    // Whether to enable disk space check before commit.
    Enabled bool `json:"enabled"`
    // Safety factor multiplied against writable layer size to estimate required space.
    // requiredBytes = writableLayerSize * safetyFactor.
    // Defaults to "1.5" if not specified.
    SafetyFactor string `json:"safetyFactor,omitempty"` // e.g. "1.5", "2.0"
}

type CommitStatus struct {
    Phase          CommitPhase      `json:"phase"`           // Pending|Running|Succeeded|Failed
    CommitID       string           `json:"commitID,omitempty"`
    Conditions     []Condition      `json:"conditions,omitempty"`
    StartTime      *Time            `json:"startTime,omitempty"`
    CompletionTime *Time            `json:"completionTime,omitempty"`
}
```

**DiskSpaceCheck mechanism**: The commit-job queries the CRI `ContainerStats` API to get the container's writable layer size, multiplies it by `safetyFactor` (default 1.5) to estimate the space needed for the new image layer, then compares against the available bytes on the containerd root filesystem (via `statfs`). If `availableBytes < writableLayerSize * safetyFactor`, the job aborts with a dedicated exit code before performing any commit/push. When estimation fails (e.g. CRI not reporting writable layer stats), the check is skipped with a warning to avoid blocking the commit.

`PushSecrets` follows the same `ReferenceObject` pattern as kruise's `ImageSpec.PullSecrets`, enabling cross-namespace secret references. The semantic difference is that `pullSecrets` are for image pulling while `pushSecrets` are for image pushing — but the resolution mechanism (DockerKeyring Lookup by registry host) is identical.

### Architecture

```
Commit CR → CommitController → SecretResolver → K8s Job (on target node)
                                                      │
                                          nerdctl commit + push
```

**Registry Auth Flow** (aligned with kruise's [docker-registry-auth-flow](https://github.com/openkruise/kruise)):

1. User-specified `spec.pushSecrets` — namespace-aware `ReferenceObject` list (same pattern as kruise's `ImageSpec.PullSecrets`)
2. DockerKeyring-based registry host matching across namespace secrets (using `credentialprovider.DockerKeyring.Lookup()`)
3. Fallback to default ServiceAccount's `imagePullSecrets`

The resolved secret is mounted as a volume into the Job pod. Inside the Job, it is converted from Kubernetes `dockerconfigjson` format to standard Docker `config.json` and exposed via `DOCKER_CONFIG` env for nerdctl.

### Registry Authentication Design

This section describes the full registry authentication chain for the Commit feature, from user input to `nerdctl push` on the target node.

#### Design Principles

1. **Align with kruise**: Use the same `ReferenceObject{namespace, name}` pattern and DockerKeyring-based matching as kruise's ImagePullJob
2. **Minimal privilege**: The Job Pod does not need cluster-level secret access; secrets are resolved by the controller and mounted into the Job
3. **Three-tier fallback**: Maximize push success rate without requiring users to always specify secrets explicitly

#### Architecture Overview

```
┌────────────────────────────────────────────────────────────────────────────────┐
│                     CommitController (control plane)                            │
│                                                                                │
│  Commit CR                                                                     │
│  spec.pushSecrets: [{ns: team-a, name: push-secret}]                           │
│       │                                                                        │
│       ▼                                                                        │
│  SecretResolver.Resolve(commit)                                                │
│  ┌──────────────────────────────────────────────────────────────────────────┐  │
│  │  Tier 1: spec.pushSecrets → Read secrets from specified namespace        │  │
│  │       ↓ (if empty or Lookup misses target registry)                      │  │
│  │  Tier 2: DockerKeyring.Lookup(targetRegistry)                            │  │
│  │          across all secrets in Commit's namespace                         │  │
│  │       ↓ (if still no match)                                              │  │
│  │  Tier 3: Pod's ServiceAccount.imagePullSecrets (best-effort)              │  │
│  └──────────────────────────────────────────────────────────────────────────┘  │
│       │                                                                        │
│       ▼                                                                        │
│  resolvedSecret (v1.Secret, type: dockerconfigjson)                            │
│       │                                                                        │
│       ▼                                                                        │
│  Create K8s Job:                                                               │
│    - Mount resolvedSecret as volume at /etc/registry-auth/                     │
│    - Set env REGISTRY_AUTH_FILE=/etc/registry-auth/.dockerconfigjson           │
│    - Node affinity → target pod's node                                         │
└────────────────────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌────────────────────────────────────────────────────────────────────────────────┐
│                     Job Pod (target node)                                       │
│                                                                                │
│  cmd/commit-job:                                                               │
│    1. setupRegistryAuth()                                                      │
│       ├── Read /etc/registry-auth/.dockerconfigjson                            │
│       ├── Convert K8s dockerconfigjson → standard Docker config.json           │
│       │   {                                                                    │
│       │     "auths": {                                                         │
│       │       "registry.example.com": {                                        │
│       │         "auth": "base64(user:pass)"                                    │
│       │       }                                                                │
│       │     }                                                                  │
│       │   }                                                                    │
│       └── Write to /tmp/.docker/config.json (0600 permissions)                 │
│           Set DOCKER_CONFIG=/tmp/.docker                                        │
│                                                                                │
│    2. nerdctl commit <containerID> <image>                                     │
│                                                                                │
│    3. nerdctl push <image>  (reads DOCKER_CONFIG automatically)                │
│                                                                                │
│    4. Report exit code → CommitController updates Commit.Status                │
└────────────────────────────────────────────────────────────────────────────────┘
```

#### Three-Tier Fallback Detail

**Tier 1: Explicit `pushSecrets`**

```go
// User specifies pushSecrets in Commit spec
for _, ref := range commit.Spec.PushSecrets {
    secret, err := client.Get(ctx, types.NamespacedName{
        Namespace: ref.Namespace,
        Name:      ref.Name,
    }, &v1.Secret{})
    // Use DockerKeyring.Lookup to verify this secret matches the target registry
    keyring, _ := credentialprovidersecrets.MakeDockerKeyring([]v1.Secret{*secret}, nil)
    if creds, ok := keyring.Lookup(targetRegistry); ok && len(creds) > 0 {
        return secret  // Found matching credentials
    }
}
```

**Tier 2: Namespace-wide DockerKeyring matching**

```go
// List all dockerconfigjson secrets in Commit's namespace
secretList := &v1.SecretList{}
client.List(ctx, secretList, client.InNamespace(commit.Namespace),
    client.MatchingFields{"type": string(v1.SecretTypeDockerConfigJson)})

// Build a combined keyring from all available secrets
keyring, _ := credentialprovidersecrets.MakeDockerKeyring(secretList.Items, defaultKeyring)
creds, ok := keyring.Lookup(targetRegistry)
// Return the secret that provided the matching credential
```

**Tier 3: ServiceAccount imagePullSecrets (best-effort)**

```go
// Get the target Pod's ServiceAccount
pod := getTargetPod(commit.Spec.PodName)
sa := getServiceAccount(pod.Spec.ServiceAccountName)
// Resolve each imagePullSecret reference
for _, ref := range sa.ImagePullSecrets {
    secret := client.Get(ctx, types.NamespacedName{
        Namespace: pod.Namespace, Name: ref.Name,
    }, &v1.Secret{})
    keyring, _ := credentialprovidersecrets.MakeDockerKeyring([]v1.Secret{*secret}, nil)
    if creds, ok := keyring.Lookup(targetRegistry); ok {
        return secret
    }
}
```

If all three tiers find no matching credentials, the Job is created without auth volume and attempts anonymous push; failure results in `Commit.Status.Phase = Failed` with a clear auth error message.

#### Difference from Kruise's Approach

Kruise's ImagePullJob needs to sync secrets to a dedicated `kruise-daemon-config` namespace because kruise-daemon is a long-running DaemonSet that cannot predict which secrets it will need at runtime. In contrast, the Commit feature uses short-lived K8s Jobs — at Job creation time the controller already knows exactly which secret is needed and can mount it directly as a volume. This eliminates the need for a sync namespace, secret lifecycle management (reference counting, GC), and daemon-side caching. The controller reads cross-namespace secrets via its cluster-level RBAC and mounts them into the Job Pod in the same namespace, so no intermediate secret copy is required.

#### Job-Side Auth: `setupRegistryAuth()`

The Job binary (`cmd/commit-job`) handles the format conversion:

```go
func setupRegistryAuth(mountPath string) (string, error) {
    // 1. Read the K8s Secret data from mounted volume
    data, err := os.ReadFile(filepath.Join(mountPath, ".dockerconfigjson"))
    if err != nil {
        return "", err
    }

    // 2. K8s dockerconfigjson has outer wrapper: {"auths": {...}}
    //    Docker config.json uses same format — direct passthrough
    dockerConfigDir := "/run/docker-config"  // points to emptyDir{medium: Memory}
    configPath := filepath.Join(dockerConfigDir, "config.json")
    err = os.WriteFile(configPath, data, 0600)
    if err != nil {
        return "", err
    }

    // 3. Set DOCKER_CONFIG for nerdctl
    os.Setenv("DOCKER_CONFIG", dockerConfigDir)
    return dockerConfigDir, nil
}
```

**Security considerations**:
- The converted `config.json` is written to an `emptyDir{medium: Memory}` volume (tmpfs-backed, **never touches disk**), matching kruise daemon's in-memory-only credential handling
- Secret source volume mounted as `readOnly: true`
- Config file written with `0600` permissions
- Both tmpfs volumes disappear immediately when the Pod terminates — no on-disk residue for forensic extraction
- No secret content is logged at any point

#### Volume Mount Configuration

```go
// In Job template generation
volumes := []v1.Volume{
    {
        Name: "registry-auth",
        VolumeSource: v1.VolumeSource{
            Secret: &v1.SecretVolumeSource{
                SecretName:  resolvedSecret.Name,
                DefaultMode: pointer.Int32(0400),
            },
        },
    },
    {
        Name: "docker-config",
        VolumeSource: v1.VolumeSource{
            EmptyDir: &v1.EmptyDirVolumeSource{
                Medium:    v1.StorageMediumMemory, // tmpfs, credentials never hit disk
                SizeLimit: resource.NewQuantity(1<<20, resource.BinarySI), // 1Mi
            },
        },
    },
}
volumeMounts := []v1.VolumeMount{
    {
        Name:      "registry-auth",
        MountPath: "/etc/registry-auth",
        ReadOnly:  true,
    },
    {
        Name:      "docker-config",
        MountPath: "/run/docker-config",
        ReadOnly:  false, // Job writes converted config.json here
    },
}
```

### Implementation Plan: Code Changes

Below lists the files/packages to be added or modified in this repository.

#### 1. API Types (New)

| Path | Action | Description |
|------|--------|-------------|
| `api/v1alpha1/commit_types.go` | **New** | `Commit` CRD types: `CommitSpec`, `CommitStatus`, `ReferenceObject`, phase constants |
| `api/v1alpha1/zz_generated.deepcopy.go` | **Regenerate** | Run `make generate` to add DeepCopy methods |

#### 2. CRD Definition (New)

| Path | Action | Description |
|------|--------|-------------|
| `config/crd/bases/agents.kruise.io_commits.yaml` | **New** | Generated via `controller-gen`; defines Commit CRD schema |

#### 3. Feature Gate (Modify)

| Path | Action | Description |
|------|--------|-------------|
| `pkg/features/features.go` | **Modify** | Add `CommitGate featuregate.Feature = "Commit"` and register in `defaultFeatureGates` |

#### 4. Controller Registration (Modify)

| Path | Action | Description |
|------|--------|-------------|
| `pkg/controller/controllers.go` | **Modify** | Add `import "github.com/openkruise/agents/pkg/controller/commit"` and `controllerAddFuncs = append(controllerAddFuncs, commit.Add)` |

#### 5. Commit Controller (New package)

| Path | Action | Description |
|------|--------|-------------|
| `pkg/controller/commit/commit_controller.go` | **New** | Main reconciler: watch Commit CR, create/monitor Job, update status, handle TTL cleanup |
| `pkg/controller/commit/commit_event_handler.go` | **New** | Watch Job completions, enqueue owning Commit for status sync |
| `pkg/controller/commit/secret_resolver.go` | **New** | `SecretResolver` interface + implementation: three-tier fallback (pushSecrets > namespace DockerKeyring > SA imagePullSecrets) |
| `pkg/controller/commit/job_template.go` | **New** | Generate K8s Job spec: container image, volumes (containerd socket + registry auth secret), node affinity, env vars |

#### 6. Job Binary (New)

| Path | Action | Description |
|------|--------|-------------|
| `cmd/commit-job/main.go` | **New** | Entry point for the commit-job container |
| `pkg/commit-job/commit_handler.go` | **New** | Core logic: `setupRegistryAuth()` → `nerdctl commit` → `nerdctl push`; structured exit codes |
| `pkg/commit-job/registry_auth.go` | **New** | Read mounted K8s Secret, convert to Docker `config.json`, set `DOCKER_CONFIG` env |
| `dockerfiles/Dockerfile.commit-job` | **New** | Dockerfile for commit-job image (base image with nerdctl binary) |

#### 7. RBAC (Modify)

| Path | Action | Description |
|------|--------|-------------|
| `config/rbac/role.yaml` | **Modify** | Add rules: Commit CR (get/list/watch/update/patch), Secrets (get/list across namespaces), Jobs (create/get/list/watch/delete), ServiceAccounts (get) |

#### 8. Webhook (Optional, New)

| Path | Action | Description |
|------|--------|-------------|
| `pkg/webhook/commit/validating.go` | **New** | Validate: image format, podName exists, pushSecrets references valid, prevent spec mutation after Running |

#### 9. Constants (Modify)

| Path | Action | Description |
|------|--------|-------------|
| `pkg/utils/constant.go` | **Modify** | Add commit-related constants: `CommitFinalizer`, `CommitJobLabelKey`, annotation keys |

#### 10. E2B API Layer (Modify)

The E2B server (`pkg/servers/e2b/`) exposes a REST API for sandbox operations. A commit endpoint needs to be added.

| Path | Action | Description |
|------|--------|-------------|
| `pkg/servers/e2b/routes.go` | **Modify** | Register `POST /sandboxes/{sandboxID}/commit` route |
| `pkg/servers/e2b/commit.go` | **New** | Handler: parse request (image, pushSecrets, ttl), create Commit CR via K8s client, poll/return status |
| `pkg/servers/e2b/models/commit.go` | **New** | Request/Response models: `CommitRequest{Image, PushSecrets, Ttl, DryRun}`, `CommitResponse{CommitID, Phase, Image}` |

E2B commit endpoint design:

```
POST /sandboxes/{sandboxID}/commit
Request:
{
  "image": "registry.example.com/team/my-env:v1",
  "pushSecrets": [{"namespace": "sandbox-system", "name": "my-push-secret"}],
  "ttl": "168h",
  "dryRun": false
}
Response:
{
  "commitID": "my-snapshot-xyz",
  "phase": "Pending",
  "image": "registry.example.com/team/my-env:v1"
}

GET /sandboxes/{sandboxID}/commits/{commitID}   (optional: query status)
```

The handler translates the HTTP request to a `Commit` CR creation, following the same pattern as `CreateSnapshot` in `pkg/servers/e2b/snapshot.go`.

### Risks and Mitigations

| Risk | Mitigation |
|------|-----------|
| Job requires root + HostNetwork | Scoped to containerd socket mount only; no privileged escalation |
| Disk space exhaustion during commit | Optional `diskSpaceCheck` with CRI ContainerStats pre-validation |
| Registry credential leakage | Secret mounted read-only, never logged; 0600 permissions on config file |
| SA imagePullSecrets lacks push permission | Tier 3 is best-effort; clear error message on auth failure |

## Alternatives

*(omitted for now)*

## Test Plan

- Unit tests for `SecretResolver` (three-tier fallback, DockerKeyring matching)
- Unit tests for `job_template.go` (Job spec generation, volume mounts)
- Unit tests for `registry_auth.go` (format conversion, edge cases)
- Integration: Commit controller reconcile loop with fake client
- E2E: Create Sandbox → Create Commit → Verify image pushed to registry

## Implementation History

- 2026-05-06: Proposal created
- 2026-05-08: Added detailed registry authentication design and code change plan
