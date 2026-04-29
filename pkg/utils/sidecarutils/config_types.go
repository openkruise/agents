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

package sidecarutils

import (
	corev1 "k8s.io/api/core/v1"
)

const (
	KEY_CSI_INJECTION_CONFIG     = "csi"
	KEY_RUNTIME_INJECTION_CONFIG = "agent-runtime"
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
