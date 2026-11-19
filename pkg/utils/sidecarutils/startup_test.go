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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
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
			assert.Contains(t, staging.Command[2], "mkdir -p /mnt/envd/claim")
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
