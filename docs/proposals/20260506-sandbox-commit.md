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
- Support registry authentication with a three-tier fallback strategy (explicit secret > registry-matched secret > SA imagePullSecrets)
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

type CommitStatus struct {
    Phase          CommitPhase      `json:"phase"`           // Pending|Running|Succeeded|Failed
    CommitID       string           `json:"commitID,omitempty"`
    Conditions     []Condition      `json:"conditions,omitempty"`
    StartTime      *Time            `json:"startTime,omitempty"`
    CompletionTime *Time            `json:"completionTime,omitempty"`
}
```

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

### Implementation Details

- **Feature Gate**: `Commit` (default: true, alpha)
- **Job binary**: `cmd/agent-job` — runs nerdctl commit/push with structured exit codes
- **Security**: Job runs as root with HostNetwork (required for containerd socket access), no privileged mode
- **Idempotency**: Job name derived from Commit UID; OwnerReference for cascade deletion
- **TTL cleanup**: Controller auto-deletes Commit CR after `spec.ttl` expires

### Risks and Mitigations

| Risk | Mitigation |
|------|-----------|
| Job requires root + HostNetwork | Scoped to containerd socket only; no privileged escalation |
| Disk space exhaustion during commit | Optional `diskSpaceCheck` with CRI ContainerStats pre-validation |
| Registry credential leakage | Secret mounted read-only, never logged; 0600 permissions on config file |

## Alternatives

1. **In-process commit** (controller directly calls containerd API): Rejected — requires controller to run on every node or access remote containerd, increases blast radius.
2. **CRI-based auth** (kruise daemon pattern): Rejected for this use case — nerdctl push needs file-based Docker config, not CRI AuthConfig.

## Test Plan

- Unit tests for SecretResolver (three-tier fallback, DockerKeyring matching)
- Unit tests for Job template generation
- Unit tests for registry_auth format conversion
- E2E: Create Sandbox → Create Commit → Verify image pushed to registry

## Implementation History

- 2026-05-06: Proposal created
