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

package e2b

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

func TestReplacer(t *testing.T) {
	url := "ws://localhost:9222/devtools/browser/12345678-1234-1234-1234-123456789012"
	url = browserWebSocketReplacer.ReplaceAllString(url, "ws://hello-world")
	if url != "ws://hello-world/devtools/browser/12345678-1234-1234-1234-123456789012" {
		t.Errorf("Expected %s, got %s", "ws://hello-world/devtools/browser/12345678-1234-1234-1234-123456789012", url)
	}
}

func TestGetNamespaceOfUser(t *testing.T) {
	controller := &Controller{}

	tests := []struct {
		name      string
		user      *models.CreatedTeamAPIKey
		namespace string
	}{
		{
			name:      "nil user keeps legacy admin cluster scope",
			user:      nil,
			namespace: "",
		},
		{
			name:      "legacy key without team keeps admin cluster scope",
			user:      &models.CreatedTeamAPIKey{ID: uuid.New(), Name: "legacy"},
			namespace: "",
		},
		{
			name:      "canonical admin team keeps cluster scope",
			user:      &models.CreatedTeamAPIKey{ID: uuid.New(), Name: "admin", Team: models.AdminTeam()},
			namespace: "",
		},
		{
			name: "admin team name with mismatched id normalizes to cluster scope",
			user: &models.CreatedTeamAPIKey{
				ID:   uuid.New(),
				Name: "admin-name-mismatched-id",
				Team: &models.Team{
					Name: models.AdminTeamName,
				},
			},
			namespace: "",
		},
		{
			name: "non-admin team name maps to team namespace",
			user: &models.CreatedTeamAPIKey{
				ID:   uuid.New(),
				Name: "not-admin-user",
				Team: &models.Team{
					Name: "not-admin",
				},
			},
			namespace: "not-admin",
		},
		{
			name: "normal team maps to team namespace",
			user: &models.CreatedTeamAPIKey{
				ID:   uuid.New(),
				Name: "team-a-user",
				Team: &models.Team{
					Name: "team-a",
				},
			},
			namespace: "team-a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.namespace, controller.getNamespaceOfUser(tt.user))
		})
	}
}

func TestConvertToE2BSandboxPodIPMetadata(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	tests := []struct {
		name        string
		annotations map[string]string
		podIP       string
		expectKey   bool
		expectValue string
	}{
		{
			name:  "disabled does not return pod ip metadata",
			podIP: "1.2.3.4",
		},
		{
			name: "enabled returns pod ip metadata",
			annotations: map[string]string{
				models.ExtensionKeyReturnPodIP: agentsv1alpha1.True,
			},
			podIP:       "1.2.3.4",
			expectKey:   true,
			expectValue: "1.2.3.4",
		},
		{
			name: "enabled skips pod ip metadata when pod ip is empty",
			annotations: map[string]string{
				models.ExtensionKeyReturnPodIP: agentsv1alpha1.True,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			annotations := map[string]string{
				agentsv1alpha1.AnnotationClaimTime: now.Format(time.RFC3339),
			}
			for k, v := range tt.annotations {
				annotations[k] = v
			}
			sbx := &sandboxcr.Sandbox{
				Sandbox: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "sandbox-1",
						Namespace:   "default",
						Annotations: annotations,
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxRunning,
						Conditions: []metav1.Condition{
							{
								Type:   string(agentsv1alpha1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
							},
						},
						PodInfo: agentsv1alpha1.PodInfo{
							PodIP: tt.podIP,
						},
					},
				},
			}

			got := (&Controller{}).convertToE2BSandbox(sbx, "")
			value, exists := got.Metadata[models.MetadataKeyPodIP]
			assert.Equal(t, tt.expectKey, exists)
			if tt.expectKey {
				assert.Equal(t, tt.expectValue, value)
			}
		})
	}
}

func TestConvertToE2BSandboxUsesLimitResources(t *testing.T) {
	tests := []struct {
		name       string
		sbx        *sandboxcr.Sandbox
		wantCPU    int64
		wantMemory int64
		wantDisk   int64
	}{
		{
			name: "uses limit cpu and memory",
			sbx: &sandboxcr.Sandbox{
				Sandbox: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-1",
						Namespace: "default",
						Annotations: map[string]string{
							agentsv1alpha1.AnnotationClaimTime: time.Now().Format(time.RFC3339),
						},
					},
					Spec: agentsv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{{
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("1000m"),
												corev1.ResourceMemory: resource.MustParse("1024Mi"),
											},
											Limits: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("4000m"),
												corev1.ResourceMemory: resource.MustParse("8192Mi"),
											},
										},
									}},
								},
							},
						},
					},
				},
			},
			wantCPU:    4,
			wantMemory: 8192,
		},
		{
			name: "falls back to request resources",
			sbx: &sandboxcr.Sandbox{
				Sandbox: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-1",
						Namespace: "default",
						Annotations: map[string]string{
							agentsv1alpha1.AnnotationClaimTime: time.Now().Format(time.RFC3339),
						},
					},
					Spec: agentsv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{{
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:              resource.MustParse("2000m"),
												corev1.ResourceMemory:           resource.MustParse("2048Mi"),
												corev1.ResourceEphemeralStorage: resource.MustParse("3Gi"),
											},
										},
									}},
								},
							},
						},
					},
				},
			},
			wantCPU:    2,
			wantMemory: 2048,
			wantDisk:   3072,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := (&Controller{}).convertToE2BSandbox(tt.sbx, "")
			assert.Equal(t, tt.wantCPU, got.CPUCount)
			assert.Equal(t, tt.wantMemory, got.MemoryMB)
			assert.Equal(t, tt.wantDisk, got.DiskSizeMB)
		})
	}
}
