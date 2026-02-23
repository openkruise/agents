/*
Copyright 2025.
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

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHashSandbox(t *testing.T) {
	tests := []struct {
		name                              string
		sandbox                           *agentsv1alpha1.Sandbox
		expectedHash                      string
		expectedHashWithoutImageResources string
		validateDifferentHashes           bool
	}{
		{
			name: "basic sandbox with containers",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: "nginx:latest",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("100m"),
												corev1.ResourceMemory: resource.MustParse("128Mi"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
			validateDifferentHashes: true,
		},
		{
			name: "sandbox with init containers",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								InitContainers: []corev1.Container{
									{
										Name:  "init-container",
										Image: "busybox:latest",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("50m"),
												corev1.ResourceMemory: resource.MustParse("64Mi"),
											},
										},
									},
								},
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: "nginx:latest",
									},
								},
							},
						},
					},
				},
			},
			validateDifferentHashes: true,
		},
		{
			name: "sandbox with multiple containers",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								InitContainers: []corev1.Container{
									{
										Name:  "init-container-1",
										Image: "busybox:1.28",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("50m"),
												corev1.ResourceMemory: resource.MustParse("64Mi"),
											},
										},
									},
									{
										Name:  "init-container-2",
										Image: "alpine:latest",
									},
								},
								Containers: []corev1.Container{
									{
										Name:  "app-container",
										Image: "myapp:1.0",
										Resources: corev1.ResourceRequirements{
											Limits: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("500m"),
												corev1.ResourceMemory: resource.MustParse("512Mi"),
											},
										},
									},
									{
										Name:  "sidecar-container",
										Image: "sidecar:latest",
									},
								},
							},
						},
					},
				},
			},
			validateDifferentHashes: true,
		},
		{
			name: "sandbox with empty containers",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{},
							},
						},
					},
				},
			},
			validateDifferentHashes: false, // Both hashes should be the same when no containers have images/resources
		},
		{
			name: "sandbox with volumes and other fields",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"app": "test",
								},
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: "nginx:latest",
									},
								},
								Volumes: []corev1.Volume{
									{
										Name: "test-volume",
										VolumeSource: corev1.VolumeSource{
											EmptyDir: &corev1.EmptyDirVolumeSource{},
										},
									},
								},
								NodeSelector: map[string]string{
									"kubernetes.io/os": "linux",
								},
							},
						},
					},
				},
			},
			validateDifferentHashes: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash, hashWithoutImageResources := HashSandbox(tt.sandbox)

			// Verify both hashes are not empty
			if hash == "" {
				t.Errorf("HashSandbox() returned empty hash")
			}
			if hashWithoutImageResources == "" {
				t.Errorf("HashSandbox() returned empty hashWithoutImageResources")
			}

			// Verify consistency - same input should always produce same output
			hash2, hashWithoutImageResources2 := HashSandbox(tt.sandbox)
			if hash != hash2 {
				t.Errorf("HashSandbox() is not consistent for hash: got %s, want %s", hash, hash2)
			}
			if hashWithoutImageResources != hashWithoutImageResources2 {
				t.Errorf("HashSandbox() is not consistent for hashWithoutImageResources: got %s, want %s", hashWithoutImageResources, hashWithoutImageResources2)
			}

			// Validate that hashes have expected format (from HashData function)
			if len(hash) < 5 || len(hashWithoutImageResources) < 5 { // Basic length check
				t.Errorf("HashSandbox() returned hashes that are too short: %s, %s", hash, hashWithoutImageResources)
			}

			// Check if the hashes should be different based on the presence of images/resources
			if tt.validateDifferentHashes {
				if hash == hashWithoutImageResources {
					t.Errorf("Expected different hashes when image/resources are present, but got same: %s", hash)
				}
			} else {
				if hash != hashWithoutImageResources {
					t.Errorf("Expected same hashes when no image/resources differences, but got different: %s vs %s", hash, hashWithoutImageResources)
				}
			}
		})
	}
}

func TestHashSandboxWithDifferentImages(t *testing.T) {
	// Test that changing only image results in different full hash but same hash without image/resources
	sandbox1 := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:1.19", // Different image
							},
						},
					},
				},
			},
		},
	}

	sandbox2 := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:1.20", // Different image
							},
						},
					},
				},
			},
		},
	}

	hash1, hashWithoutImageResources1 := HashSandbox(sandbox1)
	hash2, hashWithoutImageResources2 := HashSandbox(sandbox2)

	// Full hashes should be different because images are different
	if hash1 == hash2 {
		t.Errorf("Expected different full hashes for different images, but got same: %s", hash1)
	}

	// Hashes without images/resources should be the same
	if hashWithoutImageResources1 != hashWithoutImageResources2 {
		t.Errorf("Expected same hashes without images/resources, but got different: %s vs %s",
			hashWithoutImageResources1, hashWithoutImageResources2)
	}
}

func TestHashSandboxWithDifferentResources(t *testing.T) {
	// Test that changing only resources results in different full hash but same hash without image/resources
	sandbox1 := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:latest",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("100m"),
										corev1.ResourceMemory: resource.MustParse("128Mi"),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	sandbox2 := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:latest",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("200m"),  // Different resource
										corev1.ResourceMemory: resource.MustParse("256Mi"), // Different resource
									},
								},
							},
						},
					},
				},
			},
		},
	}

	hash1, hashWithoutImageResources1 := HashSandbox(sandbox1)
	hash2, hashWithoutImageResources2 := HashSandbox(sandbox2)

	// Full hashes should be different because resources are different
	if hash1 == hash2 {
		t.Errorf("Expected different full hashes for different resources, but got same: %s", hash1)
	}

	// Hashes without images/resources should be the same
	if hashWithoutImageResources1 != hashWithoutImageResources2 {
		t.Errorf("Expected same hashes without images/resources, but got different: %s vs %s",
			hashWithoutImageResources1, hashWithoutImageResources2)
	}
}

func TestGeneratePVCName(t *testing.T) {
	tests := []struct {
		name         string
		templateName string
		sandboxName  string
		expectError  bool
		expectName   string
	}{
		{
			name:         "normal case",
			templateName: "www",
			sandboxName:  "test-sandbox",
			expectError:  false,
			expectName:   "www-test-sandbox",
		},
		{
			name:         "template name with hyphen",
			templateName: "data-volume",
			sandboxName:  "test-sandbox",
			expectError:  false,
			expectName:   "data-volume-test-sandbox",
		},
		{
			name:         "sandbox name with number",
			templateName: "cache",
			sandboxName:  "app-123",
			expectError:  false,
			expectName:   "cache-app-123",
		},
		{
			name:         "empty template name",
			templateName: "",
			sandboxName:  "test-sandbox",
			expectError:  true,
		},
		{
			name:         "empty sandbox name",
			templateName: "www",
			sandboxName:  "",
			expectError:  true,
		},
		{
			name:         "both empty names",
			templateName: "",
			sandboxName:  "",
			expectError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := GeneratePVCName(tt.templateName, tt.sandboxName)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
				// Verify that the error message is meaningful
				if err != nil && err.Error() == "" {
					t.Errorf("Expected error message but got empty string")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
				if result != tt.expectName {
					t.Errorf("Expected name %s, but got %s", tt.expectName, result)
				}
			}
		})
	}
}
