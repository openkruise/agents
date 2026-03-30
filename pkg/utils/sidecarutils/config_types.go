package sidecarutils

import (
	corev1 "k8s.io/api/core/v1"
)

const (
	KEY_CSI_INJECTION_CONFIG     = "csi-config"
	KEY_RUNTIME_INJECTION_CONFIG = "agent-runtime-config"
	SandboxInjectionConfigName   = "sandbox-injection-config"
)

type SidecarInjectConfig struct {
	// Configuration injection for the main container (by convention, one container is designated as the main container)
	// Injection configuration items for business-specified main containers, such as volumeMount, environment variables, etc. Format: corev1.Container
	MainContainer corev1.Container `json:"mainContainer"`
	// Support injection for multiple independent sidecar containers; CSI container plugins are all injected from this
	Sidecars []corev1.Container `json:"csiSidecar"`
	// Support injection for volume mount configurations
	Volumes []corev1.Volume `json:"volume"`
}
