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

package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

func TestWrapMainContainerWithRunWithEnvs(t *testing.T) {
	execProbe := func(command ...string) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: command}}}
	}
	httpProbe := func() *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(8080)},
		}}
	}
	wrapperCommand := []string{"/bin/bash", utils.RunWithEnvsScript}
	wrappedProbe := func(command ...string) *corev1.Probe {
		return execProbe(append([]string{"/bin/bash", utils.RunWithEnvsScript, "--probe", "--"}, command...)...)
	}
	envDir := corev1.EnvVar{Name: utils.EnvSandboxEnvDir, Value: utils.SandboxEnvClaimDir}
	podUID := corev1.EnvVar{Name: utils.EnvSandboxPodUID, ValueFrom: &corev1.EnvVarSource{
		FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.uid"},
	}}
	sidecar := func() corev1.Container {
		return corev1.Container{
			Name:          "sidecar",
			Command:       []string{"/bin/sidecar"},
			Args:          []string{"--serve"},
			LivenessProbe: execProbe("/bin/sidecar-check"),
		}
	}

	tests := []struct {
		name     string
		mode     agentsv1alpha1.StartupMode
		main     corev1.Container
		wantMain corev1.Container
	}{
		{
			name:     "immediate leaves the pod untouched",
			mode:     agentsv1alpha1.StartupModeImmediate,
			main:     corev1.Container{Name: "main", Command: []string{"/app"}, StartupProbe: execProbe("/check")},
			wantMain: corev1.Container{Name: "main", Command: []string{"/app"}, StartupProbe: execProbe("/check")},
		},
		{
			name:     "unset mode leaves the pod untouched",
			main:     corev1.Container{Name: "main", Command: []string{"/app"}},
			wantMain: corev1.Container{Name: "main", Command: []string{"/app"}},
		},
		{
			name: "command and args become the template argv and every exec probe is wrapped",
			mode: agentsv1alpha1.StartupModeDeferred,
			main: corev1.Container{
				Name:           "main",
				Command:        []string{"/app"},
				Args:           []string{"--serve", "-c"},
				Env:            []corev1.EnvVar{{Name: "FOO", Value: "bar"}},
				StartupProbe:   execProbe("/startup"),
				ReadinessProbe: execProbe("/ready", "--port", "8080"),
				LivenessProbe:  httpProbe(),
			},
			wantMain: corev1.Container{
				Name:           "main",
				Command:        wrapperCommand,
				Args:           []string{"--", "/app", "--serve", "-c"},
				Env:            []corev1.EnvVar{{Name: "FOO", Value: "bar"}, envDir, podUID},
				StartupProbe:   wrappedProbe("/startup"),
				ReadinessProbe: wrappedProbe("/ready", "--port", "8080"),
				LivenessProbe:  httpProbe(),
			},
		},
		{
			name: "liveness exec probe is wrapped",
			mode: agentsv1alpha1.StartupModeDeferred,
			main: corev1.Container{Name: "main", Command: []string{"/app"}, LivenessProbe: execProbe("/live")},
			wantMain: corev1.Container{
				Name:          "main",
				Command:       wrapperCommand,
				Args:          []string{"--", "/app"},
				Env:           []corev1.EnvVar{envDir, podUID},
				LivenessProbe: wrappedProbe("/live"),
			},
		},
		{
			name: "no command and no args gives an empty template argv",
			mode: agentsv1alpha1.StartupModeDeferred,
			main: corev1.Container{Name: "main"},
			wantMain: corev1.Container{
				Name:    "main",
				Command: wrapperCommand,
				Args:    []string{"--"},
				Env:     []corev1.EnvVar{envDir, podUID},
			},
		},
		{
			name: "args without a command are dropped because they belong to the image entrypoint",
			mode: agentsv1alpha1.StartupModeDeferred,
			main: corev1.Container{Name: "main", Args: []string{"-c", "echo hi"}},
			wantMain: corev1.Container{
				Name:    "main",
				Command: wrapperCommand,
				Args:    []string{"--"},
				Env:     []corev1.EnvVar{envDir, podUID},
			},
		},
		{
			name: "template values of the controller-owned env are overwritten",
			mode: agentsv1alpha1.StartupModeDeferred,
			main: corev1.Container{
				Name:    "main",
				Command: []string{"/app"},
				Env: []corev1.EnvVar{
					{Name: utils.EnvSandboxPodUID, Value: "stale"},
					{Name: "FOO", Value: "bar"},
					{Name: utils.EnvSandboxEnvDir, Value: "/data"},
				},
			},
			wantMain: corev1.Container{
				Name:    "main",
				Command: wrapperCommand,
				Args:    []string{"--", "/app"},
				Env:     []corev1.EnvVar{podUID, {Name: "FOO", Value: "bar"}, envDir},
			},
		},
		{
			name: "an already wrapped command still gets the wrapper env and probes",
			mode: agentsv1alpha1.StartupModeDeferred,
			main: corev1.Container{
				Name:         "main",
				Command:      wrapperCommand,
				Args:         []string{"--", "/app"},
				StartupProbe: execProbe("/startup"),
			},
			wantMain: corev1.Container{
				Name:         "main",
				Command:      wrapperCommand,
				Args:         []string{"--", "/app"},
				Env:          []corev1.EnvVar{envDir, podUID},
				StartupProbe: wrappedProbe("/startup"),
			},
		},
		{
			name: "an already wrapped probe is kept",
			mode: agentsv1alpha1.StartupModeDeferred,
			main: corev1.Container{Name: "main", Command: []string{"/app"}, StartupProbe: wrappedProbe("/startup")},
			wantMain: corev1.Container{
				Name:         "main",
				Command:      wrapperCommand,
				Args:         []string{"--", "/app"},
				Env:          []corev1.EnvVar{envDir, podUID},
				StartupProbe: wrappedProbe("/startup"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			box := &agentsv1alpha1.Sandbox{}
			box.Spec.StartupMode = tt.mode
			pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{tt.main, sidecar()}}}

			// Applying twice must give the same result as applying once.
			wrapMainContainerWithRunWithEnvs(t.Context(), box, pod)
			wrapMainContainerWithRunWithEnvs(t.Context(), box, pod)

			assert.Equal(t, tt.wantMain, pod.Spec.Containers[0])
			assert.Equal(t, sidecar(), pod.Spec.Containers[1], "only the main container is wrapped")
		})
	}
}

func TestWrapMainContainerWithRunWithEnvsNoPod(t *testing.T) {
	box := &agentsv1alpha1.Sandbox{}
	box.Spec.StartupMode = agentsv1alpha1.StartupModeDeferred
	assert.NotPanics(t, func() {
		wrapMainContainerWithRunWithEnvs(t.Context(), nil, &corev1.Pod{})
		wrapMainContainerWithRunWithEnvs(t.Context(), box, nil)
		wrapMainContainerWithRunWithEnvs(t.Context(), box, &corev1.Pod{})
	})
}
