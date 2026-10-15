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
)

// AgentRoleReference names an AgentRole in the same namespace.
//
// Cross-namespace references are not supported: a namespace must not be able to
// grant itself permissions defined somewhere it does not control.
type AgentRoleReference struct {
	// APIGroup of the referent. Only security.agents.kruise.io is accepted.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=security.agents.kruise.io
	APIGroup string `json:"apiGroup"`

	// Kind of the referent. Only AgentRole is accepted.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=AgentRole
	Kind string `json:"kind"`

	// Name of the referent in the same namespace.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// AgentIdentityReference names an AgentIdentity in the same namespace.
//
// This carries only a name, matching the shape in the proposal. It differs from
// AgentRoleReference above, which carries apiGroup and kind as well.
type AgentIdentityReference struct {
	// Name of the AgentIdentity in the same namespace.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// AgentRoleBindingSpec attaches one AgentRole to one AgentIdentity.
//
// Both references are immutable. Repointing a binding would silently change
// what an identity may do while keeping the name an operator audits by, so a
// change of either is a delete and a create rather than an edit.
//
// A binding grants one role to one identity. An identity needing several roles
// gets several bindings, and their Allow rules combine.
type AgentRoleBindingSpec struct {
	// AgentRoleRef is the role whose permissions are granted.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="agentRoleRef is immutable"
	AgentRoleRef AgentRoleReference `json:"agentRoleRef"`

	// AgentIdentityRef is the identity receiving them.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="agentIdentityRef is immutable"
	AgentIdentityRef AgentIdentityReference `json:"agentIdentityRef"`
}

// AgentRoleBindingStatus reports whether the binding is in effect.
type AgentRoleBindingStatus struct {
	// ObservedGeneration is the most recent generation observed by the
	// controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the observations of the binding's state. Ready is True
	// when the current generation validated and both referenced resources are
	// themselves Ready.
	//
	// Readiness is reporting, not enforcement. Authorization reads the current
	// bindings on every request, so a binding that has not yet been observed
	// here still takes effect, and a deleted one stops granting immediately.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=arb,categories=agents
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.agentRoleRef.name`
// +kubebuilder:printcolumn:name="Identity",type=string,JSONPath=`.spec.agentIdentityRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentRoleBinding grants an AgentRole to an AgentIdentity.
type AgentRoleBinding struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the binding.
	Spec AgentRoleBindingSpec `json:"spec"`

	// Status is the current state of the binding.
	// +optional
	Status AgentRoleBindingStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentRoleBindingList contains a list of AgentRoleBinding.
type AgentRoleBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentRoleBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentRoleBinding{}, &AgentRoleBindingList{})
}
