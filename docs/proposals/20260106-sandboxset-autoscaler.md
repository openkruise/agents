---
title: SandboxSet supports autoscaler
authors:
  - "@sivanzcw"
reviewers:
  - "@furykerry"
creation-date: 2026-01-06
last-updated: 2026-01-06
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
            - [Story 1](#story-1)
            - [Story 2](#story-2)
        - [Requirements (Optional)](#requirements-optional)
            - [Functional Requirements](#functional-requirements)
                - [FR1](#fr1)
                - [FR2](#fr2)
            - [Non-Functional Requirements](#non-functional-requirements)
                - [NFR1](#nfr1)
                - [NFR2](#nfr2)
        - [Implementation Details/Notes/Constraints](#implementation-detailsnotesconstraints)
        - [Risks and Mitigations](#risks-and-mitigations)
    - [Alternatives](#alternatives)
    - [Upgrade Strategy](#upgrade-strategy)
    - [Additional Details](#additional-details)
        - [Test Plan [optional]](#test-plan-optional)
    - [Implementation History](#implementation-history)

## Summary

This enhancement proposes providing autoscaler capabilities for sandboxSet. Currently, sandboxSet
provides sandbox pre-warming capabilities. To enable more fine-grained management of pre-warmed
resources, it becomes essential to add autoscaler capabilities to sandboxSet. This enhancement will
provide greater flexibility and efficiency in managing the overall resource consumption of sandboxSet.

## Motivation

Agents are often part of latency-sensitive execution paths and operate under highly dynamic workloads.
As a result, they have strong requirements around startup performance. By pre-warming sandbox objects
that provide the agent runtime environment through sandboxSet, agents can achieve near-real-time startup
and stable startup latency. However, sandboxSet currently supports only a fixed-size pre-warmed resource
pool, which cannot handle batch agent startup scenarios nor support predictable startup behavior.
Agents frequently create multiple sandboxes in parallel due to task decomposition or fan-out execution.
The system must support launching many sandboxes concurrently without linear degradation.
Startup latency should be correlated with load and capacity rather than exhibiting random jitter.
Predictability is critical for planners and schedulers that make execution decisions based on expected
availability.

Enabling autoscaler for sandboxSet by allowing:

- Intelligently and dynamically adjust the resource pool capacity: Operators can configure intelligent
  pre-warmed pool management policies based on workload concurrency demands. This can helps prevent
  unpredictable startup latency, excessive request queuing during peak traffic, and situations where
  the planner is forced into serial execution.
- Improved resource utilization: By dynamically adjusting the capacity of the resource pre-warming pool,
  the cluster can more efficiently handle fluctuations in overall resource usage, thereby reducing 
  over-reservation and over-provision.
- Reduced operational overhead: By eliminating the need for frequent manual adjustments to the pre-warmed
  pool capacity, without the need for frequent manual intervention in resource pool pre-warming,
  operational overhead is significantly reduced.

### Goals

This proposal aims to:

- Provide periodic autoscaling capabilities for the pre-warming pool, allowing it to scale up or down
  on a recurring basis.
- Support maintaining a target range of idle resources in the pre-warming pool, ensuring timely
  replenishment while preventing excessive pre-warming that could increase costs.

### Non-Goals/Future Work

- This enhancement doesn't aim to use complex algorithms, focus on interpretable strategies.
- Focused on sandboxes, cluster-level resource scaling is handled by the cluster autoscaler.
- There may be HPA policies configured for pods within the cluster, and these HPA policies may
  conflict with the scaling policies of the sandboxSet. This proposal does not address this
  issue for now and will be extended in the future based on usage scenarios.

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

### Implementation Details/Notes/Constraints

- What are some important details that didn't come across above.
- What are the caveats to the implementation?
- Go in to as much detail as necessary here.
- Talk about core concepts and how they release.

### Risks and Mitigations

- What are the risks of this proposal and how do we mitigate? Think broadly.
- How will UX be reviewed and by whom?
- How will security be reviewed and by whom?
- Consider including folks that also work outside the SIG or subproject.

## Design Details

### API

Provide autoscaler configurations for the warming pool, supporting both cron-based
and watermark-based capacity control policies.

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
	// minReplicas is the lower limit for the number of replicas to which the autoscaler
	// can scale down.
	// It defaults to 0 pod.
	MinReplicas int32

	// CronPolicies is a list of potential cron scaling polices which can be used during scaling.
	// +optional
	CronPolicies []CronScalingPolicy

	// CapacityPolicy defines the capacity configuration of the target resource pool.
	// +optional
	CapacityPolicy CapacityPolicy
}

// CronScalingPolicy defines the cron-based scaling configuration for the resource pool.
type CronScalingPolicy struct {
	// Name is used to specify the scaling policy.
	// +required
	Name string
	// The time zone name for the given schedule.
	// If not specified, this will default to the time zone of the agent-controller-manager process.
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
	// The maintenance windows of the policy.
	// The schedule in Cron format.
	FreezeWindows []string
	// This flag tells the controller to suspend subsequent executions
	// Defaults to false.
	// +optional
	Suspend bool
}

// CapacityPolicy defines the capacity configuration of the target resource pool.
type CapacityPolicy struct {
	// scaleUp is scaling policy for scaling Up.
	// +optional
	ScaleUp *CapacityScalingRules
	// scaleDown is scaling policy for scaling Down.
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
	// AvailableReplicas is the waterline for available sandboxes.
	// For scaleup:
	// When the actual available replicas is below this value, the controller will
	// scale up the warming pool to ensure that the number of available instances is
	// maintained above the value.
	// For scaledown:
	// When the actual available replicas is greater than this value, the controller will
	// scale down the warming pool to ensure that the number of available instances is
	// maintained above the value.
	// +optional
	AvailableReplicas CapacityValue
	// Tolerance is the tolerance on the ratio between the current and desired
	// value under which no updates are made to the desired number of
	// replicas (e.g. 0.01 for 1%). Must be greater than or equal to zero. If not
	// set, the default cluster-wide tolerance is applied (by default 10%).
	// +optional
	Tolerance *CapacityValue
}

// CapacityValue is a value holder that abstracts literal versus percentage based value
type CapacityValue struct {
	// The following fields are exclusive. Only the topmost non-zero field is used.

	// absolute Value.
	Value int32
	// Percentage of replicas.
	Percentage float32
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

	// Conditions is the set of conditions required for this autoscaler to scale its target,
	// and indicates whether or not those conditions are met.
	Conditions []PoolAutoscalerCondition
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

#### Pool capacity control

To ensure cost control and avoid runaway scaling due to misconfiguration, uses need the ability
to cap the total capacity of the resource pool. At the same time, a minimum level of capacity is
required during off-peak periods to serve baseline workloads. To support these requirements, the
pool autoscaler introduces configurable upper and lower capacity limits.

| Field         | Description                                                                        | Use Cases                                                                                                                                                                                                                                                                                                                                                                                            | Validation Rules                                                                                                                                  |
|---------------|------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------|
| *maxReplicas* | The upper limit for the number of replicas to which the autoscaler can scale up.   | 1. *Limit scaling up*: Prevents autoscaler from scaling beyond the specified maximum, protecting cluster resources and controlling costs. Prevents infinite scaling that could exhaust cluster resources<br/>2. *Immediate scale down*: When current replicas exceed `maxReplicas` (e.g., manual scaling or autoscaler config update), autoscaler immediately scales down to the maximum limit.<br/> | 1. *Required field*: Must be specified in the autoscaler spec.<br/>2. *Must be greater than 0*<br/>3. *Must be >= `minReplicas`*                    |
| *minReplicas* | The lower limit for the number of replicas to which the autoscaler can scale down. | 1. *Prevent excessive scale down*: Ensures a minimum number of pod are always available, maintaining service availability and handing baseline traffic.<br/> 2. *Default behavior* If not specified, defaults to 1 pod, ensuring at least one pod is always running.                                                                                                                                 | 1. *Option field*: Can be omitted, in which case it defaults to 1.<br/>2. *Relationship with maxReplicas*: `maxReplicas` must be >= `minReplicas` |

All policy behaviors are governed by the pool capacity control.

#### Pool scaling policy

The autoscaler supports dynamic management of both the total resource pool size and the available
capacity, balancing user's requirements for overall resource limits and capacity availability.

##### Cron-based policy

Cron-based scaling policies are provided to support planned resource management and recurring
resource consumption patterns.

| Field            | Description                                                                                                                                                                                                                                                                 | User Cases                                                                                                                                                                                                                                                                                                                          | Validation                                                                                                                                                                                                                                              |
|------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| *name*           | A unique identifier for the scaling policy. Used to track and manage individual scaling policy in the status conditions. The name helps distinguish between multiple policies and provides a reference for monitoring and debugging.                                        | 1. *Policy identification*: Uniquely identify each scaling policy in a cron autoscaler, enabling clear tracking of which policy executed and its result.<br/>2. *Status tracking*: Map policy execution results to specific policies in the `status.conditions` array, where each condition uses the policy name as the identifier. | 1. *Required field*: Must be specified, cannot be empty or omitted.                                                                                                                                                                                     |
| *timezone*       | The time zone name for interpreting the schedule. Specifies the IANA time zone database name (e.g., "Asia/Shanghai", "UTC") that should be used when evaluating the cron schedule. If not specified, defaults to the time zone of the autoscale controller manager process. | 1. *Global deployments*: Ensure consistent scheduling across clusters in different geographic regions by explicitly setting timezones                                                                                                                                                                                               | 1. *Optional field*: Can be omitted. If not specified, defaults to the autoscale controller manager process<br/>2. *IANA timezone format*: Must be a valid IANA time zone database name                                                                 |
| *schedule*       | A cron expression that defines when the scaling policy should be executed. Specifies that the exact time pattern (minute, hour, day, month, weekday) when the target workload should be scaled to the `targetReplicas`                                                      | 1. *Time-based scalling*: Define specific time for scaling operation                                                                                                                                                                                                                                                                | 1. *Required field*: Must be specified, cannot be empty or omitted.<br/>2. *Cron expression format*: Must be a valid cron expression                                                                                                                    |
| *targetReplicas* | The desired number of replicas that the target pool should be scaled to when this policy executes. Represents the exact replica count that will be set on the target pool when the policy's schedule matches                                                                | 1. *Fixed scaling targets*: Set specific replica counts for different times (e.g., 10 replicas during business hours, 2 replicas during off-hours)                                                                                                                                                                                  | 1. *Required field*: Must be specified, cannot be omitted.<br/>2. *Non-negative constraint*: Must be non-negative integer (>=0)                                                                                                                         |
| *freezeWindows*  | A list of cron expressions that define dates/times when scaling operations should not be executed. Each string in the array represents a cron expression that matches specific dates or time patterns to exclude from scling activities                                     | 1. *Holiday exclusion*: Exclude specific holidays (e.g., New Year's day, National Day) from scaling operation to prevent unnecessary resource adjustments during non-business days.<br/>2. *Maintenance windows*: Skip scaling during scheduled maintenance window to avoid conflicts with planned maintenance activities           | 1. *Optional field*: Can be omitted. If not specified, no dates are excluded.<br/>2. *Cron expression format*: Each string must be a valid cron expression<br/>3. *Duplicate checking*: The same date pattern cannot appear multiple times in the array |
| *suspend*        | A boolean flag that controls whether the autoscaler controller should suspend subsequent policy executions. When set to `true`, the controller will not sync the policy. when `false`, the policy synced normally.                                                          | 1. *Temporary suspension*: Temporarily pause policy execution during maintenance, debugging, or troubleshooting without deleting the autoscaler object.<br/>2. *Conditional execution*: Use suspend flag to enable/disable autoscaler based on external conditions or feature flags                                                 | 1. *Optional field*: Can be omitted.                                                                                                                                                                                                                    |

##### Capacity policy

To support rapid resource provisioning during sustained traffic peaks, the capacity policy ensures
that the cluster maintains a safe level of available resources.

| Field       | Description                                                                                                                                                                          | User Cases                                                                                                                                     | Validation                                                                                                                                 |
|-------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------|
| *ScaleUp*   | Configuration the scaling behavior for scaling up (increasing the number of replicas). Defines the minimum amount of available resources that must be retained in the resource pool. | 1. *Handle sustained traffic surges*: Ensure sufficient resources are available to serve applications and prevent request latency fluctuations. | 1. *Option field*: Can be omitted. If not configured, no external guarantees are provided for the available capacity of the resource pool  |
| *ScaleDown* | Configuration the scaling behavior for scaling down (decreasing the number of replicas). Defines the upper bound of available resources that the policy resource pool can retain.    | 1. *Cost optimization*: Restrict the maximum capacity of the resource pool to keep resource costs under control                                | 1. *Option field*: Can be omitted. If not configured, no additional limits are imposed on the total available capacity of the resource pool |

| Field                        | Description                                                                                                                                                                                                                                                                                                                                                                                                                       | User Cases                                                                                                                                                                                                                                                                                                                                                                                                               | Validation                                                                                                                                                                                                               |
|------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| *stabilizationWindowSeconds* | The number of seconds for which past recommendations should be considered while scaling up or scaling down. This create a stabilization window that prevent flapping by choosing the safest value from recent recommendations instead of applying the latest recommendation immediately. For scale-up, it selects the minimum recommendation from the window; for scle-down, it selects the maximum recomendation from the window | 1. *Prevent flapping*: Smooth the scaling by using a stabilization window to avoid rapid scaling oscillations<br/>2. *Conservative scale-down*: Use longer stabilization windows for scale-down to prevent premature reduction during temporary decreate in the rate of resource consumption<br/>3. *Aggresive scale-up*: Use shorter or zero stabilization windows for scale-up to respond quickly to traffic increases | 1. *Optional field*: Can be omitted. If not specified, users default values:<br/> - For scale up 0 (no stabilization)<br/> - For scale-down: 300 seconds (5min) 2. *Rnage validation*: must be >= 0 and <= 3600 (1 hour) |
| *availableReplicas*          |                                                                                                                                                                                                                                                                                                                                                                                                                                   |                                                                                                                                                                                                                                                                                                                                                                                                                          |                                                                                                                                                                                                                          |
| *tolerance*                  |                                                                                                                                                                                                                                                                                                                                                                                                                                   |                                                                                                                                                                                                                                                                                                                                                                                                                          |                                                                                                                                                                                                                          |

| Field        | Description                                                                                                                                                                                                                                                   | User Cases                                                                                     | Validation                                                                                           |
|--------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------|
| *value*      | This represents an absolute replicas. When specified, it takes precedence over the `percentage` field. Only one of `value` or `percentage` should be set, as they are mutually exclusive.                                                                     | 1. *Absoulute watermark*: Define watermark value using absolute value                          | 1. *Optional field*: Can be omitted. If not specified, the `percentage` field should be used instead |
| *percentage* | A percentage value representing the available percentage over the replicas of the resource pool. When `value` is not set, this fiedl os used to calculte the scaling value. Only one of `value` or `percentage` should be set, as they are mutually exclusive | 1. *Relative watermark*: Define watermark as percentages of the total replicas of warming pool | 1. *Optional field*: Can be omitted. If not specified, the `value` field should be used instead.     |

## Alternatives

The `Alternatives` section is used to highlight and record other possible approaches to delivering the value proposed by a proposal.

## Upgrade Strategy

If applicable, how will the component be upgraded? Make sure this is in the test plan.

Consider the following in developing an upgrade strategy for this enhancement:
- What changes (in invocations, configurations, API use, etc.) is an existing cluster required to make on upgrade in order to keep previous behavior?
- What changes (in invocations, configurations, API use, etc.) is an existing cluster required to make on upgrade in order to make use of the enhancement?

## Additional Details

### Test Plan [optional]

## Implementation History

- [ ] 06/01/2026: initial proposals draft created
