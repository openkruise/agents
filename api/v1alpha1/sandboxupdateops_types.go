/*
Copyright 2025.

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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// SandboxUpdateOpsSpec defines the desired state of SandboxUpdateOps
type SandboxUpdateOpsSpec struct {
	// Selector selects the target sandboxes to update by label.
	// +required
	Selector *metav1.LabelSelector `json:"selector"`

	// UpdateStrategy defines the strategy for the batch update.
	// +optional
	UpdateStrategy SandboxUpdateOpsStrategy `json:"updateStrategy,omitempty"`

	// Patch defines the changes to apply to each selected sandbox's template.
	// The patch is applied as a Strategic Merge Patch on the sandbox's PodTemplateSpec.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	Patch runtime.RawExtension `json:"patch,omitempty"`

	// Lifecycle defines pre/post upgrade hooks to set on each sandbox during upgrade.
	// +optional
	Lifecycle *SandboxLifecycle `json:"lifecycle,omitempty"`

	// Paused indicates whether the update operation is paused.
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// SandboxUpdateOpsStrategy defines the strategy for batch sandbox updates.
type SandboxUpdateOpsStrategy struct {
	// MaxUnavailable is the maximum number of sandboxes that can be upgrading at the same time.
	// Value can be an absolute number (e.g., 5) or a percentage of total sandboxes (e.g., 10%).
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
}

// SandboxUpdateOpsPhase represents the phase of a SandboxUpdateOps.
// +enum
type SandboxUpdateOpsPhase string

const (
	// SandboxUpdateOpsPending means the update operation has been created but not started.
	SandboxUpdateOpsPending SandboxUpdateOpsPhase = "Pending"
	// SandboxUpdateOpsUpdating means the update operation is in progress.
	SandboxUpdateOpsUpdating SandboxUpdateOpsPhase = "Updating"
	// SandboxUpdateOpsCompleted means all target sandboxes have been successfully updated.
	SandboxUpdateOpsCompleted SandboxUpdateOpsPhase = "Completed"
	// SandboxUpdateOpsFailed means the update operation has encountered failures.
	SandboxUpdateOpsFailed SandboxUpdateOpsPhase = "Failed"
)

// SandboxUpdateOpsStatus defines the observed state of SandboxUpdateOps
type SandboxUpdateOpsStatus struct {
	// ObservedGeneration is the most recent generation observed.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is the current phase of the update operation.
	Phase SandboxUpdateOpsPhase `json:"phase,omitempty"`

	// Replicas is the total number of sandboxes selected for update.
	Replicas int32 `json:"replicas"`

	// UpdatedReplicas is the number of sandboxes that have been successfully updated.
	UpdatedReplicas int32 `json:"updatedReplicas"`

	// FailedReplicas is the number of sandboxes that failed to update.
	FailedReplicas int32 `json:"failedReplicas"`

	// UpdatingReplicas is the number of sandboxes currently being updated.
	UpdatingReplicas int32 `json:"updatingReplicas"`

	// Conditions represents the latest available observations.
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=sandboxupdateops,shortName={suo},singular=sandboxupdateops
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Total",type="integer",JSONPath=".status.replicas"
// +kubebuilder:printcolumn:name="Updated",type="integer",JSONPath=".status.updatedReplicas"
// +kubebuilder:printcolumn:name="Updating",type="integer",JSONPath=".status.updatingReplicas"
// +kubebuilder:printcolumn:name="Failed",type="integer",JSONPath=".status.failedReplicas"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// SandboxUpdateOps is the Schema for batch sandbox update operations.
type SandboxUpdateOps struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// Spec defines the desired update operation.
	// +required
	Spec SandboxUpdateOpsSpec `json:"spec"`

	// Status defines the observed state.
	// +optional
	Status SandboxUpdateOpsStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true
// SandboxUpdateOpsList contains a list of SandboxUpdateOps
type SandboxUpdateOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxUpdateOps `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxUpdateOps{}, &SandboxUpdateOpsList{})
}
