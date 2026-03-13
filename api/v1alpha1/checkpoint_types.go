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
	// CheckpointFinalizer is checkpoint finalizer
	CheckpointFinalizer = "agents.kruise.io/checkpoint"

	// CheckpointLabelSandboxName is checkpointed sandbox name
	CheckpointLabelSandboxName = InternalPrefix + "sandbox-name"

	CheckpointPersistentContentMemory     = "memory"
	CheckpointPersistentContentFilesystem = "filesystem"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// CheckpointSpec defines the desired state of Checkpoint
type CheckpointSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// SandboxName is checkpoint sandboxName.
	// +kubebuilder:validation:Optional
	SandboxName *string `json:"sandboxName,omitempty"`

	// PodName is checkpoint podName.
	// +kubebuilder:validation:Optional
	PodName *string `json:"podName,omitempty"`

	// KeepRunning indicates whether the pod remains in the Running state after passing the checkpoint.
	// Default is true.
	// +kubebuilder:validation:Optional
	KeepRunning *bool `json:"keepRunning,omitempty"`

	// PersistentContents indicates resume pod with persistent content, Enum: memory, filesystem
	// +kubebuilder:validation:Optional
	PersistentContents []string `json:"persistentContents,omitempty"`

	// +kubebuilder:validation:Optional
	// valid format: 30s, 30m, 30d
	TtlAfterFinished *string `json:"ttlAfterFinished,omitempty"`
}

// CheckpointStatus defines the observed state of Checkpoint.
type CheckpointStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// observedGeneration is the most recent generation observed for this Checkpoint. It corresponds to the
	// Checkpoint's generation, which is updated on mutation by the API Server.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Checkpoint Phase
	Phase CheckpointPhase `json:"phase,omitempty"`

	// message
	Message string `json:"message,omitempty"`

	// checkpoint-id
	CheckpointId string `json:"checkpointId,omitempty"`

	// CompletionTime is checkpoint completed time, and phase is Succeeded or Failed.
	// +kubebuilder:validation:Optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
}

// CheckpointPhase is a label for the condition of a pod at the current time.
// +enum
type CheckpointPhase string

// These are the valid statuses of pods.
const (
	// CheckpointPending means the checkpoint has been accepted by the system.
	CheckpointPending     CheckpointPhase = "Pending"
	CheckpointCreating    CheckpointPhase = "Creating"
	CheckpointSucceeded   CheckpointPhase = "Succeeded"
	CheckpointFailed      CheckpointPhase = "Failed"
	CheckpointTerminating CheckpointPhase = "Terminating"
)

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:resource:path=checkpoints,shortName={cp},singular=checkpoint
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Checkpoint is the Schema for the Checkpoints API
type Checkpoint struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Checkpoint
	// +required
	Spec CheckpointSpec `json:"spec"`

	// status defines the observed state of Checkpoint
	// +optional
	Status CheckpointStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// CheckpointList contains a list of Checkpoint
type CheckpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Checkpoint `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Checkpoint{}, &CheckpointList{})
}
