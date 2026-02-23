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
)

// SandboxClaimSpec defines the desired state of SandboxClaim
type SandboxClaimSpec struct {
	// TemplateName specifies which SandboxSet pool to claim from
	// +kubebuilder:validation:Required
	TemplateName string `json:"templateName"`

	// Replicas specifies how many sandboxes to claim (default: 1)
	// For batch claiming support
	// This field is immutable once set
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="replicas is immutable"
	Replicas *int32 `json:"replicas,omitempty"`

	// ShutdownTime specifies the absolute time when the sandbox should be shut down
	// This will be set as spec.shutdownTime (absolute time) on the Sandbox
	// +optional
	ShutdownTime *metav1.Time `json:"shutdownTime,omitempty"`

	// ClaimTimeout specifies the maximum duration to wait for claiming sandboxes
	// If the timeout is reached, the claim will be marked as Completed regardless of
	// whether all replicas were successfully claimed
	// +optional
	// +kubebuilder:default="1m"
	ClaimTimeout *metav1.Duration `json:"claimTimeout,omitempty"`

	// TTLAfterCompleted specifies the time to live after the claim reaches Completed phase
	// After this duration, the SandboxClaim will be automatically deleted.
	// Note: Only the SandboxClaim resource will be deleted; the claimed sandboxes will NOT be deleted
	// +optional
	// +kubebuilder:default="5m"
	TTLAfterCompleted *metav1.Duration `json:"ttlAfterCompleted,omitempty"`

	// Labels contains key-value pairs to be added as labels
	// to claimed Sandbox resources
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations contains key-value pairs to be added as annotations
	// to claimed Sandbox resources
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// EnvVars contains environment variables to be injected into the sandbox
	// These will be passed to the sandbox's init endpoint (envd) after claiming
	// Only applicable if the SandboxSet has envd enabled
	// +optional
	EnvVars map[string]string `json:"envVars,omitempty"`
}

// SandboxClaimStatus defines the observed state of SandboxClaim
type SandboxClaimStatus struct {
	// ObservedGeneration is the most recent generation observed
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase represents the current phase of the claim
	// Claiming: In the process of claiming sandboxes
	// Completed: Claim process finished (either all replicas claimed or timeout reached)
	// +optional
	Phase SandboxClaimPhase `json:"phase,omitempty"`

	// Message provides human-readable details about the current phase
	// +optional
	Message string `json:"message,omitempty"`

	// ClaimedReplicas indicates how many sandboxes are currently claimed (total)
	// This is determined by querying sandboxes with matching ownerReference
	// Only updated during Pending and Claiming phases
	// +optional
	ClaimedReplicas int32 `json:"claimedReplicas"`

	// ClaimStartTime is the timestamp when claiming started
	// Used for calculating timeout
	// +optional
	ClaimStartTime *metav1.Time `json:"claimStartTime,omitempty"`

	// CompletionTime is the timestamp when the claim reached Completed phase
	// Used for TTL calculation
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Conditions represent the current state of the SandboxClaim
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SandboxClaimPhase defines the phase of SandboxClaim
// +enum
type SandboxClaimPhase string

const (
	SandboxClaimPhaseClaiming  SandboxClaimPhase = "Claiming"
	SandboxClaimPhaseCompleted SandboxClaimPhase = "Completed"
)

// SandboxClaimConditionType defines condition types
type SandboxClaimConditionType string

const (
	// SandboxClaimConditionCompleted indicates if the claim is completed
	SandboxClaimConditionCompleted SandboxClaimConditionType = "Completed"
	// SandboxClaimConditionTimedOut indicates if the claim has timed out
	SandboxClaimConditionTimedOut SandboxClaimConditionType = "TimedOut"
)

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=sandboxclaims,shortName={sbc},singular=sandboxclaim
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Template",type="string",JSONPath=".spec.templateName"
// +kubebuilder:printcolumn:name="Desired",type="integer",JSONPath=".spec.replicas"
// +kubebuilder:printcolumn:name="Claimed",type="integer",JSONPath=".status.claimedReplicas"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SandboxClaim is the Schema for the sandboxclaims API
type SandboxClaim struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of SandboxClaim
	// +required
	Spec SandboxClaimSpec `json:"spec"`

	// status defines the observed state of SandboxClaim
	// +optional
	Status SandboxClaimStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// SandboxClaimList contains a list of SandboxClaim
type SandboxClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxClaim{}, &SandboxClaimList{})
}
