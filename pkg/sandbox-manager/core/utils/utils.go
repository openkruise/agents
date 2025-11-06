// Package utils provides utility functions for parsing and handling Kubernetes resource quantities.
//
//nolint:revive // Package name is acceptable for this utility package
package utils

import (
	"context"
	"fmt"
	"strings"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	infra2 "github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
)

func ValidatedCustomLabelKey(key string) error {
	if strings.HasPrefix(consts.InternalPrefix, key) {
		return fmt.Errorf("label key %s is reserved", key)
	}
	return nil
}

func SandboxClaimed(sbx infra2.Sandbox) bool {
	state := sbx.GetState()
	return state == consts.SandboxStateRunning || state == consts.SandboxStatePaused
}

func CalculateResourceFromContainers(containers []corev1.Container) infra2.SandboxResource {
	resource := infra2.SandboxResource{}
	for _, container := range containers {
		if requests := container.Resources.Requests; requests != nil {
			if cpu, ok := requests[corev1.ResourceCPU]; ok {
				// Convert CPU quantity to cores (e.g., 1000m = 1 core)
				resource.CPUMilli += cpu.MilliValue()
			}
			if memory, ok := requests[corev1.ResourceMemory]; ok {
				// Convert memory quantity to MB (1 MB = 1024 * 1024 bytes)
				resource.MemoryMB += memory.Value() / (1024 * 1024)
			}
		}
	}
	return resource
}

func CalculateExpectPoolSize(ctx context.Context, total, pending int32, template *infra2.SandboxTemplate) (int32, error) {
	log := klog.FromContext(ctx).V(consts.DebugLogLevel)
	expectUsage, err := intstr.GetScaledValueFromIntOrPercent(template.Spec.ExpectUsage, int(total), true)
	if err != nil {
		return total, err
	}
	actualUsage := max(0, total-pending)
	expectTotal := total + actualUsage - int32(expectUsage)
	if expectTotal < template.Spec.MinPoolSize {
		expectTotal = template.Spec.MinPoolSize
	}
	if expectTotal > template.Spec.MaxPoolSize {
		expectTotal = template.Spec.MaxPoolSize
	}
	log.Info("expect pool size calculated", "pool", template.Name, "expectTotal", expectTotal,
		"expectUsage", expectUsage, "actualUsage", actualUsage, "total", total, "pending", pending)
	return expectTotal, nil
}
