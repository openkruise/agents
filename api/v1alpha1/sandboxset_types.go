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
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	InternalPrefix = "agents.kruise.io/"

	// LabelSandboxPool identifies which SandboxSet generated the sandbox.
	// Used by the recycle flow to find the origin SandboxSet.
	LabelSandboxPool = InternalPrefix + "sandbox-pool"
	// LabelSandboxTemplate identifies which template generated the sandbox
	LabelSandboxTemplate = InternalPrefix + "sandbox-template"
	// LabelSandboxIsClaimed indicates whether the sandbox has been claimed by user
	LabelSandboxIsClaimed = InternalPrefix + "sandbox-claimed"
	// LabelSandboxClaimName indicates the name of the SandboxClaim that claimed this sandbox
	LabelSandboxClaimName = InternalPrefix + "claim-name"
	LabelTemplateHash     = InternalPrefix + "template-hash"
	// LabelSandboxReservedFailed marks a failed sandbox retained for debugging.
	LabelSandboxReservedFailed = InternalPrefix + "reserved-failed-sandbox"
	// LabelSandboxName is the label key used by TrafficPolicy Spec.Selector to select the sandbox pod.
	LabelSandboxName = InternalPrefix + "sandbox-name"
	// LabelAllowInternetAccess indicates whether the sandbox is allowed internet access.
	// Default is "true"; set to "false" when the user explicitly disables internet access.
	// GlobalTrafficPolicy uses this label to select pods and apply egress rules.
	LabelAllowInternetAccess = InternalPrefix + "allow-internet-access"

	AnnotationLock               = InternalPrefix + "lock"
	AnnotationOwner              = InternalPrefix + "owner"
	AnnotationClaimTime          = InternalPrefix + "claim-timestamp"
	AnnotationRestoreFrom        = InternalPrefix + "restore-from"
	AnnotationInitRuntimeRequest = InternalPrefix + "init-runtime-request"
	AnnotationSandboxID          = InternalPrefix + "sandbox-id"
	AnnotationMemberlistURL      = InternalPrefix + "memberlist-url"

	// AnnotationCleanupEnabled marks a sandbox as supporting recycle.
	AnnotationCleanupEnabled = InternalPrefix + "cleanup-enabled"
	// AnnotationCleanup triggers the sandbox recycle flow. Removed by the controller after successful recycle.
	AnnotationCleanup = InternalPrefix + "cleanup"
	// AnnotationCleanupRetainOnFailure controls how long the sandbox is retained after recycle failure.
	// Accepts a Go duration string (e.g., "5m") — the sandbox is retained for that duration and then
	// deleted via ShutdownTime. By default (unset), the sandbox is deleted immediately after recycle failure.
	// If the value is invalid, the sandbox is also deleted immediately with a warning log.
	AnnotationCleanupRetainOnFailure = InternalPrefix + "cleanup-retain-on-failure"
	// AnnotationUpdatedMetadataInClaim stores the keys of labels/annotations added or modified
	// during the claim flow (JSON format, keys only). Used by the recycle flow to reset metadata.
	AnnotationUpdatedMetadataInClaim = InternalPrefix + "updated-metadata-in-claim"
)

// AnnotationsClearedOnRecycle lists all annotation keys that are removed from a
// sandbox when it is successfully recycled and returned to the pool. When adding
// a new annotation that should be cleared during recycle, append it here to avoid
// missing the recycle in resetMetadataForPool.
//
// Note: AnnotationUpdatedMetadataInClaim is handled separately because it is
// consumed before deletion to determine user-specified metadata keys.
// Annotations from other packages (e.g. identity.AgentKeyTokenRefreshStatus)
// are handled individually in resetMetadataForPool.
var AnnotationsClearedOnRecycle = []string{
	AnnotationClaimTime,
	AnnotationLock,
	AnnotationOwner,
	AnnotationInitRuntimeRequest,
	AnnotationRuntimeAccessToken,
	AnnotationCleanup,
	AnnotationCleanupRetainOnFailure,
	AnnotationCSIVolumeConfig,
	SandboxAnnotationPriority,
	AnnotationEnvdAccessToken,
	AnnotationEnvdURL,
	AnnotationRuntimeURL,
}

// InternalKeysPreservedOnCreation lists internal keys (with the InternalPrefix)
// that are preserved when creating a new sandbox from a SandboxSet template.
// All other internal keys are cleared to ensure a clean slate. When adding a
// new internal key that should survive sandbox creation, add it here to avoid
// accidental deletion in clearAndInitInnerKeys.
var InternalKeysPreservedOnCreation = map[string]struct{}{
	AnnotationCleanupEnabled:         {},
	AnnotationCleanupRetainOnFailure: {},
}

// UpdatedMetadataInClaim records the keys of labels/annotations added or modified during claim.
// Used by the recycle flow to determine which metadata to reset.
type UpdatedMetadataInClaim struct {
	Labels      []string `json:"labels,omitempty"`
	Annotations []string `json:"annotations,omitempty"`
}

const (
	SandboxStateCreating  = "creating"
	SandboxStateAvailable = "available"
	SandboxStateRunning   = "running"
	SandboxStatePaused    = "paused"
	SandboxStateDead      = "dead"
)

var SandboxSetControllerKind = GroupVersion.WithKind("SandboxSet")

// SandboxSetSpec defines the desired state of SandboxSet
type SandboxSetSpec struct {
	// Replicas is the number of unused sandboxes, including available and creating ones.
	Replicas int32 `json:"replicas"`

	// PersistentContents indicates resume pod with persistent content, Enum: ip, memory, filesystem
	// +listType=atomic
	PersistentContents []string `json:"persistentContents,omitempty"`

	// Runtimes - Runtime configuration for sandbox object
	// +optional
	// +listType=atomic
	Runtimes []RuntimeConfig `json:"runtimes,omitempty"`

	EmbeddedSandboxTemplate `json:",inline"`

	// ScaleStrategy indicates the ScaleStrategy that will be employed to
	// create and delete Sandboxes in the SandboxSet.
	ScaleStrategy SandboxSetScaleStrategy `json:"scaleStrategy,omitempty"`

	// UpdateStrategy indicates the strategy that will be employed to
	// update Sandboxes in the SandboxSet when the template changes.
	// +optional
	UpdateStrategy SandboxSetUpdateStrategy `json:"updateStrategy,omitempty"`
}

// SandboxSetScaleStrategy defines strategies for sandboxes scale.
type SandboxSetScaleStrategy struct {
	// The maximum number of sandboxes that can be unavailable for scaled sandboxes.
	// This field can control the changes rate of replicas for SandboxSet so as to minimize the impact for users' service.
	// The scale will fail if the number of unavailable sandboxes were greater than this MaxUnavailable at scaling up.
	// MaxUnavailable works only when scaling up.
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
}

// RollingUpdateStrategyType is the type of rolling update strategy.
type RollingUpdateStrategyType string

const (
	// RecreateUpdateStrategyType means the controller will delete old pods and create new ones.
	RecreateUpdateStrategyType RollingUpdateStrategyType = "Recreate"
)

// SandboxSetUpdateStrategy defines strategies for rolling update.
type SandboxSetUpdateStrategy struct {
	// MaxUnavailable is the maximum number or percentage of pods that can be unavailable during the update.
	// MaxUnavailable can be an absolute number (ex: 5) or a percentage of desired pods (ex: 10%).
	// Absolute number is calculated from percentage by rounding down.
	// Default is 20%.
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
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

	// UpdateRevision is the FNV-32 hash computed from spec.template,
	// spec.persistentContents, and spec.runtimes.
	// It represents the latest desired template version.
	UpdateRevision string `json:"updateRevision,omitempty"`

	// CurrentRevision is the SandboxTemplate name corresponding to the currently
	// materialised revision, or spec.templateRef.Name when templateRef is used.
	// +optional
	CurrentRevision string `json:"currentRevision,omitempty"`

	// UpdatedReplicas is the number of sandboxes that have been updated to the UpdateRevision.
	// +optional
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`

	// UpdatedAvailableReplicas is the number of updated sandboxes that are available.
	// +optional
	UpdatedAvailableReplicas int32 `json:"updatedAvailableReplicas,omitempty"`

	// conditions represent the current state of the SandboxSet resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Selector is a label query over pods that should match the replica count.
	// This is same as the label selector but in the string format to avoid
	// duplication for CRDs that do not support structural schemas.
	// +optional
	Selector string `json:"selector,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:resource:path=sandboxsets,shortName={sbs},singular=sandboxset
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".status.replicas"
// +kubebuilder:printcolumn:name="Available",type="integer",JSONPath=".status.availableReplicas"
// +kubebuilder:printcolumn:name="UpdatedReplicas",type="integer",JSONPath=".status.updatedReplicas"
// +kubebuilder:printcolumn:name="UpdatedAvailableReplicas",type="integer",JSONPath=".status.updatedAvailableReplicas"
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
