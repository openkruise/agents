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

package infra

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

func TestCalculateResourceFromContainers(t *testing.T) {
	cpuQuantity1, _ := resource.ParseQuantity("1000m")
	cpuQuantity2, _ := resource.ParseQuantity("500m")
	memoryQuantity1, _ := resource.ParseQuantity("1024Mi")
	memoryQuantity2, _ := resource.ParseQuantity("512Mi")

	tests := []struct {
		name string
		pod  *corev1.Pod
		want SandboxResource
	}{
		{
			name: "single container with resources",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQuantity1,
									corev1.ResourceMemory: memoryQuantity1,
								},
							},
						},
					},
				},
			},
			want: SandboxResource{
				CPUMilli: 1000,
				MemoryMB: 1024,
			},
		},
		{
			name: "multiple containers with resources",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQuantity1,
									corev1.ResourceMemory: memoryQuantity1,
								},
							},
						},
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQuantity2,
									corev1.ResourceMemory: memoryQuantity2,
								},
							},
						},
					},
				},
			},
			want: SandboxResource{
				CPUMilli: 1500,
				MemoryMB: 1536,
			},
		},
		{
			name: "no containers",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{},
				},
			},
			want: SandboxResource{
				CPUMilli: 0,
				MemoryMB: 0,
			},
		},
		{
			name: "containers without resources",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{},
							},
						},
					},
				},
			},
			want: SandboxResource{
				CPUMilli: 0,
				MemoryMB: 0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateResourceFromContainers(tt.pod.Spec.Containers)
			if got.CPUMilli != tt.want.CPUMilli {
				t.Errorf("GetResource().CPUMilli = %v, want %v", got.CPUMilli, tt.want.CPUMilli)
			}
			if got.MemoryMB != tt.want.MemoryMB {
				t.Errorf("GetResource().MemoryMB = %v, want %v", got.MemoryMB, tt.want.MemoryMB)
			}
		})
	}
}

// mockSandboxForLabels is a minimal Sandbox implementation for testing
// MergePodLabels. Only GetPodLabels and SetPodLabels have real logic;
// all other methods return zero values. When hasTemplate is false, both
// GetPodLabels and SetPodLabels are no-ops, simulating a nil pod template.
type mockSandboxForLabels struct {
	metav1.ObjectMeta
	podLabels   map[string]string
	hasTemplate bool
}

func (m *mockSandboxForLabels) GetPodLabels() map[string]string {
	if !m.hasTemplate {
		return nil
	}
	return m.podLabels
}
func (m *mockSandboxForLabels) SetPodLabels(labels map[string]string) {
	if !m.hasTemplate {
		return
	}
	m.podLabels = labels
}
func (m *mockSandboxForLabels) Pause(context.Context, PauseOptions) error { return nil }
func (m *mockSandboxForLabels) Resume(context.Context, ResumeOptions) error {
	return nil
}
func (m *mockSandboxForLabels) GetSandboxID() string             { return "" }
func (m *mockSandboxForLabels) GetRoute() proxy.Route            { return proxy.Route{} }
func (m *mockSandboxForLabels) GetState() (string, string)       { return "", "" }
func (m *mockSandboxForLabels) GetTemplate() string              { return "" }
func (m *mockSandboxForLabels) GetResource() SandboxResource     { return SandboxResource{} }
func (m *mockSandboxForLabels) SetImage(string)                  {}
func (m *mockSandboxForLabels) GetImage() string                 { return "" }
func (m *mockSandboxForLabels) SetTimeout(timeout.Options)       {}
func (m *mockSandboxForLabels) SaveTimeoutWithPolicy(context.Context, timeout.Options, timeout.UpdatePolicy) (TimeoutUpdateResult, error) {
	return TimeoutUpdateResult{}, nil
}
func (m *mockSandboxForLabels) GetTimeout() timeout.Options { return timeout.Options{} }
func (m *mockSandboxForLabels) GetClaimTime() (time.Time, error) {
	return time.Time{}, nil
}
func (m *mockSandboxForLabels) Kill(context.Context) error             { return nil }
func (m *mockSandboxForLabels) TriggerReuse(context.Context) error     { return nil }
func (m *mockSandboxForLabels) IsReuseEnabled() bool                   { return false }
func (m *mockSandboxForLabels) Phase() string                          { return "" }
func (m *mockSandboxForLabels) InplaceRefresh(context.Context, bool) error {
	return nil
}
func (m *mockSandboxForLabels) Request(context.Context, string, string, int, io.Reader) (*http.Response, error) {
	return nil, nil
}
func (m *mockSandboxForLabels) CSIMount(context.Context, string, string) error {
	return nil
}
func (m *mockSandboxForLabels) CreateCheckpoint(context.Context, CreateCheckpointOptions) (string, error) {
	return "", nil
}

func TestMergePodLabels(t *testing.T) {
	tests := []struct {
		name           string
		existingLabels map[string]string
		inputLabels    map[string]string
		wantLabels     map[string]string
	}{
		{
			name:           "nil existing labels - initializes and sets all",
			existingLabels: nil,
			inputLabels:    map[string]string{"app": "sandbox", "env": "prod"},
			wantLabels:     map[string]string{"app": "sandbox", "env": "prod"},
		},
		{
			name:           "empty existing labels - initializes and sets all",
			existingLabels: map[string]string{},
			inputLabels:    map[string]string{"app": "sandbox"},
			wantLabels:     map[string]string{"app": "sandbox"},
		},
		{
			name:           "empty input labels - no change",
			existingLabels: map[string]string{"app": "sandbox"},
			inputLabels:    map[string]string{},
			wantLabels:     map[string]string{"app": "sandbox"},
		},
		{
			name:           "nil input labels - no change",
			existingLabels: map[string]string{"app": "sandbox"},
			inputLabels:    nil,
			wantLabels:     map[string]string{"app": "sandbox"},
		},
		{
			name:           "overwrite existing label with same key",
			existingLabels: map[string]string{"app": "old", "env": "dev"},
			inputLabels:    map[string]string{"app": "new"},
			wantLabels:     map[string]string{"app": "new", "env": "dev"},
		},
		{
			name:           "add new labels to existing",
			existingLabels: map[string]string{"app": "sandbox"},
			inputLabels:    map[string]string{"env": "prod", "tier": "frontend"},
			wantLabels:     map[string]string{"app": "sandbox", "env": "prod", "tier": "frontend"},
		},
		{
			name:           "both nil - no change",
			existingLabels: nil,
			inputLabels:    nil,
			wantLabels:     nil,
		},
		{
			name:           "both empty maps - no change",
			existingLabels: map[string]string{},
			inputLabels:    map[string]string{},
			wantLabels:     map[string]string{},
		},
		{
			name:           "empty string value - valid label",
			existingLabels: map[string]string{"app": "sandbox"},
			inputLabels:    map[string]string{"note": ""},
			wantLabels:     map[string]string{"app": "sandbox", "note": ""},
		},
		{
			name:           "overwrite all existing labels",
			existingLabels: map[string]string{"app": "old", "env": "dev"},
			inputLabels:    map[string]string{"app": "new", "env": "prod"},
			wantLabels:     map[string]string{"app": "new", "env": "prod"},
		},
		{
			name:           "kubernetes-style dotted label keys",
			existingLabels: map[string]string{"app.kubernetes.io/name": "sandbox"},
			inputLabels:    map[string]string{"app.kubernetes.io/instance": "prod", "app.kubernetes.io/managed-by": "kruise"},
			wantLabels:     map[string]string{"app.kubernetes.io/name": "sandbox", "app.kubernetes.io/instance": "prod", "app.kubernetes.io/managed-by": "kruise"},
		},
		{
			name:           "single label added to multiple existing",
			existingLabels: map[string]string{"app": "sandbox", "env": "prod", "tier": "frontend"},
			inputLabels:    map[string]string{"version": "v1"},
			wantLabels:     map[string]string{"app": "sandbox", "env": "prod", "tier": "frontend", "version": "v1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &mockSandboxForLabels{podLabels: tt.existingLabels, hasTemplate: true}
			MergePodLabels(sbx, tt.inputLabels)
			got := sbx.GetPodLabels()
			if len(got) != len(tt.wantLabels) {
				t.Errorf("MergePodLabels() labels count = %d, want %d, got=%v, want=%v", len(got), len(tt.wantLabels), got, tt.wantLabels)
				return
			}
			for k, wantVal := range tt.wantLabels {
				if gotVal, ok := got[k]; !ok || gotVal != wantVal {
					t.Errorf("MergePodLabels() label[%q] = %q, want %q", k, gotVal, wantVal)
				}
			}
		})
	}
}

func TestMergePodLabels_NilTemplate(t *testing.T) {
	// When the sandbox's pod template is nil, GetPodLabels returns nil and
	// SetPodLabels is a no-op. MergePodLabels should not panic and the labels
	// are silently dropped.
	sbx := &mockSandboxForLabels{}
	assert.NotPanics(t, func() {
		MergePodLabels(sbx, map[string]string{"app": "sandbox", "env": "prod"})
	})
	assert.Nil(t, sbx.GetPodLabels())
}

func TestMergePodLabels_Idempotent(t *testing.T) {
	// Calling MergePodLabels twice with the same labels should produce the
	// same result as calling it once.
	sbx := &mockSandboxForLabels{podLabels: map[string]string{"app": "sandbox"}, hasTemplate: true}
	input := map[string]string{"env": "prod", "tier": "frontend"}
	MergePodLabels(sbx, input)
	MergePodLabels(sbx, input)
	got := sbx.GetPodLabels()
	assert.Equal(t, map[string]string{
		"app":  "sandbox",
		"env":  "prod",
		"tier": "frontend",
	}, got)
}
