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

package inplaceupdate

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	agentsapiv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGetPodInPlaceUpdateState(t *testing.T) {
	tests := []struct {
		name          string
		pod           *corev1.Pod
		expectedState *InPlaceUpdateState
		expectError   bool
	}{
		{
			name: "no annotation",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-pod",
					Namespace:   "default",
					Annotations: map[string]string{},
				},
			},
			expectedState: nil,
			expectError:   false,
		},
		{
			name: "empty annotation value",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						PodAnnotationInPlaceUpdateStateKey: "",
					},
				},
			},
			expectedState: nil,
			expectError:   false,
		},
		{
			name: "invalid json annotation",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						PodAnnotationInPlaceUpdateStateKey: `{"invalid": json}`,
					},
				},
			},
			expectedState: nil,
			expectError:   true,
		},
		{
			name: "valid annotation",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						PodAnnotationInPlaceUpdateStateKey: `{"revision":"abc123","updateTimestamp":"2023-01-01T00:00:00Z","lastContainerStatuses":{"container1":{"imageID":"image123"}}}`,
					},
				},
			},
			expectedState: &InPlaceUpdateState{
				Revision:        "abc123",
				UpdateTimestamp: metav1.Time{Time: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)},
				LastContainerStatuses: map[string]InPlaceUpdateContainerStatus{
					"container1": {ImageID: "image123"},
				},
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, err := GetPodInPlaceUpdateState(tt.pod)
			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
				return
			}
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}
			if tt.expectedState == nil && state != nil {
				t.Errorf("Expected nil state but got: %v", state)
				return
			}
			if tt.expectedState != nil && state == nil {
				t.Errorf("Expected state but got nil")
				return
			}
			if tt.expectedState != nil && state != nil {
				if state.Revision != tt.expectedState.Revision {
					t.Errorf("Revision mismatch: expected %s, got %s", tt.expectedState.Revision, state.Revision)
				}
				if len(state.LastContainerStatuses) != len(tt.expectedState.LastContainerStatuses) {
					t.Errorf("LastContainerStatuses length mismatch: expected %d, got %d", len(tt.expectedState.LastContainerStatuses), len(state.LastContainerStatuses))
					return
				}
				for name, expectedStatus := range tt.expectedState.LastContainerStatuses {
					actualStatus, exists := state.LastContainerStatuses[name]
					if !exists {
						t.Errorf("Expected container status for %s not found", name)
						continue
					}
					if actualStatus.ImageID != expectedStatus.ImageID {
						t.Errorf("ImageID mismatch for container %s: expected %s, got %s", name, expectedStatus.ImageID, actualStatus.ImageID)
					}
				}
			}
		})
	}
}

func TestInPlaceUpdateControl_Update(t *testing.T) {
	scheme, err := agentsapiv1alpha1.SchemeBuilder.Build()
	if err != nil {
		t.Fatalf("Failed to build scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("Failed to add corev1 to scheme: %v", err)
	}

	tests := []struct {
		name        string
		opts        InPlaceUpdateOptions
		expectError bool
	}{
		{
			name: "container image changes",
			opts: InPlaceUpdateOptions{
				Box: &agentsapiv1alpha1.Sandbox{
					Spec: agentsapiv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "container1",
											Image: "nginx:1.20",
										},
									},
								},
							},
						},
					},
				},
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod",
						Namespace: "default",
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "container1",
								Image: "nginx:latest", // different image
							},
						},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:    "container1",
								ImageID: "docker.io/nginx:latest",
							},
						},
					},
				},
				Revision: "abc123",
			},
			expectError: false,
		},
		{
			name: "container image and labels changes",
			opts: InPlaceUpdateOptions{
				Box: &agentsapiv1alpha1.Sandbox{
					Spec: agentsapiv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{
									Labels: map[string]string{
										"app": "test",
									},
								},
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "container1",
											Image: "nginx:1.20",
										},
									},
								},
							},
						},
					},
				},
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod",
						Namespace: "default",
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "container1",
								Image: "nginx:latest", // different image
							},
						},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:    "container1",
								ImageID: "docker.io/nginx:latest",
							},
						},
					},
				},
				Revision: "abc123",
			},
			expectError: false,
		},
		{
			name: "container not found in sandbox",
			opts: InPlaceUpdateOptions{
				Box: &agentsapiv1alpha1.Sandbox{
					Spec: agentsapiv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{}, // no containers
								},
							},
						},
					},
				},
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod",
						Namespace: "default",
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "container1",
								Image: "nginx:latest",
							},
						},
					},
				},
				Revision: "abc123",
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name != "no container changes" {
				return
			}
			client := &InPlaceUpdateControl{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.opts.Pod).Build(),
			}

			_, err = client.Update(context.TODO(), tt.opts)
			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
				return
			}
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			// Verify the pod was updated
			updatedPod := &corev1.Pod{}
			err = client.Get(context.TODO(), types.NamespacedName{Name: tt.opts.Pod.Name, Namespace: tt.opts.Pod.Namespace}, updatedPod)
			if err != nil {
				t.Errorf("Failed to get updated pod: %v", err)
				return
			}

			// Check if annotations were updated
			if updatedPod.Annotations[agentsapiv1alpha1.PodLabelTemplateHash] != tt.opts.Revision {
				t.Errorf("Expected revision annotation %s, got %s", tt.opts.Revision, updatedPod.Annotations[agentsapiv1alpha1.PodLabelTemplateHash])
			}

			// Check if the state annotation exists
			stateStr, exists := updatedPod.Annotations[PodAnnotationInPlaceUpdateStateKey]
			if !exists {
				t.Errorf("Expected inplace update state annotation to exist")
				return
			}

			// Parse the state annotation
			var state InPlaceUpdateState
			if err := json.Unmarshal([]byte(stateStr), &state); err != nil {
				t.Errorf("Failed to unmarshal state annotation: %v", err)
				return
			}

			// Validate state
			if state.Revision != tt.opts.Revision {
				t.Errorf("Expected state revision %s, got %s", tt.opts.Revision, state.Revision)
			}

			for k, v := range tt.opts.Box.Spec.Template.Labels {
				if updatedPod.Labels[k] != v {
					t.Errorf("Expected label %s=%s, got %s=%s", k, v, k, updatedPod.Labels[k])
				}
			}

			// If there were updates, validate container changes
			if len(tt.opts.Box.Spec.Template.Spec.Containers) > 0 {
				containerFound := false
				for _, container := range updatedPod.Spec.Containers {
					if container.Name == tt.opts.Box.Spec.Template.Spec.Containers[0].Name {
						containerFound = true
						if container.Image != tt.opts.Box.Spec.Template.Spec.Containers[0].Image {
							t.Errorf("Expected container image %s, got %s", tt.opts.Box.Spec.Template.Spec.Containers[0].Image, container.Image)
						}
						break
					}
				}
				if !containerFound && len(tt.opts.Box.Spec.Template.Spec.Containers) > 0 {
					t.Errorf("Expected container %s to exist in updated pod", tt.opts.Box.Spec.Template.Spec.Containers[0].Name)
				}
			}
		})
	}
}

func TestIsInplaceUpdateCompleted(t *testing.T) {
	tests := []struct {
		name              string
		pod               *corev1.Pod
		expectedCompleted bool
	}{
		{
			name: "no state annotation",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-pod",
					Namespace:   "default",
					Annotations: map[string]string{
						// No inplace update state annotation
					},
				},
			},
			expectedCompleted: true,
		},
		{
			name: "invalid state annotation",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						PodAnnotationInPlaceUpdateStateKey: `{"invalid": json}`,
					},
				},
			},
			expectedCompleted: true, // Returns true on error
		},
		{
			name: "empty last container statuses",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						PodAnnotationInPlaceUpdateStateKey: `{"revision":"abc123","updateTimestamp":"2023-01-01T00:00:00Z","lastContainerStatuses":{}}`,
					},
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:    "container1",
							ImageID: "image123",
						},
					},
				},
			},
			expectedCompleted: true,
		},
		{
			name: "incomplete update - same image ID",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						PodAnnotationInPlaceUpdateStateKey: `{"revision":"abc123","updateTimestamp":"2023-01-01T00:00:00Z","lastContainerStatuses":{"container1":{"imageID":"image123"}}}`,
					},
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:    "container1",
							ImageID: "image123", // Same as old image ID
						},
					},
				},
			},
			expectedCompleted: false,
		},
		{
			name: "complete update - different image ID",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						PodAnnotationInPlaceUpdateStateKey: `{"revision":"abc123","updateTimestamp":"2023-01-01T00:00:00Z","lastContainerStatuses":{"container1":{"imageID":"image123"}}}`,
					},
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:    "container1",
							ImageID: "image456", // Different from old image ID
						},
					},
				},
			},
			expectedCompleted: true,
		},
		{
			name: "container not found in status",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						PodAnnotationInPlaceUpdateStateKey: `{"revision":"abc123","updateTimestamp":"2023-01-01T00:00:00Z","lastContainerStatuses":{"container1":{"imageID":"image123"}}}`,
					},
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:    "container2", // Different container name
							ImageID: "image456",
						},
					},
				},
			},
			expectedCompleted: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completed := IsInplaceUpdateCompleted(context.TODO(), tt.pod)
			if completed != tt.expectedCompleted {
				t.Errorf("Expected completed=%v, got %v", tt.expectedCompleted, completed)
			}
		})
	}
}

func TestDefaultGeneratePatchBodyFunc(t *testing.T) {
	tests := []struct {
		name                          string
		opts                          InPlaceUpdateOptions
		expectedEmpty                 bool
		expectedContainerImages       map[string]string // container name -> expected image
		expectedLabels                map[string]string // expected label key-value pairs
		expectedUpdateImages          bool
		expectedLastContainerStatuses map[string]string // container name -> expected imageID
	}{
		{
			name: "no container changes",
			opts: InPlaceUpdateOptions{
				Box: &agentsapiv1alpha1.Sandbox{
					Spec: agentsapiv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "container1",
											Image: "nginx:latest",
										},
									},
								},
							},
						},
					},
				},
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod",
						Namespace: "default",
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "container1",
								Image: "nginx:latest",
							},
						},
					},
				},
				Revision: "abc123",
			},
			expectedEmpty: true,
		},
		{
			name: "container image changes",
			opts: InPlaceUpdateOptions{
				Box: &agentsapiv1alpha1.Sandbox{
					Spec: agentsapiv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "container1",
											Image: "nginx:1.20",
										},
										{
											Name:  "container2",
											Image: "redis:6.0",
										},
									},
								},
							},
						},
					},
				},
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod",
						Namespace: "default",
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "container1",
								Image: "nginx:latest",
							},
							{
								Name:  "container2",
								Image: "redis:5.0",
							},
						},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:    "container1",
								ImageID: "docker.io/nginx:latest@sha256:old1",
							},
							{
								Name:    "container2",
								ImageID: "docker.io/redis:5.0@sha256:old2",
							},
						},
					},
				},
				Revision: "abc123",
			},
			expectedEmpty: false,
			expectedContainerImages: map[string]string{
				"container1": "nginx:1.20",
				"container2": "redis:6.0",
			},
			expectedUpdateImages: true,
			expectedLastContainerStatuses: map[string]string{
				"container1": "docker.io/nginx:latest@sha256:old1",
				"container2": "docker.io/redis:5.0@sha256:old2",
			},
		},
		{
			name: "container image and labels changes",
			opts: InPlaceUpdateOptions{
				Box: &agentsapiv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app":     "myapp",
							"version": "v2",
							"env":     "production",
						},
					},
					Spec: agentsapiv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "app",
											Image: "myapp:v2",
										},
									},
								},
							},
						},
					},
				},
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod",
						Namespace: "default",
						Labels: map[string]string{
							"app":     "myapp",
							"version": "v1", // different version
							// missing "env" label
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "app",
								Image: "myapp:v1",
							},
						},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:    "app",
								ImageID: "docker.io/myapp:v1@sha256:old",
							},
						},
					},
				},
				Revision: "def456",
			},
			expectedEmpty: false,
			expectedContainerImages: map[string]string{
				"app": "myapp:v2",
			},
			expectedLabels: map[string]string{
				agentsapiv1alpha1.PodLabelTemplateHash: "def456",
				"version":                              "v2",
				"env":                                  "production",
			},
			expectedUpdateImages: true,
			expectedLastContainerStatuses: map[string]string{
				"app": "docker.io/myapp:v1@sha256:old",
			},
		},
		{
			name: "skip internal labels",
			opts: InPlaceUpdateOptions{
				Box: &agentsapiv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app":                          "myapp",
							"agents.kruise.io/internal":    "skip-me",
							"agents.kruise.io/another-key": "skip-me-too",
							"version":                      "v2",
						},
					},
					Spec: agentsapiv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "app",
											Image: "myapp:v2",
										},
									},
								},
							},
						},
					},
				},
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod",
						Namespace: "default",
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "app",
								Image: "myapp:v1",
							},
						},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:    "app",
								ImageID: "docker.io/myapp:v1@sha256:old",
							},
						},
					},
				},
				Revision: "ghi789",
			},
			expectedEmpty: false,
			expectedContainerImages: map[string]string{
				"app": "myapp:v2",
			},
			expectedLabels: map[string]string{
				agentsapiv1alpha1.PodLabelTemplateHash: "ghi789",
				"app":                                  "myapp",
				"version":                              "v2",
			},
			expectedUpdateImages: true,
			expectedLastContainerStatuses: map[string]string{
				"app": "docker.io/myapp:v1@sha256:old",
			},
		},
		{
			name: "partial container updates",
			opts: InPlaceUpdateOptions{
				Box: &agentsapiv1alpha1.Sandbox{
					Spec: agentsapiv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "app",
											Image: "myapp:v2", // changed
										},
										{
											Name:  "sidecar",
											Image: "sidecar:v1", // same
										},
									},
								},
							},
						},
					},
				},
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pod",
						Namespace: "default",
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "app",
								Image: "myapp:v1",
							},
							{
								Name:  "sidecar",
								Image: "sidecar:v1",
							},
						},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:    "app",
								ImageID: "docker.io/myapp:v1@sha256:old1",
							},
							{
								Name:    "sidecar",
								ImageID: "docker.io/sidecar:v1@sha256:old2",
							},
						},
					},
				},
				Revision: "jkl012",
			},
			expectedEmpty: false,
			expectedContainerImages: map[string]string{
				"app": "myapp:v2",
			},
			expectedUpdateImages: true,
			expectedLastContainerStatuses: map[string]string{
				"app": "docker.io/myapp:v1@sha256:old1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DefaultGeneratePatchBodyFunc(tt.opts)

			if tt.expectedEmpty {
				if result != "" {
					t.Errorf("Expected empty result but got: %s", result)
				}
				return
			}

			if result == "" {
				t.Errorf("Expected non-empty result but got empty string")
				return
			}

			// Parse the JSON result
			var patchData map[string]interface{}
			if err := json.Unmarshal([]byte(result), &patchData); err != nil {
				t.Errorf("Failed to parse JSON: %v", err)
				return
			}

			// Check metadata
			metadata, ok := patchData["metadata"].(map[string]interface{})
			if !ok {
				t.Errorf("Expected metadata to be a map")
				return
			}

			// Check annotations
			annotations, ok := metadata["annotations"].(map[string]interface{})
			if !ok {
				t.Errorf("Expected annotations to be a map")
				return
			}

			// Verify inplace update state annotation exists
			stateAnnotation, ok := annotations[PodAnnotationInPlaceUpdateStateKey]
			if !ok {
				t.Errorf("Expected annotation %s not found", PodAnnotationInPlaceUpdateStateKey)
				return
			}

			// Parse and verify the state
			stateJSON, ok := stateAnnotation.(string)
			if !ok {
				t.Errorf("Expected state annotation to be a string")
				return
			}

			var state InPlaceUpdateState
			if err := json.Unmarshal([]byte(stateJSON), &state); err != nil {
				t.Errorf("Failed to parse state JSON: %v", err)
				return
			}

			// Check revision
			if state.Revision != tt.opts.Revision {
				t.Errorf("State revision mismatch: expected %s, got %s", tt.opts.Revision, state.Revision)
			}

			// Check UpdateImages flag
			if state.UpdateImages != tt.expectedUpdateImages {
				t.Errorf("State UpdateImages mismatch: expected %v, got %v", tt.expectedUpdateImages, state.UpdateImages)
			}

			// Check LastContainerStatuses
			if len(state.LastContainerStatuses) != len(tt.expectedLastContainerStatuses) {
				t.Errorf("LastContainerStatuses length mismatch: expected %d, got %d", len(tt.expectedLastContainerStatuses), len(state.LastContainerStatuses))
			} else {
				for containerName, expectedImageID := range tt.expectedLastContainerStatuses {
					actualStatus, exists := state.LastContainerStatuses[containerName]
					if !exists {
						t.Errorf("Expected LastContainerStatuses for container %s not found", containerName)
						continue
					}
					if actualStatus.ImageID != expectedImageID {
						t.Errorf("LastContainerStatuses ImageID mismatch for container %s: expected %s, got %s", containerName, expectedImageID, actualStatus.ImageID)
					}
				}
			}

			// Check labels
			labels, ok := metadata["labels"].(map[string]interface{})
			if !ok {
				t.Errorf("Expected labels to be a map")
				return
			}

			if tt.expectedLabels != nil {
				if len(labels) != len(tt.expectedLabels) {
					t.Errorf("Labels length mismatch: expected %d, got %d", len(tt.expectedLabels), len(labels))
				}
				for expectedKey, expectedValue := range tt.expectedLabels {
					actualValue, exists := labels[expectedKey]
					if !exists {
						t.Errorf("Expected label key %s not found", expectedKey)
						continue
					}
					if actualValueStr, ok := actualValue.(string); ok {
						if actualValueStr != expectedValue {
							t.Errorf("Label value mismatch for key %s: expected %s, got %s", expectedKey, expectedValue, actualValueStr)
						}
					} else {
						t.Errorf("Label value for key %s is not a string: %v", expectedKey, actualValue)
					}
				}
			}

			// Check spec.containers
			spec, ok := patchData["spec"].(map[string]interface{})
			if !ok {
				t.Errorf("Expected spec to be a map")
				return
			}

			containers, ok := spec["containers"].([]interface{})
			if !ok {
				t.Errorf("Expected containers to be an array")
				return
			}

			if tt.expectedContainerImages != nil {
				if len(containers) != len(tt.expectedContainerImages) {
					t.Errorf("Containers length mismatch: expected %d, got %d", len(tt.expectedContainerImages), len(containers))
				}

				// Convert containers array to map for easier verification
				actualContainerImages := make(map[string]string)
				for _, container := range containers {
					containerMap, ok := container.(map[string]interface{})
					if !ok {
						t.Errorf("Expected container to be a map")
						continue
					}
					name, nameOk := containerMap["name"].(string)
					image, imageOk := containerMap["image"].(string)
					if nameOk && imageOk {
						actualContainerImages[name] = image
					}
				}

				// Verify each container's image
				for expectedContainerName, expectedImage := range tt.expectedContainerImages {
					actualImage, exists := actualContainerImages[expectedContainerName]
					if !exists {
						t.Errorf("Expected container %s not found in patch", expectedContainerName)
						continue
					}
					if actualImage != expectedImage {
						t.Errorf("Container image mismatch for %s: expected %s, got %s", expectedContainerName, expectedImage, actualImage)
					}
				}
			}
		})
	}
}
