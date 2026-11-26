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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	"k8s.io/utils/ptr"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func deferredTestPod() *corev1.Pod {
	mount := corev1.VolumeMount{Name: "envd-volume", MountPath: "/mnt/envd"}
	return &corev1.Pod{Spec: corev1.PodSpec{
		Containers:     []corev1.Container{{Name: "main", VolumeMounts: []corev1.VolumeMount{mount}}},
		InitContainers: []corev1.Container{{Name: ContainerNameRuntimeAgent, Image: "runtime:test", RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways), VolumeMounts: []corev1.VolumeMount{mount}}},
		Volumes:        []corev1.Volume{{Name: mount.Name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
	}}
}

func TestPrepareDeferredStartup(t *testing.T) {
	for _, tt := range []struct {
		name    string
		change  func(*corev1.Pod)
		wantErr string
	}{
		{name: "native sidecar"},
		{name: "read-only main mount", change: func(p *corev1.Pod) { p.Spec.Containers[0].VolumeMounts[0].ReadOnly = true }, wantErr: "writable"},
		{name: "non-root main rejected", change: func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: ptr.To[int64](1000)}
		}, wantErr: "root main"},
		{name: "pod non-root rejected", change: func(p *corev1.Pod) { p.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true)} }, wantErr: "root main"},
		{name: "non-root staging rejected", change: func(p *corev1.Pod) {
			p.Spec.InitContainers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: ptr.To[int64](1000)}
		}, wantErr: "root runtime"},
		{name: "missing main", change: func(p *corev1.Pod) { p.Spec.Containers = nil }, wantErr: "main container"},
		{name: "missing mount", change: func(p *corev1.Pod) { p.Spec.Containers[0].VolumeMounts = nil }, wantErr: "envd mount"},
		{name: "shared volume rejected", change: func(p *corev1.Pod) { p.Spec.Volumes[0].EmptyDir = nil }, wantErr: "per-pod emptyDir"},
		{name: "runtime missing", change: func(p *corev1.Pod) { p.Spec.InitContainers = nil }, wantErr: "native sidecar"},
		{name: "one-shot runtime rejected", change: func(p *corev1.Pod) { p.Spec.InitContainers[0].RestartPolicy = nil }, wantErr: "native sidecar"},
		{name: "read-only delivery mount", change: func(p *corev1.Pod) { p.Spec.InitContainers[0].VolumeMounts[0].ReadOnly = true }, wantErr: "writable"},
		{name: "different delivery mount", change: func(p *corev1.Pod) { p.Spec.InitContainers[0].VolumeMounts[0].Name = "other" }, wantErr: "same envd"},
		{name: "subpath rejected", change: func(p *corev1.Pod) { p.Spec.Containers[0].VolumeMounts[0].SubPath = "nested" }, wantErr: "subPath"},
		{name: "nested claim mount rejected", change: func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "other", MountPath: "/mnt/envd/claim"})
		}, wantErr: "shadows"},
		{name: "repeated separator nested mount rejected", change: func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "other", MountPath: "/mnt//envd/claim"})
		}, wantErr: "shadows"},
		{name: "dot nested mount rejected", change: func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "other", MountPath: "/mnt/./envd/claim/"})
		}, wantErr: "shadows"},
		{name: "parent traversal nested mount rejected", change: func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "other", MountPath: "/mnt/other/../envd/run_with_envs.sh"})
		}, wantErr: "shadows"},
		{name: "runtime nested mount alias rejected", change: func(p *corev1.Pod) {
			p.Spec.InitContainers[0].VolumeMounts = append(p.Spec.InitContainers[0].VolumeMounts, corev1.VolumeMount{Name: "other", MountPath: "/mnt//envd/claim"})
		}, wantErr: "same envd"},
		{name: "repeated separator root alias rejected", change: func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts[0].MountPath = "/mnt//envd"
		}, wantErr: "canonical path"},
		{name: "trailing separator root alias rejected", change: func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts[0].MountPath = "/mnt/envd/"
		}, wantErr: "canonical path"},
		{name: "parent traversal root alias rejected", change: func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts[0].MountPath = "/mnt/other/../envd"
		}, wantErr: "canonical path"},
		{name: "duplicate root destination rejected", change: func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "other", MountPath: "/mnt/envd"})
		}, wantErr: "duplicate normalized mount destination"},
		{name: "duplicate normalized root destination rejected", change: func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "other", MountPath: "/mnt//envd/."})
		}, wantErr: "duplicate normalized mount destination"},
		{name: "sibling mount allowed", change: func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "other", MountPath: "/mnt/envd-other/claim"})
		}},
		{name: "main root overrides pod uid", change: func(p *corev1.Pod) {
			p.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](1000)}
			p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: ptr.To[int64](0)}
			p.Spec.InitContainers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: ptr.To[int64](0)}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pod := deferredTestPod()
			if tt.change != nil {
				tt.change(pod)
			}
			box := &agentsv1alpha1.Sandbox{Spec: agentsv1alpha1.SandboxSpec{StartupMode: agentsv1alpha1.StartupModeDeferred}}
			err := PrepareDeferredStartup(box, pod)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, pod.Spec.InitContainers, 2)
			staging := pod.Spec.InitContainers[0]
			assert.Equal(t, "envd-scripts", staging.Name)
			assert.Equal(t, "runtime:test", staging.Image)
			assert.Nil(t, staging.RestartPolicy)
			assert.Equal(t, []string{"sh", "-c", "umask 077 && cp /workspace/run_with_envs.sh /mnt/envd/ && chmod 0755 /mnt/envd/run_with_envs.sh && mkdir -p -m 0700 /mnt/envd/claim && chmod 0700 /mnt/envd/claim"}, staging.Command)
			assert.Len(t, pod.Spec.Volumes, 1)
			first := pod.DeepCopy()
			require.NoError(t, PrepareDeferredStartup(box, pod))
			assert.Equal(t, first, pod)
			pod.Spec.InitContainers[1].Image = "runtime:restored"
			require.NoError(t, PrepareDeferredStartup(box, pod))
			assert.Equal(t, "runtime:restored", pod.Spec.InitContainers[0].Image)
		})
	}
	pod := deferredTestPod()
	first := pod.DeepCopy()
	require.NoError(t, PrepareDeferredStartup(&agentsv1alpha1.Sandbox{}, pod))
	assert.Equal(t, first, pod)
}

func TestStartupStagingPreservesResourcesAndQoS(t *testing.T) {
	guaranteed := corev1.ResourceList{
		corev1.ResourceCPU:              resource.MustParse("100m"),
		corev1.ResourceMemory:           resource.MustParse("64Mi"),
		corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
	}
	for _, tt := range []struct {
		name      string
		resources corev1.ResourceRequirements
		qos       corev1.PodQOSClass
	}{
		{name: "Guaranteed and quota limits", resources: corev1.ResourceRequirements{Requests: guaranteed.DeepCopy(), Limits: guaranteed.DeepCopy(), Claims: []corev1.ResourceClaim{{Name: "runtime-claim"}}}, qos: corev1.PodQOSGuaranteed},
		{name: "Burstable", resources: corev1.ResourceRequirements{Requests: guaranteed.DeepCopy()}, qos: corev1.PodQOSBurstable},
		{name: "BestEffort", qos: corev1.PodQOSBestEffort},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pod := deferredTestPod()
			pod.Spec.Containers[0].Resources = *tt.resources.DeepCopy()
			pod.Spec.InitContainers[0].Resources = *tt.resources.DeepCopy()
			assert.Equal(t, tt.qos, qos.GetPodQOS(pod))
			box := &agentsv1alpha1.Sandbox{Spec: agentsv1alpha1.SandboxSpec{StartupMode: agentsv1alpha1.StartupModeDeferred}}
			require.NoError(t, PrepareDeferredStartup(box, pod))
			assert.Equal(t, tt.resources, pod.Spec.InitContainers[0].Resources)
			assert.Equal(t, tt.qos, qos.GetPodQOS(pod))
			if tt.resources.Requests != nil {
				pod.Spec.InitContainers[1].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("200m")
				assert.Equal(t, resource.MustParse("100m"), pod.Spec.InitContainers[0].Resources.Requests[corev1.ResourceCPU])
			}
			if len(tt.resources.Claims) > 0 {
				pod.Spec.InitContainers[1].Resources.Claims[0].Name = "changed"
				assert.Equal(t, "runtime-claim", pod.Spec.InitContainers[0].Resources.Claims[0].Name)
			}
		})
	}
}

func TestStartupStagingDirectoryPermissions(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "new claim directory"
		if existing {
			name = "existing claim directory"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "envd")
			require.NoError(t, os.Mkdir(dir, 0o755))
			if existing {
				require.NoError(t, os.Mkdir(filepath.Join(dir, "claim"), 0o755))
			}
			source := filepath.Join(root, "run_with_envs.sh")
			require.NoError(t, os.WriteFile(source, []byte("#!/bin/bash\nexit 0\n"), 0o600))
			pod := deferredTestPod()
			require.NoError(t, PrepareDeferredStartup(&agentsv1alpha1.Sandbox{Spec: agentsv1alpha1.SandboxSpec{StartupMode: agentsv1alpha1.StartupModeDeferred}}, pod))
			script := pod.Spec.InitContainers[0].Command[2]
			quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
			script = strings.ReplaceAll(script, "/workspace/run_with_envs.sh", quote(source))
			script = strings.ReplaceAll(script, "/mnt/envd", quote(dir))
			if !existing {
				// Check the creation window before the later chmod can hide it.
				script = `mkdir() {
    command mkdir "$@" || return
    mode=$(ls -ld -- ` + quote(filepath.Join(dir, "claim")) + `) || return
    test "${mode:0:10}" = "drwx------"
}
` + script
			}
			cmd := exec.CommandContext(t.Context(), "bash", "-c", script)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, string(out))
			info, err := os.Stat(filepath.Join(dir, "claim"))
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
			info, err = os.Stat(filepath.Join(dir, "run_with_envs.sh"))
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
		})
	}
}

func TestValidateDeferredStartup(t *testing.T) {
	for _, tt := range []struct {
		name      string
		change    func(*corev1.PodTemplateSpec)
		noRuntime bool
		wantErr   bool
	}{
		{name: "command-less pool"},
		{name: "non-root pool", wantErr: true, change: func(p *corev1.PodTemplateSpec) {
			p.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](1000)}
		}},
		{name: "main root overrides pod uid", change: func(p *corev1.PodTemplateSpec) {
			p.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](1000)}
			p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: ptr.To[int64](0)}
		}},
		{name: "missing runtime", noRuntime: true, wantErr: true},
		{name: "nested mount alias", wantErr: true, change: func(p *corev1.PodTemplateSpec) {
			p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "other", MountPath: "/mnt//envd/claim"})
		}},
		{name: "root mount alias", wantErr: true, change: func(p *corev1.PodTemplateSpec) {
			p.Spec.Containers[0].VolumeMounts[0].MountPath = "/mnt/./envd/"
		}},
		{name: "duplicate normalized destination", wantErr: true, change: func(p *corev1.PodTemplateSpec) {
			p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "other", MountPath: "/mnt/envd/."})
		}},
		{name: "app TCP probe", wantErr: true, change: func(p *corev1.PodTemplateSpec) {
			p.Spec.Containers[0].LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)}}}
		}},
		{name: "runtime TCP probe", change: func(p *corev1.PodTemplateSpec) {
			p.Spec.Containers[0].LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(49983)}}}
		}},
		{name: "exec probe", change: func(p *corev1.PodTemplateSpec) {
			p.Spec.Containers[0].StartupProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}}
		}},
		{name: "reserved init name", wantErr: true, change: func(p *corev1.PodTemplateSpec) {
			p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{Name: "envd-scripts"})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := &corev1.PodTemplateSpec{Spec: deferredTestPod().Spec}
			if tt.change != nil {
				tt.change(p)
			}
			runtimes := []agentsv1alpha1.RuntimeConfig{{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime}}
			if tt.noRuntime {
				runtimes = nil
			}
			errs := ValidateDeferredStartup(agentsv1alpha1.StartupModeDeferred, runtimes, p, field.NewPath("spec"))
			assert.Equal(t, tt.wantErr, len(errs) > 0, "%v", errs)
			assert.Empty(t, ValidateDeferredStartup(agentsv1alpha1.StartupModeImmediate, runtimes, p, field.NewPath("spec")))
		})
	}
}
