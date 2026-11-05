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
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/utils"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
	trafficproxy "github.com/openkruise/agents/pkg/utils/sidecarutils/traffic-proxy"
)

const (
	// KeyRuntimeAIOInjectionConfig is the template of the all-in-one profile,
	// in which one runtimed container runs the built-in runtimes as services.
	KeyRuntimeAIOInjectionConfig = "runtime-aio"
	// KeyRuntimeAIODisabled stops selecting the all-in-one profile when its
	// merged value is "true". A resume whose checkpoint pod template delta was
	// taken from an all-in-one pod keeps that profile.
	KeyRuntimeAIODisabled = "runtime-aio-disabled"

	// AnnotationRuntimeInjectionProfileReason tells on a pod why the legacy
	// profile was selected although the all-in-one profile is enabled.
	AnnotationRuntimeInjectionProfileReason = "agents.kruise.io/runtime-injection-profile-reason"
	// AnnotationRuntimedPlugins lists the runtimed services the all-in-one
	// container runs, which it reads through the downward API. The key keeps
	// its historical name while the value holds service names.
	AnnotationRuntimedPlugins = "agents.kruise.io/runtimed-plugins"

	// RuntimeInjectionProfileLegacy injects each built-in runtime from its own
	// template.
	RuntimeInjectionProfileLegacy = "legacy"
	// RuntimeInjectionProfileAllInOne injects the built-in runtimes as services
	// of one runtimed container.
	RuntimeInjectionProfileAllInOne = "all-in-one"

	// envRuntimedEnabledServices only exists on the all-in-one container and
	// selects the runtimed services to run.
	envRuntimedEnabledServices = "RUNTIMED_ENABLED_SERVICES"

	// runtimeAIOGateName names the controller-owned master switch. The
	// controller reads the gate and passes the decision into this package, which
	// must not depend on pkg/features for it.
	runtimeAIOGateName = "RuntimeAIO"

	// runtimedPluginsFieldPath is the downward API field the all-in-one
	// container must read its enabled services from. The injector writes the
	// services to AnnotationRuntimedPlugins.
	runtimedPluginsFieldPath = "metadata.annotations['" + AnnotationRuntimedPlugins + "']"
)

// ErrRuntimeAIOInjection reports a pod that must be injected with the
// all-in-one profile but cannot be. Such a pod is never injected with the
// legacy profile instead.
var ErrRuntimeAIOInjection = errors.New("all-in-one runtime injection failed")

// runtimeAIOServices lists the built-in runtimes the all-in-one container
// takes over, in service order, and the runtimed services that replace each of
// them.
var runtimeAIOServices = []struct {
	runtime  string
	services []string
}{
	{agentsv1alpha1.RuntimeConfigForInjectAgentRuntime, []string{"agent-runtime"}},
	{agentsv1alpha1.RuntimeConfigForInjectCsiMount, []string{"csi", "csi-agent"}},
	{agentsv1alpha1.RuntimeConfigForInjectTrafficProxy, []string{"traffic-proxy"}},
}

// RuntimeInjectionOptions carries the controller-owned decisions for runtime
// injection. The zero value keeps the historical behavior: the profile is
// selected from the configuration and the all-in-one profile is unavailable.
type RuntimeInjectionOptions struct {
	// AIOEnabled is the controller's decision on the RuntimeAIO feature gate.
	// This package must not read the gate itself.
	AIOEnabled bool
	// ResumeProfile is the injection profile of the containers a checkpoint
	// delta is about to replay on the generated pod (see
	// RuntimeInjectionProfileOfDelta). It is empty for pods generated from the
	// current configuration, including first creations and resumes without a
	// checkpoint.
	ResumeProfile string
}

// InjectSandboxRuntimesWithOptions is the pod-creation entrypoint for sidecar
// injection. It injects the pod with opts.ResumeProfile when a resume already
// knows the profile its checkpointed containers were injected with, and
// selects one from the current configuration otherwise.
func InjectSandboxRuntimesWithOptions(ctx context.Context, sandbox *agentsv1alpha1.Sandbox, pod *corev1.Pod, cli client.Client, opts RuntimeInjectionOptions) error {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(sandbox))
	// These annotations describe the injection below; drop any copied into the
	// pod template, e.g. from a live pod.
	for _, key := range []string{AnnotationRuntimeInjectionProfileReason, AnnotationRuntimedPlugins} {
		delete(pod.Annotations, key)
	}
	// A resume keeping the all-in-one profile goes on even without a runtime to
	// inject, and fails rather than resuming the pod without its runtime.
	if opts.ResumeProfile == "" && len(sandbox.Spec.Runtimes) == 0 {
		return nil
	}

	configs, err := fetchInjectionConfiguration(ctx, cli)
	if err != nil {
		logger.Error(err, "failed to fetch injection configuration")
		return err
	}
	merged := configs.Merged()
	profile, reason := opts.ResumeProfile, ""
	if profile == "" {
		profile, reason = runtimeInjectionProfileFor(sandbox, pod, configs, merged, opts.AIOEnabled)
	}
	if profile == RuntimeInjectionProfileAllInOne {
		// The gate is the master switch. Injecting the legacy profile into a pod
		// whose checkpoint delta replays its all-in-one container would mix both
		// topologies, so the resume fails instead.
		if !opts.AIOEnabled {
			return fmt.Errorf("%w: resume keeps the %s profile but feature gate %s is disabled",
				ErrRuntimeAIOInjection, RuntimeInjectionProfileAllInOne, runtimeAIOGateName)
		}
		return injectRuntimeAIO(ctx, sandbox, pod, merged)
	}
	if reason != "" {
		logger.Info("selected the legacy runtime injection profile", "reason", reason)
		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string)
		}
		pod.Annotations[AnnotationRuntimeInjectionProfileReason] = reason
	}
	return doSidecarInjection(ctx, sandbox, pod, merged)
}

// IsRuntimeAIOContainer reports whether c is the all-in-one runtime container.
// It shares its name with the legacy agent-runtime container and is told apart
// by the service list it reads: a literal value, which the injector replaces,
// or the downward API field the injector writes the services to. Any other
// source cannot convey the requested services and is rejected.
func IsRuntimeAIOContainer(c *corev1.Container) bool {
	if c.Name != ContainerNameRuntimeAgent {
		return false
	}
	for i := range c.Env {
		if c.Env[i].Name != envRuntimedEnabledServices {
			continue
		}
		return isRuntimedServicesSource(&c.Env[i])
	}
	return false
}

// isRuntimedServicesSource reports whether env conveys the runtimed services:
// either a literal value or the downward API reference of
// AnnotationRuntimedPlugins.
func isRuntimedServicesSource(env *corev1.EnvVar) bool {
	if env.ValueFrom == nil {
		return true
	}
	ref := env.ValueFrom.FieldRef
	if ref == nil || ref.FieldPath != runtimedPluginsFieldPath {
		return false
	}
	return ref.APIVersion == "" || ref.APIVersion == "v1"
}

// RuntimeInjectionProfileOfDelta returns the injection profile of the pod a
// checkpoint pod template delta was taken from, judged by the agent-runtime
// container the delta carries and its RUNTIMED_ENABLED_SERVICES env. It
// returns "" when the delta carries no agent-runtime container.
func RuntimeInjectionProfileOfDelta(raw []byte) (string, error) {
	var delta struct {
		Spec struct {
			InitContainers []corev1.Container `json:"initContainers"`
			Containers     []corev1.Container `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &delta); err != nil {
		return "", err
	}
	runtimeContainer := utils.FindContainer(ContainerNameRuntimeAgent, delta.Spec.InitContainers)
	if runtimeContainer == nil {
		runtimeContainer = utils.FindContainer(ContainerNameRuntimeAgent, delta.Spec.Containers)
	}
	switch {
	case runtimeContainer == nil:
		return "", nil
	case IsRuntimeAIOContainer(runtimeContainer):
		return RuntimeInjectionProfileAllInOne, nil
	default:
		return RuntimeInjectionProfileLegacy, nil
	}
}

// runtimeInjectionProfileFor returns the profile to inject a newly generated
// pod with, selected from the current configuration. The reason explains a
// legacy selection made although the all-in-one profile is enabled.
func runtimeInjectionProfileFor(sandbox *agentsv1alpha1.Sandbox, pod *corev1.Pod,
	configs injectionConfigs, merged map[string]string, runtimeAIOEnabled bool) (profile, reason string) {
	if !runtimeAIOEnabled {
		return RuntimeInjectionProfileLegacy, ""
	}
	// A missing template is not a reason to fall back, see injectRuntimeAIO.
	if merged[KeyRuntimeAIODisabled] == "true" {
		return RuntimeInjectionProfileLegacy, ""
	}
	// The all-in-one container always runs the runtime service, so a sandbox
	// without agent-runtime cannot use the profile.
	if !IsRuntimeEnabled(sandbox, agentsv1alpha1.RuntimeConfigForInjectAgentRuntime) {
		return RuntimeInjectionProfileLegacy, ""
	}
	// The all-in-one profile would drop the user's own templates of the
	// built-in runtimes this sandbox uses.
	var overridden []string
	for _, s := range runtimeAIOServices {
		if _, ok := configs.User[s.runtime]; ok && IsRuntimeEnabled(sandbox, s.runtime) {
			overridden = append(overridden, s.runtime)
		}
	}
	if len(overridden) > 0 {
		return RuntimeInjectionProfileLegacy, fmt.Sprintf("user injection configuration overrides %s", strings.Join(overridden, ","))
	}
	// The legacy injection keeps a runtime container the pod template already
	// defines, while the all-in-one container would run beside it.
	for _, containers := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for i := range containers {
			if RuntimeContainerNames.Has(containers[i].Name) {
				return RuntimeInjectionProfileLegacy, fmt.Sprintf("pod template already defines container %s", containers[i].Name)
			}
		}
	}
	return RuntimeInjectionProfileAllInOne, ""
}

// injectRuntimeAIO injects the pod with the all-in-one profile: the built-in
// runtimes of the sandbox run as services of the runtime-aio container, while
// every other runtime keeps its own template.
func injectRuntimeAIO(ctx context.Context, sandbox *agentsv1alpha1.Sandbox, pod *corev1.Pod, configs map[string]string) error {
	if !IsRuntimeEnabled(sandbox, agentsv1alpha1.RuntimeConfigForInjectAgentRuntime) {
		return fmt.Errorf("%w: sandbox does not declare runtime %s", ErrRuntimeAIOInjection, agentsv1alpha1.RuntimeConfigForInjectAgentRuntime)
	}
	config, err := parseRuntimeAIOConfig(configs)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRuntimeAIOInjection, err)
	}
	if utils.FindContainer(ContainerNameRuntimeAgent, pod.Spec.InitContainers) != nil ||
		utils.FindContainer(ContainerNameRuntimeAgent, pod.Spec.Containers) != nil {
		return fmt.Errorf("%w: pod already defines container %s", ErrRuntimeAIOInjection, ContainerNameRuntimeAgent)
	}

	services := runtimedServices(sandbox)
	aioIndex := runtimeAIOContainerIndex(config.InitContainers)
	config.InitContainers[aioIndex].Env = setEnabledServicesEnv(config.InitContainers[aioIndex].Env, services)
	if len(pod.Spec.Containers) > 0 {
		setMainContainerConfigWhenInjectRuntimeSidecar(ctx, &pod.Spec.Containers[0], config)
	}
	// Volumes are deduplicated instead of conflict-checked: the template shares
	// volumes with the sandbox pod template, e.g. mount-root.
	for _, vol := range config.Volumes {
		if !findVolumeByName(pod.Spec.Volumes, vol.Name) {
			pod.Spec.Volumes = append(pod.Spec.Volumes, vol)
		}
	}
	// The rest of the template, including metadata and any extra init or
	// regular containers, is injected as is, like the legacy profile. Volumes
	// are already handled above, so clear them to avoid a duplicate append.
	config.Volumes = nil
	applyInjectionTemplate(pod, config)
	usesTrafficProxy := IsRuntimeEnabled(sandbox, agentsv1alpha1.RuntimeConfigForInjectTrafficProxy)
	pod.Annotations[AnnotationRuntimedPlugins] = services

	others := sandbox.DeepCopy()
	others.Spec.Runtimes = slices.DeleteFunc(others.Spec.Runtimes, func(r agentsv1alpha1.RuntimeConfig) bool {
		return takesOverRuntime(r.Name)
	})
	if len(others.Spec.Runtimes) > 0 {
		if err := doSidecarInjection(ctx, others, pod, configs); err != nil {
			return err
		}
	}

	// doSidecarInjection ends with steps that depend on the built-in runtimes,
	// which the copy above no longer declares, so they run here against the
	// whole sandbox. CA injection skips what the copy already got injected.
	if usesTrafficProxy {
		if err := trafficproxy.ApplyHealthProbeRewrite(pod); err != nil {
			return err
		}
	}
	if utilfeature.DefaultFeatureGate.Enabled(features.SecurityIdentityProviderGate) {
		identity.InjectAllCAVolumes(ctx, sandbox, pod)
		identity.InjectAllCAIntoContainers(ctx, sandbox, pod)
	}
	return nil
}

// parseRuntimeAIOConfig parses the runtime-aio template. It has the same shape
// as the legacy injection templates and must declare the all-in-one container
// as a native sidecar init container: one that reads the enabled services from
// a literal value or the runtimed plugins annotation and restarts Always.
func parseRuntimeAIOConfig(configs map[string]string) (SidecarInjectConfig, error) {
	var config SidecarInjectConfig
	raw := configs[KeyRuntimeAIOInjectionConfig]
	if raw == "" {
		return config, fmt.Errorf("injection template %s not found", KeyRuntimeAIOInjectionConfig)
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return config, fmt.Errorf("failed to parse injection template %s: %w", KeyRuntimeAIOInjectionConfig, err)
	}
	aioIndex := runtimeAIOContainerIndex(config.InitContainers)
	if aioIndex < 0 {
		return config, fmt.Errorf("injection template %s must define exactly one init container, %s reading %s from a literal value or the downward API field %s",
			KeyRuntimeAIOInjectionConfig, ContainerNameRuntimeAgent, envRuntimedEnabledServices, runtimedPluginsFieldPath)
	}
	if aio := config.InitContainers[aioIndex]; aio.RestartPolicy == nil || *aio.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		return config, fmt.Errorf("injection template %s must declare init container %s with restartPolicy %s",
			KeyRuntimeAIOInjectionConfig, ContainerNameRuntimeAgent, corev1.ContainerRestartPolicyAlways)
	}
	if len(config.Sidecars) > 0 {
		return config, fmt.Errorf("injection template %s must declare its containers in initContainers, not csiSidecar",
			KeyRuntimeAIOInjectionConfig)
	}
	return config, nil
}

// runtimeAIOContainerIndex returns the index of the only all-in-one container
// in initContainers, or -1 when there is none or more than one.
func runtimeAIOContainerIndex(initContainers []corev1.Container) int {
	index := -1
	for i := range initContainers {
		if !IsRuntimeAIOContainer(&initContainers[i]) {
			continue
		}
		if index >= 0 {
			return -1
		}
		index = i
	}
	return index
}

// setEnabledServicesEnv makes the all-in-one container run services. A literal
// value is replaced so it cannot disagree with the annotation written next to
// it, while the downward API reference of the shipped template is kept; parse
// only accepts the field of AnnotationRuntimedPlugins, which the injector sets.
func setEnabledServicesEnv(env []corev1.EnvVar, services string) []corev1.EnvVar {
	for i := range env {
		if env[i].Name != envRuntimedEnabledServices {
			continue
		}
		if env[i].ValueFrom == nil {
			env[i].Value = services
		}
		return env
	}
	return append(env, corev1.EnvVar{Name: envRuntimedEnabledServices, Value: services})
}

// runtimedServices returns the runtimed services the all-in-one container runs
// for the built-in runtimes the sandbox uses.
func runtimedServices(sandbox *agentsv1alpha1.Sandbox) string {
	var services []string
	for _, s := range runtimeAIOServices {
		if IsRuntimeEnabled(sandbox, s.runtime) {
			services = append(services, s.services...)
		}
	}
	return strings.Join(services, ",")
}

// takesOverRuntime reports whether the built-in runtime runs inside the
// all-in-one container instead of from its own template.
func takesOverRuntime(runtime string) bool {
	for _, s := range runtimeAIOServices {
		if s.runtime == runtime {
			return true
		}
	}
	return false
}
