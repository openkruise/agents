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
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/utils"
	agentsruntime "github.com/openkruise/agents/pkg/utils/runtime"
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
			name: "sandbox with labels in template but empty containers",
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
								Containers: []corev1.Container{},
							},
						},
					},
				},
			},
			validateDifferentHashes: true, // Both hashes should be the same when no containers have images/resources
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
		{
			name: "sandbox with templateRef only",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &agentsv1alpha1.SandboxTemplateRef{
							Name: "template-a",
						},
					},
				},
			},
			validateDifferentHashes: false,
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

func TestHashSandboxWithNilTemplateAndNilTemplateRef(t *testing.T) {
	sandbox := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template:    nil,
				TemplateRef: nil,
			},
		},
	}

	hash, hashWithoutImageResources := HashSandbox(sandbox)
	if hash != "" {
		t.Fatalf("expected empty hash, got %q", hash)
	}
	if hashWithoutImageResources != "" {
		t.Fatalf("expected empty hashWithoutImageResources, got %q", hashWithoutImageResources)
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

func TestGeneratePodFromSandbox(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name    string
		sandbox *agentsv1alpha1.Sandbox

		revision string
		wantErr  bool
		checkPod func(t *testing.T, pod *corev1.Pod)
	}{
		{
			name: "inline template - basic pod generation",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "app", Image: "nginx:latest"},
								},
							},
						},
					},
				},
			},
			revision: "rev-001",
			wantErr:  false,
			checkPod: func(t *testing.T, pod *corev1.Pod) {
				if pod.Name != "test-sandbox" {
					t.Errorf("pod.Name = %s, want test-sandbox", pod.Name)
				}
				if pod.Namespace != "default" {
					t.Errorf("pod.Namespace = %s, want default", pod.Namespace)
				}
				if len(pod.OwnerReferences) != 1 {
					t.Errorf("expected 1 owner reference, got %d", len(pod.OwnerReferences))
				}
				if pod.Annotations[utils.PodAnnotationCreatedBy] != utils.CreatedBySandbox {
					t.Errorf("annotation %s = %s, want %s", utils.PodAnnotationCreatedBy, pod.Annotations[utils.PodAnnotationCreatedBy], utils.CreatedBySandbox)
				}
				if pod.Labels[utils.PodLabelCreatedBy] != utils.CreatedBySandbox {
					t.Errorf("label %s = %s, want %s", utils.PodLabelCreatedBy, pod.Labels[utils.PodLabelCreatedBy], utils.CreatedBySandbox)
				}
				if pod.Labels[agentsv1alpha1.PodLabelTemplateHash] != "rev-001" {
					t.Errorf("label PodLabelTemplateHash = %s, want rev-001", pod.Labels[agentsv1alpha1.PodLabelTemplateHash])
				}
				if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Image != "nginx:latest" {
					t.Errorf("unexpected containers: %v", pod.Spec.Containers)
				}
			},
		},
		{
			name: "inline template - with labels and annotations from template",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "labeled-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels:      map[string]string{"env": "prod"},
								Annotations: map[string]string{"team": "platform"},
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "app", Image: "nginx:latest"},
								},
							},
						},
					},
				},
			},
			revision: "rev-abc",
			wantErr:  false,
			checkPod: func(t *testing.T, pod *corev1.Pod) {
				if pod.Labels["env"] != "prod" {
					t.Errorf("label env = %s, want prod", pod.Labels["env"])
				}
				if pod.Labels[agentsv1alpha1.PodLabelTemplateHash] != "rev-abc" {
					t.Errorf("label PodLabelTemplateHash = %s, want rev-abc", pod.Labels[agentsv1alpha1.PodLabelTemplateHash])
				}
				if pod.Annotations["team"] != "platform" {
					t.Errorf("annotation team = %s, want platform", pod.Annotations["team"])
				}
				if pod.Annotations[utils.PodAnnotationCreatedBy] != utils.CreatedBySandbox {
					t.Errorf("annotation CreatedBy missing or wrong: %s", pod.Annotations[utils.PodAnnotationCreatedBy])
				}
				if pod.Labels[utils.PodLabelCreatedBy] != utils.CreatedBySandbox {
					t.Errorf("label CreatedBy missing or wrong: %s", pod.Labels[utils.PodLabelCreatedBy])
				}
			},
		},
		{
			name: "inline template - with volumeClaimTemplates",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pvc-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "app", Image: "nginx:latest"},
								},
							},
						},
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
							{
								ObjectMeta: metav1.ObjectMeta{Name: "data"},
								Spec: corev1.PersistentVolumeClaimSpec{
									AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
									Resources: corev1.VolumeResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceStorage: resource.MustParse("1Gi"),
										},
									},
								},
							},
						},
					},
				},
			},
			revision: "rev-002",
			wantErr:  false,
			checkPod: func(t *testing.T, pod *corev1.Pod) {
				if len(pod.Spec.Volumes) != 1 {
					t.Errorf("expected 1 volume, got %d", len(pod.Spec.Volumes))
					return
				}
				vol := pod.Spec.Volumes[0]
				if vol.Name != "data" {
					t.Errorf("volume name = %s, want data", vol.Name)
				}
				if vol.PersistentVolumeClaim == nil {
					t.Errorf("expected PVC volume source, got nil")
					return
				}
				if vol.PersistentVolumeClaim.ClaimName != "data-pvc-sandbox" {
					t.Errorf("PVC ClaimName = %s, want data-pvc-sandbox", vol.PersistentVolumeClaim.ClaimName)
				}
				if vol.PersistentVolumeClaim.ReadOnly {
					t.Errorf("expected PVC ReadOnly = false")
				}
			},
		},
		{
			name: "inline template - volumeClaimTemplate with empty name returns error",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "bad-pvc-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "app", Image: "nginx:latest"},
								},
							},
						},
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
							{
								ObjectMeta: metav1.ObjectMeta{Name: ""},
							},
						},
					},
				},
			},
			revision: "rev-003",
			wantErr:  true,
		},

		{
			name: "templateRef - SandboxTemplate not found returns error",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "missing-ref-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &agentsv1alpha1.SandboxTemplateRef{
							Name: "nonexistent-template",
						},
					},
				},
			},
			revision: "rev-404",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli := fake.NewClientBuilder().
				WithScheme(scheme).
				WithRuntimeObjects(tt.sandbox).
				Build()

			pod, err := GeneratePodFromSandbox(context.Background(), PodGenerateArgs{Client: cli, Box: tt.sandbox, NewStatus: &agentsv1alpha1.SandboxStatus{UpdateRevision: tt.revision}})

			if (err != nil) != tt.wantErr {
				t.Errorf("GeneratePodFromSandbox() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if pod == nil {
				t.Fatal("expected non-nil pod")
			}
			if tt.checkPod != nil {
				tt.checkPod(t, pod)
			}
		})
	}
}

func TestEnsureStopPaused(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	now := metav1.Now()
	tests := []struct {
		name            string
		pod             *corev1.Pod
		seedPod         bool
		deleteErr       error
		wantErr         bool
		wantDeleteCalls int
		wantPodGone     bool
		validate        func(*testing.T, *agentsv1alpha1.SandboxStatus)
	}{
		{
			name: "pod gone - pause completed with success reason",
			pod:  nil,
			validate: func(t *testing.T, status *agentsv1alpha1.SandboxStatus) {
				cond := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionPaused))
				if cond == nil {
					t.Fatal("Paused condition should exist")
				}
				if cond.Status != metav1.ConditionTrue {
					t.Errorf("Expected Paused condition to be True, got %v", cond.Status)
				}
				if cond.Reason != agentsv1alpha1.SandboxPausedReasonStopPauseSucceed {
					t.Errorf("Expected reason %s, got %s", agentsv1alpha1.SandboxPausedReasonStopPauseSucceed, cond.Reason)
				}
			},
		},
		{
			name: "pod terminating - wait without deleting",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-sandbox",
					Namespace:         "default",
					DeletionTimestamp: &now,
					Finalizers:        []string{"test.io/finalizer"},
				},
			},
			wantDeleteCalls: 0,
			validate: func(t *testing.T, status *agentsv1alpha1.SandboxStatus) {
				cond := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionPaused))
				if cond == nil || cond.Status != metav1.ConditionFalse {
					t.Errorf("Expected Paused condition to stay False while waiting, got %v", cond)
				}
			},
		},
		{
			name: "pod alive - delete issued",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
			},
			seedPod:         true,
			wantDeleteCalls: 1,
			wantPodGone:     true,
			validate: func(t *testing.T, status *agentsv1alpha1.SandboxStatus) {
				cond := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionPaused))
				if cond == nil || cond.Status != metav1.ConditionFalse {
					t.Errorf("Expected Paused condition to stay False until the pod is gone, got %v", cond)
				}
			},
		},
		{
			name: "pod missing from cluster - NotFound treated as success",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
			},
			wantDeleteCalls: 1,
		},
		{
			name: "delete fails - error returned",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
			},
			seedPod:         true,
			deleteErr:       fmt.Errorf("injected delete failure"),
			wantErr:         true,
			wantDeleteCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deleteCalls := 0
			builder := fake.NewClientBuilder().WithScheme(scheme).
				WithInterceptorFuncs(interceptor.Funcs{
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						deleteCalls++
						if tt.deleteErr != nil {
							return tt.deleteErr
						}
						return c.Delete(ctx, obj, opts...)
					},
				})
			if tt.seedPod && tt.pod != nil {
				builder = builder.WithObjects(tt.pod.DeepCopy())
			}
			cli := builder.Build()

			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
			}
			newStatus := &agentsv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionPaused),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxPausedReasonPending,
						LastTransitionTime: now,
					},
				},
			}

			err := ensureStopPaused(context.Background(), cli, EnsureFuncArgs{
				Pod:       tt.pod,
				Box:       box,
				NewStatus: newStatus,
			}, agentsv1alpha1.SandboxPausedReasonStopPauseSucceed)

			if (err != nil) != tt.wantErr {
				t.Fatalf("ensureStopPaused() error = %v, wantErr %v", err, tt.wantErr)
			}
			if deleteCalls != tt.wantDeleteCalls {
				t.Errorf("Delete calls = %d, want %d", deleteCalls, tt.wantDeleteCalls)
			}
			if tt.wantPodGone {
				got := &corev1.Pod{}
				getErr := cli.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-sandbox"}, got)
				if getErr == nil {
					t.Error("Expected pod to be deleted from the cluster")
				}
			}
			if tt.validate != nil {
				tt.validate(t, newStatus)
			}
		})
	}
}

func TestEnsureCheckpointPaused(t *testing.T) {
	now := metav1.Now()

	changedImagePod := newCheckpointTestPod()
	changedImagePod.Spec.Containers[0].Image = "nginx:1.22"

	fsBox := newCheckpointTestSandbox()
	fsBox.Spec.PersistentContents = []string{agentsv1alpha1.PersistentContentFilesystem}

	tests := []struct {
		name        string
		pod         *corev1.Pod
		box         *agentsv1alpha1.Sandbox
		existingCPs []client.Object
		condReason  string
		wantReason  string
		wantTrue    bool
		wantCPCount int
		wantCPLabel string
		wantPodGone bool
	}{
		{
			// The dump content is passed through as-is: the checkpoint is
			// filesystem-only, not combined with podInfo.
			name:        "fresh pause - checkpoint created and pause waits",
			pod:         newCheckpointTestPod(),
			box:         fsBox,
			condReason:  agentsv1alpha1.SandboxPausedReasonPending,
			wantReason:  agentsv1alpha1.SandboxPausedReasonCheckpointCreating,
			wantCPCount: 1,
			wantCPLabel: agentsv1alpha1.CheckpointPersistentContentFilesystem,
		},
		{
			name:        "image changed - rejected before checkpoint creation",
			pod:         changedImagePod,
			box:         newCheckpointTestSandbox(),
			condReason:  agentsv1alpha1.SandboxPausedReasonPending,
			wantReason:  agentsv1alpha1.SandboxPausedReasonImageChanged,
			wantCPCount: 0,
		},
		{
			name: "checkpoint succeeded and pod gone - snapshot pause completed",
			pod:  nil,
			box:  newCheckpointTestSandbox(),
			existingCPs: []client.Object{
				newCheckpointTestCP("test-sandbox-cp1", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointSucceeded),
			},
			condReason:  agentsv1alpha1.SandboxPausedReasonCheckpointCreating,
			wantReason:  agentsv1alpha1.SandboxPausedReasonSnapshotPauseSucceed,
			wantTrue:    true,
			wantCPCount: 1,
		},
		{
			name: "checkpoint succeeded and pod alive - pod deleted",
			pod:  newCheckpointTestPod(),
			box:  newCheckpointTestSandbox(),
			existingCPs: []client.Object{
				newCheckpointTestCP("test-sandbox-cp1", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointSucceeded),
			},
			condReason:  agentsv1alpha1.SandboxPausedReasonCheckpointCreating,
			wantReason:  agentsv1alpha1.SandboxPausedReasonCheckpointSucceeded,
			wantCPCount: 1,
			wantPodGone: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := append([]client.Object{}, tt.existingCPs...)
			if tt.pod != nil {
				objs = append(objs, tt.pod)
			}
			control, cli := newCheckpointTestControl(objs...)

			newStatus := &agentsv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionPaused),
						Status:             metav1.ConditionFalse,
						Reason:             tt.condReason,
						LastTransitionTime: now,
					},
				},
			}

			err := ensureCheckpointPaused(context.Background(), cli, control, EnsureFuncArgs{
				Pod:       tt.pod,
				Box:       tt.box,
				NewStatus: newStatus,
			})
			if err != nil {
				t.Fatalf("ensureCheckpointPaused() error = %v", err)
			}

			cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
			if cond == nil {
				t.Fatal("Paused condition should exist")
			}
			if cond.Reason != tt.wantReason {
				t.Errorf("Expected reason %s, got %s", tt.wantReason, cond.Reason)
			}
			wantStatus := metav1.ConditionFalse
			if tt.wantTrue {
				wantStatus = metav1.ConditionTrue
			}
			if cond.Status != wantStatus {
				t.Errorf("Expected Paused condition status %v, got %v", wantStatus, cond.Status)
			}

			cpList := &agentsv1alpha1.CheckpointList{}
			if err := cli.List(context.Background(), cpList, client.InNamespace("default")); err != nil {
				t.Fatalf("Failed to list checkpoints: %v", err)
			}
			if len(cpList.Items) != tt.wantCPCount {
				t.Errorf("Expected %d checkpoints, got %d", tt.wantCPCount, len(cpList.Items))
			}
			if tt.wantCPLabel != "" && len(cpList.Items) > 0 {
				if got := cpList.Items[0].Labels[agentsv1alpha1.CheckpointLabelType]; got != tt.wantCPLabel {
					t.Errorf("Expected checkpoint label %s, got %s", tt.wantCPLabel, got)
				}
			}

			if tt.pod != nil {
				got := &corev1.Pod{}
				getErr := cli.Get(context.Background(), types.NamespacedName{Namespace: tt.pod.Namespace, Name: tt.pod.Name}, got)
				if tt.wantPodGone && getErr == nil {
					t.Error("Expected pod to be deleted after checkpoint succeeded")
				}
				if !tt.wantPodGone && getErr != nil {
					t.Errorf("Expected pod to still exist, got error: %v", getErr)
				}
			}
		})
	}
}

func TestStaleSandboxPodOwner(t *testing.T) {
	sandboxRef := func(uid types.UID, apiVersion string) metav1.OwnerReference {
		return metav1.OwnerReference{
			APIVersion: apiVersion,
			Kind:       "Sandbox",
			Name:       "test-sandbox",
			UID:        uid,
		}
	}

	tests := []struct {
		name      string
		pod       *corev1.Pod
		box       *agentsv1alpha1.Sandbox
		wantUID   types.UID
		wantStale bool
	}{
		{
			name: "no owner references",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
			},
			box:       &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default", UID: "current-uid"}},
			wantUID:   "",
			wantStale: false,
		},
		{
			name: "owned by another controller",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-sandbox", Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs-1", UID: "rs-uid"},
					},
				},
			},
			box:       &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default", UID: "current-uid"}},
			wantUID:   "",
			wantStale: false,
		},
		{
			name: "sandbox owner reference with matching uid",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-sandbox", Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{sandboxRef("current-uid", agentsv1alpha1.SchemeGroupVersion.String())},
				},
			},
			box:       &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default", UID: "current-uid"}},
			wantUID:   "",
			wantStale: false,
		},
		{
			name: "sandbox owner reference with mismatched uid",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-sandbox", Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{sandboxRef("old-uid", agentsv1alpha1.SchemeGroupVersion.String())},
				},
			},
			box:       &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default", UID: "current-uid"}},
			wantUID:   "old-uid",
			wantStale: true,
		},
		{
			name: "sandbox kind with mismatched api version",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-sandbox", Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{sandboxRef("old-uid", "agents.example.io/v1beta1")},
				},
			},
			box:       &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default", UID: "current-uid"}},
			wantUID:   "",
			wantStale: false,
		},
		{
			name: "sandbox owner reference is not the first entry",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-sandbox", Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs-1", UID: "rs-uid"},
						sandboxRef("old-uid", agentsv1alpha1.SchemeGroupVersion.String()),
					},
				},
			},
			box:       &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default", UID: "current-uid"}},
			wantUID:   "old-uid",
			wantStale: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uid, stale := StaleSandboxPodOwner(tt.pod, tt.box)
			if uid != tt.wantUID {
				t.Errorf("StaleSandboxPodOwner() uid = %q, want %q", uid, tt.wantUID)
			}
			if stale != tt.wantStale {
				t.Errorf("StaleSandboxPodOwner() stale = %v, want %v", stale, tt.wantStale)
			}
		})
	}
}

// TestPauseRemovesPropagatedCredential pins the ordering both pause strategies
// depend on: the credential has to leave the sandbox while the runtime is still
// reachable, which means before the pod is deleted under Stop and before any
// state is dumped under the checkpoint strategy.
func TestPauseRemovesPropagatedCredential(t *testing.T) {
	origCleanup := cleanupSecurityTokenFunc
	origCount := securityTokenCleanerCountFunc
	origInterval := securityCredentialCleanupRetryInterval
	t.Cleanup(func() {
		cleanupSecurityTokenFunc = origCleanup
		securityTokenCleanerCountFunc = origCount
		securityCredentialCleanupRetryInterval = origInterval
	})
	securityCredentialCleanupRetryInterval = time.Millisecond

	optedInSandbox := func() *agentsv1alpha1.Sandbox {
		box := newCheckpointTestSandbox()
		if box.Annotations == nil {
			box.Annotations = map[string]string{}
		}
		box.Annotations[identity.AnnotationAgentName] = "reviewer-agent"
		return box
	}

	tests := []struct {
		name string
		// checkpointStrategy selects ensureCheckpointPaused over ensureStopPaused.
		checkpointStrategy bool
		optedIn            bool
		cleanerCount       int
		cleanupErr         error
		// wantOrder is the exact sequence of observed side effects.
		wantOrder   []string
		expectError string
	}{
		{
			name:         "stop strategy removes the credential before deleting the pod",
			optedIn:      true,
			cleanerCount: 1,
			wantOrder:    []string{"cleanup", "delete"},
		},
		{
			// The pod deletion is the point after which the runtime is gone, so a
			// credential that could not be removed must stop the pause there.
			name:         "a failed removal stops the stop-strategy pause before the delete",
			optedIn:      true,
			cleanerCount: 1,
			cleanupErr:   fmt.Errorf("runtime unreachable"),
			wantOrder:    []string{"cleanup", "cleanup", "cleanup"},
			expectError:  "failed to remove propagated security credential before pause",
		},
		{
			name:         "a sandbox that never asked for an ID token is untouched",
			optedIn:      false,
			cleanerCount: 1,
			wantOrder:    []string{"delete"},
		},
		{
			name:         "the community default registers no cleaner and pauses unchanged",
			optedIn:      true,
			cleanerCount: 0,
			wantOrder:    []string{"delete"},
		},
		{
			// Once a filesystem or memory dump is taken the credential is inside a
			// Checkpoint artifact that outlives both the pod and the pause.
			name:               "checkpoint strategy removes the credential before the dump",
			checkpointStrategy: true,
			optedIn:            true,
			cleanerCount:       1,
			wantOrder:          []string{"cleanup", "checkpoint"},
		},
		{
			// No dump may be taken while a credential is still in the sandbox.
			name:               "a failed removal stops the pause before any dump is taken",
			checkpointStrategy: true,
			optedIn:            true,
			cleanerCount:       1,
			cleanupErr:         fmt.Errorf("runtime unreachable"),
			wantOrder:          []string{"cleanup", "cleanup", "cleanup"},
			expectError:        "failed to remove propagated security credential before pause",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var order []string
			securityTokenCleanerCountFunc = func() int { return tt.cleanerCount }
			cleanupSecurityTokenFunc = func(_ context.Context, _ *agentsv1alpha1.Sandbox,
				_ ...agentsruntime.Option) error {
				order = append(order, "cleanup")
				return tt.cleanupErr
			}

			box := newCheckpointTestSandbox()
			if tt.optedIn {
				box = optedInSandbox()
			}
			pod := newCheckpointTestPod()

			interceptors := interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					order = append(order, "delete")
					return c.Delete(ctx, obj, opts...)
				},
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, ok := obj.(*agentsv1alpha1.Checkpoint); ok {
						order = append(order, "checkpoint")
					}
					return c.Create(ctx, obj, opts...)
				},
			}

			newStatus := &agentsv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{{
					Type:               string(agentsv1alpha1.SandboxConditionPaused),
					Status:             metav1.ConditionFalse,
					Reason:             agentsv1alpha1.SandboxPausedReasonPending,
					LastTransitionTime: metav1.Now(),
				}},
			}
			args := EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus}

			var err error
			if tt.checkpointStrategy {
				control, cli := newCheckpointTestControlWithInterceptors(interceptors, pod)
				err = ensureCheckpointPaused(context.Background(), cli, control, args)
			} else {
				cli := fake.NewClientBuilder().WithScheme(scheme).
					WithObjects(pod.DeepCopy()).
					WithInterceptorFuncs(interceptors).Build()
				err = ensureStopPaused(context.Background(), cli, args,
					agentsv1alpha1.SandboxPausedReasonStopPauseSucceed)
			}

			assert.Equal(t, tt.wantOrder, order)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
		})
	}
}
