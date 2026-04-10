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
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/utils/webhookutils"
)

func enableInjectCsiMountConfig(sandbox *agentsv1alpha1.Sandbox) bool {
	for _, runtime := range sandbox.Spec.Runtimes {
		if runtime.Name == agentsv1alpha1.RuntimeConfigForInjectCsiMount {
			return true
		}
	}
	return false
}

func enableInjectAgentRuntimeConfig(sandbox *agentsv1alpha1.Sandbox) bool {
	for _, runtime := range sandbox.Spec.Runtimes {
		if runtime.Name == agentsv1alpha1.RuntimeConfigForInjectAgentRuntime {
			return true
		}
	}
	return false
}

func fetchInjectionConfiguration(ctx context.Context, cli client.Client) (map[string]string, error) {
	logger := logf.FromContext(ctx)
	config := &corev1.ConfigMap{}
	err := cli.Get(ctx, types.NamespacedName{
		Namespace: webhookutils.GetNamespace(), // Todo considering the security concern and rbac issue
		Name:      SandboxInjectionConfigName,
	}, config)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.V(consts.DebugLogLevel).Info("injection configuration not found, skip injection")
			return map[string]string{}, nil
		}
		return map[string]string{}, err
	}
	return config.Data, nil
}

func parseInjectConfig(ctx context.Context, configKey string, configRaw map[string]string) (SidecarInjectConfig, error) {
	log := logf.FromContext(ctx)
	sidecarConfig := SidecarInjectConfig{}

	configValue, exists := configRaw[configKey]
	if !exists || configValue == "" {
		log.V(5).Info("config key not found or empty, using default configuration")
		return sidecarConfig, nil
	}

	err := json.Unmarshal([]byte(configRaw[configKey]), &sidecarConfig)
	if err != nil {
		log.Error(err, "failed to unmarshal sidecar config", "configKey", configKey)
		return sidecarConfig, err
	}
	return sidecarConfig, nil
}

// setCSIMountContainer injects CSI mount configurations into the SandboxTemplate's pod spec.
// It configures the main container (first container in the spec) with CSI sidecar settings,
// appends additional CSI sidecar containers, and mounts shared volumes.
// Volumes are only added if they don't already exist in the template.
func setCSIMountContainer(ctx context.Context, podSpec *corev1.PodSpec, config SidecarInjectConfig) {
	log := logf.FromContext(ctx)

	// set main container, the first container is the main container
	if len(podSpec.Containers) == 0 {
		log.Info("no container found in sidecar template")
		return
	}

	mainContainer := &podSpec.Containers[0]
	setMainContainerWhenInjectCSISidecar(mainContainer, config)

	// set csi sidecars into init containers
	if podSpec.InitContainers == nil {
		podSpec.InitContainers = make([]corev1.Container, 0, 1)
	}
	for _, csiSidecar := range config.Sidecars {
		podSpec.InitContainers = append(podSpec.InitContainers, csiSidecar)
	}

	// set share volume
	if len(config.Volumes) > 0 {
		if podSpec.Volumes == nil {
			podSpec.Volumes = make([]corev1.Volume, 0, len(config.Volumes))
		}
		for _, vol := range config.Volumes {
			if findVolumeByName(podSpec.Volumes, vol.Name) {
				continue
			}
			podSpec.Volumes = append(podSpec.Volumes, vol)
		}
	}
}

// setMainContainerWhenInjectCSISidecar configures the main container with environment variables and volume mounts from the CSI sidecar configuration.
// It appends environment variables and volume mounts to the main container, skipping any that already exist (matched by name) to avoid duplicates.
func setMainContainerWhenInjectCSISidecar(mainContainer *corev1.Container, config SidecarInjectConfig) {
	// append some envs in main container when processing csi mount
	if mainContainer.Env == nil {
		mainContainer.Env = make([]corev1.EnvVar, 0, 1)
	}
	for _, env := range config.MainContainer.Env {
		if findEnvByName(mainContainer.Env, env.Name) {
			continue
		}
		mainContainer.Env = append(mainContainer.Env, env)
	}

	// append some volumeMounts config in main container
	if config.MainContainer.VolumeMounts != nil {
		if mainContainer.VolumeMounts == nil {
			mainContainer.VolumeMounts = make([]corev1.VolumeMount, 0, len(config.MainContainer.VolumeMounts))
		}
		for _, volMount := range config.MainContainer.VolumeMounts {
			if findVolumeMountByName(mainContainer.VolumeMounts, volMount.Name) {
				continue
			}
			mainContainer.VolumeMounts = append(mainContainer.VolumeMounts, volMount)
		}
	}
}

// setAgentRuntimeContainer injects agent runtime configurations into the SandboxTemplate's pod spec.
// It appends agent runtime containers as init containers and configures the main container (first container) with runtime settings.
// The init containers run before the main containers to prepare the runtime environment.
func setAgentRuntimeContainer(ctx context.Context, podSpec *corev1.PodSpec, config SidecarInjectConfig) {
	log := logf.FromContext(ctx)

	// append init agent runtime container
	if podSpec.InitContainers == nil {
		podSpec.InitContainers = make([]corev1.Container, 0, 1)
	}
	podSpec.InitContainers = append(podSpec.InitContainers, config.Sidecars...)

	if len(podSpec.Containers) == 0 {
		log.Info("no container found in sidecar template for agent runtime")
		return
	}
	mainContainer := &podSpec.Containers[0]
	setMainContainerConfigWhenInjectRuntimeSidecar(ctx, mainContainer, config)

	podSpec.Volumes = append(podSpec.Volumes, config.Volumes...)
}

func setMainContainerConfigWhenInjectRuntimeSidecar(ctx context.Context, mainContainer *corev1.Container, config SidecarInjectConfig) {
	log := logf.FromContext(ctx)

	// Check if main container already has a valid postStart hook (with actual handler)
	mainContainerHasValidPostStart := mainContainer.Lifecycle != nil &&
		mainContainer.Lifecycle.PostStart != nil &&
		hasValidLifecycleHandler(mainContainer.Lifecycle.PostStart)

	configHasValidPostStart := config.MainContainer.Lifecycle != nil &&
		config.MainContainer.Lifecycle.PostStart != nil &&
		hasValidLifecycleHandler(config.MainContainer.Lifecycle.PostStart)

	if mainContainerHasValidPostStart {
		if configHasValidPostStart {
			log.V(consts.DebugLogLevel).Info("conflicting postStart hooks detected, main container already has a postStart hook defined",
				"existingHook", mainContainer.Lifecycle.PostStart,
				"injectedHook", config.MainContainer.Lifecycle.PostStart)
		}
	} else {
		// Main container doesn't have valid postStart, apply config if available
		if configHasValidPostStart {
			// set main container lifecycle
			if mainContainer.Lifecycle == nil {
				mainContainer.Lifecycle = &corev1.Lifecycle{}
			}
			mainContainer.Lifecycle.PostStart = config.MainContainer.Lifecycle.PostStart
		}
	}

	// set main container env
	if mainContainer.Env == nil {
		mainContainer.Env = make([]corev1.EnvVar, 0, len(config.MainContainer.Env))
	}
	mainContainer.Env = append(mainContainer.Env, config.MainContainer.Env...)

	// set main container volumeMounts
	if mainContainer.VolumeMounts == nil {
		mainContainer.VolumeMounts = make([]corev1.VolumeMount, 0, len(config.MainContainer.VolumeMounts))
	}
	mainContainer.VolumeMounts = append(mainContainer.VolumeMounts, config.MainContainer.VolumeMounts...)
}

func findVolumeMountByName(volumeMounts []corev1.VolumeMount, name string) bool {
	for _, volumeMount := range volumeMounts {
		if volumeMount.Name == name {
			return true
		}
	}
	return false
}

func findVolumeByName(volumes []corev1.Volume, name string) bool {
	for _, volume := range volumes {
		if volume.Name == name {
			return true
		}
	}
	return false
}

func findEnvByName(envs []corev1.EnvVar, name string) bool {
	for _, env := range envs {
		if env.Name == name {
			return true
		}
	}
	return false
}

// isContainersExists checks if any container name in injectContainers
// already exists in podContainers.
// Returns true if any inject container name already exists in podContainers (conflict),
// Returns false if no conflict (all inject container names are unique).
func isContainersExists(podContainers []corev1.Container, injectContainers []corev1.Container) bool {
	existingNames := sets.NewString()
	for _, c := range podContainers {
		existingNames.Insert(c.Name)
	}
	for _, c := range injectContainers {
		if existingNames.Has(c.Name) {
			return true
		}
	}
	return false
}

// hasValidLifecycleHandler checks if the lifecycle handler has at least one valid action defined.
// A valid handler must have at least one of: Exec, HTTPGet, or TCPSocket.
// Returns false if the handler is nil or all actions are nil (empty handler).
func hasValidLifecycleHandler(handler *corev1.LifecycleHandler) bool {
	if handler == nil {
		return false
	}
	return handler.Exec != nil || handler.HTTPGet != nil || handler.TCPSocket != nil
}
