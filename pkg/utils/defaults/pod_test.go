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
