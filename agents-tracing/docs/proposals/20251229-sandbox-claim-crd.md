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
  templateName: python-pool
  shutdownTime: "2025-12-30T12:00:00Z"
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
  templateName: test-pool
  replicas: 10
```

#### Story 3: Claim with Timeout and Auto Cleanup

I want to claim sandboxes with a timeout, and automatically clean up the claim after completion.

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxClaim
metadata:
  name: timed-claim
spec:
  templateName: python-pool
  replicas: 5
  claimTimeout: 10m  # Wait up to 10 minutes to claim sandboxes
  ttlAfterCompleted: 1h  # Auto-delete the claim 1 hour after completion
  labels:
    project: "my-project"
    environment: "test"
  annotations:
    claim-purpose: "integration-test"
```

### API Design

#### SandboxClaim CRD

```go
// SandboxClaimSpec defines the desired state of SandboxClaim
type SandboxClaimSpec struct {
    // TemplateName specifies which SandboxSet pool to claim from
    // +kubebuilder:validation:Required
    TemplateName string `json:"templateName"`

    // Replicas specifies how many sandboxes to claim (default: 1)
    // For batch claiming support
    // This field is immutable once set
    // +optional
    // +kubebuilder:default=1
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:XValidation:rule="self == oldSelf",message="replicas is immutable"
    // TODO: XValidation may not work in older Kubernetes versions. Consider using webhook validation for better compatibility.
    Replicas *int32 `json:"replicas,omitempty"`

    // ShutdownTime specifies the absolute time when the sandbox should be shut down
    // This will be set as spec.shutdownTime (absolute time) on the Sandbox
    // +optional
    ShutdownTime *metav1.Time `json:"shutdownTime,omitempty"`

    // ClaimTimeout specifies the maximum duration to wait for claiming sandboxes
    // If the timeout is reached, the claim will be marked as Completed regardless of
    // whether all replicas were successfully claimed
    // +optional
    ClaimTimeout *metav1.Duration `json:"claimTimeout,omitempty"`

    // TTLAfterCompleted specifies the time to live after the claim reaches Completed phase
    // After this duration, the SandboxClaim will be automatically deleted.
    // Note: Only the SandboxClaim resource will be deleted; the claimed sandboxes will NOT be deleted
    // +optional
    TTLAfterCompleted *metav1.Duration `json:"ttlAfterCompleted,omitempty"`

    // Labels contains key-value pairs to be added as labels
    // to claimed Sandbox resources
    // +optional
    Labels map[string]string `json:"labels,omitempty"`

    // Annotations contains key-value pairs to be added as annotations
    // to claimed Sandbox resources
    // +optional
    Annotations map[string]string `json:"annotations,omitempty"`

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
    // Pending: Waiting to start claiming
    // Claiming: In the process of claiming sandboxes
    // Completed: Claim process finished (either all replicas claimed or timeout reached)
    // +optional
    Phase SandboxClaimPhase `json:"phase,omitempty"`

    // Message provides human-readable details about the current phase
    // +optional
    Message string `json:"message,omitempty"`

    // ClaimedReplicas indicates how many sandboxes are currently claimed (total)
    // This is determined by querying sandboxes with matching ownerReference
    // Only updated during Pending and Claiming phases
    // +optional
    ClaimedReplicas int32 `json:"claimedReplicas,omitempty"`

    // TODO: Consider adding Sandboxes field to list claimed sandbox instances for routing.
    // However, storing/save all sandbox infos in status may cause etcd issues when replicas count is large.

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
    SandboxClaimPhasePending   SandboxClaimPhase = "Pending"
    SandboxClaimPhaseClaiming  SandboxClaimPhase = "Claiming"
    SandboxClaimPhaseCompleted SandboxClaimPhase = "Completed"
)

// SandboxClaimConditionType defines condition types
type SandboxClaimConditionType string

const (
    // SandboxClaimConditionCompleted indicates if the claim is completed
    SandboxClaimConditionCompleted SandboxClaimConditionType = "Completed"
)

```


#### Fields Comparison with E2B HTTP API

The SandboxClaim CRD is designed to align with the E2B HTTP API's `NewSandboxRequest`, with the following mappings:

| E2B API Field | SandboxClaim Field | Status | Notes |
|---------------|-------------------|--------|-------|
| `templateID` | `templateName` |  implementable | Maps to SandboxSet pool name |
| `timeout` | `shutdownTime` |  implementable | Absolute shutdown time |
| `envVars` | `envVars` | implementable | Environment variables for envd init |
| `autoPause` | N/A |  Future Work | Not implemented in E2B Api |
| `secure` | N/A |  Future Work | Not implemented in E2B Api |



### Implementation Details

**Claim Process**:
1. Track the start time when entering `Claiming` phase (for claimTimeout calculation)
2. Calculate how many sandboxes still need to be claimed: `remaining = spec.replicas - status.claimedReplicas`
3. List available sandboxes from the specified SandboxSet
4. Filter candidates by checking:
   - `Status.Phase == Running` (sandbox must be running)
   - `AnnotationLock == ""` (pre-check to skip already-locked sandboxes)
5. **Batch Claiming**: Attempt to claim multiple sandboxes within a single reconcile cycle:
   - Determine batch size: `min(remaining, maxBatchSize)` where `maxBatchSize` is a configurable limit (e.g., 10)
   - Randomly select multiple candidates (up to batch size) from the filtered list
   - For each sandbox, update with:
     - Lock annotation (UUID)
     - Claim timestamp annotation
     - Labels (from `claim.spec.labels`)
     - Annotations (from `claim.spec.annotations`)
     - Remove SandboxSet ownerReference (detach from pool)
     - Add SandboxClaim ownerReference
     - Set `spec.shutdownTime` (absolute time) if `shutdownTime` specified
6. Inject environment variables via envd `/init` endpoint (if `claim.spec.envVars` specified)
7. Update `status.claimedReplicas` with the actual number of successfully claimed sandboxes
8. Check completion conditions:
   - If `ClaimedReplicas == spec.replicas`: transition to `Completed`
   - If `spec.claimTimeout` is set and exceeded: transition to `Completed` (regardless of claimed count)
   - If SandboxSet is deleted: transition to `Completed` immediately
9. If `spec.ttlAfterCompleted` is set and claim is `Completed`, schedule deletion after the TTL duration

**Conflict Handling**:
The claim process uses **optimistic locking** via Kubernetes resourceVersion to handle concurrent claims:

- **Pre-check Filter**: Before attempting to claim, the controller filters out sandboxes that already have a `AnnotationLock` annotation. This reduces but doesn't eliminate race conditions.

- **Race Window**: Between the pre-check and the actual update, another process (e.g., other sandbox-manager) may claim the same sandbox.

- **Optimistic Locking**: When updating the sandbox, Kubernetes validates the resourceVersion. If the sandbox was modified by another process, the API returns a `Conflict` error.

- **Declarative Retry Model**:
  
  - **Conflict Handling**:
    - Each sandbox claim is independent and uses optimistic locking
    - If a `Conflict` error occurs for a specific sandbox:
      - Logs the conflict event for that sandbox
      - Other sandboxes in the batch continue to be processed
      - Successfully claimed sandboxes are counted in `status.claimedReplicas`
    - Partial success is acceptable: the controller tracks `claimedReplicas` and continues claiming remaining sandboxes
  - **Continuous Reconciliation**: Controller keeps reconciling until:
    - **Success**: All required sandboxes are claimed (ClaimedReplicas == spec.replicas, transitions to `Completed` phase)
    - **Timeout**: If `spec.claimTimeout` is set and exceeded, transitions to `Completed` phase regardless of claimed replicas count
    - **Immediate Failure**: If SandboxSet is deleted, transitions to `Completed` phase immediately
  - **Completion Behavior**: Once `Completed`, the controller stops reconciling and no longer watches claimed sandboxes

**Automatic Proxy Configuration**:
- Existing infra watches sandbox changes and automatically updates proxy routes
- No explicit proxy configuration needed in controller

#### State Management

The controller tracks claim state through phases:

- **Pending**: SandboxClaim created, waiting to start claiming
- **Claiming**: Actively claiming sandboxes within the timeout period
- **Completed**: Claim process finished (either all replicas claimed or timeout reached)

Once a claim reaches `Completed` phase, the controller stops watching or managing the claimed sandboxes. The sandboxes remain claimed but their lifecycle is no longer managed by the SandboxClaim controller.

State transitions:
```
         ┌──────────────┐
         │   Pending    │
         └──────┬───────┘
                │
                ▼
         ┌──────────────┐
         │   Claiming   │
         └──────┬───────┘
                │
                │ (all replicas claimed OR timeout reached)
                ▼
         ┌──────────────┐
         │  Completed   │
         └──────────────┘
```

**Key Transitions**:
- `Pending` → `Claiming`: Start claiming process
- `Claiming` → `Completed`: 
  - All required sandboxes claimed (ClaimedReplicas == spec.replicas), OR
  - Timeout reached (spec.claimTimeout exceeded)
- Once `Completed`, the claim will not transition to any other phase

**User Experience**:
```bash
# Get claim status (shows counts)
kubectl get sandboxclaim my-claim
# NAME       PHASE      TEMPLATE   DESIRED   CLAIMED   AGE
# my-claim   Completed  my-pool    3         2         5m


```

#### Completion and Cleanup

**Completion Conditions**:
- The claim transitions to `Completed` phase when:
  1. All required sandboxes are claimed (ClaimedReplicas == spec.replicas), OR
  2. The timeout period (spec.claimTimeout) is reached, OR
  3. The SandboxSet is deleted (immediate completion)

**Post-Completion Behavior**:
- Once `Completed`, the controller stops watching or managing the claimed sandboxes
- The sandboxes remain claimed (with ownerReference to SandboxClaim) but their lifecycle is no longer managed
- The `status.claimedReplicas` reflects the state at completion time

**Automatic Cleanup**:
- If `spec.ttlAfterCompleted` is set, the SandboxClaim will be automatically deleted after the specified duration once it reaches `Completed` phase
- This helps prevent accumulation of completed claims in the cluster
- **Important**: Only the SandboxClaim resource is deleted; the claimed sandboxes remain untouched and continue to exist independently


#### Querying Claimed Sandboxes

Query claimed sandboxes via Kubernetes API using labels or annotations. This approach works even after the SandboxClaim is deleted (e.g., via `ttlAfterCompleted`).

**Step 1**: Set unique labels or annotations in SandboxClaim spec (these will be copied to claimed sandboxes):
```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxClaim
metadata:
  name: test-batch
spec:
  templateName: python-pool
  replicas: 3
  labels:
    claim-id: "test-batch"  # Unique identifier for querying (recommended for label selector)
  annotations:
    project: "my-project"   # Additional metadata
```

**Step 2**: Query sandboxes via Kubernetes API:

**Using Labels (Recommended)**:
```bash
# Query sandboxes by label selector (most efficient)
kubectl get sandboxes -l claim-id=test-batch

# Query with multiple labels
kubectl get sandboxes -l claim-id=test-batch,project=my-project
```

**Using Annotations**:
```bash
# Using field selector (if annotations are indexed)
kubectl get sandboxes --field-selector metadata.annotations.claim-id=test-batch

# Or using jq to filter
kubectl get sandboxes -o json | jq '.items[] | select(.metadata.annotations."claim-id" == "test-batch")'
```



#### Routing to Specific Sandbox Instances


##### Load Balancing Implementation

Upper-layer platforms can implement custom load balancing strategies:
##### Traffic Flow

```
┌─────────────────────┐
│  Upper Platform     │
│  (Load Balancer)    │
└──────────┬──────────┘
           │
           │ 1. Query sandboxes via ownerReference
           │    kubectl get sandboxes -l <selector> or
           │    query by ownerReference to SandboxClaim
           │ 
           │
           ▼
┌─────────────────────────────────────┐
│  Sandbox Resources                  │
│  - sandboxID: sb-abc123, state: Running │
│  - sandboxID: sb-def456, state: Running │
│  - sandboxID: sb-ghi789, state: Running │
└──────────┬──────────────────────────┘
           │
           │ 2. Select sandbox
           │    Selected: sb-abc123
           │
           ▼
┌─────────────────────────────────────┐
│  Envoy Proxy                        │
│  (External Processor)               │
└──────────┬──────────────────────────┘
           │
           │ 3. Route to: 8080-sb-abc123.example.com
           │    Envoy looks up route for sandboxID: sb-abc123
           │    Sets x-envoy-original-dst-host: 10.244.0.10:8080
           │
           ▼
┌─────────────────────────────────────┐
│  Sandbox Pod (10.244.0.10:8080)    │
│  sandboxID: sb-abc123               │
└─────────────────────────────────────┘
```


