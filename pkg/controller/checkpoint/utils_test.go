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

package checkpoint

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/configuration"
)

func TestGetTemplateContainers(t *testing.T) {
	tests := []struct {
		name     string
		box      *agentsv1alpha1.Sandbox
		expected int
	}{
		{
			name: "template with containers",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx:1.21"},
									{Name: "sidecar", Image: "envoy:1.20"},
								},
							},
						},
					},
				},
			},
			expected: 2,
		},
		{
			name: "template is nil",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: nil,
					},
				},
			},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			containers := getTemplateContainers(tt.box)
			assert.Equal(t, tt.expected, len(containers))
		})
	}
}

func TestGetTemplateInitContainers(t *testing.T) {
	tests := []struct {
		name     string
		box      *agentsv1alpha1.Sandbox
		expected int
	}{
		{
			name: "template with init containers",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								InitContainers: []corev1.Container{
									{Name: "init-db", Image: "busybox:1.35"},
								},
							},
						},
					},
				},
			},
			expected: 1,
		},
		{
			name: "template is nil",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: nil,
					},
				},
			},
			expected: 0,
		},
		{
			name: "template with no init containers",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx:1.21"},
								},
							},
						},
					},
				},
			},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			containers := getTemplateInitContainers(tt.box)
			assert.Equal(t, tt.expected, len(containers))
		})
	}
}

func TestBuildMetadataDelta(t *testing.T) {
	tests := []struct {
		name      string
		pod       *corev1.Pod
		whitelist *configuration.SandboxResumePodPersistentContent
		checkFn   func(t *testing.T, meta metav1.ObjectMeta)
	}{
		{
			name: "whitelisted labels and annotations extracted",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"topology.kubernetes.io/zone": "cn-hangzhou-b",
						"app":                         "test",
					},
					Annotations: map[string]string{
						"scheduling.k8s.io/group-name": "pool-a",
						"non-whitelisted":              "ignored",
					},
				},
			},
			whitelist: &configuration.SandboxResumePodPersistentContent{
				LabelKeys:      []string{"topology.kubernetes.io/zone"},
				AnnotationKeys: []string{"scheduling.k8s.io/group-name"},
			},
			checkFn: func(t *testing.T, meta metav1.ObjectMeta) {
				assert.Equal(t, map[string]string{"topology.kubernetes.io/zone": "cn-hangzhou-b"}, meta.Labels)
				assert.Equal(t, map[string]string{"scheduling.k8s.io/group-name": "pool-a"}, meta.Annotations)
			},
		},
		{
			name: "nil whitelist - empty metadata",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"app": "test"},
					Annotations: map[string]string{"key": "val"},
				},
			},
			whitelist: nil,
			checkFn: func(t *testing.T, meta metav1.ObjectMeta) {
				assert.Nil(t, meta.Labels)
				assert.Nil(t, meta.Annotations)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configuration.SetSandboxResumePodPersistentContentForTest(tt.whitelist)
			defer configuration.SetSandboxResumePodPersistentContentForTest(nil)

			meta := buildMetadataDelta(tt.pod)
			tt.checkFn(t, meta)
		})
	}
}

func TestBuildTemplateContainerDelta(t *testing.T) {
	tests := []struct {
		name     string
		pod      *corev1.Pod
		box      *agentsv1alpha1.Sandbox
		expected []string
	}{
		{
			name: "resource drift detected",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "nginx:1.21",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("2"),
								},
							},
						},
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "main",
										Image: "nginx:1.21",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU: resource.MustParse("1"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: []string{"main"},
		},
		{
			name: "no resource drift - empty result",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "nginx:1.21",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("1"),
								},
							},
						},
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "main",
										Image: "nginx:1.21",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU: resource.MustParse("1"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: nil,
		},
		{
			name: "non-template container skipped",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
						{Name: "istio-proxy", Image: "istio/proxyv2:1.20"},
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx:1.21"},
								},
							},
						},
					},
				},
			},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildTemplateContainerDelta(tt.pod, tt.box)
			if tt.expected == nil {
				assert.Empty(t, result)
			} else {
				assert.Equal(t, len(tt.expected), len(result))
				for i, name := range tt.expected {
					assert.Equal(t, name, result[i].Name)
					assert.NotEmpty(t, result[i].Resources)
				}
			}
		})
	}
}

func TestBuildInjectedContainerDelta(t *testing.T) {
	tests := []struct {
		name                   string
		pod                    *corev1.Pod
		box                    *agentsv1alpha1.Sandbox
		expectedContainers     []string
		expectedInitContainers []string
	}{
		{
			name: "runtime and webhook containers extracted",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
						{Name: "agent-runtime", Image: "runtime:v1.0"},
						{Name: "istio-proxy", Image: "istio/proxyv2:1.20"},
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx:1.21"},
								},
							},
						},
					},
				},
			},
			expectedContainers:     []string{"agent-runtime", "istio-proxy"},
			expectedInitContainers: nil,
		},
		{
			name: "webhook init containers extracted",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
					},
					InitContainers: []corev1.Container{
						{Name: "init-db", Image: "busybox:1.35"},
						{Name: "istio-init", Image: "istio/proxyv2:1.20"},
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx:1.21"},
								},
								InitContainers: []corev1.Container{
									{Name: "init-db", Image: "busybox:1.35"},
								},
							},
						},
					},
				},
			},
			expectedContainers:     nil,
			expectedInitContainers: []string{"istio-init"},
		},
		{
			name: "all containers are template - empty result",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx:1.21"},
								},
							},
						},
					},
				},
			},
			expectedContainers:     nil,
			expectedInitContainers: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			containers, initContainers := buildInjectedContainerDelta(tt.pod, tt.box)
			if tt.expectedContainers == nil {
				assert.Empty(t, containers)
			} else {
				assert.Equal(t, len(tt.expectedContainers), len(containers))
				for i, name := range tt.expectedContainers {
					assert.Equal(t, name, containers[i].Name)
				}
			}
			if tt.expectedInitContainers == nil {
				assert.Empty(t, initContainers)
			} else {
				assert.Equal(t, len(tt.expectedInitContainers), len(initContainers))
				for i, name := range tt.expectedInitContainers {
					assert.Equal(t, name, initContainers[i].Name)
				}
			}
		})
	}
}

func TestBuildPodTemplateDelta(t *testing.T) {
	newSandbox := func(containers []corev1.Container) *agentsv1alpha1.Sandbox {
		return &agentsv1alpha1.Sandbox{
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: containers},
					},
				},
			},
		}
	}

	tests := []struct {
		name        string
		pod         *corev1.Pod
		box         *agentsv1alpha1.Sandbox
		whitelist   *configuration.SandboxResumePodPersistentContent
		expectError string
		checkFn     func(t *testing.T, delta runtime.RawExtension)
	}{
		{
			name: "no difference - empty delta",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
					},
				},
			},
			box:         newSandbox([]corev1.Container{{Name: "main", Image: "nginx:1.21"}}),
			whitelist:   nil,
			expectError: "",
			checkFn: func(t *testing.T, delta runtime.RawExtension) {
				assert.Nil(t, delta.Raw)
			},
		},
		{
			name: "resource drift captured",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "nginx:1.21",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("2"),
									corev1.ResourceMemory: resource.MustParse("4Gi"),
								},
							},
						},
					},
				},
			},
			box: newSandbox([]corev1.Container{{
				Name:  "main",
				Image: "nginx:1.21",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("1"),
						corev1.ResourceMemory: resource.MustParse("2Gi"),
					},
				},
			}}),
			whitelist:   nil,
			expectError: "",
			checkFn: func(t *testing.T, delta runtime.RawExtension) {
				assert.NotNil(t, delta.Raw)
				var patch map[string]any
				err := json.Unmarshal(delta.Raw, &patch)
				assert.NoError(t, err)
				spec, ok := patch["spec"].(map[string]any)
				assert.True(t, ok, "delta should contain spec")
				containers, ok := spec["containers"].([]any)
				assert.True(t, ok, "delta spec should contain containers")
				assert.Greater(t, len(containers), 0)
			},
		},
		{
			name: "extra container captured",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
						{Name: "istio-proxy", Image: "istio/proxyv2:1.20"},
					},
				},
			},
			box:         newSandbox([]corev1.Container{{Name: "main", Image: "nginx:1.21"}}),
			whitelist:   nil,
			expectError: "",
			checkFn: func(t *testing.T, delta runtime.RawExtension) {
				assert.NotNil(t, delta.Raw)
				var patch map[string]any
				err := json.Unmarshal(delta.Raw, &patch)
				assert.NoError(t, err)
				spec, ok := patch["spec"].(map[string]any)
				assert.True(t, ok)
				containers, ok := spec["containers"].([]any)
				assert.True(t, ok)
				assert.GreaterOrEqual(t, len(containers), 1)
				found := false
				for _, c := range containers {
					cMap, ok := c.(map[string]any)
					if ok && cMap["name"] == "istio-proxy" {
						found = true
					}
				}
				assert.True(t, found, "delta should contain istio-proxy container")
			},
		},
		{
			name: "annotation drift captured with whitelist",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"scheduling.k8s.io/group-name": "sandbox-pool-a",
						"non-whitelisted":              "ignored",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
					},
				},
			},
			box: newSandbox([]corev1.Container{{Name: "main", Image: "nginx:1.21"}}),
			whitelist: &configuration.SandboxResumePodPersistentContent{
				AnnotationKeys: []string{"scheduling.k8s.io/group-name"},
			},
			expectError: "",
			checkFn: func(t *testing.T, delta runtime.RawExtension) {
				assert.NotNil(t, delta.Raw)
				var patch map[string]any
				err := json.Unmarshal(delta.Raw, &patch)
				assert.NoError(t, err)
				metadata, ok := patch["metadata"].(map[string]any)
				assert.True(t, ok, "delta should contain metadata")
				annotations, ok := metadata["annotations"].(map[string]any)
				assert.True(t, ok, "metadata should contain annotations")
				assert.Equal(t, "sandbox-pool-a", annotations["scheduling.k8s.io/group-name"])
				_, exists := annotations["non-whitelisted"]
				assert.False(t, exists)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configuration.SetSandboxResumePodPersistentContentForTest(tt.whitelist)
			defer configuration.SetSandboxResumePodPersistentContentForTest(nil)

			delta, err := BuildPodTemplateDelta(tt.pod, tt.box)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
			if tt.checkFn != nil && tt.expectError == "" {
				tt.checkFn(t, delta)
			}
		})
	}
}

func TestApplyPodTemplateDelta(t *testing.T) {
	tests := []struct {
		name        string
		pod         *corev1.Pod
		delta       runtime.RawExtension
		expectError string
		checkFn     func(t *testing.T, pod *corev1.Pod)
	}{
		{
			name: "nil delta - no change",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
					},
				},
			},
			delta:       runtime.RawExtension{},
			expectError: "",
			checkFn: func(t *testing.T, pod *corev1.Pod) {
				assert.Equal(t, 1, len(pod.Spec.Containers))
				assert.Equal(t, "nginx:1.21", pod.Spec.Containers[0].Image)
			},
		},
		{
			name: "apply resource change",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "nginx:1.21",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("1"),
								},
							},
						},
					},
				},
			},
			delta: runtime.RawExtension{
				Raw: []byte(`{"spec":{"containers":[{"name":"main","resources":{"requests":{"cpu":"2"}}}]}}`),
			},
			expectError: "",
			checkFn: func(t *testing.T, pod *corev1.Pod) {
				assert.Equal(t, 1, len(pod.Spec.Containers))
				cpu := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
				assert.Equal(t, "2", cpu.String())
			},
		},
		{
			name: "apply extra container",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
					},
				},
			},
			delta: runtime.RawExtension{
				Raw: []byte(`{"spec":{"containers":[{"name":"main","image":"nginx:1.21"},{"name":"istio-proxy","image":"istio/proxyv2:1.20"}]}}`),
			},
			expectError: "",
			checkFn: func(t *testing.T, pod *corev1.Pod) {
				assert.Equal(t, 2, len(pod.Spec.Containers))
				found := false
				for _, c := range pod.Spec.Containers {
					if c.Name == "istio-proxy" {
						found = true
						assert.Equal(t, "istio/proxyv2:1.20", c.Image)
					}
				}
				assert.True(t, found, "istio-proxy container should be present")
			},
		},
		{
			name: "invalid JSON delta - returns error",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
					},
				},
			},
			delta: runtime.RawExtension{
				Raw: []byte(`invalid-json`),
			},
			expectError: "failed to apply strategic merge patch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ApplyPodTemplateDelta(tt.pod, tt.delta)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
			if tt.checkFn != nil && tt.expectError == "" {
				tt.checkFn(t, tt.pod)
			}
		})
	}
}
func TestBuildAndApplyPodTemplateDeltaRoundTrip(t *testing.T) {
	configuration.SetSandboxResumePodPersistentContentForTest(&configuration.SandboxResumePodPersistentContent{
		AnnotationKeys: []string{"scheduling.k8s.io/group-name"},
		LabelKeys:      []string{"topology.kubernetes.io/zone"},
	})
	defer configuration.SetSandboxResumePodPersistentContentForTest(nil)

	livePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"scheduling.k8s.io/group-name": "sandbox-pool-a",
			},
			Labels: map[string]string{
				"topology.kubernetes.io/zone": "cn-hangzhou-b",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx:1.21",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("2"),
							corev1.ResourceMemory: resource.MustParse("4Gi"),
						},
					},
				},
				{
					Name:  "istio-proxy",
					Image: "istio/proxyv2:1.20",
				},
			},
		},
	}

	box := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "main",
								Image: "nginx:1.21",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("1"),
										corev1.ResourceMemory: resource.MustParse("2Gi"),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	delta, err := BuildPodTemplateDelta(livePod, box)
	assert.NoError(t, err)
	assert.NotNil(t, delta.Raw)

	resumeBasePod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx:1.21",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1"),
							corev1.ResourceMemory: resource.MustParse("2Gi"),
						},
					},
				},
			},
		},
	}

	err = ApplyPodTemplateDelta(resumeBasePod, delta)
	assert.NoError(t, err)

	assert.Equal(t, "sandbox-pool-a", resumeBasePod.Annotations["scheduling.k8s.io/group-name"])
	assert.Equal(t, "cn-hangzhou-b", resumeBasePod.Labels["topology.kubernetes.io/zone"])

	mainContainer := resumeBasePod.Spec.Containers[0]
	assert.Equal(t, "main", mainContainer.Name)
	cpu := mainContainer.Resources.Requests[corev1.ResourceCPU]
	assert.Equal(t, "2", cpu.String())
	mem := mainContainer.Resources.Requests[corev1.ResourceMemory]
	assert.Equal(t, "4Gi", mem.String())

	assert.Equal(t, 2, len(resumeBasePod.Spec.Containers))
	istioFound := false
	for _, c := range resumeBasePod.Spec.Containers {
		if c.Name == "istio-proxy" {
			istioFound = true
			assert.Equal(t, "istio/proxyv2:1.20", c.Image)
		}
	}
	assert.True(t, istioFound, "istio-proxy container should be present after applying delta")
}

func TestBuildAndApplyPodTemplateDeltaRoundTripWithTemplateLabels(t *testing.T) {
	configuration.SetSandboxResumePodPersistentContentForTest(&configuration.SandboxResumePodPersistentContent{
		AnnotationKeys: []string{"scheduling.k8s.io/group-name"},
		LabelKeys:      []string{"topology.kubernetes.io/zone"},
	})
	defer configuration.SetSandboxResumePodPersistentContentForTest(nil)

	livePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				"app":                         "sandbox-foo",
				"topology.kubernetes.io/zone": "cn-hangzhou-b",
				"controller-uid":              "uid-12345",
			},
			Annotations: map[string]string{
				"scheduling.k8s.io/group-name":                     "sandbox-pool-a",
				"kubectl.kubernetes.io/last-applied-configuration": "{}",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx:1.21",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("2"),
							corev1.ResourceMemory: resource.MustParse("4Gi"),
						},
					},
				},
				{Name: "agent-runtime", Image: "runtime:v1.0"},
				{Name: "istio-proxy", Image: "istio/proxyv2:1.20"},
			},
		},
	}

	box := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "main",
								Image: "nginx:1.21",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("1"),
										corev1.ResourceMemory: resource.MustParse("2Gi"),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	delta, err := BuildPodTemplateDelta(livePod, box)
	assert.NoError(t, err)
	assert.NotNil(t, delta.Raw)

	resumeBasePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				"app": "sandbox-foo",
			},
			Annotations: map[string]string{
				"agents.kruise.io/sandbox-version": "v1",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "nginx:1.21",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1"),
							corev1.ResourceMemory: resource.MustParse("2Gi"),
						},
					},
				},
				{Name: "agent-runtime", Image: "runtime:v1.0"},
			},
		},
	}

	err = ApplyPodTemplateDelta(resumeBasePod, delta)
	assert.NoError(t, err)

	assert.Equal(t, "sandbox-foo", resumeBasePod.Labels["app"])
	assert.Equal(t, "cn-hangzhou-b", resumeBasePod.Labels["topology.kubernetes.io/zone"])
	_, hasControllerUID := resumeBasePod.Labels["controller-uid"]
	assert.False(t, hasControllerUID, "non-whitelisted label should not appear")

	assert.Equal(t, "v1", resumeBasePod.Annotations["agents.kruise.io/sandbox-version"])
	assert.Equal(t, "sandbox-pool-a", resumeBasePod.Annotations["scheduling.k8s.io/group-name"])
	_, hasLastApplied := resumeBasePod.Annotations["kubectl.kubernetes.io/last-applied-configuration"]
	assert.False(t, hasLastApplied, "non-whitelisted annotation should not appear")

	mainContainer := resumeBasePod.Spec.Containers[0]
	assert.Equal(t, "main", mainContainer.Name)
	assert.Equal(t, "2", mainContainer.Resources.Requests.Cpu().String())
	assert.Equal(t, "4Gi", mainContainer.Resources.Requests.Memory().String())

	runtimeFound := false
	for _, c := range resumeBasePod.Spec.Containers {
		if c.Name == "agent-runtime" {
			runtimeFound = true
			assert.Equal(t, "runtime:v1.0", c.Image)
		}
	}
	assert.True(t, runtimeFound, "runtime container should be preserved")

	assert.Equal(t, 3, len(resumeBasePod.Spec.Containers))
	istioFound := false
	for _, c := range resumeBasePod.Spec.Containers {
		if c.Name == "istio-proxy" {
			istioFound = true
			assert.Equal(t, "istio/proxyv2:1.20", c.Image)
		}
	}
	assert.True(t, istioFound, "webhook-injected container should be added by delta")
}

// TestApplyPodTemplateDelta_PreservesContainerOrder verifies that the
// $setElementOrder/containers directive emitted by BuildPodTemplateDelta
// rewrites the resumed Pod's container slice to match the live Pod observed
// at pause time, even when the resume-side base Pod was generated with a
// different sidecar order (e.g. InjectSandboxRuntimes injecting agent-runtime
// before csi-mount instead of the reverse). Without this directive the
// resume path keeps the wrong base order and breaks startup dependencies.
func TestApplyPodTemplateDelta_PreservesContainerOrder(t *testing.T) {
	tests := []struct {
		name          string
		livePod       *corev1.Pod
		box           *agentsv1alpha1.Sandbox
		resumeBasePod *corev1.Pod
		expectOrder   []string
		expectInitOrd []string
	}{
		{
			name: "sidecars in live pod order, base pod has them swapped",
			livePod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
						{Name: "csi-mount", Image: "csi:v1"},
						{Name: "agent-runtime", Image: "runtime:v1"},
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx:1.21"},
								},
							},
						},
					},
				},
			},
			resumeBasePod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
						// Wrong order produced by the resume-side injection.
						{Name: "agent-runtime", Image: "runtime:v1"},
						{Name: "csi-mount", Image: "csi:v1"},
					},
				},
			},
			expectOrder: []string{"main", "csi-mount", "agent-runtime"},
		},
		{
			name: "injected container missing in base pod is appended in correct slot",
			livePod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
						{Name: "csi-mount", Image: "csi:v1"},
						{Name: "agent-runtime", Image: "runtime:v1"},
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx:1.21"},
								},
							},
						},
					},
				},
			},
			resumeBasePod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
						// agent-runtime missing entirely from base.
						{Name: "csi-mount", Image: "csi:v1"},
					},
				},
			},
			expectOrder: []string{"main", "csi-mount", "agent-runtime"},
		},
		{
			name: "init containers order also corrected",
			livePod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{Name: "init-csi", Image: "csi-init:v1"},
						{Name: "init-runtime", Image: "runtime-init:v1"},
					},
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx:1.21"},
								},
							},
						},
					},
				},
			},
			resumeBasePod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{Name: "init-runtime", Image: "runtime-init:v1"},
						{Name: "init-csi", Image: "csi-init:v1"},
					},
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
					},
				},
			},
			expectOrder:   []string{"main", "init-csi", "init-runtime"}, // unused for containers comparison below
			expectInitOrd: []string{"init-csi", "init-runtime"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			delta, err := BuildPodTemplateDelta(tt.livePod, tt.box)
			assert.NoError(t, err)
			assert.NotNil(t, delta.Raw, "delta should not be empty")

			// Sanity check: the patch should embed $setElementOrder/<list>
			// directives so the SMP layer can rewrite container order.
			var patch map[string]any
			assert.NoError(t, json.Unmarshal(delta.Raw, &patch))
			spec, _ := patch["spec"].(map[string]any)
			assert.NotNil(t, spec)
			if tt.expectInitOrd == nil && len(tt.livePod.Spec.Containers) > 0 {
				_, ok := spec["$setElementOrder/containers"]
				assert.True(t, ok, "patch should include $setElementOrder/containers directive")
			}
			if len(tt.expectInitOrd) > 0 {
				_, ok := spec["$setElementOrder/initContainers"]
				assert.True(t, ok, "patch should include $setElementOrder/initContainers directive")
			}

			assert.NoError(t, ApplyPodTemplateDelta(tt.resumeBasePod, delta))

			if tt.expectInitOrd != nil {
				actualInit := make([]string, 0, len(tt.resumeBasePod.Spec.InitContainers))
				for _, c := range tt.resumeBasePod.Spec.InitContainers {
					actualInit = append(actualInit, c.Name)
				}
				assert.Equal(t, tt.expectInitOrd, actualInit, "init container order should match the live pod")
			} else {
				actual := make([]string, 0, len(tt.resumeBasePod.Spec.Containers))
				for _, c := range tt.resumeBasePod.Spec.Containers {
					actual = append(actual, c.Name)
				}
				assert.Equal(t, tt.expectOrder, actual, "container order should match the live pod")
			}
		})
	}
}

// TestBuildContainerNameOrder verifies the helper directly so callers that
// rely on the partial-key list shape (used by the SMP $setElementOrder/<list>
// directive) are protected against regressions.
func TestBuildContainerNameOrder(t *testing.T) {
	tests := []struct {
		name     string
		input    []corev1.Container
		expected []map[string]string
	}{
		{
			name:     "empty input returns nil",
			input:    nil,
			expected: nil,
		},
		{
			name: "order preserved",
			input: []corev1.Container{
				{Name: "main"},
				{Name: "csi-mount"},
				{Name: "agent-runtime"},
			},
			expected: []map[string]string{
				{"name": "main"},
				{"name": "csi-mount"},
				{"name": "agent-runtime"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, buildContainerNameOrder(tt.input))
		})
	}
}
