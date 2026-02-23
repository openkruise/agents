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
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SandboxTemplateSpec defines the desired state of SandboxTemplate
type SandboxTemplateSpec struct {
	// Template describes the pods that will be created.
	// Template is mutual exclusive with TemplateRef
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Value is immutable"
	// +optional
	Template *v1.PodTemplateSpec `json:"template,omitempty"`

	// VolumeClaimTemplates is a list of PVC templates to create for this Sandbox.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Value is immutable"
	// +optional
	VolumeClaimTemplates []v1.PersistentVolumeClaim `json:"volumeClaimTemplates,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=sandboxtemplates,shortName={sbt},singular=sandboxtemplate
// +kubebuilder:storageversion
// SandboxSet is the Schema for the sandboxsets API, which is an advanced workload for managing sandboxes.
type SandboxTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of SandboxSet
	// +required
	Spec SandboxTemplateSpec `json:"spec"`
}

// +kubebuilder:object:root=true
// SandboxTemplateList contains a list of SandboxTemplate
type SandboxTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxTemplate `json:"items"`
}
