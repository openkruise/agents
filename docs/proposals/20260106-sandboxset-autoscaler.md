---
title: SandboxSet supports autoscaler
authors:
  - "@sivanzcw"
reviewers:
  - "@furykerry"
creation-date: 2026-01-06
last-updated: 2026-01-15
status: implementable
see-also:
replaces:
superseded-by:
---

# SandboxSet supports autoscaler

## Table of Contents
- [Title](#title)
    - [Table of Contents](#table-of-contents)
    - [Summary](#summary)
    - [Motivation](#motivation)
        - [Goals](#goals)
        - [Non-Goals/Future Work](#non-goalsfuture-work)
    - [Proposal](#proposal)
        - [User Stories](#user-stories)
            - [Conversational Agent](#conversational-agent)
            - [Workflow Agent](#workflow-agent)
            - [Agent Fan-out](#agent-fan-out)
            - [RL](#RL)
        - [Design Details](#design-details)
            - [API](#API)
                - [Pool Capacity Control](#pool-capacity-control)
                - [Pool Scaling Policy](#pool-scaling-policy)
                    - [Cron-based Policy](#cron-based-policy)
                    - [Capacity Availability Policy](#capacity-availability-policy)
                - [Status](#Status)
            - [Metrics](#Metrics)
                - [Reconciliation Metrics](#reconciliation-metrics)
            - [User Configuration Examples](#user-configuration-examples)
                - [Bounds Enforcer](#bounds-enforcer)
                - [Cron-Based Scaling](#cron-based-scaling)
                - [Capacity-Based Scaling with Watermarks](#capacity-based-scaling-with-watermarks)
                    - [Absolute Value Watermark Configuration](#absolute-value-watermark-configuration)
                    - [Percentage-Based Watermark Configuration](#percentage-based-watermark-configuration)
        - [Implementation Details/Notes/Constraints](#implementation-detailsnotesconstraints)
            - [Policy Combination Limitations](#policy-combination-limitations)
            - [Observation Window and Sampling Configuration](#observation-window-and-sampling-configuration)
            - [Cron Policy Maintenance Window Support](#cron-policy-maintenance-window-support)
            - [One-to-One Relationship Between Warm Pool and Autoscaler](#one-to-one-relationship-between-warm-pool-and-autoscaler)
        - [Risks and Mitigations](#risks-and-mitigations)
            - [Controller Computational Complexity and Resource Consumption](#controller-computational-complexity-and-resource-consumption)
            - [Frequent Scaling Due to Misconfiguration](#frequent-scaling-due-to-misconfiguration)
            - [Extreme Behavior from Invalid Configuration Combinations](#extreme-behavior-from-invalid-configuration-combinations)
            - [Observability and Debugging Challenges](#observability-and-debugging-challenges)
    - [Alternatives](#alternatives)
        - [Extend Existing HPA for SandboxSet](#extend-existing-hpa-for-sandboxSet)
        - [Use External Autoscaling Tools](#use-external-autoscaling-tools)
    - [Upgrade Strategy](#upgrade-strategy)
        - [API Versioning](#api-versioning)
        - [Backward Compatibility](#backward-compatibility)
        - [Upgrade Path](#upgrade-path)
            - [From No Autoscaler to PoolAutoscaler](#from-no-autoscaler-to-poolAutoscaler)
            - [Upgrading Autoscaler Configuration](#upgrading-autoscaler-configuration)
            - [Controller Upgrade](#controller-upgrade)
        - [Downgrade Strategy](#downgrade-strategy)
            - [Downgrading Controller Version](#downgrading-controller-version)
            - [Removing Autoscaler](#removing-autoscaler)
        - [Version Skew Strategy](#version-skew-strategy)
            - [API Server and Controller Version Skew](#api-server-and-controller-version-skew)
    - [Additional Details](#additional-details)
    - [Test Plan](#test-plan-optional)
        - [Unit Tests](#unit-tests)
        - [Integration Tests](#integration-tests)
        - [Performance Tests](#performance-tests)
    - [Implementation History](#implementation-history)

## Summary

This enhancement proposes providing autoscaler capabilities for `SandboxSet`,
enabling intelligent and dynamic management of pre-warmed sandbox resource pools.
Currently, `SandboxSet` provides sandbox pre-warming capabilities with fixed-size resource pools.
This enhancement adds two complementary autoscaling policy types:

1. **Cron-based policies**: Enable time-driven scaling based on recurring schedules
(e.g., scale up during business hours, scale down during off-peak periods)
2. **Capacity-based policies**: Enable demand-driven scaling based on available resource watermarks,
ensuring sufficient idle capacity while preventing over-provisioning

The autoscaler integrates seamlessly with `SandboxSet`, providing operators with fine-grained
control over resource pool capacity while reducing operational overhead. This enhancement addresses
critical requirements for latency-sensitive agent workloads that require predictable startup performance
and efficient resource utilization.

## Motivation

Agents are often part of latency-sensitive execution paths and operate under highly dynamic workloads.
As a result, they have strong requirements around startup performance. By pre-warming sandbox objects
that provide the agent runtime environment through SandboxSet, agents can achieve near-real-time startup
and stable startup latency. However, SandboxSet currently supports only a fixed-size pre-warmed resource
pool, which cannot handle batch agent startup scenarios nor support predictable startup behavior.
Agents frequently create multiple sandboxes in parallel due to task decomposition or fan-out execution.
The system must support launching many sandboxes concurrently without linear degradation.
Startup latency should be correlated with load and capacity rather than exhibiting random jitter.
Predictability is critical for planners and schedulers that make execution decisions based on expected
availability.

Enabling autoscaler for `SandboxSet` addresses these challenges by providing:

- **Intelligent and dynamic resource pool management**: Operators can configure intelligent pre-warmed
pool management policies based on workload concurrency demands. This helps prevent unpredictable
startup latency, excessive request queuing during peak traffic, and situations where the planner
is forced into serial execution. The autoscaler automatically adjusts pool capacity based on
configured policies, ensuring optimal resource availability.
- **Improved resource utilization**: By dynamically adjusting the capacity of the resource pre-warming pool,
the cluster can more efficiently handle fluctuations in overall resource usage, thereby reducing
over-reservation and over-provisioning. The capacity-based policy maintains optimal idle resource
levels, scaling up during demand surges and scaling down during low-utilization periods.
- **Reduced operational overhead**: By eliminating the need for frequent manual adjustments to the
pre-warmed pool capacity, operational overhead is significantly reduced. Operators can define policies
once and let the autoscaler handle routine scaling decisions, freeing up time for higher-value tasks.

### Goals

This proposal aims to:

- **Provide periodic autoscaling capabilities**: Enable the pre-warming pool to scale up or down on a
recurring basis using cron-based policies. This supports predictable, time-driven scaling patterns
(e.g., scale up before business hours, scale down during off-peak periods).
- **Support capacity-based autoscaling**: Maintain a target range of idle resources in the pre-warming pool
through watermark-based policies. This ensures timely replenishment of available resources while
preventing excessive pre-warming that could increase costs.
- **Ensure predictable scaling behavior**: Provide clear, interpretable scaling policies with
configurable bounds, stabilization windows, and tolerance values. Operators should be able to
reason about and predict autoscaler behavior.
- **Maintain system stability**: Prevent scaling conflicts, oscillations, and resource exhaustion
through proper validation, bounds enforcement, and stabilization mechanisms.

### Non-Goals/Future Work

- **Complex machine learning algorithms**: This enhancement focuses on interpretable,
rule-based scaling strategies rather than complex ML-driven algorithms.
Future enhancements may explore predictive scaling based on historical patterns.
- **Cluster-level resource scaling**: This proposal focuses on sandbox-level resource pool management.
Cluster-level node scaling is handled by the cluster autoscaler and is out of scope.
- **HPA policy coordination**: There may be HPA policies configured for pods within the cluster
that could conflict with `SandboxSet` scaling policies. This proposal does not address coordination
between HPA and `PoolAutoscaler` for now and will be extended in the future based on usage scenarios
and user feedback.
- **Multi-policy combination**: Currently, cron-based and capacity-based policies cannot be used simultaneously.
Future enhancements may support policy composition with clear precedence rules.
- **Maintenance window support**: Cron-based policies do not currently support maintenance window configuration.
This will be added based on user requirements and feedback.

## Proposal

### User Stories

#### Conversational Agent

Conversational agents are interactive and highly sensitive to latency, with users having little tolerance
for delays. Consequently, sandbox startup is expected to be effectively instantaneous, which requires
maintaining a steady pool of idle sandboxes to reliably absorb high levels of concurrent demand.

#### Workflow Agent

Workflow agents typically handle complex, multi-step workflows that involve invoking multiple tools
and running across multiple sandboxes. The underlying platform must therefore support parallel startup
of multiple sandboxes, as startup latency directly impacts overall task completion time. Pre-warming
management must be able to dynamically adjust the size of the pre-warmed pool based on task scale,
enabling rapid capacity expansion during peak periods.

#### Agent Fan-out

In data analytics scenarios, a single agent may fan-out into multiple sub-agents, resulting in a
sudden surge in sandbox demand. To prevent task queuing, the environment must be able to provide
a large number of ready sandboxes within a very short time. The pre-warming pool must therefore
be designed to immediately absorb peak fan-out traffic.

#### RL

During RL training, a large number of short-lived sandboxes are created, which demands extremely
low creation overhead and highly efficient batch startup. This makes it essential for the system
to maintain a large, steady pre-warmed sandbox pool.

### Design Details

#### API

Provide autoscaler configurations for the warming pool, supporting both cron-based
and watermark-based capacity control policies.

The `PoolAutoscaler` API provides comprehensive autoscaler configurations for the warming pool,
supporting both cron-based and capacity-based (watermark) scaling policies.
The API is designed following Kubernetes best practices, with clear separation between spec
(desired state) and status (observed state).

**Key Design Principles**:
- **One autoscaler per target**: Each `SandboxSet` can be managed by at most one `PoolAutoscaler`
to prevent conflicts
- **Policy exclusivity**: Cron-based and capacity-based policies are mutually exclusive
to ensure predictable behavior
- **Bounds enforcement**: All scaling operations respect `minReplicas` and `maxReplicas` constraints
- **Status transparency**: Rich status information enables observability and debugging

```go
// PoolAutoscaler is the configuration for a warming pool autoscaler,
// which automatically manages the replica count of the warming pool
// based on the policies specified.
type PoolAutoscaler struct {
	metav1.TypeMeta
	// Metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta

	// spec is the specification for the behavior of the autoscaler.
	// +optional
	Spec PoolAutoscalerSpec

	// status is the current information about the autoscaler.
	// +optional
	Status PoolAutoscalerStatus
}

// PoolAutoscalerSpec describes the desired functionality of the PoolAutoscaler.
type PoolAutoscalerSpec struct {
	// ScaleTargetRef points to the target warming pool to scale, and is used to the pods for which instance status
	// should be collected, as well as to actually change the replica count.
	// +required
	ScaleTargetRef CrossVersionObjectReference
	// MaxReplicas is the upper limit for the number of replicas to which the autoscaler can scale up.
	// It cannot be less that minReplicas.
	// +required
	MaxReplicas int32
	// MinReplicas is the lower limit for the number of replicas to which the autoscaler
	// can scale down.
	// It defaults to 0 pod.
	MinReplicas *int32

	// CronPolicies is a list of potential cron scaling polices which can be used during scaling.
	// +optional
	CronPolicies []CronScalingPolicy

	// CapacityPolicy defines the capacity configuration of the target resource pool.
	// +optional
	CapacityPolicy *CapacityPolicy

	// This flag tells the controller to suspend subsequent executions
	// Defaults to false.
	// +optional
	Suspend bool
}

// CronScalingPolicy defines the cron-based scaling configuration for the resource pool.
type CronScalingPolicy struct {
	// Name is used to specify the scaling policy.
	// +required
	Name string
	// The time zone name for the given schedule.
	// If not specified, this will default to the time zone of the autoscaler controller manager process.
	// The set of valid time zone names and the time zone offset is loaded from the system-wide time zone
	// database by the webhook during PoolAutoscaler validation and the controller manager during execution.
	// If no system-wide time zone database can be found a bundled version of the database is used instead.
	// If the time zone name becomes invalid during the lifetime of a PoolAutoscaler or due to a change in host
	// configuration, the controller will stop syncing the warming pool and will create a system event with the
	// reason UnknownTimeZone.
	// +optional
	TimeZone *string
	// Schedule is a cron expression that defines when this policy should be executed.
	// Supports standard cron format with 5 fields (minute hour day month weekday)
	// Example:
	// "0 8 * * *"     - Every day at 8:00 AM
	// "0 0 * * 1-5"   - Every weekday at midnight
	// "0 */2 * * *"   - Every 2 hours
	// "30 0 * * *"    - Every day at 00:30
	// "0 0 1 * *"     - First day of every month at midnight
	// +required
	Schedule string
	// TargetReplicas is the desired replicas.
	// +required
	TargetReplicas *int32
}

// CapacityPolicy defines the capacity configuration of the target resource pool.
type CapacityPolicy struct {
	// TargetAvailable is the desired available replicas.
	// +required
	TargetAvailable intstr.IntOrString
	// Tolerance is the tolerance between the watermark and desired
	// value under which no updates are made to the desired number of
	// replicas (e.g. 0.01 for 1%). Must be greater than or equal to zero. If not
	// set, the default cluster-wide tolerance is applied (by default 10%).
	// +optional
	Tolerance *intstr.IntOrString
	// scaleUp is scaling rule for scaling Up.
	// +optional
	ScaleUp *CapacityScalingRules
	// scaleDown is scaling rule for scaling Down.
	// +optional
	ScaleDown *CapacityScalingRules
}

type CapacityScalingRules struct {
	// StabilizationWindowSeconds is the number of seconds for which past recommendations should be
	// considered while scaling up or scaling down.
	// StabilizationWindowSeconds must be greater than or equal to zero and less than or equal to 3600 (one hour).
	// If not set, use the default values:
	// - For scale up: 0 (i.e. no stabilization is done).
	// - For scale down: 300 (i.e. the stabilization window is 300 seconds long).
	// +optional
	StabilizationWindowSeconds *int32
}

// PoolAutoscalerStatus describes the current status of a pool autoscaler.
type PoolAutoscalerStatus struct {
	// ObservedGeneration is the most recent generation observed by this autoscaler.
	// +optional
	ObservedGeneration *int64

	// LastScaleTime is the last time the PoolAutoscaler scaled the number of pods,
	// used by the autoscaler to control how often the number of pods is changed.
	// +optional
	LastScaleTime *metav1.Time

	// CurrentReplicas is current number of replicas of pods managed by this autoscaler,
	// as last seen by the autoscaler.
	CurrentReplicas int32

	// DesiredReplicas is the desired number of replicas of pods managed by this autoscaler,
	// as last calculated by the autoscaler.
	DesiredReplicas int32
	// This flag tells the controller to suspend subsequent executions
	// Defaults to false.
	// +optional
	Suspended bool
	// Policy execution status.
	AppliedCronPolicies []CronScalingPolicyStatus
	// CurrentCapacity is the last read state of the capacity used by this autoscaler.
	// +listType=atomic
	// +optional
	CurrentCapacity CapacityStatus

	// Conditions is the set of conditions required for this autoscaler to scale its target,
	// and indicates whether those conditions are met.
	Conditions []PoolAutoscalerCondition
}

type CronScalingPolicyStatus struct {
	// Name is used to specify the scaling policy.
	// +required
	Name string
	// Information when was the last time the policy was successfully scheduled.
	// +optional
	LastScheduleTime *metav1.Time
}

type CapacityStatus struct {
	// Available is current number of available pods managed by this autoscaler,
	// as last seen by the autoscaler.
	Available int32
}

// PoolAutoscalerCondition describes the state of
// a PoolAutoscaler at a certain point.
type PoolAutoscalerCondition struct {
	// Type describes the current condition
	Type PoolAutoscalerConditionType
	// Status is the status of the condition (True, False, Unknown)
	Status ConditionStatus
	// LastTransitionTime is the last time the condition transitioned from
	// one status to another
	// +optional
	LastTransitionTime metav1.Time
	// Reason is the reason for the condition's last transition.
	// +optional
	Reason string
	// Message is a human-readable explanation containing details about
	// the transition
	// +optional
	Message string
}

// PoolAutoscalerConditionType are the valid conditions of
// a PoolAutoscaler.
type PoolAutoscalerConditionType string
```

##### Pool Capacity Control

To ensure cost control and avoid runaway scaling due to misconfiguration, user need the ability
to cap the total capacity of the resource pool. At the same time, a minimum level of capacity is
required during off-peak periods to serve baseline workloads. To support these requirements, the
pool autoscaler introduces configurable upper and lower capacity limits.

| Field           | Description                                                                                                                                                                                                        | Use Cases                                                                                                                                                                                                                                                                                                                                                                                             | Validation Rules                                                                                                                                   |
|-----------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------|
| **maxReplicas** | The upper limit for the number of replicas to which the autoscaler can scale up.                                                                                                                                   | 1. *Limit scaling up*: Prevents autoscaler from scaling beyond the specified maximum, protecting cluster resources and controlling costs. Prevents infinite scaling that could exhaust cluster resources.<br/>2. *Immediate scale down*: When current replicas exceed `maxReplicas` (e.g., manual scaling or autoscaler config update), autoscaler immediately scales down to the maximum limit.<br/> | 1. *Required field*: Must be specified in the autoscaler spec.<br/>2. *Must be greater than 0.*<br/>3. *Must be >= `minReplicas`.*                 |
| **minReplicas** | The lower limit for the number of replicas to which the autoscaler can scale down.                                                                                                                                 | 1. *Prevent excessive scale down*: Ensures a minimum number of pod are always available, maintaining service availability and handing baseline traffic.<br/> 2. *Default behavior*: If not specified, defaults to 0 pod.                                                                                                                                                                              | 1. *Option field*: Can be omitted, in which case it defaults to 0.<br/>2. *Relationship with maxReplicas*: `maxReplicas` must be >= `minReplicas`. |
| **suspend**     | A boolean flag that controls whether the autoscaler controller should suspend subsequent policy executions. When set to `true`, the controller will not sync the policy. when `false`, the policy synced normally. | 1. *Temporary suspension*: Temporarily pause policy execution during maintenance, debugging, or troubleshooting without deleting the autoscaler object.<br/> 2. *Conditional execution*: Use suspend flag to enable/disable autoscaler based on external conditions or feature flags.                                                                                                                 | 1. *Optional field*: Can be omitted.                                                                                                               |

All policy behaviors are governed by the pool capacity control.

##### Pool Scaling Policy

The autoscaler supports dynamic management of both the total resource pool size and the available
capacity, balancing user's requirements for overall resource limits and capacity availability.

###### Cron-based Policy

Cron-based scaling policies are provided to support planned resource management and recurring
resource consumption patterns.

| Field              | Description                                                                                                                                                                                                                                                                 | Use Cases                                                                                                                                                                                                                                                                                                                           | Validation                                                                                                                                                                                |
|--------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **name**           | A unique identifier for the scaling policy. Used to track and manage individual scaling policy in the status conditions. The name helps distinguish between multiple policies and provides a reference for monitoring and debugging.                                        | 1. *Policy identification*: Uniquely identify each scaling policy in a cron autoscaler, enabling clear tracking of which policy executed and its result.<br/>2. *Status tracking*: Map policy execution results to specific policies in the `status.conditions` array, where each condition uses the policy name as the identifier. | 1. *Required field*: Must be specified, cannot be empty or omitted.                                                                                                                       |
| **timezone**       | The time zone name for interpreting the schedule. Specifies the IANA time zone database name (e.g., "Asia/Shanghai", "UTC") that should be used when evaluating the cron schedule. If not specified, defaults to the time zone of the autoscale controller manager process. | 1. *Global deployments*: Ensure consistent scheduling across clusters in different geographic regions by explicitly setting timezones.                                                                                                                                                                                              | 1. *Optional field*: Can be omitted. If not specified, defaults to the autoscale controller manager process.<br/>2. *IANA timezone format*: Must be a valid IANA time zone database name. |
| **schedule**       | A cron expression that defines when the scaling policy should be executed. Specifies that the exact time pattern (minute, hour, day, month, weekday) when the target workload should be scaled to the `targetReplicas`.                                                     | 1. *Time-based scalling*: Define specific time for scaling operation.                                                                                                                                                                                                                                                               | 1. *Required field*: Must be specified, cannot be empty or omitted.<br/>2. *Cron expression format*: Must be a valid cron expression.                                                     |
| **targetReplicas** | The desired number of replicas that the target pool should be scaled to when this policy executes. Represents the exact replica count that will be set on the target pool when the policy's schedule matches.                                                               | 1. *Fixed scaling targets*: Set specific replica counts for different times (e.g., 10 replicas during business hours, 2 replicas during off-hours).                                                                                                                                                                                 | 1. *Required field*: Must be specified, cannot be omitted.<br/>2. *Non-negative constraint*: Must be non-negative integer (>=0).                                                          |

###### Capacity Availability Policy

To support rapid resource provisioning during sustained traffic peaks, the capacity policy ensures
that the cluster maintains a safe level of available resources.

| Field               | Description                                                                                                                                                                                                                                     | Use Cases                                                                                                                                       | Validation                                                                                                                                     |
|---------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------|
| **targetAvailable** | Used to specify the usable idle capacity of the resource pool.                                                                                                                                                                                  | 1. *Available capacity management*: To manage resource pool capacity while balancing cost efficiency and resource provisioning speed.           | 1. *Required field*: Must be specified, cannot be empty or omitted.                                                                            |
| **tolerance**       | The tolerance on the difference between the current and desired available capacity under which no updates are made to the desired number of replicas. This prevents scaling for small value variations that are within the tolerance threshold. | 1. *Reduce unnecessary scaling*: Prevent scaling actions for small available fluctuations that don't warrant replicas count changes.            | 1. *Optional field*: Can be omitted.<br/>2. *Non-negative constraint*: Must be >=0.                                                            |
| **scaleUp**         | Configures the scaling behavior for scaling up (increasing the number of replicas). Defines the minimum amount of available resources that must be retained in the resource pool.                                                               | 1. *Handle sustained traffic surges*: Ensure sufficient resources are available to serve applications and prevent request latency fluctuations. | 1. *Optional field*: Can be omitted. If not configured, no external guarantees are provided for the available capacity of the resource pool.   |
| **scaleDown**       | Configures the scaling behavior for scaling down (decreasing the number of replicas). Defines the upper bound of available resources that the policy resource pool can retain.                                                                  | 1. *Cost optimization*: Restrict the maximum capacity of the resource pool to keep resource costs under control.                                | 1. *Optional field*: Can be omitted. If not configured, no additional limits are imposed on the total available capacity of the resource pool. |

| Field                          | Description                                                                                                                                                                                                                                                                                                                                                                                                                            | Use Cases                                                                                                                                                                                                                                                                                                                                                                                                                    | Validation                                                                                                                                                                                                                           |
|--------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **stabilizationWindowSeconds** | The number of seconds for which past recommendations should be considered while scaling up or scaling down. This creates a stabilization window that prevents flapping by choosing the safest value from recent recommendations instead of applying the latest recommendation immediately. For scale-up, it selects the minimum recommendation from the window. For scale-down, it selects the maximum recommendation from the window. | 1. *Prevent flapping*: Smooth the scaling by using a stabilization window to avoid rapid scaling oscillations.<br/>2. *Conservative scale-down*: Use longer stabilization windows for scale-down to prevent premature reduction during temporary decrease in the rate of resource consumption.<br/>3. *Aggressive scale-up*: Use shorter or zero stabilization windows for scale-up to respond quickly to traffic increases. | 1. *Optional field*: Can be omitted. If not specified, uses the default values:<br/> - For scale up: 0 (no stabilization).<br/> - For scale-down: 300 seconds (5min).<br/> 2. *Range validation*: must be >= 0 and <= 3600 (1 hour). |

Available capacity can be configured using absolute values or percentage.
The percentage is defined relative to the current replica count of the resource pool,
enabling proportional scaling that adapts to pool size changes.

##### Status

| Field                   | Description                                                                                                                                                                                                                                                                                                                |
|-------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **observedGeneration**  | The most recent generation observed by this autoscaler. This field is used to track which version of the autoscaler spec the controller has processed. It helps prevent the controller from acting on stale configuration when the autoscaler spec is updated while the controller is processing a previous version.       |
| **lastScaleTime**       | The last time the autoscaler scaled the number of pods. This timestamp is used by the autoscaler to control how often the number of pods is changed, helping to prevent rapid scaling oscillations and ensuring scaling actions are spaced appropriately.                                                                  |
| **currentReplicas**     | Current number of replicas of pods managed by this autoscaler, as last seen by the autoscaler. This represents the actual current replica count of the target warming pool that the autoscaler is managing.                                                                                                                |
| **desiredReplicas**     | The desired number of the replicas of pods managed by this autoscaler, as last calculated by the autoscaler. This represents the target replica count that the autoscaler controller has calculated based on policies, but may differ from the actual currentReplicas due to rate limiting, stabilization, or constraints. |
| **suspended**           | Whether the autoscaler controller has suspend subsequent policy executions. When `true`, the controller will not sync the policy. when `false`, the policy synced normally.                                                                                                                                                |
| **appliedCronPolicies** | Cron policy execution status.                                                                                                                                                                                                                                                                                              |
| **currentCapacity**     | The current number of available capacity managed by this autoscaler, as last seen by the autoscaler. This represents the actual current available capacity of the target warming pool that the autoscaler is managing.                                                                                                     |
| **condition**           | The set of conditions required for this autoscaler to scale its target, and indicates whether or not those conditions are met. This is an array of PoolAutoscalerCondition objects, each representing a specific condition type with its status, reason and message.                                                       |

#### Metrics

The autoscaler exposes comprehensive metrics to enable monitoring, alerting, and performance analysis.
Metrics follow Prometheus conventions and are compatible with standard Kubernetes monitoring tools.

##### Reconciliation Metrics

`reconciliations_total`

**Type**: Counter

**Description**: Total number of autoscaler reconciliation operation performed by the controller.

**Labels**:
- `action`: The scaling action taken during reconciliation
  - Values: `scale_up`, `scale_down`, `none`
- `error`: Error type encountered during reconciliation
  - Values: `internal`, `none`

**Use Cases**:
- Monitor reconciliation frequency
- Track scaling action distribution
- Identify error rates by type
- Alert on high error rates

`reconciliation_duration_total`

**Type**: Histogram

**Description**: Time taken by the autoscaler controller to complete one reconciliation operation.

**Labels**:
- `action`: The scaling action taken during reconciliation
  - Values: `scale_up`, `scale_down`, `none`
- `error`: Error type encountered during reconciliation
  - Values: `internal`, `none`

**Use Cases**:
- Monitor reconciliation performance
- Identify slow reconciliations
- Track performance degradation
- Set SLOs for reconciliation latency

#### User Configuration Examples

The following sections provide practical configuration examples demonstrating common use cases
and best practices for `PoolAutoscaler`.

##### Bounds Enforcer

An autoscaler with only `minReplicas` and `maxReplicas` (and no scaling policies)
acts as a replica count guardrail rather than an active autoscaler.
This configuration enforces hard bounds on the `SandboxSet` replica count
while leaving scaling decisions to external actors (e.g., manual scaling, other controllers).

```yaml
spec:
  minReplicas: 5
  maxReplicas: 10
```

If the current number of replicas is below `minReplicas`, it is increased to `minReplicas`.
If it exceeds `maxReplicas`, it is reduced to `maxReplicas`. Otherwise, the replica count
remains unchanged.

##### Cron-Based Scaling

When using cron-based policies, the desired replica count specified by each
policy's `targetReplicas` is subject to the `minReplicas` and `maxReplicas`
constraints defined in the `spec`. The final replica count is always
clamped within these bounds, ensuring that cron policies cannot violate capacity limits.

$$
\text{finalReplicas} = \max\left(\text{minReplicas}, \min\left(\text{cronDesiredReplicas}, \text{maxReplicas}\right)\right)
$$

```yaml
spec:
  cronPolicies:
    - name: scale-up
      schedule: "0 8 * * *"
      targetReplicas: 100
    - name: scale-down
      schedule: "0 20 * * *"
      targetReplicas: 20
```

The warming pool will be scaled up to 100 replicas at 8:00 AM.
At 8:00 PM, the warming pool will be scaled down to 20 replicas.

```yaml
spec:
  minReplicas: 30
  maxReplicas: 50
  cronPolicies:
    - name: scale-up
      schedule: "0 8 * * *"
      targetReplicas: 100
    - name: scale-down
      schedule: "0 20 * * *"
      targetReplicas: 20
```

The warming pool will be scaled up to 50 replicas at 8:00 AM. This is constrained by
the configured maximum number of replicas.
At 8:00 PM, the warming pool will be scaled down to 30 replicas. This is constrained by
the configured minimum number of replicas.

##### Capacity-Based Scaling with Watermarks

The capacity-based policy maintains the idle warming pool size within predefined bounds
using watermarks. This approach ensures sufficient available resources for
rapid scaling while preventing over-provisioning during low-demand periods.

The watermark mechanism uses a target available capacity (`targetAvailable`)
as the desired idle resource level, with tolerance zones for scale-up and scale-down operations.
This creates a "dead zone" around the target, preventing frequent oscillations while ensuring
the pool maintains adequate idle capacity.

###### Absolute Value Watermark Configuration

The simplest configuration uses absolute values for watermarks,
providing fixed capacity targets regardless of pool size.

**Configuration Example**:
```yaml
spec:
  capacityPolicy:
    targetAvailable: 10  # Target: maintain 10 idle sandboxes
    tolerance: 5  # Scale up when available < 10 - 5 = 5. Scale down when available > 10 + 5 = 15
```
**Watermark Calculation**:
- **Lower Watermark (Scale-Up Trigger)**: `targetAvailable - tolerance = 10 - 5 = 5`
    - When available resources fall below 5, the autoscaler scales up to restore the target
- **Upper Watermark (Scale-Down Trigger)**: `targetAvailable + tolerance = 10 + 5 = 15`
    - When available resources exceed 15, the autoscaler scales down to optimize resource usage
- **Dead Zone**: Between 5 and 15, no scaling occurs (prevents oscillation)

**Scaling Behavior Timeline**:

| Time | Event              | Replica Count | Available Resources | Used Resources | Autoscaler Decision Logic                                                                                               |
|------|--------------------|---------------|---------------------|----------------|-------------------------------------------------------------------------------------------------------------------------|
| t0   | Initial State      | 1             | 1                   | 0              | No Action: Available (1) is within dead zone [5, 15]                                                                    |
| t1   | Autoscaler Sync    | 10            | 10                  | 0              | **Scale Up**: Available (1) < Lower Watermark (5). Target: 10 available → Scale to 10 replicas                          |
| t2   | Resources Consumed | 10            | 0                   | 10             | No Action: Autoscaler not yet synchronized                                                                              |
| t3   | Autoscaler Sync    | 20            | 10                  | 10             | **Scale Up**: Available (0) < Lower Watermark (5). Target: 10 available → Scale to 20 replicas (10 used + 10 available) |
| t4   | Resources Consumed | 20            | 0                   | 20             | No Action: Autoscaler not yet synchronized                                                                              |
| t5   | Autoscaler Sync    | 30            | 10                  | 20             | **Scale Up**: Available (0) < Lower Watermark (5). Target: 10 available → Scale to 30 replicas (20 used + 10 available) |
| t6   | Resources Released | 30            | 30                  | 0              | No Action: Autoscaler not yet synchronized                                                                              |
| t7   | Autoscaler Sync    | 10            | 10                  | 0              | **Scale Down**: Available (30) > Upper Watermark (15). Target: 10 available → Scale to 10 replicas                      |

**Detailed Timeline Explanation**:

**t0 - Initial State**: The resource pool starts with 1 replica,
all of which are available. The autoscaler evaluates the current state: available resources (1)
are below the lower watermark (5), but since this is the initial state
and the autoscaler hasn't synchronized yet, no action is taken immediately.

**t1 - First Autoscaler Sync**: The autoscaler synchronizes and detects
that available resources (1) are below the lower watermark threshold (5).
It calculates the required replica count to maintain the target available capacity (10).
Since there are no used resources, it scales the pool to 10 replicas,
achieving the target of 10 available resources.

**t2 - Resource Consumption**: All 10 idle resources are consumed by workloads.
The autoscaler has not yet synchronized, so no scaling action occurs.
The system now has 10 replicas in use and 0 available.

**t3 - Second Autoscaler Sync**: The autoscaler synchronizes and detects
that available resources (0) are below the lower watermark (5).
To restore the target available capacity (10) while maintaining the 10 currently used resources,
it scales up to 20 replicas (10 used + 10 available).

**t4 - Continued Resource Consumption**: Another 10 idle resources are consumed.
The autoscaler has not yet synchronized, so no scaling action occurs.
The system now has 20 replicas in use and 0 available.

**t5 - Third Autoscaler Sync**: The autoscaler synchronizes
and detects that available resources (0) are still below the lower watermark (5).
To restore the target available capacity (10) while maintaining the 20 currently used resources,
it scales up to 30 replicas (20 used + 10 available).

**t6 - Resource Release**: All 30 previously used resources are released back to the resource pool.
The system now has 30 available resources and 0 used resources.
The autoscaler has not yet synchronized, so no scaling action occurs.

**t7 - Scale-Down Sync**: The autoscaler synchronizes and detects
that available resources (30) exceed the upper watermark (15).
To optimize resource usage while maintaining the target available capacity (10),
it scales down to 10 replicas, resulting in 10 available resources.

**Visual Representation**:
```
30 |                       ●    ●○
   |
20 |
   |
15 |              ●   ●        ───── Upper Watermark
   |
   |
10 |   ●○    ●    ○        ○         ●○  ───── Target Available
   |
   |
 5 |         ───── Lower Watermark
   |
   |●○
 0 |         ○         ○
   +----+----+----+----+----+----+----+----+----+----+
   t0   t1   t2   t3   t4   t5   t6   t7

Legend:
● = Replica Count
○ = Available Resources
```

###### Percentage-Based Watermark Configuration

For dynamic workloads where pool size varies significantly,
percentage-based watermarks provide fully adaptive scaling
that maintains proportional idle capacity across all pool sizes.
When all watermarks (target, scale-up tolerance, and scale-down tolerance)
are configured as percentages, they scale proportionally together,
ensuring uniform scaling behavior regardless of pool scale.

**Configuration Example**:
```yaml
spec:
  capacityPolicy:
    targetAvailable: "70%"  # Target: maintain 70% of replicas as idle
    tolerance: "10%"  # Scale up when available < 70% - 10% = 60% of replicas. Scale down when available > 70% + 10% = 80% of replicas
```

**Watermark Calculation (Dynamic)**:
- **Target Available**: `Current Replicas × 70%` (rounded up)
- **Lower Watermark (Scale-Up Trigger)**: `Current Replicas × (70% - 10%) = Current Replicas × 60%` (rounded up)
- **Upper Watermark (Scale-Down Trigger)**: `Current Replicas × (70% + 10%) = Current Replicas × 80%` (rounded up)
- **Dead Zone**: Between lower and upper watermarks, no scaling occurs

**Scaling Behavior Timeline with Percentage-Based Watermarks**:

| Time | Event              | Replica Count | Available Resources | Used Resources | Watermark Calculation                                                   | Autoscaler Decision                                                                                                      |
|------|--------------------|---------------|---------------------|----------------|-------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------|
| t0   | Initial State      | 4             | 4                   | 0              | Target: 4×70%↑=3<br/>Lower: 4×60%↑=3<br/>Upper: 4×80%↑=4                | **No Action**: Available (4) equals upper watermark (4), within acceptable range                                         |
| t1   | Resources Consumed | 4             | 0                   | 4              | -                                                                       | No Action: Autoscaler not yet synchronized                                                                               |
| t2   | Autoscaler Sync    | 7             | 3                   | 4              | Target: 4×70%↑=3<br/>Lower: 4×60%↑=3<br/>Current: 0                     | **Scale Up**: Available (0) < Lower Watermark (3). Target: 3 available → Scale to 7 replicas (4 used + 3 available)      |
| t3   | Resources Consumed | 7             | 0                   | 7              | -                                                                       | No Action: Autoscaler not yet synchronized                                                                               |
| t4   | Autoscaler Sync    | 12            | 5                   | 7              | Target: 7×70%↑=5<br/>Lower: 7×60%↑=5<br/>Current: 0                     | **Scale Up**: Available (0) < Lower Watermark (5). Target: 5 available → Scale to 12 replicas (7 used + 5 available)     |
| t5   | Resources Consumed | 12            | 0                   | 12             | -                                                                       | No Action: Autoscaler not yet synchronized                                                                               |
| t6   | Autoscaler Sync    | 21            | 9                   | 12             | Target: 12×70%↑=9<br/>Lower: 12×60%↑=8<br/>Current: 0                   | **Scale Up**: Available (0) < Lower Watermark (8). Target: 9 available → Scale to 21 replicas (12 used + 9 available)    |
| t7   | Resources Consumed | 21            | 0                   | 21             | -                                                                       | No Action: Autoscaler not yet synchronized                                                                               |
| t8   | Autoscaler Sync    | 36            | 15                  | 21             | Target: 21×70%↑=15<br/>Lower: 21×60%↑=13<br/>Current: 0                 | **Scale Up**: Available (0) < Lower Watermark (13). Target: 15 available → Scale to 36 replicas (21 used + 15 available) |
| t9   | Resources Released | 36            | 36                  | 0              | -                                                                       | No Action: Autoscaler not yet synchronized                                                                               |
| t10  | Autoscaler Sync    | 26            | 26                  | 0              | Target: 36×70%↑=26<br/>Upper: 36×80%↑=29<br/>Current: 36                | **Scale Down**: Available (36) > Upper Watermark (29). Target: 26 available → Scale to 26 replicas                       |
| t11  | Autoscaler Sync    | 19            | 19                  | 0              | Target: 26×70%↑=19<br/>Upper: 26×80%↑=21<br/>Current: 26                | **Scale Down**: Available (26) > Upper Watermark (21). Target: 19 available → Scale to 19 replicas                       |
| t12  | Autoscaler Sync    | 14            | 14                  | 0              | Target: 19×70%↑=14<br/>Upper: 19×80%↑=16<br/>Current: 19                | **Scale Down**: Available (19) > Upper Watermark (16). Target: 14 available → Scale to 14 replicas                       |
| t13  | Autoscaler Sync    | 10            | 10                  | 0              | Target: 14×70%↑=10<br/>Upper: 14×80%↑=12<br/>Current: 14                | **Scale Down**: Available (14) > Upper Watermark (12). Target: 10 available → Scale to 10 replicas                       |
| t14  | Autoscaler Sync    | 7             | 7                   | 0              | Target: 10×70%↑=7<br/>Upper: 10×80%↑=8<br/>Current: 10                  | **Scale Down**: Available (10) > Upper Watermark (8). Target: 7 available → Scale to 7 replicas                          |
| t15  | Autoscaler Sync    | 5             | 5                   | 0              | Target: 7×70%↑=5<br/>Upper: 7×80%↑=6<br/>Current: 7                     | **Scale Down**: Available (7) > Upper Watermark (6). Target: 5 available → Scale to 5 replicas                           |
| t16  | Autoscaler Sync    | 4             | 4                   | 0              | Target: 5×70%↑=4<br/>Upper: 5×80%↑=4<br/>Current: 5                     | **Scale Down**: Available (5) > Upper Watermark (4). Target: 4 available → Scale to 4 replicas                           |
| t17  | Final State        | 4             | 4                   | 0              | Target: 4×70%↑=3<br/>Lower: 4×60%↑=3<br/>Upper: 4×80%↑=4<br/>Current: 4 | **No Action**: Available (4) equals upper watermark (4), within acceptable range. Pool reaches steady state              |


**Detailed Timeline Explanation for Percentage-Based Configuration**:

**t0 - Initial State**: The resource pool starts with 4 replicas, all available.
The autoscaler calculates watermarks:
- Target Available: `4 × 70% = 2.8 → 3` (rounded up)
- Lower Watermark: `4 × (70% - 10%) = 4 × 60% = 2.4 → 3` (rounded up)
- Upper Watermark: `4 × (70% + 10%) = 4 × 80% = 3.2 → 4` (rounded up)
- Current Available: 4

Since available resources (4) equal the upper watermark (4),
the pool is at the maximum acceptable idle capacity.
No scaling action is required as the pool is within the dead zone [3, 4].

**t1 - Resource Consumption**: All 4 instances are consumed by workloads.
The autoscaler has not yet synchronized, so no scaling action occurs.

**t2 - First Scale-Up**: The autoscaler synchronizes and evaluates:
- Current Available: 0
- Lower Watermark: `4 × 60% = 3` (rounded up)
- Target Available: `4 × 80% = 4` (rounded up)

Since available (0) < lower watermark (3), the autoscaler scales up.
To achieve the target of 3 available resources while maintaining 4 used resources,
it scales to 7 replicas (4 used + 3 available).

**t3 - Continued Consumption**: All 7 instances are now consumed.
The autoscaler has not yet synchronized.

**t4 - Second Scale-Up**: The autoscaler recalculates watermarks based on current replica count (7):
- Target Available: `7 × 70% = 4.9 → 5` (rounded up)
- Lower Watermark: `7 × 60% = 4.2 → 5` (rounded up)
- Current Available: 0

Since available (0) < lower watermark (5), the autoscaler scales up to 12 replicas (7 used + 5 available).

**t5-t8 - Progressive Scale-Up**: The pattern continues as resources are consumed.
Each autoscaler sync recalculates watermarks based on the current replica count,
ensuring the target available capacity grows proportionally with pool size.
By t8, the pool has scaled to 36 replicas with 15 available resources (maintaining the 70% target).

**t9 - Resource Release**: All 36 previously used resources are released,
resulting in 36 available resources and 0 used resources.

**t10-t16 - Progressive Scale-Down**: The autoscaler begins scaling down to optimize resource usage.
At each sync, it recalculates watermarks based on the current replica count.
Since available resources exceed the upper watermark at each step,
the autoscaler scales down to restore the target available capacity.
The scale-down process is gradual (36 → 26 → 19 → 14 → 10 → 7 → 5 → 4),
ensuring stability and preventing sudden capacity drops that could impact future availability.
Each step maintains the proportional idle ratio while respecting the upper watermark threshold.

**t17 - Steady State**: The pool reaches a steady state at 4 replicas with 4 available resources.
The autoscaler calculates:
- Target Available: `4 × 70% = 2.8 → 3` (rounded up)
- Lower Watermark: `4 × 60% = 2.4 → 3` (rounded up)
- Upper Watermark: `4 × 80% = 3.2 → 4` (rounded up)
- Current Available: 3

Since available (4) equals the upper watermark (4),
the pool is at the maximum acceptable idle capacity for this pool size.
The pool has reached a stable state where it maintains the target idle ratio
while respecting the upper bound, and no further scaling is needed.

**Visual Timeline - Percentage-Based Watermarks**:
```
36 |                                      ●   ●○
   |
30 |
   |
   |                                               ●○
   |                            ●    ●
20 |
   |                                                     ●○
15 |                                      ○
   |                                                          ●○
12 |                   ●   ●
   |
10 |                                                               ●○
   |                            ○
 8 |
 7 |         ●    ●                                                     ●○
   |
 5 |                   ○                                                     ●○
   |
 4 |●○  ●                                                                         ●○   ●○
   |
 3 |         ○
   |
 0 |    ○         ○         ○         ○
   +----+----+----+----+----+----+----+----+----+----+----+----+----+----+----+----+----+----+
   t0   t1   t2   t3   t4   t5   t6   t7   t8   t9   t10 t11  t12  t13  t14  t15  t16  t17

Legend:
● = Replica Count
○ = Idle Resources
```

**Key Observations from the Timeline**:
1. **Proportional Growth**: As the pool grows from 4 to 36 replicas,
both the target available capacity and watermark thresholds grow proportionally
- Target: 3 (at 4 replicas) → 21 (at 36 replicas), maintaining ~70% ratio
- Lower: 3 (at 4 replicas) → 13 (at 36 replicas), maintaining ~60% ratio
- Upper: 4 (at 4 replicas) → 29 (at 36 replicas), maintaining ~80% ratio

This maintains consistent proportional relationships across all scales.

2. **Fully Dynamic Watermarks**: All watermarks (target, lower, upper) scale proportionally with pool size,
ensuring uniform scaling behavior regardless of pool scale.
This provides predictable and consistent behavior across different workload sizes.

3. **Adaptive Scaling**: The watermark thresholds adjust dynamically based on current pool size,
ensuring appropriate scaling triggers at all scales.
The percentage-based approach eliminates the need for manual threshold adjustments as the pool grows or shrinks.

4. **Gradual Scale-Down**: The scale-down process is gradual
(36 → 26 → 19 → 14 → 10 → 7 → 5 → 4), with each step triggered when available resources exceed
the dynamically calculated upper watermark. This prevents sudden capacity drops that could impact availability.

5. **Steady State**: The pool eventually reaches a minimal steady state (4 replicas)
where available resources equal the upper watermark,
maintaining the target idle ratio while respecting the upper bound.

### Implementation Details/Notes/Constraints

#### Policy Combination Limitations

Currently, the autoscaler does not support simultaneous configuration of cron-based policies and
capacity-based policies. This limitation exists to avoid potential conflicts and ensure
predictable scaling behavior. The autoscaler will prioritize one policy type when both are configured,
which may lead to unexpected scaling results.

*Future Enhancement*: Support for combining cron-based and capacity-based policies will be added
based on product requirements and user feedback. The implementation will include conflict
resolution mechanisms and clear precedence rules to ensure consistent and predictable autoscaling behavior.

#### Observation Window and Sampling Configuration

The autoscaler uses an **observation window** mechanism to collect resource state samples
over time before making scaling decisions. This approach prevents reacting to transient
fluctuations and ensures scaling decisions are based on sustained trends rather than momentary spikes.

**Core Concepts**:

1. **Observation Window**: The time period over which resource state samples are collected and
analyzed to determine scaling actions. This represents the historical data period
considered for each scaling decision.
2. **Sampling Interval**: The time interval between consecutive sampling points.
This controls how frequently the autoscaler queries the current resource state.
3. **Metric Aggregation**: The method used to determine the final metric value from
multiple samples collected within the observation window.

**Configuration Parameters**:

The autoscaler exposes the following configuration parameters:

- **Observation Window Duration** (`observationWindowSeconds`):
The total time window over which samples are collected and aggregated
    - **Default**: 60 seconds
    - **Range**: 30-300 seconds
    - **Purpose**: Determines how much historical data is considered for scaling decisions

- **Sampling Interval** (`samplingIntervalSeconds`): The time interval between consecutive sampling operations
    - **Default**: 15 seconds
    - **Range**: 5-30 seconds
    - **Purpose**: Controls the frequency of resource state queries

**Metric Value Determination Within Observation Window**:

The autoscaler collects multiple samples of resource state
(available replicas, total replicas) over the observation window and
aggregates them to determine the final value used for scaling decisions.
The autoscaler supports Average (Mean) aggregation methods to
determine the final metric value from samples within the observation window:

```
Final Available = sum(availableReplicas from all samples) / number of samples
```
- **Use Case**: General purpose, smooths out temporary fluctuations
- **Advantage**: Provides stable, representative value

**Integration with Stabilization Windows**:

The observation window is separate from but complementary to stabilization windows used in scaling policies:

- **Observation Window**: Determines **when** and **how** samples are collected
    - Collects raw resource state samples
    - Aggregates samples to determine current metric value
    - Provides input to scaling decision algorithm

- **Stabilization Window**: Determines **how** past recommendations are considered
    - Operates on scaling recommendations (not raw samples)
    - Selects conservative recommendation from historical recommendations
    - Applied after observation window aggregation

#### Cron Policy Maintenance Window Support

For cron-based policy configurations, the system currently does not support configuring
maintenance windows. Maintenance windows are time periods during which scheduled scaling
operations should be skipped or deferred, typically used for system maintenance,
updates, or other planned activities.

*Current Behavior*: All cron-based scaling jobs will execute according to their schedules
regardless of maintenance activities, which may cause conflicts during system maintenance periods.

*Future Enhancement*: Support for maintenance window configuration will be added based on
user requirements and feedback. This enhancement may allow users to:

- Define maintenance window periods using cron expressions or time ranges
- Specify behavior during maintenance windows (skip, defer, or override)
- Configure multiple maintenance windows for different scenarios

The implementation timeline will be determined based on user demand.

#### One-to-One Relationship Between Warm Pool and Autoscaler

Each warm pool can only be associated with a single autoscaler instance. This one-to-one
relationship is enforced to ensure predictable and consistent scaling behavior,
prevent conflicts, and maintain system stability.

### Risks and Mitigations

#### Controller Computational Complexity and Resource Consumption

*Risk*: The introduction of multiple scaling policies (cron-based and capacity-based) increases
the computational complexity of the autoscaler controller. The controller needs to evaluate
multiple policy types, calculate scaling recommendations, maintain historical state for
stabilization windows, and process cron schedules. This requires additional memory and
storage resources, which can be amplified in large-scale clusters with many autoscaler instances.

*Impact*:
- Increased CPU usage for policy evaluation and calculation
- Higher memory consumption for storing historical recommendations and cron schedule state
- Potential performance degradation in clusters with hundreds or thousands of autoscaler resources
- Increased latency in scaling decisions due to complex calculations

*Mitigation*:
- *Configurable Parameters*: Provide system-level configuration options to limit the complexity
  of policy evaluation, such as:
    - Configurable observation windows and sampling intervals to reduce evaluation frequency
- *Policy Configuration Limits*: Enforce reasonable limits on policy configurations:
    - Limit the number of cron policy per autoscaler
- *Optimized Algorithms*: Implement efficient algorithms that avoid unnecessary recalculations:
    - Cache cron schedule evaluations
    - Use incremental updates for recommendation history
    - Optimize memory usage through efficient data structures
- *Resource Monitoring*: Provide metrics to monitor controller resource consumption and
  alert when thresholds are exceeded

#### Frequent Scaling Due to Misconfiguration

**Risk**: Users may misconfigure autoscaler policies, leading to frequent and unnecessary
scaling operations. This can occur due to:
- Overly sensitive thresholds that react to minor resource usage fluctuations
- Conflicting policies that cause oscillation between scale-up and scale-down
- Missing stabilization mechanisms that allow rapid scaling changes
- Incorrect tolerance values that trigger scaling on insignificant changes

*Impact*:
- Unnecessary pod churn and resource waste
- Increased API server load from frequent scaling operations
- Application instability due to constant replica count changes
- Higher operational costs from unnecessary resource provisioning

*Mitigation*:
- *Tolerance Configuration*: Provide configurable tolerance values to prevent
  scaling on minor resources changes:
    - Default tolerance values that require meaningful changes before scaling
    - Per-autoscaler tolerance configuration for fine-grained control
    - Separate tolerance values for scale-up and scale-down operations
- *Cooldown Periods*: Implement cooldown mechanisms to prevent rapid successive scaling operations:
    - Minimum time between scaling operations
    - Separate cooldown periods for scale-up and scale-down
    - Configurable cooldown duration based on workload characteristics
- *Watermark Configuration*: Support high and low watermarks to create buffer zones:
    - High watermark: Threshold that must be exceeded before scaling down
    - Low watermark: Threshold that must be crossed before scaling up
    - Prevents oscillation around a single threshold value
- **Stabilization Windows**: Implement stabilization windows to:
    - Collect multiple recommendations over a time period
    - Select the most conservative recommendation to prevent flapping
    - Provide separate stabilization windows for scale-up and scale-down

#### Extreme Behavior from Invalid Configuration Combinations

*Risk*: Invalid or conflicting parameter combinations can lead to extreme scaling behavior, such as:
- Simultaneous configuration of conflicting policies (e.g., cron and capacity-based) without clear precedence
- Parameter values that result in excessive scaling (e.g., very high scale-up percentages)
- Missing bounds that allow scaling beyond cluster capacity
- Invalid cron expressions that cause unexpected scheduling behavior

*Impact*:
- Unpredictable scaling behavior that violates user expectations
- Resource exhaustion from excessive scaling
- Application downtime from scaling to zero or beyond capacity
- Difficult troubleshooting due to unexpected parameter interactions

*Mitigation*:
- *API Validation*: Implement comprehensive API validation to prevent invalid configurations:
    - Validate cron expression syntax at API admission time
    - Reject conflicting policy combinations (e.g., cron and capacity-based policies together)
    - Validate parameter ranges (e.g., min/max replicas, scaling percentages)
    - Check for logical inconsistencies (e.g., minReplicas > maxReplicas)
- *Restricted API Combinations*: Enforce restrictions on how policies can be combined:
    - Only allow one policy type per autoscaler (cron OR capacity-based, not both)
    - Prevent mutually exclusive configurations
- *Reasonable Default Values*: Provide sensible defaults that prevent extreme behavior:
    - Default stabilization windows and cooldown periods
    - Default tolerance values that prevent over-reaction
- *Documentation and Examples*: Provide clear documentation with:
    - Best practices for policy configuration
    - Examples of valid and invalid configurations
    - Warnings about common misconfiguration patterns

#### Observability and Debugging Challenges

*Risk*: The complexity of multiple policy types and configuration options makes it
difficult for users to understand why the autoscaler made specific scaling decisions.
Without proper observability, users cannot:
- Understand the reasoning behind scaling actions
- Debug why expected scaling did not occur
- Identify which policy triggered a scaling operation
- Troubleshoot conflicts between different policies

*Impact*:
- Increased support burden due to troubleshooting difficulties
- Delayed incident resolution when scaling issues occur
- Difficulty in optimizing autoscaler configurations

*Mitigation*:
- *Status Field Enhancements*: Expose detailed decision-making information in the autoscaler status:
    - Current active policy (cron or capacity-based)
    - Last scaling decision and reasoning
    - Policy evaluation results (which policies were considered, which triggered)
    - Conditions indicating the current state
- *Event Logging*: Provide detailed events for scaling operations:
    - Events for each scaling action with reason and policy source
    - Events for errors or warnings during policy evaluation
    - Events for skipped scaling operations (e.g., due to cooldown)
- *Metrics Exposure*: Implement comprehensive metrics to track autoscaler behavior:
    - Metrics for scaling operations (scale-up/down counts, rates)
    - Metrics for observation cycles (observation window frequency, decision duration)
    - Metrics for controller performance (CPU usage, memory consumption)
- *Logging Enhancements*: Provide structured logging with appropriate verbosity levels:
    - Decision logs explaining why scaling actions were taken or skipped
    - Policy evaluation logs showing the evaluation process
    - Configuration validation logs
    - Error logs with sufficient context for debugging
- *Documentation*: Create comprehensive documentation covering:
    - How to interpret status fields and conditions
    - How to read and understand events
    - How to use metrics for monitoring and alerting
    - Troubleshooting guides for common scenarios
    - Examples of interpreting autoscaler behavior

## Alternatives

### Extend Existing HPA for SandboxSet

**Approach**: Extend Kubernetes HPA to support `SandboxSet` workloads by
implementing custom metrics adapters that expose SandboxSet-specific metrics
(e.g., available sandbox count, idle pool capacity).

**Pros**:
- Leverages existing, well-tested HPA infrastructure
- Consistent API and behavior with standard Kubernetes autoscaling
- Benefits from ongoing HPA improvements and community support

**Cons**:
- HPA is designed for pod-level metrics and may not fit warm pool management semantics
- Requires custom metrics adapter development and maintenance
- Limited support for cron-based scheduling (would require external tools like KEDA)
- Complex integration with SandboxSet's unique resource model (available vs. total replicas)
- HPA's stabilization windows and tolerance mechanisms may not align with warm pool requirements

**Rejection Rationale**: While HPA is excellent for pod-level autoscaling,
warm pool management requires different semantics (maintaining idle capacity vs. scaling based on utilization).
The proposed solution provides a more natural fit for SandboxSet's use cases with direct
support for cron-based policies and capacity-based watermarks.

### Use External Autoscaling Tools

**Approach**: Integrate with existing external autoscaling solutions like
KEDA (for cron-based scaling) or Alibaba's CronHPA controller.

**Pros**:
- Leverages mature, production-tested solutions
- Reduces development and maintenance burden
- Benefits from community contributions and bug fixes

**Cons**:
- Requires additional components and dependencies in the cluster
- May not fully support SandboxSet's unique resource model
- Limited control over scaling behavior specific to warm pool semantics
- Potential version compatibility issues and upgrade complexity
- Less integrated with SandboxSet's lifecycle and status management

**Rejection Rationale**: While external tools provide valuable functionality,
a native autoscaler integrated directly with `SandboxSet` offers better control,
tighter integration, and a more cohesive user experience.
The proposed solution is designed specifically for SandboxSet's warm pool management needs.

## Upgrade Strategy

### API Versioning

The `PoolAutoscaler` resource will be introduced as `v1alpha1` API version,
following Kubernetes API versioning best practices. This allows for future API evolution
based on user feedback and requirements.

### Backward Compatibility

Since this is a new feature with no existing autoscaler resources, there are no
backward compatibility concerns for initial release.
However, future API changes will maintain backward compatibility through:

- **Additive Changes**: New optional fields can be added without breaking existing configurations
- **API Version Conversion**: When promoting to `v1beta1` or `v1`,
conversion logic will ensure existing `v1alpha1` resources continue to work
- **Field Deprecation**: Deprecated fields will be supported for at least two API versions before removal

### Upgrade Path

#### From No Autoscaler to PoolAutoscaler

**Scenario**: Users currently managing SandboxSet replicas manually want to adopt PoolAutoscaler.

**Steps**:
1. Create `PoolAutoscaler` resource targeting existing `SandboxSet`
2. Configure appropriate policies (cron-based or capacity-based)
3. Set `minReplicas` and `maxReplicas` to match current replica count or desired bounds
4. Monitor autoscaler behavior and adjust policies as needed

**Rollback**: Delete the `PoolAutoscaler` resource. The `SandboxSet` will retain its current replica count,
allowing manual management to resume.

#### Upgrading Autoscaler Configuration

**Scenario**: Users need to update autoscaler policies or bounds.

**Steps**:
1. Update `PoolAutoscaler` spec with new configuration
2. Controller reconciles changes during next sync cycle
3. Scaling behavior adjusts according to new policies

**Rollback**: Revert `PoolAutoscaler` spec to previous configuration.
The controller will reconcile to the previous state.

#### Controller Upgrade

**Scenario**: Upgrading the autoscaler controller to a new version.

**Behavior**:
- Existing `PoolAutoscaler` resources continue to work without modification
- New controller features may require updating autoscaler configurations to opt-in
- Controller maintains backward compatibility with existing API versions

### Downgrade Strategy

#### Downgrading Controller Version

**Scenario**: Rolling back to a previous controller version.

**Considerations**:
- Ensure all `PoolAutoscaler` resources use supported API versions
- If new API fields were used, they will be ignored by older controller versions
- Autoscaler will continue operating with supported features only

**Recommendation**: Before downgrading, review `PoolAutoscaler` resources
to ensure they don't rely on features only available in the newer controller version.

#### Removing Autoscaler

**Scenario**: Completely removing autoscaler functionality from a cluster.

**Steps**:
1. Delete all `PoolAutoscaler` resources
2. `SandboxSet` resources retain their current replica counts
3. Resume manual replica management if needed

**Note**: The `SandboxSet` resources themselves are not affected by autoscaler removal.

### Version Skew Strategy

#### API Server and Controller Version Skew

**Scenario**: Since `PoolAutoscaler` is implemented as a CustomResourceDefinition (CRD),
the version skew considerations differ from built-in Kubernetes APIs.
Once the CRD is installed in the cluster, the Kubernetes API server immediately
supports the `PoolAutoscaler` API for that version. Version skew primarily occurs between
the CRD definition and the controller implementation.

## Additional Details

## Test Plan [optional]

### Unit Tests

**API Validation Tests**:
- Validate `PoolAutoscaler` spec fields (minReplicas, maxReplicas, policies)
- Test cron expression parsing and validation
- Verify capacity policy validation (targetAvailable, tolerance, stabilization windows)
- Test one-to-one relationship enforcement (rejecting multiple autoscalers for same target)
- Validate policy combination restrictions (cron vs. capacity-based)

**Controller Logic Tests**:
- Test cron policy evaluation and scheduling
- Test capacity policy calculation (absolute and percentage-based)
- Test stabilization window logic for scale-up and scale-down
- Test tolerance calculation and application
- Test bounds enforcement (minReplicas, maxReplicas)
- Test suspend flag behavior

**Reconciliation Tests**:
- Test reconciliation with various policy configurations
- Test error handling and retry logic
- Test status condition updates
- Test event recording

### Integration Tests

**End-to-End Scaling Tests**:
- Test cron-based scaling with multiple policies
- Test capacity-based scaling with various watermarks
- Test scaling within min/max bounds
- Test scaling when bounds are exceeded

**Policy Interaction Tests**:
- Test cron policy execution timing
- Test capacity policy evaluation during resource consumption/release
- Test policy precedence when both types are configured (should be rejected)

**Status and Observability Tests**:
- Verify status fields are updated correctly
- Test condition transitions
- Verify event generation for scaling operations
- Test metrics exposure

**Error Handling Tests**:
- Test behavior when target SandboxSet doesn't exist
- Test behavior when target SandboxSet is deleted
- Test behavior with invalid cron expressions
- Test behavior with invalid capacity configurations
- Test recovery from transient errors

### Performance Tests

**Scalability Tests**:
- Test controller performance with 100+ PoolAutoscaler resources
- Test reconciliation latency under load
- Test memory consumption with large recommendation histories
- Test cron schedule evaluation performance

**Stress Tests**:
- Test rapid policy changes
- Test concurrent scaling operations
- Test controller restart and recovery

## Implementation History

- [ ] 06/01/2026: Initial proposals draft created
- [ ] 15/01/2026: Proposal reviewed and approved
