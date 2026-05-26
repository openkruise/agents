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
)

const (
	CommitFinalizer = "agents.kruise.io/commit"
)

// +enum
type CommitPhase string

const (
	CommitPending   CommitPhase = "Pending"
	CommitRunning   CommitPhase = "Running"
	CommitSucceeded CommitPhase = "Succeeded"
	CommitFailed    CommitPhase = "Failed"
)

type CommitSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="podName is immutable"
	PodName string `json:"podName"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="containerName is immutable"
	ContainerName string `json:"containerName"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="image is immutable"
	Image string `json:"image"`

	// +kubebuilder:validation:Optional
	Ttl *metav1.Duration `json:"ttl,omitempty"`

	// PushSecrets is a list of references to secrets in the same namespace containing
	// registry credentials for pushing the committed image.
	// +kubebuilder:validation:Optional
	PushSecrets []corev1.LocalObjectReference `json:"pushSecrets,omitempty"`

	// Whether to perform a dry run that only checks prerequisites
	// without actually committing or pushing the image.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=false
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="dryRun is immutable"
	DryRun bool `json:"dryRun,omitempty"`
}

type CommitStatus struct {
	// +kubebuilder:default=Pending
	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
	Phase CommitPhase `json:"phase"`

	// +kubebuilder:validation:Optional
	CommitID string `json:"commitID,omitempty"`

	// conditions represent the current state of the Commit resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +kubebuilder:validation:Optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// +kubebuilder:validation:Optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cmt
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`,description="Current phase"
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`,description="Target image"
// +kubebuilder:printcolumn:name="TTL",type=string,JSONPath=`.spec.ttl`,description="Time to live"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`,description="Age"

// Commit is the Schema for the commits API.
// It allows users to commit a running Sandbox container's filesystem changes
// into a new Docker image and push it to a registry.
type Commit struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec CommitSpec `json:"spec,omitempty"`

	// +kubebuilder:default:={phase: Pending}
	Status CommitStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CommitList contains a list of Commit
type CommitList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Commit `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Commit{}, &CommitList{})
}
