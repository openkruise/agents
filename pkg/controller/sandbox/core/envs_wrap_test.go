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
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

func execProbe(command ...string) *corev1.Probe {
	return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: command}}}
}

func TestWrapMainContainerWithRunWithEnvs(t *testing.T) {
	tests := []struct {
		name        string
		mode        agentsv1alpha1.StartupMode
		command     []string
		probe       *corev1.Probe
		wantWrapped bool
	}{
		{
			name:    "deferred with explicit command is wrapped",
			mode:    agentsv1alpha1.StartupModeDeferred,
			command: []string{"/usr/local/bin/app", "--serve"},
			probe:   execProbe("/usr/bin/healthcheck", "--port", "8080"),

			wantWrapped: true,
		},
		{
			name:    "immediate leaves the pod untouched",
			mode:    agentsv1alpha1.StartupModeImmediate,
			command: []string{"/usr/local/bin/app"},
		},
		{
			name:    "unset mode leaves the pod untouched",
			command: []string{"/usr/local/bin/app"},
		},
		{
			name:    "empty command (image entrypoint) is wrapped for a claim command",
			mode:    agentsv1alpha1.StartupModeDeferred,
			command: nil,

			wantWrapped: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			box := &agentsv1alpha1.Sandbox{}
			box.Spec.StartupMode = tt.mode
			pod := &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:         "main",
						Command:      tt.command,
						StartupProbe: tt.probe,
					}},
				},
			}
			original := tt.command

			wrapMainContainerWithRunWithEnvs(box, pod)

			main := &pod.Spec.Containers[0]
			if !tt.wantWrapped {
				assert.Equal(t, original, main.Command)
				assert.Empty(t, main.Env)
				if tt.probe != nil {
					assert.NotEqual(t, "--probe", tt.probe.Exec.Command[2])
				}
				return
			}

			// Command/args rewrite.
			require.Equal(t, []string{"/bin/bash", utils.RunWithEnvsScript}, main.Command)
			expectedArgs := append([]string{"--"}, original...)
			assert.Equal(t, expectedArgs, main.Args)

			// Wrapper contract env.
			envByName := map[string]corev1.EnvVar{}
			for _, e := range main.Env {
				envByName[e.Name] = e
			}
			envDir, ok := envByName[utils.EnvSandboxEnvDir]
			require.True(t, ok)
			assert.Equal(t, utils.SandboxEnvClaimDir, envDir.Value)
			uid, ok := envByName[utils.EnvSandboxPodUID]
			require.True(t, ok)
			require.NotNil(t, uid.ValueFrom)
			require.NotNil(t, uid.ValueFrom.FieldRef)
			assert.Equal(t, "metadata.uid", uid.ValueFrom.FieldRef.FieldPath)

			// Exec probe rewrite.
			if tt.probe != nil {
				require.NotNil(t, main.StartupProbe)
				require.NotNil(t, main.StartupProbe.Exec)
				wantProbe := append([]string{"/bin/bash", utils.RunWithEnvsScript, "--probe", "--"}, "/usr/bin/healthcheck", "--port", "8080")
				assert.Equal(t, wantProbe, main.StartupProbe.Exec.Command)
			}
		})
	}
}

func TestWrapMainContainerWithRunWithEnvsIdempotent(t *testing.T) {
	box := &agentsv1alpha1.Sandbox{}
	box.Spec.StartupMode = agentsv1alpha1.StartupModeDeferred
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:    "main",
				Command: []string{"/usr/local/bin/app"},
			}},
		},
	}

	wrapMainContainerWithRunWithEnvs(box, pod)
	firstCommand := append([]string{}, pod.Spec.Containers[0].Command...)
	firstArgs := append([]string{}, pod.Spec.Containers[0].Args...)
	firstEnv := len(pod.Spec.Containers[0].Env)

	// Second pass must not double-wrap.
	wrapMainContainerWithRunWithEnvs(box, pod)
	assert.Equal(t, firstCommand, pod.Spec.Containers[0].Command)
	assert.Equal(t, firstArgs, pod.Spec.Containers[0].Args)
	assert.Equal(t, firstEnv, len(pod.Spec.Containers[0].Env))
}

func TestWrapExecProbes(t *testing.T) {
	c := &corev1.Container{
		Name:         "main",
		StartupProbe: execProbe("/bin/true"),
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(8080)},
			},
		},
	}
	wrapExecProbes(c)

	require.NotNil(t, c.StartupProbe.Exec)
	assert.Equal(t, []string{"/bin/bash", utils.RunWithEnvsScript, "--probe", "--", "/bin/true"}, c.StartupProbe.Exec.Command)

	// Non-exec probes are left untouched.
	require.Nil(t, c.ReadinessProbe.Exec)
	require.NotNil(t, c.ReadinessProbe.HTTPGet)
	assert.Equal(t, "/healthz", c.ReadinessProbe.HTTPGet.Path)

	// Already-wrapped probes stay as they are.
	wrapExecProbes(c)
	assert.Equal(t, []string{"/bin/bash", utils.RunWithEnvsScript, "--probe", "--", "/bin/true"}, c.StartupProbe.Exec.Command)
}
