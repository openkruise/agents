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
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/identity"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
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

// fixedOrderRuntimes defines the injection order for built-in runtimes.
// Preserve historical injection order: agent-runtime first, then csi-mount,
// to keep init container ordering stable for pre-existing pods regardless of
// the declaration order in sandbox.Spec.Runtimes.
var fixedOrderRuntimes = []string{
	agentsv1alpha1.RuntimeConfigForInjectAgentRuntime,
	agentsv1alpha1.RuntimeConfigForInjectCsiMount,
}

func doSidecarInjection(ctx context.Context, sandbox *agentsv1alpha1.Sandbox, pod *corev1.Pod, injectConfigMap map[string]string) error {
	logger := logf.FromContext(ctx)

	// Build a quick-lookup set of declared runtimes for the fixed-order phase.
	declared := sets.NewString()
	for _, r := range sandbox.Spec.Runtimes {
		declared.Insert(r.Name)
	}

	injectedRuntimes := sets.NewString()

	// Phase 1: inject built-in runtimes in a deterministic fixed order so that
	// the resulting InitContainers sequence is stable across spec permutations.
	for _, runtime := range fixedOrderRuntimes {
		if !declared.Has(runtime) {
			continue
		}

		runtimeInjectConfig, err := parseInjectConfig(ctx, runtime, injectConfigMap)
		if err != nil {
			logger.Error(err, "failed to parse runtime injection configuration")
			return err
		}
		logger.V(5).Info("injecting runtime", "name", runtime)
		injectedRuntimes.Insert(runtime)
		switch runtime {
		case agentsv1alpha1.RuntimeConfigForInjectAgentRuntime:
			if !isContainersExists(pod.Spec.InitContainers, runtimeInjectConfig.Sidecars) && !isContainersExists(pod.Spec.Containers, runtimeInjectConfig.Sidecars) {
				setAgentRuntimeContainer(ctx, &pod.Spec, runtimeInjectConfig)
			}
		case agentsv1alpha1.RuntimeConfigForInjectCsiMount:
			if !isContainersExists(pod.Spec.InitContainers, runtimeInjectConfig.Sidecars) && !isContainersExists(pod.Spec.Containers, runtimeInjectConfig.Sidecars) {
				setCSIMountContainer(ctx, &pod.Spec, runtimeInjectConfig)
			}
		}
	}

	// Phase 2: inject remaining runtimes in their declaration order. New runtime
	// types (e.g. traffic-proxy) fall through to the generic handler.
	for _, r := range sandbox.Spec.Runtimes {
		runtime := r.Name
		if injectedRuntimes.Has(runtime) {
			continue
		}

		runtimeInjectConfig, err := parseInjectConfig(ctx, runtime, injectConfigMap)
		if err != nil {
			logger.Error(err, "failed to parse runtime injection configuration")
			return err
		}
		logger.V(5).Info("injecting runtime", "name", runtime)
		if conflictErr := checkInjectionConflicts(pod, runtimeInjectConfig); conflictErr != nil {
			return fmt.Errorf("failed to inject runtime %s: %v", runtime, conflictErr)
		}
		injectedRuntimes.Insert(runtime)
		applyInjectionTemplate(pod, runtimeInjectConfig)
	}

	// Enable health probes rewrite if needed.
	if injectedRuntimes.Has(agentsv1alpha1.RuntimeConfigForInjectTrafficProxy) {
		if err := trafficproxy.ApplyHealthProbeRewrite(pod); err != nil {
			logger.Error(err, "failed to apply health probe rewrite")
			return err
		}
	}

	// Inject every enabled CA bundle as Volume + VolumeMount + EnvVar entries.
	// SecurityIdentityProviderGate is the cluster-level kill switch; per-spec
	// EnabledFor predicates (bound via identity.BindCAEnabledFor at controller
	// startup) decide which specs apply to this sandbox. Runtime-level gating
	// (e.g. traffic-proxy) lives exclusively inside those predicates to avoid
	// drift with the caller side.
	if utilfeature.DefaultFeatureGate.Enabled(features.SecurityIdentityProviderGate) {
		identity.InjectAllCAVolumes(ctx, sandbox, pod)
		identity.InjectAllCAIntoContainers(ctx, sandbox, pod)
	}

	return nil
}
