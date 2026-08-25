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

package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestStripSandboxCacheFields(t *testing.T) {
	pauseTime := metav1.NewTime(time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC))
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"agents.kruise.io/wake-on-traffic":      "true",
				"agents.kruise.io/wake-timeout-seconds": "30",
			},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "controller"}},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			Paused:    true,
			PauseTime: &pauseTime,
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				TemplateRef: &agentsv1alpha1.SandboxTemplateRef{Name: "template-a"},
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sandbox"}}},
				},
				VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{}},
			},
		},
		Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxPaused},
	}
	expected := sandbox.DeepCopy()
	expected.ManagedFields = nil
	expected.Spec.EmbeddedSandboxTemplate = agentsv1alpha1.EmbeddedSandboxTemplate{}

	transformed, err := stripSandboxCacheFields(sandbox)
	require.NoError(t, err)
	assert.Equal(t, expected, transformed)
}

func TestStripSandboxCacheFieldsPassesThroughOtherObjects(t *testing.T) {
	pod := &corev1.Pod{}

	transformed, err := stripSandboxCacheFields(pod)
	require.NoError(t, err)
	assert.Same(t, pod, transformed)
}
