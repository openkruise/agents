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
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	// CheckpointFinalizer is checkpoint finalizer
	CheckpointFinalizer = "agents.kruise.io/checkpoint"

	// CheckpointLabelSandboxName is checkpointed sandbox name
	CheckpointLabelSandboxName = InternalPrefix + "sandbox-name"

	// CheckpointLabelType is the checkpoint type label key
	CheckpointLabelType = InternalPrefix + "checkpoint-type"

	// CheckpointLabelID is the checkpoint ID label key
	CheckpointLabelID = InternalPrefix + "checkpoint-id"

	CheckpointPersistentContentPodInfo    = "podInfo"
	CheckpointPersistentContentMemory     = "memory"
	CheckpointPersistentContentFilesystem = "filesystem"
)

// Legacy checkpoint type label values used by v0.5.22 and earlier.
// These exist solely for backward compatibility during controller upgrade:
// the old controller used "pod-info" (with hyphen) and "upgrade" as label
// values, while the new controller derives labels from PersistentContents
// ("podInfo", "filesystem").
// TODO(legacy-compat): remove once all v0.5.22 checkpoints are garbage collected.
const (
	LegacyCheckpointTypePodInfo = "pod-info"
	LegacyCheckpointTypeUpgrade = "upgrade"
)

// IsPodInfoOnly returns true when PersistentContents contains only podInfo,
// meaning the checkpoint records pod template delta without dumping any
// container state. Such checkpoints skip the finalizer and succeed
// immediately in the checkpoint controller.
func IsPodInfoOnly(contents []string) bool {
	if len(contents) != 1 {
		return false
	}
	return contents[0] == CheckpointPersistentContentPodInfo
}

// ContainsPodInfo returns true when PersistentContents includes podInfo,
// meaning the checkpoint records (or will record) pod template delta.
func ContainsPodInfo(contents []string) bool {
	for _, c := range contents {
		if c == CheckpointPersistentContentPodInfo {
			return true
		}
	}
	return false
}

// LegacyCheckpointLabel returns the v0.5.22 label value for the given new
// checkpoint type label, or empty if there is no legacy equivalent. Used
// during controller upgrade to find existing checkpoints created by the old
// controller before falling back to creating a new one.
// TODO(legacy-compat): remove once all v0.5.22 checkpoints are garbage collected.
func LegacyCheckpointLabel(label string) string {
	switch label {
	case CheckpointPersistentContentPodInfo:
		return LegacyCheckpointTypePodInfo
	case CheckpointPersistentContentFilesystem:
		return LegacyCheckpointTypeUpgrade
	}
	return ""
}

// IsPodInfoOnlyCheckpoint returns true for a checkpoint that only records pod
// template delta (no container state dump). This includes new checkpoints with
// PersistentContents=["podInfo"], and legacy v0.5.22 pod-info checkpoints that
// carry the old "pod-info" label and have no PersistentContents set.
// TODO(legacy-compat): remove legacy branch after v0.5.22 checkpoints are GC'd.
func IsPodInfoOnlyCheckpoint(cp *Checkpoint) bool {
	if IsPodInfoOnly(cp.Spec.PersistentContents) {
		return true
	}
	// Legacy: v0.5.22 pod-info checkpoints have no PersistentContents
	// and carry the old "pod-info" label.
	return len(cp.Spec.PersistentContents) == 0 &&
		cp.Labels[CheckpointLabelType] == LegacyCheckpointTypePodInfo
}

// IsPodInfoCheckpoint returns true for any checkpoint that records pod
// template delta, including legacy v0.5.22 pod-info checkpoints. Unlike
// IsPodInfoOnlyCheckpoint, this also matches checkpoints whose contents
// combine podInfo with other contents.
// TODO(legacy-compat): remove legacy branch after v0.5.22 checkpoints are GC'd.
func IsPodInfoCheckpoint(cp *Checkpoint) bool {
	if ContainsPodInfo(cp.Spec.PersistentContents) {
		return true
	}
	// Legacy: v0.5.22 pod-info checkpoints have no PersistentContents
	// and carry the old "pod-info" label.
	return len(cp.Spec.PersistentContents) == 0 &&
		cp.Labels[CheckpointLabelType] == LegacyCheckpointTypePodInfo
}

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

	// PersistentContents indicates resume pod with persistent content, Enum: podInfo, memory, filesystem
	// +kubebuilder:validation:Optional
	// +listType=atomic
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

	// PodTemplateDelta stores a Strategic Merge Patch that captures the delta between
	// the running Pod at pause time and the base Pod generated from sandbox.spec.template
	// + runtime injection. Applied at resume time to reconstruct the Pod faithfully.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	PodTemplateDelta runtime.RawExtension `json:"podTemplateDelta,omitempty"`
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

var CheckpointControllerKind = GroupVersion.WithKind("Checkpoint")

func init() {
	SchemeBuilder.Register(&Checkpoint{}, &CheckpointList{})
}
