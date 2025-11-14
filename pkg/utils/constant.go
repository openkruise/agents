package utils

const (
	PodAnnotationAcsInstanceId = "alibabacloud.com/instance-id"

	PodAnnotationSandboxPause = "ops.alibabacloud.com/sandbox-pause"

	PodAnnotationEnablePaused = "ops.alibabacloud.com/pause-enabled"

	PodAnnotationReserveInstance = "ops.alibabacloud.com/reserve-instance"

	PodAnnotationSourcePodUID          = "ops.alibabacloud.com/source-pod-uid"
	PodAnnotationDeleteOnPaused        = "ops.alibabacloud.com/auto-delete-on-paused"
	PodAnnotationRecoverFromInstanceID = "ops.alibabacloud.com/recover-from-instance-id"

	PodConditionContainersPaused  = "ContainersPaused"
	PodConditionContainersResumed = "ContainersResumed"

	// SandboxAnnotationEnableVKDeleteInstance 用于标识是否需要 VK 来处理 sandbox deletion 事件
	SandboxAnnotationEnableVKDeleteInstance = "alibabacloud.com/enable-vk-delete-instance"
	// SandboxFinalizer is sandbox finalizer
	SandboxFinalizer = "agents.kruise.io/sandbox"
	// PodLabelEnableAutoCreateSandbox is used to select pods that will be processed by the bypass-sandbox webhook
	PodLabelEnableAutoCreateSandbox = "ops.alibabacloud.com/enable-auto-create-sandbox"
	// SandboxAnnotationDisablePodCreation 表示禁止 Sandbox 管理 Pod 的生命周期，即 Sandbox 不再会创建与删除 Pod，而只做相应的更新操作。
	SandboxAnnotationDisablePodCreation = "agents.kruise.io/disable-pod-creation"
	// SandboxAnnotationDisablePodDeletion 表示禁止 Sandbox 管理 Pod 的生命周期，即 Sandbox 不再会创建与删除 Pod，而只做相应的更新操作。
	SandboxAnnotationDisablePodDeletion = "agents.kruise.io/disable-pod-deletion"
	// PodAnnotationCreatedBy 用于标识 Pod 来源：被 Sandbox 控制器创建或外部创建（旁路 Sandbox 语法糖）
	PodAnnotationCreatedBy = "agents.kruise.io/created-by"
	// PodAnnotationRecreating 用于标识 Pod 是一个正在重建的唤醒 Pod
	PodAnnotationRecreating = "agents.kruise.io/recreating"
)

const (
	True  = "true"
	False = "false"

	CreatedByExternal = "external"
	CreatedBySandbox  = "sandbox"
)

const DebugLogLevel = 5
