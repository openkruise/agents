// Package utils provides utility functions for parsing and handling Kubernetes resource quantities.
//
//nolint:revive // Package name is acceptable for this utility package
package utils

import (
	"fmt"
	"strings"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func ValidatedCustomLabelKey(key string) error {
	if strings.HasPrefix(v1alpha1.InternalPrefix, key) {
		return fmt.Errorf("label key %s is reserved", key)
	}
	return nil
}

func SandboxClaimed(sbx infra.Sandbox) bool {
	state := sbx.GetState()
	return state == v1alpha1.SandboxStateRunning || state == v1alpha1.SandboxStatePaused
}

func CalculateResourceFromContainers(containers []corev1.Container) infra.SandboxResource {
	resource := infra.SandboxResource{}
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
			if disk, ok := requests[corev1.ResourceEphemeralStorage]; ok {
				// Convert disk quantity to MB (1 MB = 1024 * 1024 bytes)
				resource.DiskSizeMB += disk.Value() / (1024 * 1024)
			}
		}
	}
	return resource
}

func LockSandbox(sbx client.Object, lock string, owner string) {
	annotations := sbx.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string, 2)
	}
	annotations[v1alpha1.AnnotationLock] = lock
	annotations[v1alpha1.AnnotationOwner] = owner
	sbx.SetAnnotations(annotations)
}
