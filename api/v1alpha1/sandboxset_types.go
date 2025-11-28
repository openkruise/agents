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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	InternalPrefix = "agents.kruise.io/"

	LabelSandboxPool  = InternalPrefix + "sandbox-pool"
	LabelSandboxState = InternalPrefix + "sandbox-state"
	LabelSandboxID    = InternalPrefix + "sandbox-id"
	LabelTemplateHash = InternalPrefix + "template-hash"

	AnnotationLock  = InternalPrefix + "lock"
	AnnotationOwner = InternalPrefix + "owner"
)

const (
	SandboxStateAvailable = "available"
	SandboxStateRunning   = "running"
	SandboxStatePaused    = "paused"
	SandboxStateKilling   = "killing"
)

// SandboxSetSpec defines the desired state of SandboxSet
type SandboxSetSpec struct {
	// Replicas is the number of unused sandboxes, including available and creating ones.
	Replicas int32 `json:"replicas"`

	// Template describes the pods that will be created.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	Template corev1.PodTemplateSpec `json:"template"`
}

// SandboxSetStatus defines the observed state of SandboxSet.
type SandboxSetStatus struct {
	// observedGeneration is the most recent generation observed for this SandboxSet. It corresponds to the
	// SandboxSet's generation, which is updated on mutation by the API Server.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Replicas is the total number of creating, available, running and paused sandboxes.
	Replicas int32 `json:"replicas"`

	// AvailableReplicas is the number of available sandboxes, which are ready to be claimed.
	AvailableReplicas int32 `json:"availableReplicas"`

	// UpdateRevision is the template-hash calculated from `spec.template`.
	UpdateRevision string `json:"updateRevision,omitempty"`

	// conditions represent the current state of the SandboxSet resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=sandboxsets,shortName={sbs},singular=sandboxset
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".spec.replicas"
// +kubebuilder:printcolumn:name="Available",type="integer",JSONPath=".status.availableReplicas"
// +kubebuilder:printcolumn:name="UpdateRevision",type="string",JSONPath=".status.updateRevision"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SandboxSet is the Schema for the sandboxsets API, which is an advanced workload for managing sandboxes.
type SandboxSet struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of SandboxSet
	// +required
	Spec SandboxSetSpec `json:"spec"`

	// status defines the observed state of SandboxSet
	// +optional
	Status SandboxSetStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// SandboxSetList contains a list of SandboxSet
type SandboxSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxSet{}, &SandboxSetList{})
}
