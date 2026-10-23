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
	"context"
	"maps"

	corev1 "k8s.io/api/core/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

const (
	KEY_CSI_INJECTION_CONFIG           = "csi"
	KEY_TRAFFIC_PROXY_INJECTION_CONFIG = "traffic-proxy"
	KEY_RUNTIME_INJECTION_CONFIG       = "agent-runtime"

	// SandboxInjectionConfigName is the default ConfigMap for injection templates.
	SandboxInjectionConfigName = "sandbox-injection-config"
	// SandboxInjectionUserConfigName is an optional ConfigMap whose matching keys override the defaults;
	// keys not present here continue to use SandboxInjectionConfigName.
	SandboxInjectionUserConfigName = "sandbox-injection-config-user"
)

// injectionConfigs preserves the two sources of sidecar injection templates.
// A nil source means its ConfigMap was not found; an empty, non-nil source
// means its ConfigMap was found but contained no data. Loaded maps are copied
// from the ConfigMaps, so callers do not share the client's cached data.
type injectionConfigs struct {
	Default map[string]string
	User    map[string]string
}

// Merged returns a new map in which user-provided keys override only matching
// defaults. An explicitly empty user value still overrides the default value.
// The result is always non-nil and does not alias either source map.
func (c injectionConfigs) Merged() map[string]string {
	merged := make(map[string]string, len(c.Default)+len(c.User))
	maps.Copy(merged, c.Default)
	maps.Copy(merged, c.User)
	return merged
}

// InjectionOptions supplies context for a single injection call. The zero
// value preserves the baseline behavior when no extension is registered.
type InjectionOptions struct {
	// RenderFromTemplate is true for first creation and template upgrades,
	// but false for resume even when resume creates a new instance.
	RenderFromTemplate bool
}

// RuntimeInjectionHook optionally handles a subset of the declared runtimes.
// It defaults to nil; deployment-specific implementations register during
// initialization, before reconciliation starts, and must not replace it while
// workers run. defaultConfigs and userConfigs are the loaded sources: a nil map
// means its ConfigMap was not found, an empty map means it contained no data,
// and user keys override matching defaults. The hook may mutate pod but must
// treat sandbox and both configuration maps as read-only. It returns only
// declared runtime names; duplicates are harmless.
// Other runtimes, probe rewriting, and CA injection remain baseline-owned.
// Errors abort injection and the caller must discard the partially built pod.
var RuntimeInjectionHook func(ctx context.Context, sandbox *agentsv1alpha1.Sandbox, pod *corev1.Pod, defaultConfigs, userConfigs map[string]string, options InjectionOptions) ([]string, error)

type SidecarInjectConfig struct {
	// Configuration injection for the main container (by convention, one container is designated as the main container)
	// Injection configuration items for business-specified main containers, such as volumeMount, environment variables, etc. Format: corev1.Container
	MainContainer corev1.Container `json:"mainContainer"`
	// Support injection for multiple independent sidecar containers; CSI container plugins are all injected from this
	Sidecars []corev1.Container `json:"csiSidecar"`
	// Support injection for volume mount configurations
	Volumes []corev1.Volume `json:"volume"`

	// Annotations to be added to the pod.
	Annotations map[string]string `json:"annotations"`
	// Labels to be added to the pod.
	Labels map[string]string `json:"labels"`
	// InitContainers to be appended to the pod spec.
	InitContainers []corev1.Container `json:"initContainers"`
	// Containers to be appended to the pod spec.
	Containers []corev1.Container `json:"containers"`
}
