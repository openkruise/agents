---
title: Support SandboxClaim CRD
authors:
  - "@PersistentJZH"
reviewers:
  - "@furykerry"
  - "@zmberg"
creation-date: 2025-12-29
status: implementable
---

# SandboxClaim CRD for Declarative Sandbox Allocation

This proposal introduces a **SandboxClaim** CRD that provides a Kubernetes-native, declarative API for claiming pre-warmed sandbox instances from a SandboxSet.

The SandboxClaim controller will implement the same claim logic as the sandbox-manager, focusing on allocating sandboxes by adding annotations and removing ownerReferences. This design leverages the existing proxy's listwatch mechanism to automatically configure routing when sandbox state changes.

## Motivation

It is easy and fast to use high-level API such E2B API to claim a sandbox, however for platform users, they may use their own high-level apiserver, so the claim logic should be also available in K8S api

## Proposal

### User Stories

#### Story 1: Claim Single Sandbox

As a platform developer, I want to declaratively claim a sandbox from a pool using Kubernetes API.

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxClaim
metadata:
  name: user-sandbox
spec:
  sandboxSetName: python-pool
  timeout: 3600
  envVars:
    API_KEY: "sk-..."
```

#### Story 2: Batch Claiming

I want to claim multiple sandboxes for parallel test execution.

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxClaim
metadata:
  name: test-batch
spec:
  sandboxSetName: test-pool
  replicas: 10
```

### API Design

#### SandboxClaim CRD

```go
// SandboxClaimSpec defines the desired state of SandboxClaim
type SandboxClaimSpec struct {
    // SandboxSetName specifies which SandboxSet pool to claim from
    // +kubebuilder:validation:Required
    SandboxSetName string `json:"sandboxSetName"`

    // Owner identifies who is claiming the sandbox (e.g., user ID, service account)
    // This will be set as the agents.kruise.io/owner annotation on claimed sandboxes
    // If not specified, defaults to "<namespace>/<claim-name>"
    // +optional
    Owner string `json:"owner,omitempty"`

    // Replicas specifies how many sandboxes to claim (default: 1)
    // For batch claiming support
    // +optional
    // +kubebuilder:default=1
    // +kubebuilder:validation:Minimum=1
    Replicas int32 `json:"replicas,omitempty"`

    // Timeout specifies the lifetime of claimed sandboxes in seconds
    // This will be converted to shutdownTime on the Sandbox
    // +optional
    Timeout *int64 `json:"timeout,omitempty"`

    // Metadata contains arbitrary key-value pairs to be added as annotations
    // to claimed Sandbox resources
    // +optional
    Metadata map[string]string `json:"metadata,omitempty"`

    // EnvVars contains environment variables to be injected into the sandbox
    // These will be passed to the sandbox's init endpoint (envd) after claiming
    // Only applicable if the SandboxSet has envd enabled
    // +optional
    EnvVars map[string]string `json:"envVars,omitempty"`
}

// SandboxClaimStatus defines the observed state of SandboxClaim
type SandboxClaimStatus struct {
    // ObservedGeneration is the most recent generation observed
    // +optional
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`

    // Phase represents the current phase of the claim
    // Pending: Waiting to claim sandboxes
    // Claiming: In the process of claiming
    // Ready: Successfully claimed and sandboxes are ready
    // Failed: Failed to claim (no available sandboxes, timeout, etc.)
    // +optional
    Phase SandboxClaimPhase `json:"phase,omitempty"`

    // Message provides human-readable details about the current phase
    // +optional
    Message string `json:"message,omitempty"`

    // ClaimedReplicas indicates how many sandboxes are currently claimed (total)
    // This is determined by querying sandboxes with matching ownerReference
    // +optional
    ClaimedReplicas int32 `json:"claimedReplicas,omitempty"`

    // ReadyReplicas indicates how many sandboxes are successfully claimed and ready
    // +optional
    ReadyReplicas int32 `json:"readyReplicas,omitempty"`

    // Conditions represent the current state of the SandboxClaim
    // +optional
    // +listType=map
    // +listMapKey=type
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SandboxClaimPhase defines the phase of SandboxClaim
// +enum
type SandboxClaimPhase string

const (
    SandboxClaimPhasePending  SandboxClaimPhase = "Pending"
    SandboxClaimPhaseClaiming SandboxClaimPhase = "Claiming"
    SandboxClaimPhaseReady    SandboxClaimPhase = "Ready"
    SandboxClaimPhaseFailed   SandboxClaimPhase = "Failed"
)

// SandboxClaimConditionType defines condition types
type SandboxClaimConditionType string

const (
    // SandboxClaimConditionReady indicates if the claim is ready (all sandboxes claimed and ready)
    SandboxClaimConditionReady SandboxClaimConditionType = "Ready"
)

```


#### Fields Comparison with E2B HTTP API

The SandboxClaim CRD is designed to align with the E2B HTTP API's `NewSandboxRequest`, with the following mappings:

| E2B API Field | SandboxClaim Field | Status | Notes |
|---------------|-------------------|--------|-------|
| `templateID` | `sandboxSetName` |  implementable | Maps to SandboxSet pool name |
| `timeout` | `timeout` |  implementable | Lifetime in seconds |
| `metadata` | `metadata` |  implementable | Custom annotations |
| `envVars` | `envVars` | implementable | Environment variables for envd init |
| `autoPause` | N/A |  Future Work | Not implemented in E2B Api |
| `secure` | N/A |  Future Work | Not implemented in E2B Api |



### Implementation Details

**Claim Process**:
1. List available sandboxes from the specified SandboxSet (state = Running and available)
2. Randomly select a sandbox from candidates
3. Update sandbox with:
    - Lock annotation (UUID)
    - Owner annotation (from `claim.spec.owner`)
    - Claim timestamp annotation
    - Custom metadata annotations (from `claim.spec.metadata`)
    - Remove SandboxSet ownerReference (detach from pool)
    - Add SandboxClaim ownerReference
    - Set `shutdownTime` if timeout specified
4. Inject environment variables via envd `/init` endpoint (if `claim.spec.envVars` specified)
    - Failure to inject envVars logs error but doesn't fail the claim

**Automatic Proxy Configuration**:
- Existing infra watches sandbox changes and automatically updates proxy routes
- No explicit proxy configuration needed in controller

#### State Management

The controller tracks claim state through phases:

- **Pending**: SandboxClaim created, waiting to start claiming
- **Claiming**: Actively claiming sandboxes or waiting for unhealthy sandboxes to recover
- **Ready**: All required sandboxes claimed and ready
- **Failed**: Unable to claim (no available sandboxes, timeout exceeded, etc.)

State transitions:
```
         ┌──────────────┐
         │   Pending    │
         └──────┬───────┘
                │
                ▼
         ┌──────────────┐
    ┌───│   Claiming   │◄──┐
    │   └──────┬───────┘   │
    │          │            │
    │          ▼            │
    │   ┌──────────────┐   │
    │   │    Ready     │───┘ (sandbox unhealthy/deleted)
    │   └──────────────┘
    │
    ▼
┌──────────────┐
│   Failed     │
└──────────────┘
```

**Key Transitions**:
- `Pending` → `Claiming`: Start claiming process
- `Claiming` → `Ready`: All sandboxes claimed and ready
- `Ready` → `Claiming`: Sandbox becomes unhealthy or deleted (automatic recovery)
- `Claiming` ⟲ `Claiming`: Waiting for sandbox recovery or replacement
- Any → `Failed`: Persistent errors (rare, usually stays in Claiming)

**User Experience**:
```bash
# Get claim status (shows counts)
kubectl get sandboxclaim my-claim
# NAME       PHASE   OWNER      SANDBOXSET   DESIRED   CLAIMED   READY   AGE
# my-claim   Ready   user-123   my-pool      3         3         3       5m


```

#### Unhealthy Sandbox Handling

The controller continuously monitors claimed sandboxes and handles unhealthy/deleted cases:

**Detection and Recovery**:

1. **Deleted Sandbox**:
    - Controller detects sandbox no longer exists (via ownerReference query)
    - `ClaimedReplicas` decreases automatically
    - Phase transitions from `Ready` → `Claiming`
    - Controller claims a replacement sandbox

2. **Unhealthy Sandbox**:
    - Controller detects sandbox is not ready (Phase != Running or Ready condition false)
    - `ReadyReplicas` decreases, `ClaimedReplicas` unchanged
    - Phase transitions from `Ready` → `Claiming`
    - Sandbox remains claimed (opportunity to recover)
    - If sandbox recovers, `ReadyReplicas` increases

3. **Recovery**:
    - When all claimed sandboxes become ready
    - Phase transitions back to `Ready`
    - `ReadyReplicas` == `ClaimedReplicas` == `spec.replicas`


