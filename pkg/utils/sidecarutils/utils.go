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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	trafficproxy "github.com/openkruise/agents/pkg/utils/sidecarutils/traffic-proxy"
)

func InjectSandboxRuntimes(ctx context.Context, sandbox *agentsv1alpha1.Sandbox, pod *corev1.Pod, cli client.Client) error {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(sandbox))
	if len(sandbox.Spec.Runtimes) == 0 {
		return nil
	}

	// fetch the custom injection configuration
	config, err := fetchInjectionConfiguration(ctx, cli)
	if err != nil {
		logger.Error(err, "failed to fetch injection configuration")
		return err
	}
	return doSidecarInjection(ctx, sandbox, pod, config)
}

func doSidecarInjection(ctx context.Context, sandbox *agentsv1alpha1.Sandbox, pod *corev1.Pod, injectConfigMap map[string]string) error {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(sandbox))

	injectedRuntimes := sets.NewString()
	for _, runtime := range sandbox.Spec.Runtimes {
		if injectedRuntimes.Has(runtime.Name) {
			logger.V(5).Info("skipping duplicate runtime, already processed", "runtime", runtime.Name)
			continue
		}
		injectedRuntimes.Insert(runtime.Name)

		runtimeInjectConfig, err := parseInjectConfig(ctx, runtime.Name, injectConfigMap)
		if err != nil {
			logger.Error(err, "failed to parse runtime injection configuration")
			return err
		}
		switch runtime.Name {
		case agentsv1alpha1.RuntimeConfigForInjectAgentRuntime:
			if !isContainersExists(pod.Spec.InitContainers, runtimeInjectConfig.Sidecars) && !isContainersExists(pod.Spec.Containers, runtimeInjectConfig.Sidecars) {
				setAgentRuntimeContainer(ctx, &pod.Spec, runtimeInjectConfig)
			}
		case agentsv1alpha1.RuntimeConfigForInjectCsiMount:
			if !isContainersExists(pod.Spec.InitContainers, runtimeInjectConfig.Sidecars) && !isContainersExists(pod.Spec.Containers, runtimeInjectConfig.Sidecars) {
				setCSIMountContainer(ctx, &pod.Spec, runtimeInjectConfig)
			}
		default:
			logger.V(5).Info("injecting runtime", "name", runtime)
			// not mute the conflicts
			if conflictErr := checkInjectionConflicts(pod, runtimeInjectConfig); conflictErr != nil {
				return fmt.Errorf("failed to inject runtime %s: %v", runtime, conflictErr)
			}
			applyInjectionTemplate(pod, runtimeInjectConfig)
		}
	}

	// Enable health probes rewrite if needed.
	if injectedRuntimes.Has(agentsv1alpha1.RuntimeConfigForInjectTrafficProxy) {
		if err := trafficproxy.ApplyHealthProbeRewrite(pod); err != nil {
			logger.Error(err, "failed to apply health probe rewrite")
			return err
		}
	}

	return nil
}
