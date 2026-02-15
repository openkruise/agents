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

package defaults

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestSetDefaultPodVolumes(t *testing.T) {
	tests := []struct {
		name     string
		input    []corev1.Volume
		expected []corev1.Volume
	}{
		{
			name:     "empty volumes",
			input:    []corev1.Volume{},
			expected: []corev1.Volume{},
		},
		{
			name: "host path volume",
			input: []corev1.Volume{
				{
					Name: "host-path-vol",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: "/tmp/data",
							// Type will be defaulted
						},
					},
				},
			},
			expected: []corev1.Volume{
				{
					Name: "host-path-vol",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: "/tmp/data",
							// Type will be defaulted to "" which means "DirectoryOrCreate"
						},
					},
				},
			},
		},
		{
			name: "secret volume",
			input: []corev1.Volume{
				{
					Name: "secret-vol",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: "my-secret",
						},
					},
				},
			},
			expected: []corev1.Volume{
				{
					Name: "secret-vol",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: "my-secret",
							// Default mode will be set
						},
					},
				},
			},
		},
		{
			name: "config map volume",
			input: []corev1.Volume{
				{
					Name: "configmap-vol",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: "my-configmap",
							},
						},
					},
				},
			},
			expected: []corev1.Volume{
				{
					Name: "configmap-vol",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: "my-configmap",
							},
							// Default mode will be set
						},
					},
				},
			},
		},
		{
			name: "projected volume",
			input: []corev1.Volume{
				{
					Name: "projected-vol",
					VolumeSource: corev1.VolumeSource{
						Projected: &corev1.ProjectedVolumeSource{
							Sources: []corev1.VolumeProjection{
								{
									Secret: &corev1.SecretProjection{
										LocalObjectReference: corev1.LocalObjectReference{
											Name: "secret1",
										},
									},
								},
							},
						},
					},
				},
			},
			expected: []corev1.Volume{
				{
					Name: "projected-vol",
					VolumeSource: corev1.VolumeSource{
						Projected: &corev1.ProjectedVolumeSource{
							Sources: []corev1.VolumeProjection{
								{
									Secret: &corev1.SecretProjection{
										LocalObjectReference: corev1.LocalObjectReference{
											Name: "secret1",
										},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "downward API volume",
			input: []corev1.Volume{
				{
					Name: "downward-api-vol",
					VolumeSource: corev1.VolumeSource{
						DownwardAPI: &corev1.DownwardAPIVolumeSource{
							Items: []corev1.DownwardAPIVolumeFile{
								{
									Path: "labels",
									FieldRef: &corev1.ObjectFieldSelector{
										APIVersion: "v1",
										FieldPath:  "metadata.labels",
									},
								},
							},
						},
					},
				},
			},
			expected: []corev1.Volume{
				{
					Name: "downward-api-vol",
					VolumeSource: corev1.VolumeSource{
						DownwardAPI: &corev1.DownwardAPIVolumeSource{
							Items: []corev1.DownwardAPIVolumeFile{
								{
									Path: "labels",
									FieldRef: &corev1.ObjectFieldSelector{
										APIVersion: "v1",
										FieldPath:  "metadata.labels",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setDefaultPodVolumes(tt.input)

			assert.Equal(t, len(tt.expected), len(tt.input))

			for i := range tt.input {
				assert.Equal(t, tt.expected[i].Name, tt.input[i].Name)

				if tt.expected[i].HostPath != nil && tt.input[i].HostPath != nil {
					if tt.expected[i].HostPath.Type != nil && tt.input[i].HostPath.Type != nil {
						assert.Equal(t, *tt.expected[i].HostPath.Type, *tt.input[i].HostPath.Type)
					}

				}

				if tt.expected[i].Secret != nil && tt.input[i].Secret != nil {
					if tt.expected[i].Secret.DefaultMode == nil {
						assert.NotNil(t, tt.input[i].Secret.DefaultMode)
					} else {
						assert.Equal(t, *tt.expected[i].Secret.DefaultMode, *tt.input[i].Secret.DefaultMode)
					}
				}

				if tt.expected[i].ConfigMap != nil && tt.input[i].ConfigMap != nil {
					if tt.expected[i].ConfigMap.DefaultMode == nil {
						assert.NotNil(t, tt.input[i].ConfigMap.DefaultMode)
					} else {
						assert.Equal(t, *tt.expected[i].ConfigMap.DefaultMode, *tt.input[i].ConfigMap.DefaultMode)
					}
				}

				if tt.expected[i].DownwardAPI != nil && tt.input[i].DownwardAPI != nil {
					for j, expectedItem := range tt.expected[i].DownwardAPI.Items {
						if expectedItem.FieldRef != nil && tt.input[i].DownwardAPI.Items[j].FieldRef != nil {
							assert.Equal(t, expectedItem.FieldRef.APIVersion, tt.input[i].DownwardAPI.Items[j].FieldRef.APIVersion)
						}
					}
				}
			}
		})
	}
}

func TestSetDefaultPodSpec(t *testing.T) {
	tests := []struct {
		name string
		spec *corev1.PodSpec
	}{
		{
			name: "empty pod spec",
			spec: &corev1.PodSpec{},
		},
		{
			name: "pod spec with containers",
			spec: &corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test-container",
						Image: "test-image",
						Ports: []corev1.ContainerPort{
							{
								ContainerPort: 8080,
								// Protocol will be defaulted
							},
						},
					},
				},
			},
		},
		{
			name: "pod spec with init containers",
			spec: &corev1.PodSpec{
				InitContainers: []corev1.Container{
					{
						Name:  "init-container",
						Image: "init-image",
						Ports: []corev1.ContainerPort{
							{
								ContainerPort: 9090,
							},
						},
					},
				},
			},
		},
		{
			name: "pod spec with ephemeral containers",
			spec: &corev1.PodSpec{
				EphemeralContainers: []corev1.EphemeralContainer{
					{
						EphemeralContainerCommon: corev1.EphemeralContainerCommon{
							Name:  "ephemeral-container",
							Image: "ephemeral-image",
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: 8080,
								},
							},
						},
					},
				},
			},
		},
		{
			name: "pod spec with probes",
			spec: &corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test-container",
						Image: "test-image",
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/health",
									Port: intstr.FromInt32(8080),
								},
							},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/ready",
									Port: intstr.FromInt32(8080),
								},
							},
						},
						StartupProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/startup",
									Port: intstr.FromInt32(8080),
								},
							},
						},
					},
				},
			},
		},
		{
			name: "pod spec with lifecycle hooks",
			spec: &corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test-container",
						Image: "test-image",
						Lifecycle: &corev1.Lifecycle{
							PostStart: &corev1.LifecycleHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/poststart",
									Port: intstr.FromInt32(8080),
								},
							},
							PreStop: &corev1.LifecycleHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/prestop",
									Port: intstr.FromInt32(8080),
								},
							},
						},
					},
				},
			},
		},
		{
			name: "pod spec with env field ref",
			spec: &corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test-container",
						Image: "test-image",
						Env: []corev1.EnvVar{
							{
								Name: "POD_NAME",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "metadata.name",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Make a copy to avoid modifying the original
			specCopy := tt.spec.DeepCopy()
			SetDefaultPodSpec(specCopy)

			// Verify that defaults were applied
			assert.NotNil(t, specCopy)

			// Verify container ports have protocol defaulted
			for i := range specCopy.Containers {
				for j := range specCopy.Containers[i].Ports {
					if specCopy.Containers[i].Ports[j].Protocol == "" {
						assert.Equal(t, corev1.ProtocolTCP, specCopy.Containers[i].Ports[j].Protocol)
					}
				}
			}

			// Verify init container ports have protocol defaulted
			for i := range specCopy.InitContainers {
				for j := range specCopy.InitContainers[i].Ports {
					if specCopy.InitContainers[i].Ports[j].Protocol == "" {
						assert.Equal(t, corev1.ProtocolTCP, specCopy.InitContainers[i].Ports[j].Protocol)
					}
				}
			}

			// Verify ephemeral container ports have protocol defaulted
			for i := range specCopy.EphemeralContainers {
				for j := range specCopy.EphemeralContainers[i].EphemeralContainerCommon.Ports {
					if specCopy.EphemeralContainers[i].EphemeralContainerCommon.Ports[j].Protocol == "" {
						assert.Equal(t, corev1.ProtocolTCP, specCopy.EphemeralContainers[i].EphemeralContainerCommon.Ports[j].Protocol)
					}
				}
			}
		})
	}
}

func TestSetDefaultContainerPorts(t *testing.T) {
	tests := []struct {
		name      string
		container *corev1.Container
	}{
		{
			name: "container with port without protocol",
			container: &corev1.Container{
				Ports: []corev1.ContainerPort{
					{
						ContainerPort: 8080,
					},
				},
			},
		},
		{
			name: "container with port with UDP protocol",
			container: &corev1.Container{
				Ports: []corev1.ContainerPort{
					{
						ContainerPort: 8080,
						Protocol:      corev1.ProtocolUDP,
					},
				},
			},
		},
		{
			name: "container with multiple ports",
			container: &corev1.Container{
				Ports: []corev1.ContainerPort{
					{
						ContainerPort: 8080,
					},
					{
						ContainerPort: 9090,
						Protocol:      corev1.ProtocolUDP,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setDefaultContainerPorts(tt.container)

			for i := range tt.container.Ports {
				if tt.container.Ports[i].Protocol == "" {
					assert.Equal(t, corev1.ProtocolTCP, tt.container.Ports[i].Protocol)
				}
			}
		})
	}
}

func TestSetDefaultEphemeralContainerPorts(t *testing.T) {
	tests := []struct {
		name      string
		container *corev1.EphemeralContainer
	}{
		{
			name: "ephemeral container with port without protocol",
			container: &corev1.EphemeralContainer{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon{
					Ports: []corev1.ContainerPort{
						{
							ContainerPort: 8080,
						},
					},
				},
			},
		},
		{
			name: "ephemeral container with port with UDP protocol",
			container: &corev1.EphemeralContainer{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon{
					Ports: []corev1.ContainerPort{
						{
							ContainerPort: 8080,
							Protocol:      corev1.ProtocolUDP,
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setDefaultEphemeralContainerPorts(tt.container)

			for i := range tt.container.EphemeralContainerCommon.Ports {
				if tt.container.EphemeralContainerCommon.Ports[i].Protocol == "" {
					assert.Equal(t, corev1.ProtocolTCP, tt.container.EphemeralContainerCommon.Ports[i].Protocol)
				}
			}
		})
	}
}