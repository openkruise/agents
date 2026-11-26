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
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

const startupScriptsContainer = "envd-scripts"

func ValidateDeferredStartup(mode agentsv1alpha1.StartupMode, runtimes []agentsv1alpha1.RuntimeConfig, template *corev1.PodTemplateSpec, fld *field.Path) field.ErrorList {
	if mode != agentsv1alpha1.StartupModeDeferred {
		return nil
	}
	var errs field.ErrorList
	hasRuntime := false
	for _, runtime := range runtimes {
		hasRuntime = hasRuntime || runtime.Name == agentsv1alpha1.RuntimeConfigForInjectAgentRuntime
	}
	if !hasRuntime {
		errs = append(errs, field.Required(fld.Child("runtimes"), "Deferred startup requires agent-runtime"))
	}
	if template == nil || len(template.Spec.Containers) == 0 {
		return append(errs, field.Required(fld.Child("template", "spec", "containers"), "Deferred startup requires a main container"))
	}
	main := &template.Spec.Containers[0]
	mainPath := fld.Child("template", "spec", "containers").Index(0)
	if !startupRootUser(template.Spec.SecurityContext, main.SecurityContext) {
		errs = append(errs, field.Invalid(mainPath.Child("securityContext"), nil, "Deferred startup currently requires a root main container and root envd"))
	}
	if _, err := startupMount(main); err != nil {
		errs = append(errs, field.Invalid(mainPath.Child("volumeMounts"), nil, err.Error()))
	}
	for _, c := range template.Spec.InitContainers {
		if c.Name == startupScriptsContainer {
			errs = append(errs, field.Invalid(fld.Child("template", "spec", "initContainers"), c.Name, "container name is reserved for startup staging"))
		}
	}
	for _, probe := range []struct {
		name  string
		value *corev1.Probe
	}{{"startupProbe", main.StartupProbe}, {"readinessProbe", main.ReadinessProbe}, {"livenessProbe", main.LivenessProbe}} {
		p := probe.value
		if p == nil || p.Exec != nil {
			continue
		}
		port := intstr.FromInt32(0)
		if p.HTTPGet != nil && p.HTTPGet.Host == "" {
			port = p.HTTPGet.Port
		} else if p.TCPSocket != nil && p.TCPSocket.Host == "" {
			port = p.TCPSocket.Port
		}
		if port.Type == intstr.String {
			for _, declared := range main.Ports {
				if declared.Name == port.StrVal {
					port = intstr.FromInt32(declared.ContainerPort)
					break
				}
			}
		}
		if port.Type != intstr.Int || port.IntVal != utils.RuntimePort {
			errs = append(errs, field.Invalid(mainPath.Child(probe.name), nil, "Deferred startup requires an exec probe or a probe on the runtime port"))
		}
	}
	return errs
}

func startupRootUser(pod *corev1.PodSecurityContext, container *corev1.SecurityContext) bool {
	var uid *int64
	var nonRoot *bool
	if pod != nil {
		uid, nonRoot = pod.RunAsUser, pod.RunAsNonRoot
	}
	if container != nil {
		if container.RunAsUser != nil {
			uid = container.RunAsUser
		}
		if container.RunAsNonRoot != nil {
			nonRoot = container.RunAsNonRoot
		}
	}
	return (uid == nil || *uid == 0) && (nonRoot == nil || !*nonRoot)
}

func startupMount(c *corev1.Container) (corev1.VolumeMount, error) {
	var mount *corev1.VolumeMount
	root := path.Dir(utils.RunWithEnvsScript)
	destinations := make(map[string]string, len(c.VolumeMounts))
	for i := range c.VolumeMounts {
		m := &c.VolumeMounts[i]
		destination := path.Clean(m.MountPath)
		if previous, exists := destinations[destination]; exists {
			return corev1.VolumeMount{}, fmt.Errorf("duplicate normalized mount destination %q (%q and %q)", destination, previous, m.MountPath)
		}
		destinations[destination] = m.MountPath
		if destination == root {
			if m.MountPath != root {
				return corev1.VolumeMount{}, fmt.Errorf("envd startup mount path %q must use canonical path %q", m.MountPath, root)
			}
			if m.ReadOnly {
				return corev1.VolumeMount{}, fmt.Errorf("envd startup mount must be writable")
			}
			if m.SubPath != "" || m.SubPathExpr != "" {
				return corev1.VolumeMount{}, fmt.Errorf("envd mount must not use subPath")
			}
			mount = m
		} else if strings.HasPrefix(destination, root+"/") {
			return corev1.VolumeMount{}, fmt.Errorf("a nested mount shadows the envd startup directory: %q normalizes to %q", m.MountPath, destination)
		}
	}
	if mount == nil {
		return corev1.VolumeMount{}, fmt.Errorf("Deferred startup requires an envd mount at %s", path.Dir(utils.RunWithEnvsScript))
	}
	return *mount, nil
}

func PrepareDeferredStartup(box *agentsv1alpha1.Sandbox, pod *corev1.Pod) error {
	if box.Spec.StartupMode != agentsv1alpha1.StartupModeDeferred {
		return nil
	}
	if len(pod.Spec.Containers) == 0 {
		return fmt.Errorf("Deferred startup requires a main container")
	}
	if !startupRootUser(pod.Spec.SecurityContext, pod.Spec.Containers[0].SecurityContext) {
		return fmt.Errorf("Deferred startup currently requires a root main container and root envd")
	}
	mount, err := startupMount(&pod.Spec.Containers[0])
	if err != nil {
		return err
	}
	emptyDir := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == mount.Name {
			emptyDir = v.EmptyDir != nil
		}
	}
	if !emptyDir {
		return fmt.Errorf("Deferred startup requires the envd volume to be a per-pod emptyDir")
	}
	var runtime *corev1.Container
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		if c.Name == ContainerNameRuntimeAgent || c.Name == "runtime" {
			runtime = c
			break
		}
	}
	if runtime == nil || runtime.RestartPolicy == nil || *runtime.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		return fmt.Errorf("Deferred startup requires an agent-runtime native sidecar")
	}
	if !startupRootUser(pod.Spec.SecurityContext, runtime.SecurityContext) {
		return fmt.Errorf("Deferred startup staging requires a root runtime container")
	}
	runtimeMount, err := startupMount(runtime)
	if err != nil || runtimeMount.Name != mount.Name || runtimeMount.ReadOnly {
		return fmt.Errorf("agent-runtime must mount the same envd volume writable at %s", mount.MountPath)
	}
	mount.ReadOnly = false
	staging := corev1.Container{
		Name: startupScriptsContainer, Image: runtime.Image, ImagePullPolicy: runtime.ImagePullPolicy,
		Command: []string{"sh", "-c", fmt.Sprintf("umask 077 && cp /workspace/%s %s/ && chmod 0755 %s && mkdir -p -m 0700 %s && chmod 0700 %s",
			path.Base(utils.RunWithEnvsScript), path.Dir(utils.RunWithEnvsScript), utils.RunWithEnvsScript, utils.SandboxEnvClaimDir, utils.SandboxEnvClaimDir)},
		VolumeMounts:    []corev1.VolumeMount{mount},
		SecurityContext: runtime.SecurityContext.DeepCopy(),
		// A resource-less init container downgrades Guaranteed Pods and may be
		// rejected by quotas requiring resource requests or limits.
		Resources: *runtime.Resources.DeepCopy(),
	}
	// Stage before native sidecars so their startup probes cannot race the copy.
	containers := []corev1.Container{staging}
	for _, c := range pod.Spec.InitContainers {
		if c.Name != startupScriptsContainer {
			containers = append(containers, c)
		}
	}
	pod.Spec.InitContainers = containers
	return nil
}
