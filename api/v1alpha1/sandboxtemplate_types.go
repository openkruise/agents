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

const (
	// SandboxTemplateBackendKubernetes selects the Kubernetes sandboxcr
	// backend. Its public wire value is "k8s".
	SandboxTemplateBackendKubernetes = "k8s"
	// SandboxTemplateBackendSubstrate selects the Substrate backend.
	SandboxTemplateBackendSubstrate = "substrate"
)

// SandboxTemplateSpec defines the desired state of SandboxTemplate.
type SandboxTemplateSpec struct {
	// Backend selects the build backend. The public value "k8s" maps to the
	// internal sandboxcr backend. Once written, the backend cannot change.
	// +kubebuilder:validation:Enum=k8s
	// +kubebuilder:default:=k8s
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="backend is immutable"
	// +optional
	Backend string `json:"backend,omitempty"`

	// BuildOptions is the declarative snapshot of the Template Build request.
	// It is populated by a build entry and consumed by the SandboxTemplate
	// controller. Logical Template admission keeps it immutable after the
	// build input is accepted.
	// +optional
	BuildOptions *SandboxTemplateBuildOptions `json:"buildOptions,omitempty"`

	// Template describes the pods that will be created.
	// Template is mutual exclusive with TemplateRef
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +optional
	Template *v1.PodTemplateSpec `json:"template,omitempty"`

	// VolumeClaimTemplates is a list of PVC templates to create for this Sandbox.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +optional
	VolumeClaimTemplates []v1.PersistentVolumeClaim `json:"volumeClaimTemplates,omitempty"`

	// PersistentContents indicates resume pod with persistent content, Enum: ip, memory, filesystem
	// +listType=atomic
	PersistentContents []string `json:"persistentContents,omitempty"`

	// Runtimes - Runtime configuration for sandbox object
	// +optional
	// +listType=atomic
	Runtimes []RuntimeConfig `json:"runtimes,omitempty"`

	// PauseStrategy configures how sandboxes created from this template are
	// paused when their spec.paused is true. Claim-owned companion pools inherit
	// this value; non-companion templateRef pools retain SandboxSetSpec policy.
	// +optional
	PauseStrategy *PauseStrategy `json:"pauseStrategy,omitempty"`

	// Probes defines the named probes that sandboxes created from this template
	// run while they are Running. Claim-owned companion pools inherit this value;
	// non-companion templateRef pools retain SandboxSetSpec.Probes policy.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	Probes []Probe `json:"probes,omitempty"`

	// AutoPausePolicy defines the pause/resume decision rules for sandboxes
	// created from this template. Claim-owned companion pools inherit this value;
	// non-companion templateRef pools retain SandboxSetSpec.AutoPausePolicy.
	// +optional
	AutoPausePolicy *AutoPausePolicy `json:"autoPausePolicy,omitempty"`

	// Lifecycle stores pause/resume hooks for direct checkpoint/clone use. When
	// referenced by a non-companion SandboxSet, SandboxSetSpec.Lifecycle remains
	// authoritative; Claim-owned companion pools inherit the version hooks.
	// Upgrade hooks remain instance policy and are not allowed here.
	// +kubebuilder:validation:XValidation:rule="!has(self.preUpgrade) && !has(self.postUpgrade)",message="preUpgrade and postUpgrade are not allowed on SandboxTemplate"
	// +optional
	Lifecycle *SandboxLifecycle `json:"lifecycle,omitempty"`
}

// SandboxTemplateBuildOptions is the immutable declarative input of one
// Template Build.
type SandboxTemplateBuildOptions struct {
	// FromTemplate names an existing logical Template used as the build base.
	// +optional
	FromTemplate string `json:"fromTemplate,omitempty"`

	// Steps is the ordered provisioning command sequence.
	// +listType=atomic
	// +optional
	Steps []TemplateBuildCommand `json:"steps,omitempty"`

	// StartCommand starts the business process before capture.
	// +optional
	StartCommand *TemplateBuildCommand `json:"startCommand,omitempty"`

	// ReadyCommand checks business process readiness before capture.
	// +optional
	ReadyCommand *TemplateBuildCommand `json:"readyCommand,omitempty"`

	// WarmPool declares managed pool and autoscaling intent for a Claim-owned
	// Kubernetes template version. Existing command-build fields above remain
	// part of the persisted API; the custom template input does not expose them.
	// +optional
	WarmPool *SandboxTemplateWarmPoolOptions `json:"warmPool,omitempty"`
}

// SandboxTemplateWarmPoolOptions declares optional companion resources.
// Enabled and Size intentionally have no admission defaults: disabled input
// must be validated before a missing size is normalized to zero.
// +kubebuilder:validation:XValidation:rule="!has(self.enabled) || self.enabled || (!has(self.size) && !has(self.autoscaler))",message="disabled warmPool must omit size and autoscaler"
// +kubebuilder:validation:XValidation:rule="!has(self.autoscaler) || !has(self.size) || self.size <= self.autoscaler.maxReplicas",message="initial size must not exceed autoscaler maxReplicas"
type SandboxTemplateWarmPoolOptions struct {
	// Enabled defaults to true in the custom-input normalization path.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// Size is a fixed target without an autoscaler and initial capacity with
	// an autoscaler. Missing enabled size is normalized to zero.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Size *int32 `json:"size,omitempty"`
	// Autoscaler selects dynamic capacity even while the companion PA is
	// temporarily absent. Nil selects fixed capacity.
	// +optional
	Autoscaler *SandboxTemplatePoolAutoscalerOptions `json:"autoscaler,omitempty"`
}

// SandboxTemplatePoolAutoscalerOptions reuses the existing policy vocabulary
// without exposing a resource name, scaleTargetRef, suspend or status.
// +kubebuilder:validation:XValidation:rule="self.minReplicas <= self.maxReplicas",message="minReplicas must not exceed maxReplicas"
// +kubebuilder:validation:XValidation:rule="has(self.capacityPolicy) || (has(self.cronPolicies) && size(self.cronPolicies) > 0)",message="at least one capacity or cron policy is required"
type SandboxTemplatePoolAutoscalerOptions struct {
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	MinReplicas int32 `json:"minReplicas"`
	// +required
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`
	// +optional
	CapacityPolicy *CapacityPolicy `json:"capacityPolicy,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=name
	CronPolicies []CronScalingPolicy `json:"cronPolicies,omitempty"`
}

// TemplateBuildCommand is one self-contained shell command in a Template
// Build. User, Cwd, and Env are the complete execution context folded from
// the caller's build DSL.
type TemplateBuildCommand struct {
	// Command is the shell command to execute.
	// +kubebuilder:validation:MinLength=1
	// +required
	Command string `json:"command"`

	// User optionally selects the executing user inside the container.
	// +optional
	User string `json:"user,omitempty"`

	// Cwd optionally selects the working directory inside the container.
	// +kubebuilder:validation:XValidation:rule="self == '' || self.startsWith('/')",message="cwd must be an absolute path"
	// +optional
	Cwd string `json:"cwd,omitempty"`

	// Env is the complete environment of this command.
	// +optional
	Env map[string]string `json:"env,omitempty"`
}

// SandboxTemplateBuildPhase is the lifecycle phase of a Template Build.
type SandboxTemplateBuildPhase string

const (
	// SandboxTemplatePhasePending means the build input is accepted and build
	// execution has not started.
	SandboxTemplatePhasePending SandboxTemplateBuildPhase = "Pending"
	// SandboxTemplatePhaseBuilding means build execution is in progress.
	SandboxTemplatePhaseBuilding SandboxTemplateBuildPhase = "Building"
	// SandboxTemplatePhaseReady means all declared work is complete and the
	// Template can be consumed.
	SandboxTemplatePhaseReady SandboxTemplateBuildPhase = "Ready"
	// SandboxTemplatePhaseFailed means the Build failed terminally.
	SandboxTemplatePhaseFailed SandboxTemplateBuildPhase = "Failed"
)

// SandboxTemplateArtifact is an immutable reference to one backend build
// artifact. The Type identifies the reference vocabulary and Name identifies
// the backend-owned resource.
type SandboxTemplateArtifact struct {
	// Type is the artifact reference type.
	// +required
	Type string `json:"type"`

	// Name is the backend artifact resource name.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// SandboxTemplateStatus defines the observed state of a Template Build. The
// SandboxTemplate controller is its sole writer.
type SandboxTemplateStatus struct {
	// ObservedGeneration is the most recent generation processed by the
	// controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is the overall Template Build lifecycle phase.
	// +kubebuilder:validation:Enum=Pending;Building;Ready;Failed
	// +optional
	Phase SandboxTemplateBuildPhase `json:"phase,omitempty"`

	// Artifacts contains at most one immutable reference of each artifact type.
	// +listType=map
	// +listMapKey=type
	// +optional
	Artifacts []SandboxTemplateArtifact `json:"artifacts,omitempty"`

	// Conditions report build progress, readiness, and sanitized failures.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=sandboxtemplates,shortName={sbt},singular=sandboxtemplate
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Backend",type="string",JSONPath=".spec.backend"
// SandboxTemplate is the Schema for logical Templates and the workload
// templates referenced by SandboxSets and Checkpoints.
type SandboxTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of SandboxTemplate.
	// +required
	Spec SandboxTemplateSpec `json:"spec"`

	// status defines the observed state of a logical Template Build.
	// +optional
	Status SandboxTemplateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// SandboxTemplateList contains a list of SandboxTemplate
type SandboxTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxTemplate `json:"items"`
}

var SandboxTemplateControllerKind = GroupVersion.WithKind("SandboxTemplate")

func init() {
	SchemeBuilder.Register(&SandboxTemplate{}, &SandboxTemplateList{})
}
