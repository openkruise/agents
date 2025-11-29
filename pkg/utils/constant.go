package utils

const (
	// SandboxFinalizer is sandbox finalizer
	SandboxFinalizer = "agents.kruise.io/sandbox"
	// PodAnnotationCreatedBy 用于标识 Pod 来源：被 Sandbox 控制器创建或外部创建（旁路 Sandbox 语法糖）
	PodAnnotationCreatedBy = "agents.kruise.io/created-by"
)

const (
	True  = "true"
	False = "false"

	CreatedByExternal = "external"
	CreatedBySandbox  = "sandbox"
)

const DebugLogLevel = 5
