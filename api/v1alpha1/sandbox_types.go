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
	// Deprecated, use SandboxHashImmutablePart instead
	SandboxHashWithoutImageAndResources = "sandbox.agents.kruise.io/hash-without-image-resources"

	// SandboxHashImmutablePart represents the key of sandbox hash than exclude immutable part of sandbox
	// e.g. metadata, image and resources
	SandboxHashImmutablePart = "sandbox.agents.kruise.io/hash-immutable-part"

	// PodLabelTemplateHash is pod template hash
	PodLabelTemplateHash = "pod-template-hash"

	// SandboxAnnotationPriority is the annotation key for sandbox priority.
	// If not set, the default value is 0.
	// Larger values indicate higher priority.
	// Note: SandboxSet creates sandboxes with priority 0 by default.
	// Sandbox Manager or Sandbox Claim creates high-priority sandboxes by default.
	SandboxAnnotationPriority = "agents.kruise.io/sandbox-priority"

	// RuntimeConfigForInjectCsiMount is a valid value for RuntimeConfig.Name.
	// When set, enables CSI mount sidecar injection for the sandbox.
	RuntimeConfigForInjectCsiMount = "csi"
	// RuntimeConfigForInjectAgentRuntime is a valid value for RuntimeConfig.Name.
	// When set, enables agent runtime sidecar injection for the sandbox.
	RuntimeConfigForInjectAgentRuntime = "agent-runtime"
	// RuntimeConfigForInjectTrafficProxy is a valid value for RuntimeConfig.Name.
	// When set, enables traffic proxy sidecar injection for the sandbox.
	RuntimeConfigForInjectTrafficProxy = "traffic-proxy"
)

type RuntimeConfig struct {
	Name string `json:"name"`
}

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
	// +listType=atomic
	PersistentContents []string `json:"persistentContents,omitempty"`

	// ShutdownTime - Absolute time when the sandbox is deleted.
	// If a time in the past is provided, the sandbox will be deleted immediately.
	// +kubebuilder:validation:Format="date-time"
	ShutdownTime *metav1.Time `json:"shutdownTime,omitempty"`

	// Runtimes - Runtime configuration for sandbox object
	// +optional
	// +listType=atomic
	Runtimes []RuntimeConfig `json:"runtimes,omitempty"`

	// PauseTime - Absolute time when the sandbox will be paused automatically.
	// +kubebuilder:validation:Format="date-time"
	PauseTime *metav1.Time `json:"pauseTime,omitempty"`

	// Lifecycle defines lifecycle hooks for sandbox.
	// +optional
	Lifecycle *SandboxLifecycle `json:"lifecycle,omitempty"`

	// Probes defines a list of named probes that run periodically while the sandbox
	// is Running. Each probe writes its result to a Pod Status Condition with
	// type "agents.kruise.io/<name>". Probes are generic — their semantics (e.g.,
	// "activity detection" vs "cron task detection") are defined by
	// AutoPausePolicy.Pause/Resume, not by the probe itself.
	//
	// Probe execution is delegated to the agent-runtime sidecar via the
	// PodProbeMarker Serverless protocol (kruise.io/podprobe annotation).
	// The controller reads results from Pod.Status.Conditions and mirrors
	// them to SandboxStatus.Conditions for observability.
	// +optional
	Probes []Probe `json:"probes,omitempty"`

	// AutoPausePolicy defines pause/resume decision rules based on probe
	// Conditions. Probes are defined in Spec.Probes.
	// +optional
	AutoPausePolicy *AutoPausePolicy `json:"autoPausePolicy,omitempty"`

	// UpgradePolicy defines the upgrade strategy for the sandbox.
	// +optional
	UpgradePolicy *SandboxUpgradePolicy `json:"upgradePolicy,omitempty"`

	EmbeddedSandboxTemplate `json:",inline"`
}

type EmbeddedSandboxTemplate struct {

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
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +optional
	VolumeClaimTemplates []v1.PersistentVolumeClaim `json:"volumeClaimTemplates,omitempty"`
}

// SandboxTemplateRef references a SandboxTemplate
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

// SandboxUpgradePolicyType is the type of sandbox update policy.
type SandboxUpgradePolicyType string

const (
	// SandboxUpgradePolicyRecreate means sandbox will be updated by recreating the pod.
	SandboxUpgradePolicyRecreate SandboxUpgradePolicyType = "Recreate"
)

// SandboxUpgradePolicy defines the upgrade strategy for the sandbox.
// When Type is empty (default), the sandbox does not support upgrading.
// Only when Type is explicitly set (e.g., Recreate), the upgrade capability is enabled.
type SandboxUpgradePolicy struct {
	// Type specifies the upgrade policy type.
	// When empty (default), upgrading is disabled.
	// Supported values: Recreate.
	// +optional
	Type SandboxUpgradePolicyType `json:"type,omitempty"`
}

// SandboxLifecycle defines lifecycle hooks for sandbox.
type SandboxLifecycle struct {
	// PreUpgrade is the action executed before the upgrade.
	// It is typically used to backup workspace data.
	// +optional
	PreUpgrade *UpgradeAction `json:"preUpgrade,omitempty"`

	// PostUpgrade is the action executed after the upgrade.
	// It is typically used to restore workspace data.
	// +optional
	PostUpgrade *UpgradeAction `json:"postUpgrade,omitempty"`
}

// Probe defines a named probe that writes its result to a Pod Condition.
// Embeds corev1.Probe inline so that exec/periodSeconds/timeoutSeconds/etc.
// are directly accessible. Currently only exec probes are supported;
// other corev1.Probe fields (httpGet, tcpSocket, grpc) may be supported
// in the future as needed.
type Probe struct {
	// Name is the unique identifier for this probe within the sandbox.
	// Probe results are written to a Condition with type "agents.kruise.io/<Name>".
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// ContainerName specifies which container to execute the probe in.
	// If empty, defaults to the first container in the pod spec.
	// +optional
	ContainerName string `json:"containerName,omitempty"`

	// Probe embeds corev1.Probe inline. Currently only exec, periodSeconds,
	// timeoutSeconds, and failureThreshold are actively used.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	v1.Probe `json:",inline"`
}

// AutoPausePolicy defines pause/resume decision rules based on probe
// Conditions. Probes are defined separately in Spec.Probes.
// When set, the sandbox controller evaluates pause/resume rules.
// Probe results (from Spec.Probes) are read via Pod.Status.Conditions
// and mirrored to SandboxStatus.Conditions.
// +optional
type AutoPausePolicy struct {
	// Pause defines the pause policy for the sandbox.
	// +optional
	Pause *PausePolicy `json:"pause,omitempty"`

	// Resume defines the resume policy for the sandbox.
	// +optional
	Resume *ResumePolicy `json:"resume,omitempty"`
}

// PausePolicy defines when to pause the sandbox based on probe results.
type PausePolicy struct {
	// WhenProbedIdleState pauses the sandbox when a probe's Condition message
	// matches MessageRegex for at least ThresholdDuration.
	// +optional
	WhenProbedIdleState *ProbedIdleStateRule `json:"whenProbedIdleState,omitempty"`
}

// ResumePolicy defines when to resume the sandbox based on probe results.
type ResumePolicy struct {
	// WhenProbedScheduleTime resumes the sandbox before a scheduled task
	// by parsing the probe's Condition message as a timestamp.
	// +optional
	WhenProbedScheduleTime *ProbedScheduleTimeRule `json:"whenProbedScheduleTime,omitempty"`
}

// ProbedIdleStateRule defines the rule for pausing when a probe reports
// an idle state. The controller reads the referenced probe's Condition and
// matches its message against MessageRegex. When the match persists for at
// least ThresholdDuration (measured from the Condition's lastTransitionTime),
// the sandbox is paused.
type ProbedIdleStateRule struct {
	// Probe is the name of the probe to evaluate for pause decisions.
	// Must match a probe name in Spec.Probes.
	// +kubebuilder:validation:Required
	Probe string `json:"probe"`

	// MessageRegex is a regular expression matched against the probe's
	// Condition message (stdout). When the message matches, the Agent is
	// considered inactive. When it does not match, the Agent is considered
	// active and the sandbox stays Running.
	// +kubebuilder:validation:Required
	MessageRegex string `json:"messageRegex"`

	// ThresholdDuration is the minimum time the probe's Condition message
	// must continuously match MessageRegex before the sandbox is paused.
	// Measured from the Condition's lastTransitionTime.
	// If nil, the sandbox is paused immediately when the message matches.
	// +optional
	ThresholdDuration *metav1.Duration `json:"thresholdDuration,omitempty"`
}

// ProbedScheduleTimeRule defines the rule for resuming based on a probed
// schedule time. The controller reads the referenced probe's Condition and,
// when TimeFormat is "unix", parses its message as a Unix timestamp
// (next event time). The sandbox is resumed LeadTime before the parsed timestamp.
type ProbedScheduleTimeRule struct {
	// Probe is the name of the probe to evaluate for resume decisions.
	// Must match a probe name in Spec.Probes.
	// +kubebuilder:validation:Required
	Probe string `json:"probe"`

	// TimeFormat indicates the format of the probe's Condition message for
	// parsing as a timestamp. When set to "unix", the controller parses the
	// message as a Unix timestamp (seconds since epoch) and sets NextResumeTime
	// to timestamp - LeadTime.
	// +optional
	// +kubebuilder:validation:Enum=unix
	TimeFormat string `json:"timeFormat,omitempty"`

	// LeadTime is the duration before the parsed timestamp at which the
	// sandbox should be resumed. For example, if the probe reports the
	// next scheduled task at time T and LeadTime is 5m, the sandbox is
	// resumed at T - 5m.
	// +optional
	// +kubebuilder:default="5m"
	LeadTime *metav1.Duration `json:"leadTime,omitempty"`
}

// Schedule tracks the upcoming pause/resume timing for the auto-pause controller.
// Reason indicates which rule produced the current schedule, NextPauseTime is the
// next expected pause time, and NextResumeTime is the next expected resume time.
// Both times are cleared once the corresponding action is triggered.
type Schedule struct {
	// Reason indicates which auto-pause rule triggered this schedule entry.
	// Examples: "probedIdle" (pause triggered by WhenProbedIdleState),
	// "probedSchedule" (resume triggered by WhenProbedScheduleTime).
	// +optional
	Reason string `json:"reason,omitempty"`

	// NextPauseTime is when the sandbox is expected to be paused, computed from
	// the pause policy once the probed idle threshold is about to be reached.
	// It is cleared after a pause is triggered.
	// +optional
	NextPauseTime *metav1.Time `json:"nextPauseTime,omitempty"`

	// NextResumeTime is when the sandbox is expected to be resumed, computed from
	// the resume policy's probed schedule time. It is cleared after a resume is triggered.
	// +optional
	NextResumeTime *metav1.Time `json:"nextResumeTime,omitempty"`
}

// UpgradeAction defines an action to execute during sandbox upgrade.
// It supports multiple action types for extensibility.
type UpgradeAction struct {
	// Exec specifies the command to execute inside the sandbox via envd.
	// The first element is the command, the rest are args.
	// For shell scripts, use: ["/bin/bash", "-c", "your-script"]
	// +optional
	Exec *v1.ExecAction `json:"exec,omitempty"`

	// TimeoutSeconds is the timeout for the action execution in seconds.
	// +kubebuilder:default=60
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
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

	// RecycledCount records the number of times this sandbox has been recycled.
	// +optional
	RecycledCount int32 `json:"recycledCount,omitempty"`

	// Schedules contains upcoming scheduled pause/resume events.
	// Only populated when Spec.AutoPausePolicy.Pause/Resume is configured.
	// +optional
	// +patchMergeKey=reason
	// +patchStrategy=merge
	Schedules []Schedule `json:"schedules,omitempty"`
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
	// SandboxUpgrading means the sandbox is being upgraded (recreate or inplace update).
	SandboxUpgrading SandboxPhase = "Upgrading"
	// SandboxSucceeded means that all containers in the pod have voluntarily terminated
	// with a container exit code of 0, and the system is not going to restart any of these containers.
	SandboxSucceeded SandboxPhase = "Succeeded"
	// SandboxFailed means that all containers in the pod have terminated, and at least one container has
	// terminated in a failure (exited with a non-zero exit code or was stopped by the system).
	SandboxFailed SandboxPhase = "Failed"
	// SandboxRecycling means the sandbox is being recycled and preparing to return to pool.
	SandboxRecycling SandboxPhase = "Recycling"
	// SandboxTerminating means sandbox will perform cleanup after deletion.
	SandboxTerminating SandboxPhase = "Terminating"
)

// TODO Some external controllers have specific conditions, whether to keep them
type PodInfo struct {
	// Annotations contains pod important annotations
	// +mapType=granular
	Annotations map[string]string `json:"annotations,omitempty"`
	// Labels contains pod important labels
	// +mapType=granular
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

	// SandboxConditionUpgrading means upgrade state.
	SandboxConditionUpgrading SandboxConditionType = "Upgrading"

	// RuntimeInitialized means the agent-runtime inside
	// the sandbox pod has completed initialization (first-time init or re-init
	// after resume/recreate/upgrade).
	RuntimeInitialized SandboxConditionType = "RuntimeInitialized"

	// SandboxConditionRecycling tracks recycling progress.
	SandboxConditionRecycling SandboxConditionType = "Recycling"

	// SandboxConditionProbeValid indicates whether the probe configurations in
	// Spec.Probes are valid. Invalid probes are skipped (not injected into the Pod)
	// and the condition is set to False with the validation error details.
	SandboxConditionProbeValid SandboxConditionType = "ProbeValid"
)

const (
	// ProbeConditionPrefix is the prefix for probe Conditions written to
	// SandboxStatus.Conditions. The full type is "agents.kruise.io/<probe-name>".
	ProbeConditionPrefix = "agents.kruise.io/"

	// Probe reason constants (written to Pod/Sandbox Conditions by the probe executor).
	ProbeReasonSucceeded = "Succeeded"
	ProbeReasonTimeout   = "Timeout"
	ProbeReasonError     = "Error"
	ProbeReasonUnhealthy = "Unhealthy"
	ProbeReasonPending   = "Pending"

	// Auto-pause decision reason constants (used for logging and events).
	PauseReasonScheduledResume = "ScheduledResume"
	PauseReasonProbePaused     = "ProbePaused"
	PauseReasonAgentActive     = "AgentActive"
	PauseReasonInactivePending = "InactivePending"
	PauseReasonProbeFailed     = "ProbeFailed"
	PauseReasonProbeUnhealthy  = "ProbeUnhealthy"

	// Schedule reason constants (written to SandboxStatus.Schedules).
	ScheduleReasonProbedIdle     = "probedIdle"
	ScheduleReasonProbedSchedule = "probedSchedule"
)

const (
	// SandboxConditionReady Reason
	SandboxReadyReasonPodReady             = "PodReady"
	SandboxReadyReasonInplaceUpdating      = "InplaceUpdating"
	SandboxReadyReasonUpgrading            = "Upgrading"
	SandboxReadyReasonStartContainerFailed = "StartContainerFailed"

	// SandboxConditionInplaceUpdate Reason
	SandboxInplaceUpdateReasonInplaceUpdating = "InplaceUpdating"
	SandboxInplaceUpdateReasonSucceeded       = "Succeeded"
	SandboxInplaceUpdateReasonFailed          = "Failed"

	// SandboxConditionUpgrading Reason
	SandboxUpgradingReasonPreUpgrade        = "PreUpgrade"
	SandboxUpgradingReasonUpgradePod        = "UpgradePod"
	SandboxUpgradingReasonPostUpgrade       = "PostUpgrade"
	SandboxUpgradingReasonPreUpgradeFailed  = "PreUpgradeFailed"
	SandboxUpgradingReasonPostUpgradeFailed = "PostUpgradeFailed"
	SandboxUpgradingReasonSucceeded         = "Succeeded"
	SandboxUpgradingReasonUpgradePodFailed  = "UpgradePodFailed"

	// SandboxConditionPaused Reason
	SandboxPausedReasonPausing             = "Pausing"
	SandboxPausedReasonImageChanged        = "ImageChanged"
	SandboxPausedReasonCheckpointCreating  = "CheckpointCreating"
	SandboxPausedReasonCheckpointSucceeded = "CheckpointSucceeded"
	SandboxPausedReasonCheckpointFailed    = "CheckpointFailed"
	SandboxPausedReasonSetPause            = "SetPause"
	SandboxPausedReasonPausedSucceed       = "PauseSucceed"
	SandboxPausedReasonDeletePod           = "DeletePod"

	// SandboxConditionResume Reason
	SandboxResumeReasonCreatePod = "CreatePod"
	SandboxResumeReasonResumePod = "ResumePod"

	// SandboxConditionRuntimeInit Reason
	SandboxConditionRuntimeInitReasonPending   = "Pending"
	SandboxConditionRuntimeInitReasonSucceeded = "Succeeded"
	SandboxConditionRuntimeInitReasonFailed    = "Failed"

	// SandboxConditionRecycling Reason
	SandboxRecyclingReasonStarted   = "RecyclingStarted"
	SandboxRecyclingReasonCompleted = "RecyclingCompleted"
	SandboxRecyclingReasonSucceeded = "RecyclingSucceeded"
	SandboxRecyclingReasonFailed    = "RecyclingFailed"
	SandboxRecyclingReasonTimeout   = "RecyclingTimeout"
	SandboxRecyclingReasonRejected  = "RecyclingRejected"

	// SandboxConditionProbeValid Reason
	SandboxProbeValidReasonValidationPassed = "ValidationPassed"
	SandboxProbeValidReasonValidationFailed = "ValidationFailed"
)

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=sandboxes,shortName={sbx},singular=sandbox
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Claimed",type="string",JSONPath=".metadata.labels.agents\\.kruise\\.io/sandbox-claimed"
// +kubebuilder:printcolumn:name="shutdown_time",type="string",JSONPath=".spec.shutdownTime"
// +kubebuilder:printcolumn:name="pause_time",type="string",JSONPath=".spec.pauseTime"
// +kubebuilder:printcolumn:name="RecycledCount",type="integer",JSONPath=".status.recycledCount"
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
