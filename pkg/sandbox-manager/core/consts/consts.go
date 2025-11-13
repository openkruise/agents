package consts

const (
	InfraK8S       = "k8s"
	InfraACS       = "acs"
	InfraSandboxCR = "sandbox-cr"
	InfraMicroVM   = "micro-vm"
)

const (
	DefaultMinPoolSize       = 5
	DefaultMaxPoolSizeFactor = 2
	InternalPrefix           = "sandbox.alibabacloud.com/"

	LabelSandboxPool  = InternalPrefix + "sandbox-pool"
	LabelSandboxState = InternalPrefix + "sandbox-state"
	LabelSandboxID    = InternalPrefix + "sandbox-id"
	LabelTemplateHash = InternalPrefix + "template-hash"

	AnnotationLock  = InternalPrefix + "lock"
	AnnotationOwner = InternalPrefix + "owner"

	AnnotationPodDeletionCost = "controller.kubernetes.io/pod-deletion-cost"
	AnnotationACSPause        = "ops.alibabacloud.com/pause"

	LabelACS = "alibabacloud.com/acs"
)

const (
	SandboxStatePending = "pending"
	SandboxStateRunning = "running"
	SandboxStatePaused  = "paused"
	SandboxStateKilling = "killing"
)

const (
	ExtProcPort = 9002
)

const DebugLogLevel = 5
