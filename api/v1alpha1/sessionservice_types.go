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

// SessionServiceSpec describes a fixed-size collection of Session-capable Sandboxes.
// +kubebuilder:validation:XValidation:rule="!has(self.buildOptions)",message="buildOptions is not supported by SessionService"
type SessionServiceSpec struct {
	// Replicas counts effective members, including creating and paused members.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// SandboxTemplateSpec is the complete desired Sandbox creation input for
	// rolling replacement. Its fields are embedded directly in spec.
	SandboxTemplateSpec `json:",inline"`

	// Ports are the business ports exposed by the SessionService.
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=65535
	// +kubebuilder:validation:items:Minimum=1
	// +kubebuilder:validation:items:Maximum=65535
	Ports []int32 `json:"ports"`

	// SessionPolicy declares Session affinity and capacity.
	// +kubebuilder:default={}
	// +optional
	SessionPolicy SessionServiceSessionPolicy `json:"sessionPolicy,omitempty"`

	// UpdateStrategy controls create-before-delete rolling replacement.
	// +kubebuilder:default={}
	// +optional
	UpdateStrategy SessionServiceUpdateStrategy `json:"updateStrategy,omitempty"`
}

type SessionServiceSessionPolicy struct {
	// Affinity controls reuse of an existing Session binding. BestEffort may
	// reallocate an unavailable target; Strict keeps an existing binding even
	// when its target is unavailable. Deleting the Sandbox CR removes its bindings;
	// later requests with that Session ID return 404. An absent or empty Session ID
	// creates a new Session identity and binding.
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

type SessionServiceUpdateStrategy struct {
	// MaxSurge budgets extra members from observed active and terminating Sandbox
	// CRs during rolling replacement. Unknown create outcomes, stale caches, and
	// controller restarts can temporarily exceed it; it is not an instantaneous
	// limit and does not cap Pods still exiting after their CR disappears.
	// It must be positive even at zero replicas; whole percentages round up
	// against desired replicas. Zero is unsupported without maxUnavailable.
	// +kubebuilder:default="25%"
	// +kubebuilder:validation:XValidation:rule="type(self) == int ? self > 0 : self.matches('^[0-9]*[1-9][0-9]*%$')",message="maxSurge must be a positive integer or percentage"
	// +optional
	MaxSurge *intstr.IntOrString `json:"maxSurge,omitempty"`
}

type SessionServiceStatus struct {
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	Replicas           int32  `json:"replicas"`
	ReadyReplicas      int32  `json:"readyReplicas"`
	CurrentRevision    string `json:"currentRevision,omitempty"`
	UpdateRevision     string `json:"updateRevision,omitempty"`
	// Selector identifies member Sandboxes for the scale subresource.
	Selector string `json:"selector,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:resource:path=sessionservices,shortName={ssvc},singular=sessionservice
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SessionService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SessionServiceSpec   `json:"spec"`
	Status            SessionServiceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SessionServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SessionService `json:"items"`
}

func init() { SchemeBuilder.Register(&SessionService{}, &SessionServiceList{}) }
