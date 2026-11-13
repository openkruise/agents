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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// SandboxServingSpec describes a fixed-size collection of Session-capable Sandboxes.
type SandboxServingSpec struct {
	// Replicas counts effective members, including creating and paused members.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Provision is immutable until rolling replacement is implemented.
	Provision SandboxServingProvision `json:"provision"`

	// Ports are the business ports exposed by the Serving.
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=65535
	// +kubebuilder:validation:items:Minimum=1
	// +kubebuilder:validation:items:Maximum=65535
	Ports []int32 `json:"ports"`

	// SessionPolicy declares Session affinity and capacity.
	// +kubebuilder:default={}
	// +optional
	SessionPolicy SandboxServingSessionPolicy `json:"sessionPolicy,omitempty"`

	// UpdateStrategy reserves rolling-update configuration; this stage does not roll.
	// +kubebuilder:default={}
	// +optional
	UpdateStrategy SandboxServingUpdateStrategy `json:"updateStrategy,omitempty"`
}

// SandboxServingProvision contains all immutable Sandbox creation input.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="provision is immutable in this stage"
// +kubebuilder:validation:XValidation:rule="has(self.sandboxTemplate) && !has(self.sandboxSetRef)",message="only sandboxTemplate provisioning is supported in this stage"
type SandboxServingProvision struct {
	// SandboxTemplate contains the immutable creation input copied to each Sandbox.
	// +kubebuilder:validation:XValidation:rule="!has(self.buildOptions)",message="buildOptions is not supported by SandboxServing"
	// +optional
	SandboxTemplate *SandboxTemplateSpec `json:"sandboxTemplate,omitempty"`

	// SandboxSetRef is reserved for future Claim provisioning and is rejected.
	// +optional
	SandboxSetRef *corev1.LocalObjectReference `json:"sandboxSetRef,omitempty"`
}

type SandboxServingSessionPolicy struct {
	// Affinity controls reuse of an existing Session binding. BestEffort may
	// reallocate an unavailable target; Strict keeps an existing binding even
	// when its target is unavailable. Deleting the Sandbox CR removes its bindings;
	// a later request with the same Session ID can allocate a new target.
	// +kubebuilder:default=BestEffort
	// +kubebuilder:validation:Enum=BestEffort;Strict
	// +optional
	Affinity string `json:"affinity,omitempty"`

	// MaxSessionsPerSandbox omits a user limit when absent; platform budgets apply.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=512
	// +optional
	MaxSessionsPerSandbox *int32 `json:"maxSessionsPerSandbox,omitempty"`

	// PauseAfterIdle declares future idle behavior and currently reports Unsupported.
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ns|us|ms|s|m|h))+$"
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="idle duration must be positive"
	// +optional
	PauseAfterIdle string `json:"pauseAfterIdle,omitempty"`

	// ExpireAfterIdle declares future idle behavior and currently reports Unsupported.
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ns|us|ms|s|m|h))+$"
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="idle duration must be positive"
	// +optional
	ExpireAfterIdle string `json:"expireAfterIdle,omitempty"`
}

type SandboxServingUpdateStrategy struct {
	// MaxSurge is reserved for future rolling updates.
	// +kubebuilder:default="25%"
	// +kubebuilder:validation:XValidation:rule="type(self) == int ? self >= 0 : self.matches('^[0-9]+%$')",message="maxSurge must be a nonnegative integer or percentage"
	// +optional
	MaxSurge *intstr.IntOrString `json:"maxSurge,omitempty"`
}

type SandboxServingStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	Replicas           int32 `json:"replicas"`
	ReadyReplicas      int32 `json:"readyReplicas"`
	// Selector is a nonempty Serving UID selector used by the scale subresource.
	Selector           string           `json:"selector,omitempty"`
	CurrentRevision    string           `json:"currentRevision,omitempty"`
	UpdateRevision     string           `json:"updateRevision,omitempty"`
	ReplicasByRevision map[string]int32 `json:"replicasByRevision,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="Serving name must fit its membership label (63 characters)"
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SandboxServing struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SandboxServingSpec   `json:"spec"`
	Status            SandboxServingStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SandboxServingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxServing `json:"items"`
}

func init() { SchemeBuilder.Register(&SandboxServing{}, &SandboxServingList{}) }
