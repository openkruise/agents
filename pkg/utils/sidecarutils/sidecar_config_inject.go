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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/webhookutils"
)

func IsRuntimeEnabled(sandbox *agentsv1alpha1.Sandbox, runtimeName string) bool {
	for _, runtime := range sandbox.Spec.Runtimes {
		if runtime.Name == runtimeName {
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
			logger.V(utils.DebugLogLevel).Info("injection configuration not found, skip injection")
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
		return sidecarConfig, fmt.Errorf("injection template %s not found", configKey)
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

func applyInjectionTemplate(pod *corev1.Pod, config SidecarInjectConfig) {
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	for k, v := range config.Labels {
		// not override user-defined labels
		if _, exists := pod.Labels[k]; !exists {
			pod.Labels[k] = v
		}
	}

	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	for k, v := range config.Annotations {
		// not override user-defined annotations
		if _, exists := pod.Annotations[k]; !exists {
			pod.Annotations[k] = v
		}
	}

	for _, ic := range config.InitContainers {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, ic)
	}

	for _, c := range config.Containers {
		pod.Spec.Containers = append(pod.Spec.Containers, c)
	}

	for _, v := range config.Volumes {
		pod.Spec.Volumes = append(pod.Spec.Volumes, v)
	}
}

// checkInjectionConflicts checks if injection config conflicts with the pod. Only configuration below are checked:
// 1. Containers
// 2. Init containers
// 3. Volumes
// Annotations and Labels are not checked and user-defined values may override the injected values.
func checkInjectionConflicts(pod *corev1.Pod, config SidecarInjectConfig) error {
	for _, ic := range config.InitContainers {
		if conflicted := utils.FindContainer(ic.Name, pod.Spec.InitContainers); conflicted != nil {
			return fmt.Errorf("inject conflicting with init container: %s", ic.Name)
		}
	}

	for _, c := range config.Containers {
		if conflicted := utils.FindContainer(c.Name, pod.Spec.Containers); conflicted != nil {
			return fmt.Errorf("inject conflicting with container: %s", c.Name)
		}
	}

	for _, v := range config.Volumes {
		if findVolumeByName(pod.Spec.Volumes, v.Name) {
			return fmt.Errorf("inject conflicting with volume: %s", v.Name)
		}
	}

	return nil
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

	if configHasValidPostStart {
		if mainContainer.Lifecycle == nil {
			mainContainer.Lifecycle = &corev1.Lifecycle{}
		}
		if mainContainerHasValidPostStart {
			// The user has a postStart hook. Merge using "--" separator so that
			// envd-run.sh executes the user's command after its own initialisation.
			mergedCmd := mergePostStartExecCommands(config.MainContainer.Lifecycle.PostStart, mainContainer.Lifecycle.PostStart)
			if mergedCmd != nil {
				log.V(utils.DebugLogLevel).Info("merging user's postStart command into injected script",
					"userCommand", mainContainer.Lifecycle.PostStart.Exec.Command,
					"injectedCommand", config.MainContainer.Lifecycle.PostStart.Exec.Command)
				mainContainer.Lifecycle.PostStart = &corev1.LifecycleHandler{
					Exec: &corev1.ExecAction{Command: mergedCmd},
				}
			} else {
				// Cannot merge non-Exec handlers (e.g. HTTPGet/TCPSocket); keep the
				// existing hook and skip injection of the injected postStart.
				log.Error(nil, "cannot merge non-Exec postStart hooks, keeping existing hook",
					"existingHook", mainContainer.Lifecycle.PostStart,
					"injectedHook", config.MainContainer.Lifecycle.PostStart)
			}
		} else {
			// Main container doesn't have a valid postStart; apply the config directly.
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

// mergePostStartExecCommands merges the injected postStart Exec command with the user's
// postStart Exec command using the "--" separator pattern. Everything after "--" is passed
// to envd-run.sh as individual arguments representing the user command, executed directly
// without shell interpretation. This follows the kubectl exec convention.
//
// Precondition: the caller has already verified that both the injected config and the main
// container have valid postStart hooks (configHasValidPostStart && mainContainerHasValidPostStart),
// so neither handler is nil and both have a non-nil action. Consequently, the nil checks below
// are defensive; the only realistic failure mode is a non-Exec handler, which returns nil.
// Returns nil if either handler is non-Exec (cannot be merged).
func mergePostStartExecCommands(injected, user *corev1.LifecycleHandler) []string {
	if injected == nil || injected.Exec == nil || user == nil || user.Exec == nil {
		return nil
	}
	// If the user's command is empty, there is nothing to append.
	if len(user.Exec.Command) == 0 {
		return injected.Exec.Command
	}
	// Use "--" separator following the kubectl exec pattern. Everything after "--"
	// is passed as individual arguments to envd-run.sh which executes the command
	// directly without shell interpretation.
	merged := make([]string, 0, len(injected.Exec.Command)+1+len(user.Exec.Command))
	merged = append(merged, injected.Exec.Command...)
	merged = append(merged, "--")
	merged = append(merged, user.Exec.Command...)
	return merged
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
