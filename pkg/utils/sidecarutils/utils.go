package sidecarutils

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func InjectPodTemplateCSIAndRuntimeSidecar(ctx context.Context, sandbox *agentsv1alpha1.Sandbox, podSpec *corev1.PodSpec, cli client.Client) error {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(sandbox))
	if !enableInjectCsiMountConfig(sandbox) && !enableInjectAgentRuntimeConfig(sandbox) {
		return nil
	}
	// fetch the custom injection configuration
	config, err := fetchInjectionConfiguration(ctx, cli)
	if err != nil {
		logger.Error(err, "failed to fetch injection configuration")
		return err
	}
	return doSidecarInjection(ctx, sandbox, podSpec, config)
}

func doSidecarInjection(ctx context.Context, sandbox *agentsv1alpha1.Sandbox, podSpec *corev1.PodSpec, injectConfigMap map[string]string) error {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(sandbox))
	// set agent runtime sidecar config
	if enableInjectAgentRuntimeConfig(sandbox) {
		runTimeInjectConfig, err := parseInjectConfig(ctx, KEY_RUNTIME_INJECTION_CONFIG, injectConfigMap)
		if err != nil {
			logger.Error(err, "failed to parse agent runtime injection configuration")
			return err
		}
		if !isContainersExists(podSpec.InitContainers, runTimeInjectConfig.Sidecars) && !isContainersExists(podSpec.Containers, runTimeInjectConfig.Sidecars) {
			setAgentRuntimeContainer(ctx, podSpec, runTimeInjectConfig)
		}
	}
	// set csi sidecar config
	if enableInjectCsiMountConfig(sandbox) {
		csiInjectConfig, err := parseInjectConfig(ctx, KEY_CSI_INJECTION_CONFIG, injectConfigMap)
		if err != nil {
			logger.Error(err, "failed to parse csi injection configuration")
			return err
		}
		if !isContainersExists(podSpec.InitContainers, csiInjectConfig.Sidecars) && !isContainersExists(podSpec.Containers, csiInjectConfig.Sidecars) {
			setCSIMountContainer(ctx, podSpec, csiInjectConfig)
		}
	}
	return nil
}
