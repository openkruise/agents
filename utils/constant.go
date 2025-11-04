package utils

const (
	PodAnnotationAcsInstanceId = "alibabacloud.com/instance-id"

	PodAnnotationPause = "ops.alibabacloud.com/pause"

	PodAnnotationEnablePaused = "ops.alibabacloud.com/pause-enabled"

	PodAnnotationReserveInstance = "alibabacloud.com/reserve-instance"

	PodConditionContainersPaused = "ContainersPaused"

	// SandboxAnnotationEnableVKDeleteInstance 用于标识是否需要 VK 来处理 sandbox deletion 事件
	SandboxAnnotationEnableVKDeleteInstance = "alibabacloud.com/enable-vk-delete-instance"
	// SandboxFinalizer is sandbox finalizer
	SandboxFinalizer = "agents.kruise.io/sandbox"
)
