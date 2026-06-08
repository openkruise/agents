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

const (
	CommitFinalizer = "agents.kruise.io/commit"
)

// +enum
type CommitPhase string

const (
	CommitPhasePending   CommitPhase = "Pending"
	CommitPhaseRunning   CommitPhase = "Running"
	CommitPhaseSucceeded CommitPhase = "Succeeded"
	CommitPhaseFailed    CommitPhase = "Failed"
)

type CommitConditionType string

const (
	// CommitConditionTypeCommitContainer indicates whether the container commit (snapshot) step succeeded.
	CommitConditionTypeCommitContainer CommitConditionType = "CommitContainer"
	// CommitConditionTypePullBaseImage is reserved for future use when incremental commit requires pulling a base image.
	CommitConditionTypePullBaseImage CommitConditionType = "PullBaseImage"
	// CommitConditionTypePushCommittedImage indicates whether the committed image push step succeeded.
	CommitConditionTypePushCommittedImage CommitConditionType = "PushCommittedImage"
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

	// SquashLayer is the max number of writable layers to keep after squashing.
	// 0 means no squashing. Reserved for future implementation.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=0
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="squashLayer is immutable"
	SquashLayer int32 `json:"squashLayer,omitempty"`

	// TimeoutSeconds is the max duration (in seconds) for the commit job.
	// Exceeded jobs are terminated and marked Failed. 0 means no timeout.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=0
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="timeoutSeconds is immutable"
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// TtlAfterFinished is how long the Commit is retained after reaching a terminal phase.
	// The controller auto-deletes the Commit once TTL expires. Nil means no auto-deletion.
	// +kubebuilder:validation:Optional
	TtlAfterFinished *metav1.Duration `json:"ttl,omitempty"`

	// RegistryAuth specifies credentials for pushing the committed image.
	// If nil, credentials are resolved from namespace secrets or SA imagePullSecrets.
	// +kubebuilder:validation:Optional
	RegistryAuth *RegistryAuth `json:"registryAuth,omitempty"`
}

type RegistryAuth struct {
	// Secrets is a list of dockerconfigjson Secret names in the same namespace.
	// +kubebuilder:validation:Optional
	Secrets []string `json:"secrets,omitempty"`
	// Credentials is reserved for future use. Currently has no effect.
	// +kubebuilder:validation:Optional
	Credentials map[string]string `json:"credentials,omitempty"`
}

type CommitStatus struct {
	// +kubebuilder:default=Pending
	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
	Phase CommitPhase `json:"phase"`

	// +kubebuilder:validation:Optional
	CommitID string `json:"commitID,omitempty"`

	// +kubebuilder:validation:Optional
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
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=commits,shortName=cmt
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`,description="Current phase"
// +kubebuilder:printcolumn:name="TTL",type=string,JSONPath=`.spec.ttl`,description="Time to live"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`,description="Age"

type Commit struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec CommitSpec `json:"spec"`

	// +optional
	// +kubebuilder:default:={phase: Pending}
	Status CommitStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
type CommitList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Commit `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Commit{}, &CommitList{})
}
