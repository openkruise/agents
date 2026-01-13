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
	"k8s.io/apimachinery/pkg/types"
)

const (
	// SandboxHashWithoutImageAndResources represents the key of sandbox hash without image and resources.
	SandboxHashWithoutImageAndResources = "sandbox.agents.kruise.io/hash-without-image-resources"

	// PodLabelTemplateHash is pod template hash
	PodLabelTemplateHash = "pod-template-hash"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// SandboxSpec defines the desired state of Sandbox
type SandboxSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// Paused indicates whether pause the sandbox pod.
	// +optional
	Paused bool `json:"paused,omitempty"`

	// PersistentContents indicates resume pod with persistent content, Enum: ip, memory, filesystem
	PersistentContents []string `json:"persistentContents,omitempty"`

	// ShutdownTime - Absolute time when the sandbox is deleted.
	// If a time in the past is provided, the sandbox will be deleted immediately.
	// +kubebuilder:validation:Format="date-time"
	ShutdownTime *metav1.Time `json:"shutdownTime,omitempty"`

	// PauseTime - Absolute time when the sandbox will be paused automatically.
	// +kubebuilder:validation:Format="date-time"
	PauseTime *metav1.Time `json:"pauseTime,omitempty"`

	SandboxTemplate `json:",inline"`
}

type SandboxTemplate struct {

	// TemplateRef references a SandboxTemplate, which will be used to create the sandbox.
	// +optional
	TemplateRef *SandboxTemplateRef `json:"templateRef,omitempty"`

	// Template describes the pods that will be created.
	// Template is mutual exclusive with TemplateRef
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +optional
	Template *v1.PodTemplateSpec `json:"template,omitempty"`

	// VolumeClaimTemplates is a list of PVC templates to create for this Sandbox.
	// +optional
	VolumeClaimTemplates []v1.PersistentVolumeClaim `json:"volumeClaimTemplates,omitempty"`
}

// SandboxTemmplateRef references a SandboxTemplate
type SandboxTemplateRef struct {
	// name of the SandboxTemplate
	// +kubebuilder:validation:Required
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// name of the SandboxTemplate kind
	// Default to PodTemplate
	// +optional
	Kind *string `json:"kind,omitempty"`

	// name of the SandboxTemplate apiVersion
	// Default to v1
	// +optional
	APIVersion *string `json:"apiVersion,omitempty"`
}

const (
	PersistentContentIp         string = "ip"
	PersistentContentMemory     string = "memory"
	PersistentContentFilesystem string = "filesystem"
)

// SandboxStatus defines the observed state of Sandbox.
type SandboxStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// observedGeneration is the most recent generation observed for this Sandbox. It corresponds to the
	// Sandbox's generation, which is updated on mutation by the API Server.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Sandbox Phase
	Phase SandboxPhase `json:"phase,omitempty"`

	// message
	Message string `json:"message,omitempty"`

	// conditions represent the current state of the Sandbox resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Pod Info
	PodInfo PodInfo `json:"podInfo,omitempty"`

	// NodeName indicates in which node this sandbox is scheduled.
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// SandboxIp is the ip address allocated to the sandbox.
	// +optional
	SandboxIp string `json:"sandboxIp,omitempty"`

	// UpdateRevision is the template-hash calculated from `spec.template`.
	// +optional
	UpdateRevision string `json:"updateRevision,omitempty"`
}

// SandboxPhase is a label for the condition of a pod at the current time.
// +enum
type SandboxPhase string

// These are the valid statuses of pods.
const (
	// SandboxPending means the pod has been accepted by the system, but one or more of the containers
	// has not been started. This includes time before being bound to a node, as well as time spent
	// pulling images onto the host.
	SandboxPending SandboxPhase = "Pending"
	// SandboxRunning means the pod has been bound to a node and all of the containers have been started.
	// At least one container is still running or is in the process of being restarted.
	SandboxRunning SandboxPhase = "Running"
	// SandboxPaused means the sandbox has entered the paused state.
	SandboxPaused SandboxPhase = "Paused"
	// SandboxResuming means the sandbox has entered the resume state
	SandboxResuming SandboxPhase = "Resuming"
	// SandboxSucceeded means that all containers in the pod have voluntarily terminated
	// with a container exit code of 0, and the system is not going to restart any of these containers.
	SandboxSucceeded SandboxPhase = "Succeeded"
	// SandboxFailed means that all containers in the pod have terminated, and at least one container has
	// terminated in a failure (exited with a non-zero exit code or was stopped by the system).
	SandboxFailed SandboxPhase = "Failed"
	// SandboxTerminating means sandbox will perform cleanup after deletion.
	SandboxTerminating SandboxPhase = "Terminating"
)

// TODO Some external controllers have specific conditions, whether to keep them
type PodInfo struct {
	// Annotations contains pod important annotations
	Annotations map[string]string `json:"annotations,omitempty"`
	// Labels contains pod important labels
	Labels map[string]string `json:"labels,omitempty"`
	// NodeName indicates in which node this pod is scheduled.
	NodeName string `json:"nodeName,omitempty"`
	// PodIP address allocated to the pod.
	PodIP string `json:"podIP,omitempty"`
	// PodUID is pod uid.
	PodUID types.UID `json:"podUID,omitempty"`
}

// SandboxConditionType is a valid value for SandboxCondition.Type
type SandboxConditionType string

// These are built-in conditions of pod. An application may use a custom condition not listed here.
const (
	// SandboxConditionReady means the sandbox is able to service requests and should be added to the
	// load balancing pools of all matching services.
	SandboxConditionReady SandboxConditionType = "Ready"

	// SandboxConditionPaused means all containers of the sandbox have been paused.
	SandboxConditionPaused SandboxConditionType = "SandboxPaused"

	// SandboxConditionResumed means to resume the sandbox.
	SandboxConditionResumed SandboxConditionType = "SandboxResumed"

	// SandboxConditionInplaceUpdate means inplace update state.
	SandboxConditionInplaceUpdate SandboxConditionType = "InplaceUpdate"
)

const (
	// SandboxConditionReady Reason
	SandboxReadyReasonPodReady             = "PodReady"
	SandboxReadyReasonInplaceUpdating      = "InplaceUpdating"
	SandboxReadyReasonStartContainerFailed = "StartContainerFailed"

	// SandboxConditionInplaceUpdate Reason
	SandboxInplaceUpdateReasonInplaceUpdating = "InplaceUpdating"
	SandboxInplaceUpdateReasonSucceeded       = "Succeeded"
	SandboxInplaceUpdateReasonFailed          = "Failed"

	// SandboxConditionPaused Reason
	SandboxPausedReasonSetPause  = "SetPause"
	SandboxPausedReasonDeletePod = "DeletePod"

	// SandboxConditionResume Reason
	SandboxResumeReasonCreatePod = "CreatePod"
	SandboxResumeReasonResumePod = "ResumePod"
)

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=sandboxes,shortName={sbx},singular=sandbox
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="shutdown_time",type="string",JSONPath=".spec.shutdownTime"
// +kubebuilder:printcolumn:name="pause_time",type="string",JSONPath=".spec.pauseTime"
// +kubebuilder:printcolumn:name="Message",type="string",JSONPath=".status.message"

// Sandbox is the Schema for the sandboxes API
type Sandbox struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Sandbox
	// +required
	Spec SandboxSpec `json:"spec"`

	// status defines the observed state of Sandbox
	// +optional
	Status SandboxStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// SandboxList contains a list of Sandbox
type SandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Sandbox `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Sandbox{}, &SandboxList{})
}
