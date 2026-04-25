package utils

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
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
