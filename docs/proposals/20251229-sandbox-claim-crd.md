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

    // Sandboxes lists all claimed sandbox instances with their details
    // This allows upper-layer platforms to route requests to specific sandbox instances
    // +optional
    Sandboxes []ClaimedSandboxInfo `json:"sandboxes,omitempty"`

    // Conditions represent the current state of the SandboxClaim
    // +optional
    // +listType=map
    // +listMapKey=type
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ClaimedSandboxInfo contains information about a claimed sandbox instance
type ClaimedSandboxInfo struct {
    // Name is the Kubernetes resource name of the Sandbox
    Name string `json:"name"`

    // SandboxID is the unique identifier used for routing (e.g., "sb-abc123")
    SandboxID string `json:"sandboxID"`

    // State indicates the current state (Running, Paused, etc.)
    State string `json:"state"`

    // IP is the Pod IP address
    // +optional
    IP string `json:"ip,omitempty"`

    // ClaimedAt is the timestamp when the sandbox was claimed
    ClaimedAt metav1.Time `json:"claimedAt"`
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

#### Querying Claimed Sandboxes

##### Method 1: Query SandboxClaim Status (Recommended)

The `status.sandboxes` field lists all claimed sandbox instances with routing information:

```bash
# View claim status
kubectl get sandboxclaim test-batch -o yaml
```

Example output:
```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxClaim
metadata:
  name: test-batch
spec:
  sandboxSetName: python-pool
  replicas: 3
status:
  phase: Ready
  claimedReplicas: 3
  readyReplicas: 3
  sandboxes:
    - name: python-pool-abc123
      sandboxID: sb-abc123
      state: Running
      ip: 10.244.0.10
      claimedAt: "2025-12-30T10:00:00Z"
    - name: python-pool-def456
      sandboxID: sb-def456
      state: Running
      ip: 10.244.0.11
      claimedAt: "2025-12-30T10:00:05Z"
    - name: python-pool-ghi789
      sandboxID: sb-ghi789
      state: Running
      ip: 10.244.0.12
      claimedAt: "2025-12-30T10:00:10Z"
```

Extract sandbox IDs:
```bash
# Get all sandbox IDs
kubectl get sandboxclaim test-batch -o jsonpath='{.status.sandboxes[*].sandboxID}'
# Output: sb-abc123 sb-def456 sb-ghi789

# Get sandbox IDs in JSON format
kubectl get sandboxclaim test-batch -o jsonpath='{.status.sandboxes}' | jq
```

##### Method 2: Query via Kubernetes API (Using OwnerReference)

Claimed sandboxes have the SandboxClaim set as their ownerReference:

```bash
# List all sandboxes owned by a specific claim
kubectl get sandboxes -o json | jq '.items[] | select(.metadata.ownerReferences[]?.name == "test-batch")'

```

##### Method 3: Query via E2B API (Using Metadata)

If the SandboxClaim sets a unique metadata annotation, sandboxes can be queried via E2B API:

**Step 1**: Set unique metadata in SandboxClaim:
```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxClaim
metadata:
  name: test-batch
spec:
  sandboxSetName: python-pool
  replicas: 3
  metadata:
    claim-id: "test-batch"  # Unique identifier for querying
    project: "my-project"
```

**Step 2**: Query via E2B API:
```bash
# Query by metadata
curl -H "X-API-KEY: $API_KEY" \
  "https://api.example.com/v2/sandboxes?claim-id=test-batch&state=running,paused"
```

Response:
```json
[
  {
    "sandboxID": "sb-abc123",
    "templateID": "python-pool",
    "state": "running",
    "metadata": {
      "claim-id": "test-batch",
      "project": "my-project"
    },
    "startedAt": "2025-12-30T10:00:00Z"
  },
  ...
]
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
           │ 1. Query SandboxClaim status
           │ 
           │
           ▼
┌─────────────────────────────────────┐
│  SandboxClaim Status                │
│  sandboxes:                         │
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


