/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

var PoolAutoscalerControllerKind = GroupVersion.WithKind("PoolAutoscaler")

// CrossVersionObjectReference contains enough information to let you identify the referred resource.
type CrossVersionObjectReference struct {
	// Kind of the referent; More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
	// +required
	Kind string `json:"kind"`
	// Name of the referent; More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names
	// +required
	Name string `json:"name"`
	// API version of the referent
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`
}

// PoolAutoscalerSpec describes the desired functionality of the PoolAutoscaler.
type PoolAutoscalerSpec struct {
	// ScaleTargetRef points to the target warming pool to scale, and is used to select the pods for which instance status
	// should be collected, as well as to actually change the replica count.
	// +required
	ScaleTargetRef CrossVersionObjectReference `json:"scaleTargetRef"`

	// MaxReplicas is the upper limit for the number of replicas to which the autoscaler can scale up.
	// It cannot be less than minReplicas.
	// +required
	MaxReplicas int32 `json:"maxReplicas"`

	// MinReplicas is the lower limit for the number of replicas to which the autoscaler
	// can scale down. It defaults to 0 pods.
	// +optional
	MinReplicas int32 `json:"minReplicas"`

	// CronPolicies is a list of potential cron scaling policies which can be used during scaling.
	// When both CronPolicies and CapacityPolicy are set, CronPolicies takes higher priority.
	// +optional
	CronPolicies []CronScalingPolicy `json:"cronPolicies,omitempty"`

	// CapacityPolicy defines the capacity configuration of the target resource pool.
	// +optional
	CapacityPolicy *CapacityPolicy `json:"capacityPolicy,omitempty"`

	// Suspend tells the controller to suspend subsequent executions.
	// Defaults to false.
	// +optional
	Suspend *bool `json:"suspend,omitempty"`
}

// CronScalingPolicy defines the cron-based scaling configuration for the resource pool.
type CronScalingPolicy struct {
	// Name is used to specify the scaling policy.
	// +required
	Name string `json:"name"`

	// TimeZone is the time zone name for the given schedule, e.g. "Asia/Shanghai", "UTC".
	// If not specified, this will default to the time zone of the autoscaler controller manager process.
	// +optional
	TimeZone *string `json:"timeZone,omitempty"`

	// Schedule is a cron expression that defines when this policy should be executed.
	// Supports standard cron format with 5 fields (minute hour day month weekday).
	// +required
	Schedule string `json:"schedule"`

	// TargetReplicas is the desired replicas when this policy executes.
	// +required
	TargetReplicas int32 `json:"targetReplicas"`
}

// CapacityPolicy defines the capacity configuration of the target resource pool.
type CapacityPolicy struct {
	// TargetAvailable is the desired available replicas.
	// Can be an absolute number (ex: 5) or a percentage of current replicas (ex: 70%).
	// +required
	TargetAvailable intstr.IntOrString `json:"targetAvailable"`

	// Tolerance is the tolerance between the watermark and desired value under which
	// no updates are made to the desired number of replicas.
	// Can be an absolute number (ex: 5) or a percentage (ex: 10%).
	// If not set, defaults to 10%.
	// +optional
	Tolerance *intstr.IntOrString `json:"tolerance,omitempty"`

	// ScaleUp is the scaling rule for scaling up.
	// +optional
	ScaleUp *CapacityScalingRules `json:"scaleUp,omitempty"`

	// ScaleDown is the scaling rule for scaling down.
	// +optional
	ScaleDown *CapacityScalingRules `json:"scaleDown,omitempty"`
}

// CapacityScalingRules configures the scaling behavior for one direction.
type CapacityScalingRules struct {
	// StabilizationWindowSeconds is the number of seconds for which past recommendations should be
	// considered while scaling up or scaling down.
	// Must be >= 0 and <= 3600 (one hour).
	// Defaults: scale up: 0, scale down: 300.
	// +optional
	StabilizationWindowSeconds *int32 `json:"stabilizationWindowSeconds,omitempty"`
}

// PoolAutoscalerStatus describes the current status of a pool autoscaler.
type PoolAutoscalerStatus struct {
	// ObservedGeneration is the most recent generation observed by this autoscaler.
	// +optional
	ObservedGeneration *int64 `json:"observedGeneration,omitempty"`

	// LastScaleTime is the last time the PoolAutoscaler scaled the number of pods.
	// +optional
	LastScaleTime *metav1.Time `json:"lastScaleTime,omitempty"`

	// CurrentReplicas is current number of replicas of pods managed by this autoscaler.
	CurrentReplicas int32 `json:"currentReplicas"`

	// DesiredReplicas is the desired number of replicas of pods managed by this autoscaler.
	DesiredReplicas int32 `json:"desiredReplicas"`

	// Suspended indicates whether the autoscaler is currently suspended.
	// +optional
	Suspended bool `json:"suspended,omitempty"`

	// AppliedCronPolicies is the execution status of cron policies.
	// +optional
	AppliedCronPolicies []CronScalingPolicyStatus `json:"appliedCronPolicies,omitempty"`

	// CurrentCapacity is the last read state of the capacity used by this autoscaler.
	// +optional
	CurrentCapacity CapacityStatus `json:"currentCapacity,omitempty"`

	// Conditions is the set of conditions required for this autoscaler to scale its target.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// CronScalingPolicyStatus records the last schedule time for a cron policy.
type CronScalingPolicyStatus struct {
	// Name is the cron policy name.
	Name string `json:"name"`

	// LastScheduleTime is the last time the policy was successfully scheduled.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`
}

// CapacityStatus records the current capacity observed by the autoscaler.
type CapacityStatus struct {
	// Available is current number of available pods managed by this autoscaler.
	Available int32 `json:"available"`
}

// PoolAutoscalerConditionType are the valid conditions of a PoolAutoscaler.
type PoolAutoscalerConditionType string

const (
	// ScalingActive indicates that the autoscaler is able to scale.
	ScalingActive PoolAutoscalerConditionType = "ScalingActive"
	// AbleToScale indicates that the autoscaler is able to calculate and set scale.
	AbleToScale PoolAutoscalerConditionType = "AbleToScale"
	// ScalingLimited indicates that the autoscaler is constrained by min/max bounds.
	ScalingLimited PoolAutoscalerConditionType = "ScalingLimited"
)

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=poolautoscalers,shortName={pa},singular=poolautoscaler
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Reference",type="string",JSONPath=".spec.scaleTargetRef.name"
// +kubebuilder:printcolumn:name="MinReplicas",type="integer",JSONPath=".spec.minReplicas"
// +kubebuilder:printcolumn:name="MaxReplicas",type="integer",JSONPath=".spec.maxReplicas"
// +kubebuilder:printcolumn:name="CurrentReplicas",type="integer",JSONPath=".status.currentReplicas"
// +kubebuilder:printcolumn:name="DesiredReplicas",type="integer",JSONPath=".status.desiredReplicas"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// PoolAutoscaler is the configuration for a warming pool autoscaler,
// which automatically manages the replica count of the warming pool
// based on the policies specified.
type PoolAutoscaler struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// Spec defines the desired behavior of the autoscaler.
	// +optional
	Spec PoolAutoscalerSpec `json:"spec,omitempty"`

	// Status is the current information about the autoscaler.
	// +optional
	Status PoolAutoscalerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PoolAutoscalerList contains a list of PoolAutoscaler.
type PoolAutoscalerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PoolAutoscaler `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PoolAutoscaler{}, &PoolAutoscalerList{})
}
