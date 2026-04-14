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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsapiv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
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
		expectTerminalErr bool
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
						PodAnnotationInPlaceUpdateStateKey: `{"revision":"abc123","updateTimestamp":"2023-01-01T00:00:00Z","updateImages":true,"lastContainerStatuses":{"container1":{"imageID":"image123"}}}`,
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
						PodAnnotationInPlaceUpdateStateKey: `{"revision":"abc123","updateTimestamp":"2023-01-01T00:00:00Z","updateImages":true,"lastContainerStatuses":{"container1":{"imageID":"image123"}}}`,
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
						PodAnnotationInPlaceUpdateStateKey: `{"revision":"abc123","updateTimestamp":"2023-01-01T00:00:00Z","updateImages":true,"lastContainerStatuses":{"container1":{"imageID":"image123"}}}`,
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
			completed, terminalErr := IsInplaceUpdateCompleted(context.TODO(), tt.pod)
			if completed != tt.expectedCompleted {
				t.Errorf("Expected completed=%v, got %v", tt.expectedCompleted, completed)
			}
			if tt.expectTerminalErr && terminalErr == nil {
				t.Errorf("Expected terminal error, got nil")
			}
			if !tt.expectTerminalErr && terminalErr != nil {
				t.Errorf("Unexpected terminal error: %v", terminalErr)
			}
		})
	}
}

func TestResourceOnlyUpdatePayloads(t *testing.T) {
	opts := InPlaceUpdateOptions{
		Box: &agentsapiv1alpha1.Sandbox{
			Spec: agentsapiv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "main",
									Image: "busybox:1.36",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU: resource.MustParse("1000m"),
										},
									},
								},
							},
						},
					},
				},
			},
		},
		Pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "p1",
				Namespace: "default",
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "busybox:1.36",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("500m"),
							},
						},
					},
				},
			},
		},
		Revision: "rev-resource-only",
	}

	patchBody := DefaultGeneratePatchBodyFunc(opts)
	if patchBody == "" {
		t.Fatalf("expected patch body for resource-only update")
	}
	if strings.Contains(patchBody, `"spec"`) {
		t.Fatalf("resource-only patch should not contain spec, got: %s", patchBody)
	}

	resizeBody := DefaultGenerateResizeSubresourceBody(opts)
	if resizeBody == nil {
		t.Fatalf("expected resize subresource body")
	}
	got := resizeBody.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if got.MilliValue() != 1000 {
		t.Fatalf("expected cpu request=1000m, got=%dm", got.MilliValue())
	}
}

func TestIsInplaceUpdateCompletedWithResourceConditions(t *testing.T) {
	state := &InPlaceUpdateState{
		Revision:        "rev-1",
		UpdateTimestamp: metav1.Now(),
		UpdateResources: true,
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state failed: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p1",
			Namespace: "default",
			Annotations: map[string]string{
				PodAnnotationInPlaceUpdateStateKey: string(raw),
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("1000m"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodResizeInProgress,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}

	completed, terminalErr := IsInplaceUpdateCompleted(context.Background(), pod)
	if completed {
		t.Fatalf("expected incomplete while PodResizeInProgress is true")
	}
	if terminalErr != nil {
		t.Fatalf("unexpected terminal error: %v", terminalErr)
	}

	pod.Status.Conditions = nil
	completed, terminalErr = IsInplaceUpdateCompleted(context.Background(), pod)
	if completed {
		t.Fatalf("expected incomplete when no resize signal and no applied resources")
	}
	if terminalErr != nil {
		t.Fatalf("unexpected terminal error: %v", terminalErr)
	}

	pod.Status.Resize = corev1.PodResizeStatusInProgress
	completed, terminalErr = IsInplaceUpdateCompleted(context.Background(), pod)
	if completed {
		t.Fatalf("expected incomplete while resize status is in progress")
	}
	if terminalErr != nil {
		t.Fatalf("unexpected terminal error: %v", terminalErr)
	}

	pod.Status.Resize = ""
	pod.Status.Conditions = []corev1.PodCondition{
		{
			Type:   corev1.PodResizeInProgress,
			Status: corev1.ConditionFalse,
		},
	}
	completed, terminalErr = IsInplaceUpdateCompleted(context.Background(), pod)
	if completed {
		t.Fatalf("expected incomplete when only resize signals exist but resources not applied")
	}
	if terminalErr != nil {
		t.Fatalf("unexpected terminal error: %v", terminalErr)
	}

	// Infeasible condition should return terminal error
	pod.Status.Resize = ""
	pod.Status.Conditions = []corev1.PodCondition{
		{
			Type:    corev1.PodResizePending,
			Status:  corev1.ConditionTrue,
			Reason:  corev1.PodReasonInfeasible,
			Message: "insufficient cpu",
		},
	}
	completed, terminalErr = IsInplaceUpdateCompleted(context.Background(), pod)
	if completed {
		t.Fatalf("expected incomplete when resize is infeasible")
	}
	if terminalErr == nil {
		t.Fatalf("expected terminal error for infeasible resize")
	}
	if !strings.Contains(terminalErr.Error(), "infeasible") {
		t.Fatalf("expected error containing 'infeasible', got: %v", terminalErr)
	}

	// Deferred condition should also return terminal error
	pod.Status.Resize = ""
	pod.Status.Conditions = []corev1.PodCondition{
		{
			Type:    corev1.PodResizePending,
			Status:  corev1.ConditionTrue,
			Reason:  corev1.PodReasonDeferred,
			Message: "node resources temporarily insufficient",
		},
	}
	completed, terminalErr = IsInplaceUpdateCompleted(context.Background(), pod)
	if completed {
		t.Fatalf("expected incomplete when resize is deferred")
	}
	if terminalErr == nil {
		t.Fatalf("expected terminal error for deferred resize")
	}
	if !strings.Contains(terminalErr.Error(), "deferred") {
		t.Fatalf("expected error containing 'deferred', got: %v", terminalErr)
	}

	pod.Status.Resize = ""
	pod.Status.Conditions = nil
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			Name: "main",
			Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1000m"),
				},
			},
		},
	}
	completed, terminalErr = IsInplaceUpdateCompleted(context.Background(), pod)
	if !completed {
		t.Fatalf("expected completed when resources are applied to container status")
	}
	if terminalErr != nil {
		t.Fatalf("unexpected terminal error: %v", terminalErr)
	}
}

func Test_checkPodResizeInfeasible(t *testing.T) {
	tests := []struct {
		name      string
		pod       *corev1.Pod
		wantErr   bool
		errSubstr string
	}{
		{
			name: "no resize conditions - no error",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{},
			},
		},
		{
			name: "PodResizePending with Infeasible reason",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:    corev1.PodResizePending,
							Status:  corev1.ConditionTrue,
							Reason:  corev1.PodReasonInfeasible,
							Message: "insufficient cpu",
						},
					},
				},
			},
			wantErr:   true,
			errSubstr: "infeasible",
		},
		{
			name: "PodResizeInProgress with Error reason",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:    corev1.PodResizeInProgress,
							Status:  corev1.ConditionTrue,
							Reason:  corev1.PodReasonError,
							Message: "cgroup apply failed",
						},
					},
				},
			},
			wantErr:   true,
			errSubstr: "resize error",
		},
		{
			name: "deprecated Resize field is Infeasible",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Resize: corev1.PodResizeStatusInfeasible,
				},
			},
			wantErr:   true,
			errSubstr: "infeasible",
		},
		{
			name: "PodResizePending with Deferred reason",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:    corev1.PodResizePending,
							Status:  corev1.ConditionTrue,
							Reason:  corev1.PodReasonDeferred,
							Message: "node resources temporarily insufficient",
						},
					},
				},
			},
			wantErr:   true,
			errSubstr: "deferred",
		},
		{
			name: "deprecated Resize field is Deferred",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Resize: corev1.PodResizeStatusDeferred,
				},
			},
			wantErr:   true,
			errSubstr: "deferred",
		},
		{
			name: "PodResizePending is False - no error",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:    corev1.PodResizePending,
							Status:  corev1.ConditionFalse,
							Reason:  corev1.PodReasonInfeasible,
							Message: "stale condition",
						},
					},
				},
			},
		},
		{
			name: "Resize field is InProgress - no error",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Resize: corev1.PodResizeStatusInProgress,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkPodResizeInfeasible(tt.pod)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errSubstr)
				}
				if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.errSubstr)) {
					t.Fatalf("expected error containing %q, got: %v", tt.errSubstr, err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCheckResizeQoSChange(t *testing.T) {
	tests := []struct {
		name        string
		box         *agentsapiv1alpha1.Sandbox
		pod         *corev1.Pod
		wantOrig    corev1.PodQOSClass
		wantUpdated corev1.PodQOSClass
		wantChanged bool
	}{
		{
			name: "no QoS change - Burstable stays Burstable",
			box: &agentsapiv1alpha1.Sandbox{
				Spec: agentsapiv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name: "main",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
										Limits: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("1"),
											corev1.ResourceMemory: resource.MustParse("256Mi"),
										},
									},
								}},
							},
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "main",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("250m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
					}},
				},
			},
			wantOrig:    corev1.PodQOSBurstable,
			wantUpdated: corev1.PodQOSBurstable,
			wantChanged: false,
		},
		{
			name: "QoS changes from Burstable to Guaranteed",
			box: &agentsapiv1alpha1.Sandbox{
				Spec: agentsapiv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name: "main",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
										Limits: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("500m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
									},
								}},
							},
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "main",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("250m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					}},
				},
			},
			wantOrig:    corev1.PodQOSBurstable,
			wantUpdated: corev1.PodQOSGuaranteed,
			wantChanged: true,
		},
		{
			name: "nil template - no change",
			box: &agentsapiv1alpha1.Sandbox{
				Spec: agentsapiv1alpha1.SandboxSpec{},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "main",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
						},
					}},
				},
			},
			wantChanged: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig, updated, changed := CheckResizeQoSChange(tt.box, tt.pod)
			if changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tt.wantChanged)
			}
			if tt.wantChanged {
				if orig != tt.wantOrig {
					t.Errorf("orig = %v, want %v", orig, tt.wantOrig)
				}
				if updated != tt.wantUpdated {
					t.Errorf("updated = %v, want %v", updated, tt.wantUpdated)
				}
			}
		})
	}
}

func TestComputeQoSClass(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want corev1.PodQOSClass
	}{
		{
			name: "guaranteed",
			pod: &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
				},
			}}}},
			want: corev1.PodQOSGuaranteed,
		},
		{
			name: "burstable - only requests",
			pod: &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				},
			}}}},
			want: corev1.PodQOSBurstable,
		},
		{
			name: "best effort",
			pod:  &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}}},
			want: corev1.PodQOSBestEffort,
		},
		{
			name: "pod-level resources - guaranteed",
			pod: &corev1.Pod{Spec: corev1.PodSpec{
				Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")},
				},
				Containers: []corev1.Container{{Name: "c"}},
			}},
			want: corev1.PodQOSGuaranteed,
		},
		{
			name: "pod-level resources - burstable (limits != requests)",
			pod: &corev1.Pod{Spec: corev1.PodSpec{
				Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("2Gi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")},
				},
				Containers: []corev1.Container{{Name: "c"}},
			}},
			want: corev1.PodQOSBurstable,
		},
		{
			name: "pod-level resources - burstable (only cpu limits)",
			pod: &corev1.Pod{Spec: corev1.PodSpec{
				Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				},
				Containers: []corev1.Container{{Name: "c"}},
			}},
			want: corev1.PodQOSBurstable,
		},
		{
			name: "pod-level resources take precedence over container resources",
			pod: &corev1.Pod{Spec: corev1.PodSpec{
				Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
				},
				Containers: []corev1.Container{{
					Name: "c",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					},
				}},
			}},
			want: corev1.PodQOSGuaranteed,
		},
		{
			name: "pod-level resources - best effort (empty)",
			pod: &corev1.Pod{Spec: corev1.PodSpec{
				Resources:  &corev1.ResourceRequirements{},
				Containers: []corev1.Container{{Name: "c"}},
			}},
			want: corev1.PodQOSBestEffort,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeQoSClass(tt.pod)
			if got != tt.want {
				t.Errorf("computeQoSClass() = %v, want %v", got, tt.want)
			}
		})
	}
}
