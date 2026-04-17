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
			name: "container changes exist",
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
		name           string
		opts           InPlaceUpdateOptions
		expectedEmpty  bool
		checkPatchBody func(t *testing.T, patchBody string)
	}{
		{
			name: "no container changes - same image",
			opts: InPlaceUpdateOptions{
				Box: &agentsapiv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
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
								Image: "nginx:latest", // Same image
							},
						},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:    "container1",
								ImageID: "docker.io/nginx:latest@sha256:abc123",
							},
						},
					},
				},
				Revision: "rev-001",
			},
			expectedEmpty: false,
			checkPatchBody: func(t *testing.T, patchBody string) {
				var patch map[string]interface{}
				if err := json.Unmarshal([]byte(patchBody), &patch); err != nil {
					t.Fatalf("Failed to unmarshal patch body: %v", err)
				}

				metadata, ok := patch["metadata"].(map[string]interface{})
				if !ok {
					t.Fatalf("Patch body should have metadata")
				}

				labels, ok := metadata["labels"].(map[string]interface{})
				if !ok {
					t.Fatalf("Metadata should have labels")
				}

				if labels[agentsapiv1alpha1.PodLabelTemplateHash] != "rev-001" {
					t.Errorf("Expected label pod-template-hash=rev-001, got %v", labels[agentsapiv1alpha1.PodLabelTemplateHash])
				}

				annotations, ok := metadata["annotations"].(map[string]interface{})
				if !ok {
					t.Fatalf("Metadata should have annotations")
				}

				stateStr, ok := annotations[PodAnnotationInPlaceUpdateStateKey].(string)
				if !ok {
					t.Fatalf("Annotations should have inplace update state")
				}

				var state InPlaceUpdateState
				if err := json.Unmarshal([]byte(stateStr), &state); err != nil {
					t.Fatalf("Failed to unmarshal state: %v", err)
				}

				if state.Revision != "rev-001" {
					t.Errorf("Expected revision rev-001, got %s", state.Revision)
				}

				if state.UpdateImages {
					t.Errorf("Expected UpdateImages to be false (no container changes)")
				}

				if len(state.LastContainerStatuses) != 0 {
					t.Errorf("Expected empty LastContainerStatuses, got %d entries", len(state.LastContainerStatuses))
				}

				spec, ok := patch["spec"].(map[string]interface{})
				if !ok {
					t.Fatalf("Patch body should have spec")
				}

				containers, _ := spec["containers"].([]interface{})
				if len(containers) != 0 {
					t.Errorf("Expected empty containers array (no changes), got %v", containers)
				}
			},
		},
		{
			name: "single container image change",
			opts: InPlaceUpdateOptions{
				Box: &agentsapiv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Spec: agentsapiv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "container1",
											Image: "nginx:1.20", // New image
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
								Image: "nginx:latest", // Old image
							},
						},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:    "container1",
								ImageID: "docker.io/nginx:latest@sha256:old123",
							},
						},
					},
				},
				Revision: "rev-002",
			},
			expectedEmpty: false,
			checkPatchBody: func(t *testing.T, patchBody string) {
				// Verify patch body contains expected elements
				if !containsString(patchBody, `"containers"`) {
					t.Errorf("Patch body should contain containers")
				}
				if !containsString(patchBody, `"nginx:1.20"`) {
					t.Errorf("Patch body should contain new image nginx:1.20")
				}
				if !containsString(patchBody, `\"revision\":\"rev-002\"`) {
					t.Errorf("Patch body should contain revision rev-002")
				}

				// Parse and validate the state annotation
				var patch map[string]interface{}
				if err := json.Unmarshal([]byte(patchBody), &patch); err != nil {
					t.Fatalf("Failed to unmarshal patch body: %v", err)
				}

				metadata, ok := patch["metadata"].(map[string]interface{})
				if !ok {
					t.Fatalf("Patch body should have metadata")
				}

				annotations, ok := metadata["annotations"].(map[string]interface{})
				if !ok {
					t.Fatalf("Metadata should have annotations")
				}

				stateStr, ok := annotations[PodAnnotationInPlaceUpdateStateKey].(string)
				if !ok {
					t.Fatalf("Annotations should have inplace update state")
				}

				var state InPlaceUpdateState
				if err := json.Unmarshal([]byte(stateStr), &state); err != nil {
					t.Fatalf("Failed to unmarshal state: %v", err)
				}

				if state.Revision != "rev-002" {
					t.Errorf("Expected revision rev-002, got %s", state.Revision)
				}

				if !state.UpdateImages {
					t.Errorf("Expected UpdateImages to be true")
				}

				if len(state.LastContainerStatuses) != 1 {
					t.Fatalf("Expected 1 container status, got %d", len(state.LastContainerStatuses))
				}

				containerStatus, exists := state.LastContainerStatuses["container1"]
				if !exists {
					t.Fatalf("Expected container1 in LastContainerStatuses")
				}

				if containerStatus.ImageID != "docker.io/nginx:latest@sha256:old123" {
					t.Errorf("Expected old image ID, got %s", containerStatus.ImageID)
				}
			},
		},
		{
			name: "multiple container image changes",
			opts: InPlaceUpdateOptions{
				Box: &agentsapiv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
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
										{
											Name:  "sidecar",
											Image: "envoy:v1.2",
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
								Image: "envoy:v1.1",
							},
						},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:    "app",
								ImageID: "myapp:v1@sha256:aaa",
							},
							{
								Name:    "sidecar",
								ImageID: "envoy:v1.1@sha256:bbb",
							},
						},
					},
				},
				Revision: "rev-multi",
			},
			expectedEmpty: false,
			checkPatchBody: func(t *testing.T, patchBody string) {
				if !containsString(patchBody, `"myapp:v2"`) {
					t.Errorf("Patch body should contain myapp:v2")
				}
				if !containsString(patchBody, `"envoy:v1.2"`) {
					t.Errorf("Patch body should contain envoy:v1.2")
				}

				var patch map[string]interface{}
				if err := json.Unmarshal([]byte(patchBody), &patch); err != nil {
					t.Fatalf("Failed to unmarshal patch body: %v", err)
				}

				metadata := patch["metadata"].(map[string]interface{})
				annotations := metadata["annotations"].(map[string]interface{})
				stateStr := annotations[PodAnnotationInPlaceUpdateStateKey].(string)

				var state InPlaceUpdateState
				if err := json.Unmarshal([]byte(stateStr), &state); err != nil {
					t.Fatalf("Failed to unmarshal state: %v", err)
				}

				if len(state.LastContainerStatuses) != 2 {
					t.Fatalf("Expected 2 container statuses, got %d", len(state.LastContainerStatuses))
				}

				if _, exists := state.LastContainerStatuses["app"]; !exists {
					t.Errorf("Expected app in LastContainerStatuses")
				}
				if _, exists := state.LastContainerStatuses["sidecar"]; !exists {
					t.Errorf("Expected sidecar in LastContainerStatuses")
				}
			},
		},
		{
			name: "partial container changes - only some images changed",
			opts: InPlaceUpdateOptions{
				Box: &agentsapiv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Spec: agentsapiv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "container1",
											Image: "nginx:1.20", // Changed
										},
										{
											Name:  "container2",
											Image: "redis:7.0", // Unchanged
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
								Image: "redis:7.0", // Same
							},
						},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:    "container1",
								ImageID: "nginx:latest@sha256:old",
							},
							{
								Name:    "container2",
								ImageID: "redis:7.0@sha256:same",
							},
						},
					},
				},
				Revision: "rev-partial",
			},
			expectedEmpty: false,
			checkPatchBody: func(t *testing.T, patchBody string) {
				// Should only patch container1
				if containsString(patchBody, `"redis:7.0"`) {
					t.Errorf("Patch body should not contain unchanged redis:7.0")
				}
				if !containsString(patchBody, `"nginx:1.20"`) {
					t.Errorf("Patch body should contain nginx:1.20")
				}

				var patch map[string]interface{}
				if err := json.Unmarshal([]byte(patchBody), &patch); err != nil {
					t.Fatalf("Failed to unmarshal patch body: %v", err)
				}

				metadata := patch["metadata"].(map[string]interface{})
				annotations := metadata["annotations"].(map[string]interface{})
				stateStr := annotations[PodAnnotationInPlaceUpdateStateKey].(string)

				var state InPlaceUpdateState
				if err := json.Unmarshal([]byte(stateStr), &state); err != nil {
					t.Fatalf("Failed to unmarshal state: %v", err)
				}

				// Should only track container1
				if len(state.LastContainerStatuses) != 1 {
					t.Fatalf("Expected 1 container status, got %d", len(state.LastContainerStatuses))
				}
				if _, exists := state.LastContainerStatuses["container1"]; !exists {
					t.Errorf("Expected container1 in LastContainerStatuses")
				}
			},
		},
		{
			name: "propagate labels from sandbox template to pod",
			opts: InPlaceUpdateOptions{
				Box: &agentsapiv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Spec: agentsapiv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{
									Labels: map[string]string{
										"app":     "myapp",
										"version": "v2",
										"team":    "platform",
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
						Labels: map[string]string{
							"app":               "myapp",
							"version":           "v1", // Different version
							"pod-template-hash": "old-hash",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "container1",
								Image: "nginx:latest",
							},
						},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:    "container1",
								ImageID: "nginx:latest@sha256:old",
							},
						},
					},
				},
				Revision: "rev-labels",
			},
			expectedEmpty: false,
			checkPatchBody: func(t *testing.T, patchBody string) {
				var patch map[string]interface{}
				if err := json.Unmarshal([]byte(patchBody), &patch); err != nil {
					t.Fatalf("Failed to unmarshal patch body: %v", err)
				}

				metadata := patch["metadata"].(map[string]interface{})
				labels := metadata["labels"].(map[string]interface{})

				// Check that labels are propagated
				if labels["version"] != "v2" {
					t.Errorf("Expected label version=v2, got %v", labels["version"])
				}
				if labels["team"] != "platform" {
					t.Errorf("Expected label team=platform, got %v", labels["team"])
				}
				// Check pod-template-hash is updated
				if labels["pod-template-hash"] != "rev-labels" {
					t.Errorf("Expected pod-template-hash=rev-labels, got %v", labels["pod-template-hash"])
				}
			},
		},
		{
			name: "container in pod but not in sandbox template - skip it",
			opts: InPlaceUpdateOptions{
				Box: &agentsapiv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
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
								Image: "nginx:latest",
							},
							{
								Name:  "extra-container", // Not in sandbox template
								Image: "redis:latest",
							},
						},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:    "container1",
								ImageID: "nginx:latest@sha256:old",
							},
							{
								Name:    "extra-container",
								ImageID: "redis:latest@sha256:extra",
							},
						},
					},
				},
				Revision: "rev-extra",
			},
			expectedEmpty: false,
			checkPatchBody: func(t *testing.T, patchBody string) {
				// Should only patch container1, not extra-container
				if containsString(patchBody, `"redis"`) {
					t.Errorf("Patch body should not contain extra-container redis")
				}
				if !containsString(patchBody, `"nginx:1.20"`) {
					t.Errorf("Patch body should contain nginx:1.20")
				}

				var patch map[string]interface{}
				if err := json.Unmarshal([]byte(patchBody), &patch); err != nil {
					t.Fatalf("Failed to unmarshal patch body: %v", err)
				}

				metadata := patch["metadata"].(map[string]interface{})
				annotations := metadata["annotations"].(map[string]interface{})
				stateStr := annotations[PodAnnotationInPlaceUpdateStateKey].(string)

				var state InPlaceUpdateState
				if err := json.Unmarshal([]byte(stateStr), &state); err != nil {
					t.Fatalf("Failed to unmarshal state: %v", err)
				}

				// Should only track container1
				if len(state.LastContainerStatuses) != 1 {
					t.Fatalf("Expected 1 container status, got %d", len(state.LastContainerStatuses))
				}
				if _, exists := state.LastContainerStatuses["extra-container"]; exists {
					t.Errorf("Should not track extra-container in LastContainerStatuses")
				}
			},
		},
		{
			name: "empty sandbox template - generate metadata only",
			opts: InPlaceUpdateOptions{
				Box: &agentsapiv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Spec: agentsapiv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{},
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
				Revision: "rev-empty",
			},
			expectedEmpty: false,
			checkPatchBody: func(t *testing.T, patchBody string) {
				var patch map[string]interface{}
				if err := json.Unmarshal([]byte(patchBody), &patch); err != nil {
					t.Fatalf("Failed to unmarshal patch body: %v", err)
				}

				metadata, ok := patch["metadata"].(map[string]interface{})
				if !ok {
					t.Fatalf("Patch body should have metadata")
				}

				labels, ok := metadata["labels"].(map[string]interface{})
				if !ok {
					t.Fatalf("Metadata should have labels")
				}

				if labels[agentsapiv1alpha1.PodLabelTemplateHash] != "rev-empty" {
					t.Errorf("Expected label pod-template-hash=rev-empty, got %v", labels[agentsapiv1alpha1.PodLabelTemplateHash])
				}

				annotations, ok := metadata["annotations"].(map[string]interface{})
				if !ok {
					t.Fatalf("Metadata should have annotations")
				}

				stateStr, ok := annotations[PodAnnotationInPlaceUpdateStateKey].(string)
				if !ok {
					t.Fatalf("Annotations should have inplace update state")
				}

				var state InPlaceUpdateState
				if err := json.Unmarshal([]byte(stateStr), &state); err != nil {
					t.Fatalf("Failed to unmarshal state: %v", err)
				}

				if state.Revision != "rev-empty" {
					t.Errorf("Expected revision rev-empty, got %s", state.Revision)
				}

				if state.UpdateImages {
					t.Errorf("Expected UpdateImages to be false (no containers in sandbox template)")
				}

				if len(state.LastContainerStatuses) != 0 {
					t.Errorf("Expected empty LastContainerStatuses, got %d entries", len(state.LastContainerStatuses))
				}

				spec, ok := patch["spec"].(map[string]interface{})
				if !ok {
					t.Fatalf("Patch body should have spec")
				}

				containers, _ := spec["containers"].([]interface{})
				if len(containers) != 0 {
					t.Errorf("Expected empty containers array (sandbox has no containers), got %v", containers)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			patchBody := DefaultGeneratePatchBodyFunc(tt.opts)

			if tt.expectedEmpty {
				if patchBody != "" {
					t.Errorf("Expected empty patch body, got: %s", patchBody)
				}
				return
			}

			if patchBody == "" {
				t.Fatalf("Expected non-empty patch body, got empty string")
			}

			// Validate JSON format
			var parsed interface{}
			if err := json.Unmarshal([]byte(patchBody), &parsed); err != nil {
				t.Fatalf("Patch body is not valid JSON: %v\nBody: %s", err, patchBody)
			}

			// Run custom checks if provided
			if tt.checkPatchBody != nil {
				tt.checkPatchBody(t, patchBody)
			}
		})
	}
}

// Helper function to check if a string contains a substring
func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) &&
		(s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
			findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
