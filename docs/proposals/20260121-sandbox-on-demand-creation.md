---
title: Sandbox On-Demand Creation
authors:
  - "@uucloud"
reviewers:
  - "@furykerry"
  - "@sivanzcw"
creation-date: 2026-01-20
last-updated: 2026-01-21
status: provisional
see-also:
  - "/docs/proposals/20260106-sandboxset-autoscaler.md"
  - "/docs/proposals/20251229-sandbox-claim-crd.md"
replaces:
superseded-by:
---

# Sandbox On-Demand Creation

## Table of Contents

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals/Future Work](#non-goalsfuture-work)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1: Zero-Replica Cold Start](#story-1-zero-replica-cold-start)
    - [Story 2: Pool Exhaustion Fallback](#story-2-pool-exhaustion-fallback)
    - [Story 3: Cost-Optimized Development Environment](#story-3-cost-optimized-development-environment)
  - [Design Details](#design-details)
    - [API Changes](#api-changes)
      - [SandboxSet Spec Extension](#sandboxset-spec-extension)
      - [ClaimSandboxOptions Extension](#claimsandboxoptions-extension)
    - [Implementation Details](#implementation-details)
      - [On-Demand Creation Flow](#on-demand-creation-flow)
      - [Core Implementation](#core-implementation)
      - [Sandbox Lifecycle Management](#sandbox-lifecycle-management)
      - [Error Handling](#error-handling)
    - [Configuration Examples](#configuration-examples)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Upgrade Strategy](#upgrade-strategy)
- [Implementation History](#implementation-history)

## Summary

This proposal introduces **On-Demand Sandbox Creation** capability for SandboxSet, allowing the sandbox-manager to dynamically create new Sandbox instances when the warm pool is empty or exhausted. Currently, the ClaimSandbox operation strictly requires pre-warmed sandboxes in the pool; if `spec.replicas` is 0 or no available sandboxes exist, the claim fails immediately. This enhancement adds a fallback mechanism that creates sandboxes on-demand when no pre-warmed instances are available, enabling cost-optimized deployments and graceful handling of pool exhaustion scenarios.

## Motivation

The current SandboxSet implementation follows a pure "warm pool" model where:

1. **Pre-warming is mandatory**: `ClaimSandbox` only succeeds if there are available sandboxes in the pool
2. **Zero replicas means failure**: Setting `spec.replicas=0` results in all `ClaimSandbox` calls failing
3. **Pool exhaustion is not handled gracefully**: When all pre-warmed sandboxes are consumed, new requests fail until the pool replenishes

This design has limitations:

- **Cost inefficiency for low-traffic scenarios**: Users must maintain minimum replicas even during low-traffic periods, incurring unnecessary costs
- **No graceful degradation**: When the pool is exhausted, users experience immediate failures rather than degraded (slower) service
- **Limited flexibility**: Cannot support "cold start acceptable" use cases where startup latency is tolerable

### Goals

- **Enable on-demand sandbox creation**: When pool is empty, create sandboxes dynamically instead of failing
- **Support zero-replica configuration**: Allow `spec.replicas=0` for cost optimization while maintaining service availability
- **Provide graceful degradation**: When warm pool is exhausted, fall back to on-demand creation with acceptable latency
- **Configurable behavior**: Allow users to enable/disable on-demand creation and configure timeouts
- **Maintain backward compatibility**: Existing SandboxSet configurations continue to work without modification

### Non-Goals/Future Work

- **Replacing warm pool**: On-demand creation is a fallback, not a replacement for pre-warming
- **Complex scheduling logic**: This proposal focuses on simple on-demand creation; advanced scheduling (e.g., node selection, resource optimization) is out of scope
- **Automatic pool sizing**: This is addressed by the PoolAutoscaler proposal
- **Priority-based queue**: Request prioritization when multiple on-demand creations are pending

## Proposal

### User Stories

#### Story 1: Zero-Replica Cold Start

As a platform operator, I want to configure a SandboxSet with `replicas=0` for development environments to minimize costs, while still allowing sandboxes to be created on-demand when developers request them.

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxSet
metadata:
  name: dev-sandbox-pool
spec:
  replicas: 0  # No pre-warming for cost savings
  onDemandCreation:
    enabled: true
    timeout: 120s  # Allow up to 2 minutes for cold start
  template:
    spec:
      containers:
        - name: sandbox
          image: sandbox:latest
```

#### Story 2: Pool Exhaustion Fallback

As a platform operator, I want the system to create sandboxes on-demand when my warm pool is temporarily exhausted during traffic spikes, rather than failing requests immediately.

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxSet
metadata:
  name: prod-sandbox-pool
spec:
  replicas: 10  # Maintain warm pool of 10
  onDemandCreation:
    enabled: true
    timeout: 60s
    maxConcurrent: 5  # Limit concurrent on-demand creations
  template:
    # ...
```

#### Story 3: Cost-Optimized Development Environment

As a cost-conscious user, I want sandboxes to be created only when needed and automatically cleaned up, without maintaining any idle capacity.

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxSet
metadata:
  name: ephemeral-sandbox-pool
spec:
  replicas: 0
  onDemandCreation:
    enabled: true
    timeout: 180s
  template:
    # ...
```

### Design Details

#### API Changes

##### SandboxSet Spec Extension

Add a new field `OnDemandCreation` to `SandboxSetSpec`:

```go
// SandboxSetSpec defines the desired state of SandboxSet
type SandboxSetSpec struct {
    // Replicas is the number of unused sandboxes, including available and creating ones.
    Replicas int32 `json:"replicas"`

    // OnDemandCreation configures the on-demand sandbox creation behavior.
    // When enabled, the sandbox-manager will create sandboxes dynamically
    // if no pre-warmed sandboxes are available.
    // +optional
    OnDemandCreation *OnDemandCreationPolicy `json:"onDemandCreation,omitempty"`

    // ... existing fields ...
}

// OnDemandCreationPolicy defines the configuration for on-demand sandbox creation
type OnDemandCreationPolicy struct {
    // Enabled indicates whether on-demand creation is enabled.
    // When true, sandbox-manager will create sandboxes dynamically if pool is empty.
    // +optional
    // +kubebuilder:default=false
    Enabled bool `json:"enabled,omitempty"`

    // Timeout is the maximum duration to wait for an on-demand sandbox to become ready.
    // If the sandbox does not reach Running state within this timeout, the claim fails.
    // +optional
    // +kubebuilder:default="60s"
    Timeout *metav1.Duration `json:"timeout,omitempty"`

    // MaxConcurrent limits the maximum number of concurrent on-demand creation operations.
    // This prevents resource exhaustion during traffic spikes.
    // 0 means no limit.
    // +optional
    // +kubebuilder:default=0
    MaxConcurrent int32 `json:"maxConcurrent,omitempty"`
}
```

##### ClaimSandboxOptions Extension

Extend `ClaimSandboxOptions` to support on-demand creation:

```go
type ClaimSandboxOptions struct {
    Modifier func(sandbox Sandbox)
    Image    string

    // AllowOnDemand indicates whether to allow on-demand creation if pool is empty.
    // This is used by sandbox-manager to pass the on-demand flag from SandboxSet config.
    // +optional
    AllowOnDemand bool

    // OnDemandTimeout is the timeout for on-demand sandbox creation.
    // Only used when AllowOnDemand is true.
    // +optional
    OnDemandTimeout time.Duration
}
```

#### Implementation Details

##### On-Demand Creation Flow

The modified `ClaimSandbox` flow:

```
ClaimSandbox Request
        │
        ▼
┌───────────────────────┐
│ Try to pick available │
│ sandbox from pool     │
└───────────┬───────────┘
            │
            ▼
      ┌───────────┐     Yes    ┌──────────────────┐
      │ Available │ ─────────► │ Claim & Return   │
      │ sandbox?  │            │ (existing flow)  │
      └─────┬─────┘            └──────────────────┘
            │ No
            ▼
      ┌───────────────┐   No    ┌──────────────────┐
      │ OnDemand      │ ───────►│ Return Error:    │
      │ enabled?      │         │ "no stock"       │
      └───────┬───────┘         └──────────────────┘
              │ Yes
              ▼
      ┌───────────────┐   Yes   ┌──────────────────┐
      │ MaxConcurrent │ ───────►│ Return Error:    │
      │ exceeded?     │         │ "rate limited"   │
      └───────┬───────┘         └──────────────────┘
              │ No
              ▼
┌─────────────────────────────┐
│ Create Sandbox CR           │
│ (with claimed=true label)   │
└─────────────┬───────────────┘
              │
              ▼
┌─────────────────────────────┐
│ Wait for Sandbox to become  │
│ Running (with timeout)      │
└─────────────┬───────────────┘
              │
              ▼
      ┌───────────────┐  Timeout ┌──────────────────┐
      │ Sandbox       │ ────────►│ Delete Sandbox & │
      │ Running?      │          │ Return Error     │
      └───────┬───────┘          └──────────────────┘
              │ Yes
              ▼
┌─────────────────────────────┐
│ Return claimed Sandbox      │
└─────────────────────────────┘
```

##### Core Implementation

Modify `pool.go` to add on-demand creation support:

```go
// ClaimSandbox claims a Sandbox CR as Sandbox from SandboxSet
func (p *Pool) ClaimSandbox(ctx context.Context, user string, candidateCounts int, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
    lock := uuid.New().String()
    log := klog.FromContext(ctx).WithValues("pool", p.Namespace+"/"+p.Name)

    // Try to claim from warm pool first
    sbx, err := p.tryClaimFromPool(ctx, user, candidateCounts, lock, opts)
    if err == nil {
        return sbx, nil
    }

    // Check if it's a "no stock" error and on-demand is enabled
    if !isNoStockError(err) {
        return nil, err
    }

    if !opts.AllowOnDemand {
        return nil, err // Return original "no stock" error
    }

    log.Info("pool empty, attempting on-demand creation")

    // Check concurrent creation limit
    if !p.acquireOnDemandSlot() {
        return nil, fmt.Errorf("on-demand creation rate limited: max concurrent creations reached")
    }
    defer p.releaseOnDemandSlot()

    // Create sandbox on-demand
    return p.createOnDemandSandbox(ctx, user, lock, opts)
}

func (p *Pool) createOnDemandSandbox(ctx context.Context, user, lock string, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
    log := klog.FromContext(ctx).WithValues("pool", p.Namespace+"/"+p.Name)

    // Create a new Sandbox CR directly (bypassing SandboxSet controller)
    sbx, err := p.createSandboxCR(ctx, user, lock)
    if err != nil {
        return nil, fmt.Errorf("failed to create on-demand sandbox: %w", err)
    }

    log.Info("on-demand sandbox created, waiting for ready", "sandbox", klog.KObj(sbx.Sandbox))

    // Wait for sandbox to become Running
    timeout := opts.OnDemandTimeout
    if timeout == 0 {
        timeout = DefaultOnDemandTimeout
    }

    if err := p.waitForSandboxReady(ctx, sbx, timeout); err != nil {
        // Cleanup failed sandbox
        log.Error(err, "on-demand sandbox failed to become ready, cleaning up")
        _ = p.client.ApiV1alpha1().Sandboxes(sbx.Namespace).Delete(ctx, sbx.Name, metav1.DeleteOptions{})
        return nil, fmt.Errorf("on-demand sandbox failed to become ready: %w", err)
    }

    log.Info("on-demand sandbox ready", "sandbox", klog.KObj(sbx.Sandbox))
    return sbx, nil
}

func (p *Pool) createSandboxCR(ctx context.Context, user, lock string) (*Sandbox, error) {
    // Get SandboxSet spec for template
    sbs, err := p.getSandboxSet(ctx)
    if err != nil {
        return nil, err
    }

    sbx := &agentsv1alpha1.Sandbox{
        ObjectMeta: metav1.ObjectMeta{
            GenerateName: fmt.Sprintf("%s-ondemand-", p.Name),
            Namespace:    p.Namespace,
            Labels: map[string]string{
                agentsv1alpha1.LabelSandboxPool:      p.Name,
                agentsv1alpha1.LabelSandboxIsClaimed: "true",  // Mark as claimed immediately
            },
            Annotations: map[string]string{
                agentsv1alpha1.AnnotationLock:      lock,
                agentsv1alpha1.AnnotationOwner:     user,
                agentsv1alpha1.AnnotationClaimTime: time.Now().Format(time.RFC3339),
            },
        },
        Spec: agentsv1alpha1.SandboxSpec{
            PersistentContents: sbs.Spec.PersistentContents,
            SandboxTemplate: agentsv1alpha1.SandboxTemplate{
                TemplateRef:          sbs.Spec.TemplateRef,
                Template:             sbs.Spec.Template.DeepCopy(),
                VolumeClaimTemplates: sbs.Spec.VolumeClaimTemplates,
            },
        },
    }

    // Note: No ownerReference to SandboxSet - this sandbox is independent
    created, err := p.client.ApiV1alpha1().Sandboxes(p.Namespace).Create(ctx, sbx, metav1.CreateOptions{})
    if err != nil {
        return nil, err
    }

    return AsSandbox(created, p.cache, p.client), nil
}

func (p *Pool) waitForSandboxReady(ctx context.Context, sbx *Sandbox, timeout time.Duration) error {
    return p.cache.WaitForSandboxSatisfied(ctx, sbx.Sandbox, WaitActionOnDemandReady, func(s *agentsv1alpha1.Sandbox) (bool, error) {
        state, reason := stateutils.GetSandboxState(s)
        if state == agentsv1alpha1.SandboxStateRunning {
            sbx.Sandbox = s
            return true, nil
        }
        if state == agentsv1alpha1.SandboxStateDead {
            return false, fmt.Errorf("sandbox entered dead state: %s", reason)
        }
        return false, nil
    }, timeout)
}

// Concurrent creation limiting
var (
    onDemandSemaphore sync.Map // pool name -> *semaphore
)

func (p *Pool) acquireOnDemandSlot() bool {
    // Implementation of semaphore-based rate limiting
    // Returns true if slot acquired, false if limit reached
}

func (p *Pool) releaseOnDemandSlot() {
    // Release the acquired slot
}
```

##### Sandbox Lifecycle Management

On-demand created sandboxes have different lifecycle characteristics:

| Aspect | Warm Pool Sandbox | On-Demand Sandbox |
|--------|-------------------|-------------------|
| OwnerReference | Points to SandboxSet | None (independent) |
| `LabelSandboxIsClaimed` | `false` → `true` on claim | `true` from creation |
| Pool Membership | Managed by SandboxSet controller | Independent, but labeled with pool name |
| Scaling | Counted in SandboxSet replicas | Not counted (not managed) |
| Cleanup | SandboxSet controller on scale-down | User/timeout only |

##### Error Handling

| Error Scenario | Handling |
|----------------|----------|
| Pool empty, on-demand disabled | Return `NoAvailableError` (existing behavior) |
| Pool empty, on-demand enabled, rate limited | Return rate limit error |
| Sandbox creation failed | Return creation error |
| Sandbox not ready within timeout | Delete sandbox, return timeout error |
| Sandbox enters Dead state during wait | Delete sandbox, return failure error |

#### Configuration Examples

**Cost-Optimized Development Pool**:
```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxSet
metadata:
  name: dev-pool
spec:
  replicas: 0  # No pre-warming
  onDemandCreation:
    enabled: true
    timeout: 120s
  template:
    spec:
      containers:
        - name: sandbox
          image: dev-sandbox:latest
```

**Production Pool with Fallback**:
```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxSet
metadata:
  name: prod-pool
spec:
  replicas: 20
  onDemandCreation:
    enabled: true
    timeout: 60s
    maxConcurrent: 10  # Limit burst creation
  template:
    spec:
      containers:
        - name: sandbox
          image: prod-sandbox:latest
```

**High-Performance Pool (No On-Demand)**:
```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxSet
metadata:
  name: latency-critical-pool
spec:
  replicas: 50
  onDemandCreation:
    enabled: false  # Strict pre-warming only
  template:
    # ...
```

### Risks and Mitigations

#### Risk 1: Resource Exhaustion from Unbounded Creation

**Risk**: Without limits, on-demand creation could exhaust cluster resources during traffic spikes.

**Mitigation**:
- `maxConcurrent` configuration limits concurrent creations
- Integration with PoolAutoscaler for dynamic capacity management
- Cluster resource quotas remain effective

#### Risk 2: Increased Latency Variance

**Risk**: On-demand creation introduces unpredictable latency compared to pre-warmed pools.

**Mitigation**:
- Clear documentation that on-demand adds startup latency
- Metrics to track on-demand vs. warm pool claim latency
- Recommend appropriate `replicas` for latency-sensitive workloads

#### Risk 3: Orphaned Sandboxes

**Risk**: On-demand sandboxes without ownerReference may become orphaned if cleanup fails.

**Mitigation**:
- Sandbox controller's existing Dead state cleanup handles failed sandboxes
- Add garbage collection for sandboxes that exceed timeout without becoming Ready
- Monitoring and alerting for long-lived unclaimed sandboxes

## Upgrade Strategy

### Backward Compatibility

- Existing SandboxSets without `onDemandCreation` field continue to work unchanged
- Default behavior (`enabled: false`) maintains current semantics
- No migration required

## Implementation History

- [x] 2026-01-20: Initial proposal created
- [ ] YYYY-MM-DD: Proposal reviewed and approved
- [ ] YYYY-MM-DD: Implementation started
