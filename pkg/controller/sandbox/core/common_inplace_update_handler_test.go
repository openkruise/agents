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

package core

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
)

// MockInPlaceUpdateHandler mocks the handler implementation
type MockInPlaceUpdateHandler struct {
	control  *inplaceupdate.InPlaceUpdateControl
	recorder record.EventRecorder
	logger   logr.Logger
}

func (m *MockInPlaceUpdateHandler) GetInPlaceUpdateControl() *inplaceupdate.InPlaceUpdateControl {
	return m.control
}

func (m *MockInPlaceUpdateHandler) GetRecorder() record.EventRecorder {
	return m.recorder
}

func (m *MockInPlaceUpdateHandler) GetLogger(ctx context.Context, box *agentsv1alpha1.Sandbox) logr.Logger {
	return m.logger
}

// runClaimInplaceUpdate drives the SandboxClaim adapter with the legacy test
// inputs, so the pre-SUO claim expectations below are checked unchanged.
func runClaimInplaceUpdate(ctx context.Context, handler InPlaceUpdateHandler, pod *corev1.Pod, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus) (bool, error) {
	r := &commonControl{inplaceUpdateControl: handler.GetInPlaceUpdateControl(), recorder: handler.GetRecorder()}
	return r.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
}

func assertInplaceConditions(t *testing.T, status *agentsv1alpha1.SandboxStatus, ready bool, reason string) {
	t.Helper()
	update := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
	require.NotNil(t, update)
	require.Equal(t, reason, update.Reason)
	if reason == agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded {
		require.Equal(t, metav1.ConditionTrue, update.Status)
	} else {
		require.Equal(t, metav1.ConditionFalse, update.Status)
	}
	condition := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionReady))
	require.NotNil(t, condition)
	if ready {
		require.Equal(t, metav1.ConditionTrue, condition.Status)
	} else {
		require.Equal(t, metav1.ConditionFalse, condition.Status)
		if reason == agentsv1alpha1.SandboxInplaceUpdateReasonFailed {
			require.Equal(t, agentsv1alpha1.SandboxInplaceUpdateReasonFailed, condition.Reason)
			require.Equal(t, update.Message, condition.Message)
		} else {
			require.Equal(t, agentsv1alpha1.SandboxReadyReasonInplaceUpdating, condition.Reason)
		}
	}
}

// Create test event recorder
func createTestRecorder() record.EventRecorder {
	scheme := runtime.NewScheme()
	agentsv1alpha1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)
	return record.NewFakeRecorder(100)
}

func TestHandleInPlaceUpdateCommon(t *testing.T) {
	// Test cases definition
	testCases := []struct {
		name           string
		pod            *corev1.Pod
		box            *agentsv1alpha1.Sandbox
		newStatus      *agentsv1alpha1.SandboxStatus
		setupHandler   func() InPlaceUpdateHandler
		expectedResult bool
		expectError    bool
		description    string
	}{
		{
			name: "pod without template hash label should return true",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{}, // No pod-template-hash label
				},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
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
			newStatus: &agentsv1alpha1.SandboxStatus{
				UpdateRevision: "test-revision",
			},
			setupHandler: func() InPlaceUpdateHandler {
				recorder := createTestRecorder()
				return &MockInPlaceUpdateHandler{
					control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
					recorder: recorder,
					logger:   logr.Discard(),
				}
			},
			expectedResult: true,
			expectError:    false,
			description:    "When Pod has no template hash label, should return true immediately",
		},
		{
			name: "hash mismatch should return true",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						agentsv1alpha1.PodLabelTemplateHash: "old-hash",
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.SandboxHashImmutablePart: "new-hash", // Mismatch with Pod label
					},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
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
			newStatus: &agentsv1alpha1.SandboxStatus{
				UpdateRevision: "test-revision",
			},
			setupHandler: func() InPlaceUpdateHandler {
				recorder := createTestRecorder()
				return &MockInPlaceUpdateHandler{
					control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
					recorder: recorder,
					logger:   logr.Discard(),
				}
			},
			expectedResult: true,
			expectError:    false,
			description:    "When hash mismatch occurs, should return true",
		},
		{
			name: "missing SandboxHashImmutablePart annotation should skip hash check",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.PodLabelTemplateHash: "test-revision",
					},
					Annotations: map[string]string{
						// Previous inplace update completed (no updateImages/updateResources flags)
						inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"prev-revision","updateTimestamp":"2024-01-01T00:00:00Z"}`,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "nginx:latest",
						},
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-sandbox",
					Namespace:   "default",
					Annotations: map[string]string{}, // No SandboxHashImmutablePart annotation
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
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
			newStatus: &agentsv1alpha1.SandboxStatus{
				UpdateRevision: "test-revision",
			},
			setupHandler: func() InPlaceUpdateHandler {
				scheme := runtime.NewScheme()
				_ = clientgoscheme.AddToScheme(scheme)
				_ = agentsv1alpha1.AddToScheme(scheme)

				recorder := createTestRecorder()
				return &MockInPlaceUpdateHandler{
					control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
					recorder: recorder,
					logger:   logr.Discard(),
				}
			},
			expectedResult: true,
			expectError:    false,
			description:    "When SandboxHashImmutablePart annotation is missing, hash check should be skipped and continue processing",
		},
		{
			name: "revision consistent and update completed should return true",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						agentsv1alpha1.PodLabelTemplateHash: "test-revision", // Matches newStatus.UpdateRevision
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.SandboxHashImmutablePart: "test-revision",
					},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
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
			newStatus: &agentsv1alpha1.SandboxStatus{
				UpdateRevision: "test-revision",
			},
			setupHandler: func() InPlaceUpdateHandler {
				recorder := createTestRecorder()
				return &MockInPlaceUpdateHandler{
					control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
					recorder: recorder,
					logger:   logr.Discard(),
				}
			},
			expectedResult: true,
			expectError:    false,
			description:    "When revision is consistent and update completed, should return true",
		},
	}

	// Execute test cases
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Set up test context
			ctx := context.Background()

			// Create handler
			handler := tc.setupHandler()

			// Execute function
			result, err := runClaimInplaceUpdate(ctx, handler, tc.pod, tc.box, tc.newStatus)

			// Verify result
			if result != tc.expectedResult {
				t.Errorf("Expected result %v, but got %v", tc.expectedResult, result)
			}

			// Verify error
			if tc.expectError && err == nil {
				t.Error("Expected error but got none")
			}
			if !tc.expectError && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}
}

func TestHandleInPlaceUpdateCommon_WithUpdateInProgress(t *testing.T) {
	// Test when update is in progress
	ctx := context.Background()

	// Create Pod with update state
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-revision",
			},
			Annotations: map[string]string{
				inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"new-revision","updateTimestamp":"2023-01-01T00:00:00Z"}`,
			},
		},
	}

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.SandboxHashImmutablePart: "current-hash",
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
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
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
	}

	// Create handler
	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	// Execute function
	result, err := runClaimInplaceUpdate(ctx, handler, pod, box, newStatus)

	// Verify result
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// Should return true because there's an ongoing update
	if result != true {
		t.Errorf("Expected result true, but got %v", result)
	}
}

func TestHandleInPlaceUpdateCommon_QoSChangeRejected(t *testing.T) {
	ctx := context.Background()

	// Pod is Burstable: CPU req=250m lim=500m, Memory req=128Mi lim=128Mi
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-revision",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "main",
				Image: "nginx:latest",
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
	}

	_, hashWithoutImageAndResource := HashSandbox(&agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: pod.Spec,
				},
			},
		},
	})

	// Sandbox template resizes CPU to 500m/500m → with memory 128Mi/128Mi → all req==lim → Guaranteed
	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.SandboxHashImmutablePart: hashWithoutImageAndResource,
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "main",
							Image: "nginx:latest",
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
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
	}

	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := runClaimInplaceUpdate(ctx, handler, pod, box, newStatus)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result {
		t.Error("Expected result true (done, no requeue), got false")
	}
	assertInplaceConditions(t, newStatus, false, agentsv1alpha1.SandboxInplaceUpdateReasonFailed)

	// Verify InplaceUpdate condition is set to Failed
	var found bool
	for _, cond := range newStatus.Conditions {
		if cond.Type == string(agentsv1alpha1.SandboxConditionInplaceUpdate) {
			found = true
			if cond.Reason != agentsv1alpha1.SandboxInplaceUpdateReasonFailed {
				t.Errorf("Expected reason %s, got %s", agentsv1alpha1.SandboxInplaceUpdateReasonFailed, cond.Reason)
			}
			if cond.Status != metav1.ConditionFalse {
				t.Errorf("Expected status ConditionFalse, got %s", cond.Status)
			}
			if cond.Message == "" {
				t.Error("Expected non-empty message about QoS change")
			}
		}
	}
	if !found {
		t.Error("InplaceUpdate condition not found in status")
	}
}

func TestHandleInPlaceUpdateCommon_MemoryDownscaleSkippedAdvisory(t *testing.T) {
	// The pod's live memory (256Mi) was raised above the template (128Mi) by
	// the environment. A memory downscale must NOT hard-fail the rollout:
	// the image update proceeds and only an advisory event is emitted.
	ctx := context.Background()

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	oldPodSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "nginx:old",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
			},
		}},
	}

	newPodSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "nginx:new",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
			},
		}},
	}

	box := buildMatchingHashBox("test-sandbox", "default", newPodSpec)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-revision",
			},
		},
		Spec: oldPodSpec,
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
	}

	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := runClaimInplaceUpdate(ctx, handler, pod, box, newStatus)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if result {
		t.Error("Expected result false (update in progress), got true")
	}

	// The image rollout must proceed: condition is InplaceUpdating, not Failed.
	for _, cond := range newStatus.Conditions {
		if cond.Type != string(agentsv1alpha1.SandboxConditionInplaceUpdate) {
			continue
		}
		if cond.Reason == agentsv1alpha1.SandboxInplaceUpdateReasonFailed {
			t.Errorf("Expected no InplaceUpdate failure, got reason %s message %q", cond.Reason, cond.Message)
		}
	}

	// An advisory event about the skipped downscale must be recorded.
	fakeRecorder := recorder.(*record.FakeRecorder)
	found := false
	for len(fakeRecorder.Events) > 0 {
		if ev := <-fakeRecorder.Events; strings.Contains(ev, "MemoryDownscaleSkipped") {
			found = true
		}
	}
	if !found {
		t.Error("Expected MemoryDownscaleSkipped event, none recorded")
	}
}

func TestHandleInPlaceUpdateCommon_UnsupportedResizeRetried(t *testing.T) {
	ctx := context.Background()

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	oldPodSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "nginx:old",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
			},
		}},
	}
	newPodSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "nginx:old",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			},
		}},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-revision",
			},
		},
		Spec: oldPodSpec,
	}
	box := buildMatchingHashBox("test-sandbox", "default", newPodSpec)

	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	injected := apierrors.NewBadRequest("InPlacePodVerticalScaling feature gate disabled")
	failWrites := true
	resourcePatches := 0
	wrapped := interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if sub == "resize" {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, obj.GetName())
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			data, err := patch.Data(obj)
			require.NoError(t, err)
			if !strings.Contains(string(data), `"metadata"`) {
				resourcePatches++
				if failWrites {
					return injected
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	newStatus := &agentsv1alpha1.SandboxStatus{UpdateRevision: "new-revision"}
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(wrapped, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: createTestRecorder(),
		logger:   logr.Discard(),
	}

	for attempt := 1; attempt <= 2; attempt++ {
		before := newStatus.DeepCopy()
		injected = apierrors.NewBadRequest(fmt.Sprintf("InPlacePodVerticalScaling feature gate disabled, attempt %d", attempt))
		done, err := runClaimInplaceUpdate(ctx, handler, pod, box, newStatus)
		require.ErrorIs(t, err, injected)
		var resizeErr *inplaceupdate.ResizeNotSupportedError
		require.ErrorAs(t, err, &resizeErr)
		require.False(t, done)
		assertInplaceConditions(t, newStatus, false, agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating)
		require.False(t, isInplaceUpdateTerminal(newStatus))
		cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
		require.Equal(t, "cannot deliver in-place update: in-place pod resize not supported: BadRequest (HTTP 400)", cond.Message)
		require.Len(t, handler.recorder.(*record.FakeRecorder).Events, 1)
		if attempt > 1 {
			require.Equal(t, before, newStatus)
		}
		require.Equal(t, attempt, resourcePatches)
	}

	current := &corev1.Pod{}
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(pod), current))
	require.Equal(t, "old-revision", current.Labels[agentsv1alpha1.PodLabelTemplateHash])
	require.Equal(t, oldPodSpec, current.Spec)

	failWrites = false
	done, err := runClaimInplaceUpdate(ctx, handler, current, box, newStatus)
	require.NoError(t, err)
	require.False(t, done)
	assertInplaceConditions(t, newStatus, false, agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating)
	require.Equal(t, 3, resourcePatches)
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(pod), current))
	require.Equal(t, "new-revision", current.Labels[agentsv1alpha1.PodLabelTemplateHash])
	require.Equal(t, newPodSpec, current.Spec)
	state, err := inplaceupdate.GetPodInPlaceUpdateState(current)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.True(t, state.UpdateResources)

	current.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "main", Resources: current.Spec.Containers[0].Resources.DeepCopy(),
	}}
	done, err = runClaimInplaceUpdate(ctx, handler, current, box, newStatus)
	require.NoError(t, err)
	require.True(t, done)
	// The adapter leaves Ready to syncStatusFromPod, so the gate stays closed here.
	assertInplaceConditions(t, newStatus, false, agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded)
	require.Equal(t, 3, resourcePatches)
}

func TestHandleInPlaceUpdateCommon_ResizeInfeasibleFailFast(t *testing.T) {
	ctx := context.Background()

	// Simulate a pod where resize has been initiated (revision matches) but
	// the kubelet reported Infeasible via PodResizePending condition.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "target-revision",
			},
			Annotations: map[string]string{
				inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"target-revision","updateTimestamp":"2024-01-01T00:00:00Z","updateResources":true}`,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "main",
				Image: "nginx:latest",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("2000m"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("2000m"),
					},
				},
			}},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:    corev1.PodResizePending,
					Status:  corev1.ConditionTrue,
					Reason:  corev1.PodReasonInfeasible,
					Message: "insufficient cpu on node",
				},
			},
		},
	}

	_, hashWithoutImageAndResource := HashSandbox(&agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{Spec: pod.Spec},
			},
		},
	})

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.SandboxHashImmutablePart: hashWithoutImageAndResource,
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{Spec: pod.Spec},
			},
		},
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "target-revision",
	}

	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := runClaimInplaceUpdate(ctx, handler, pod, box, newStatus)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result {
		t.Fatal("Expected result true (done, fail-fast), got false")
	}
	assertInplaceConditions(t, newStatus, false, agentsv1alpha1.SandboxInplaceUpdateReasonFailed)

	var found bool
	for _, cond := range newStatus.Conditions {
		if cond.Type == string(agentsv1alpha1.SandboxConditionInplaceUpdate) {
			found = true
			if cond.Reason != agentsv1alpha1.SandboxInplaceUpdateReasonFailed {
				t.Errorf("Expected reason %s, got %s", agentsv1alpha1.SandboxInplaceUpdateReasonFailed, cond.Reason)
			}
			if cond.Status != metav1.ConditionFalse {
				t.Errorf("Expected status ConditionFalse, got %s", cond.Status)
			}
			if cond.Message == "" {
				t.Error("Expected non-empty message about infeasible resize")
			}
		}
	}
	if !found {
		t.Error("InplaceUpdate condition not found in status")
	}
}

func TestHandleInPlaceUpdateCommon_TerminalFailureNotOverwritten(t *testing.T) {
	ctx := context.Background()

	// Simulate the race condition: resize subresource failed (pod spec was never
	// updated), so pod spec == pod status == old values. Without the fix,
	// isPodResourceResizeCompleted would falsely report completion and overwrite
	// the Failed condition with Succeeded.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "target-revision",
			},
			Annotations: map[string]string{
				inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"target-revision","updateTimestamp":"2024-01-01T00:00:00Z","updateResources":true}`,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "main",
				Image: "nginx:latest",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				},
			}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "main",
				Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				},
			}},
		},
	}

	_, hashWithoutImageAndResource := HashSandbox(&agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{Spec: pod.Spec},
			},
		},
	})

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.SandboxHashImmutablePart: hashWithoutImageAndResource,
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{Spec: pod.Spec},
			},
		},
	}

	// newStatus already has InplaceUpdate: Failed from a previous reconcile
	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "target-revision",
		Conditions: []metav1.Condition{
			{
				Type:    string(agentsv1alpha1.SandboxConditionInplaceUpdate),
				Status:  metav1.ConditionFalse,
				Reason:  agentsv1alpha1.SandboxInplaceUpdateReasonUnsupportedResize,
				Message: "in-place pod resize not supported: the server could not find the requested resource",
			},
		},
	}

	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := runClaimInplaceUpdate(ctx, handler, pod, box, newStatus)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result {
		t.Fatal("Expected result true (done), got false")
	}

	for _, cond := range newStatus.Conditions {
		if cond.Type == string(agentsv1alpha1.SandboxConditionInplaceUpdate) {
			if cond.Reason != agentsv1alpha1.SandboxInplaceUpdateReasonUnsupportedResize {
				t.Errorf("Expected InplaceUpdate condition to remain UnsupportedResize, got %s", cond.Reason)
			}
			return
		}
	}
	t.Error("InplaceUpdate condition not found")
}

func TestHandleInPlaceUpdateCommon_InitialState(t *testing.T) {
	// Test initial state with no ongoing update
	ctx := context.Background()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-revision",
			},
		},
	}

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.SandboxHashImmutablePart: "current-hash",
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:updated",
							},
						},
					},
				},
			},
		},
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
	}

	// Create handler
	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	// Execute function
	result, err := runClaimInplaceUpdate(ctx, handler, pod, box, newStatus)

	// Verify result
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// Should return true when no changes occurred
	if result != true {
		t.Errorf("Expected result false, but got %v", result)
	}
}

// buildMatchingHashBox creates a sandbox with correct hash for the given podSpec
// so that handleInPlaceUpdateCommon passes the hash-immutable-part check.
func buildMatchingHashBox(name, ns string, podSpec corev1.PodSpec) *agentsv1alpha1.Sandbox {
	tmpBox := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: podSpec,
				},
			},
		},
	}
	_, hashImmutablePart := HashSandbox(tmpBox)
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Annotations: map[string]string{
				agentsv1alpha1.SandboxHashImmutablePart: hashImmutablePart,
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: podSpec,
				},
			},
		},
	}
}

func TestHandleInPlaceUpdateCommon_RevisionMatchCompletedSucceeded(t *testing.T) {
	// Revision matches, no in-place state annotation → IsInplaceUpdateCompleted returns true
	// → sets Succeeded condition
	ctx := context.Background()

	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "nginx:latest",
		}},
	}

	box := buildMatchingHashBox("test-sandbox", "default", podSpec)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "target-rev",
			},
			// No inplace update state annotation → completed = true
		},
		Spec: podSpec,
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "target-rev",
	}

	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := runClaimInplaceUpdate(ctx, handler, pod, box, newStatus)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result {
		t.Fatal("Expected result true, got false")
	}

	// Verify Succeeded condition is set
	var found bool
	for _, cond := range newStatus.Conditions {
		if cond.Type == string(agentsv1alpha1.SandboxConditionInplaceUpdate) {
			found = true
			if cond.Reason != agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded {
				t.Errorf("Expected reason %s, got %s", agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, cond.Reason)
			}
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("Expected status ConditionTrue, got %s", cond.Status)
			}
		}
	}
	if !found {
		t.Error("InplaceUpdate Succeeded condition not found")
	}
}

func TestHandleInPlaceUpdateCommon_AlreadySucceededIdempotent(t *testing.T) {
	// Revision matches and the InplaceUpdate condition is already Succeeded
	// → isInplaceUpdateTerminal short-circuits: done=true, newStatus untouched,
	// so the caller's updateSandboxStatus makes no write and the Reconcile
	// stays eligible for no-op filtering.
	ctx := context.Background()

	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "nginx:latest",
		}},
	}

	box := buildMatchingHashBox("test-sandbox", "default", podSpec)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "target-rev",
			},
		},
		Spec: podSpec,
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "target-rev",
		Conditions: []metav1.Condition{{
			Type:   string(agentsv1alpha1.SandboxConditionInplaceUpdate),
			Status: metav1.ConditionTrue,
			Reason: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded,
		}},
	}

	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	done, err := runClaimInplaceUpdate(ctx, handler, pod, box, newStatus)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !done {
		t.Error("Expected done true (idempotent no-op continues status sync), got false")
	}
}

func TestHandleInPlaceUpdateCommon_RevisionMatchImageUpdateInProgress(t *testing.T) {
	// Revision matches, image update in progress (not completed, no terminal error)
	// → return false, nil
	ctx := context.Background()

	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "nginx:latest",
		}},
	}

	box := buildMatchingHashBox("test-sandbox", "default", podSpec)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "target-rev",
			},
			Annotations: map[string]string{
				// In-place state with image update, but image not yet updated
				inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"target-rev","updateTimestamp":"2024-01-01T00:00:00Z","updateImages":true,"lastContainerStatuses":{"main":{"imageID":"old-image-id"}}}`,
			},
		},
		Spec: podSpec,
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:    "main",
				ImageID: "old-image-id", // Same as old, not updated yet
			}},
		},
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "target-rev",
	}

	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := runClaimInplaceUpdate(ctx, handler, pod, box, newStatus)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if result {
		t.Fatal("Expected result false (still in progress), got true")
	}
}

func TestHandleInPlaceUpdateCommon_GetPodInPlaceUpdateStateError(t *testing.T) {
	// Pod has malformed inplace state annotation → GetPodInPlaceUpdateState returns error
	ctx := context.Background()

	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "nginx:latest",
		}},
	}

	box := buildMatchingHashBox("test-sandbox", "default", podSpec)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-revision",
			},
			Annotations: map[string]string{
				// Malformed JSON for inplace update state
				inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{invalid-json`,
			},
		},
		Spec: podSpec,
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
	}

	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := runClaimInplaceUpdate(ctx, handler, pod, box, newStatus)
	require.NoError(t, err)
	require.True(t, result, "corrupt in-place update state must fail terminally without retries")
	condition := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
	require.NotNil(t, condition)
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.Equal(t, agentsv1alpha1.SandboxInplaceUpdateReasonFailed, condition.Reason)
	require.Contains(t, condition.Message, "cannot determine in-place update progress")
}

func TestHandleInPlaceUpdateCommon_StateNotNilCompleted(t *testing.T) {
	// state != nil, update is completed → return true, nil
	ctx := context.Background()

	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "nginx:latest",
		}},
	}

	box := buildMatchingHashBox("test-sandbox", "default", podSpec)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-revision",
			},
			Annotations: map[string]string{
				// Previous update state without updateImages/updateResources → completed=true
				inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"prev-revision","updateTimestamp":"2024-01-01T00:00:00Z"}`,
			},
		},
		Spec: podSpec,
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := runClaimInplaceUpdate(ctx, handler, pod, box, newStatus)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result {
		t.Error("Expected result true (completed), got false")
	}
	stored := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), stored))
	require.Equal(t, "new-revision", stored.Labels[agentsv1alpha1.PodLabelTemplateHash])
}

func TestHandleInPlaceUpdateCommon_StateNotNilNotCompletedTerminalErr(t *testing.T) {
	// A metadata-only target is delivered on top of an infeasible earlier
	// resize. The write keeps the earlier state, so observing the delivered pod
	// reports the infeasible resize as a terminal failure instead of success.
	ctx := context.Background()

	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "nginx:latest",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2000m")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2000m")},
			},
		}},
	}

	box := buildMatchingHashBox("test-sandbox", "default", podSpec)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-revision",
			},
			Annotations: map[string]string{
				inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"prev-revision","updateTimestamp":"2024-01-01T00:00:00Z","updateResources":true}`,
			},
		},
		Spec: podSpec,
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{
				Type:    corev1.PodResizePending,
				Status:  corev1.ConditionTrue,
				Reason:  corev1.PodReasonInfeasible,
				Message: "insufficient cpu",
			}},
		},
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := runClaimInplaceUpdate(ctx, handler, pod, box, newStatus)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result {
		t.Error("Expected result true (terminal failure), got false")
	}
	update := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
	require.NotNil(t, update)
	require.Equal(t, agentsv1alpha1.SandboxInplaceUpdateReasonFailed, update.Reason)
	require.Contains(t, update.Message, "insufficient cpu")
	stored := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), stored))
	require.Equal(t, "new-revision", stored.Labels[agentsv1alpha1.PodLabelTemplateHash])
}

func TestHandleInPlaceUpdateCommon_StateNotNilNotCompletedNoTerminalErr(t *testing.T) {
	// A metadata-only target is delivered on top of an earlier resize that has
	// not completed yet. The write keeps the earlier state, so the round stays
	// in progress and Ready is gated instead of reporting completion.
	ctx := context.Background()

	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "nginx:latest",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			},
		}},
	}

	box := buildMatchingHashBox("test-sandbox", "default", podSpec)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-revision",
			},
			Annotations: map[string]string{
				inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"prev-revision","updateTimestamp":"2024-01-01T00:00:00Z","updateResources":true}`,
			},
		},
		Spec: podSpec,
		Status: corev1.PodStatus{
			// No resize pending condition, but resources not yet reflected in status
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "main",
				Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
				},
			}},
		},
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := runClaimInplaceUpdate(ctx, handler, pod, box, newStatus)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if result {
		t.Error("Expected result false while the earlier resize is in progress, got true")
	}
	assertInplaceConditions(t, newStatus, false, agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating)
	stored := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), stored))
	require.Equal(t, "new-revision", stored.Labels[agentsv1alpha1.PodLabelTemplateHash])
}

func TestHandleInPlaceUpdateCommon_InplaceUpdateWithFakeClient(t *testing.T) {
	// No prior state, no QoS change, image changed → control.Update is called
	ctx := context.Background()

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	oldPodSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "nginx:old",
		}},
	}

	newPodSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "nginx:new",
		}},
	}

	box := buildMatchingHashBox("test-sandbox", "default", newPodSpec)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-revision",
			},
		},
		Spec: oldPodSpec,
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:    "main",
				ImageID: "docker://sha256:old",
			}},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
	}

	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := runClaimInplaceUpdate(ctx, handler, pod, box, newStatus)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	// control.Update should succeed (image patch), returns changed=true
	// → return done=false (in progress); the pod patch goes through the
	// write-tracking client, which marks the Reconcile as a write.
	if result {
		t.Error("Expected result false (update in progress), got true")
	}

	// Verify markInProgress was called: InplaceUpdate condition should be set to InplaceUpdating
	var foundInplace bool
	for _, cond := range newStatus.Conditions {
		if cond.Type == string(agentsv1alpha1.SandboxConditionInplaceUpdate) {
			foundInplace = true
			if cond.Reason != agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating {
				t.Errorf("Expected reason %s, got %s", agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating, cond.Reason)
			}
		}
	}
	if !foundInplace {
		t.Error("InplaceUpdate condition not found (markInProgress should have been called)")
	}
}

func TestHandleInPlaceUpdateCommon_NoChangeReturnsTrue(t *testing.T) {
	// No prior state, no QoS change, same image/resources → control.Update returns !changed
	ctx := context.Background()

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "nginx:latest",
		}},
	}

	box := buildMatchingHashBox("test-sandbox", "default", podSpec)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				// Same revision as newStatus → will NOT match (pod hash != update revision)
				agentsv1alpha1.PodLabelTemplateHash: "old-revision",
			},
		},
		Spec: podSpec,
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
		Conditions: []metav1.Condition{{
			Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse,
			Reason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating, Message: "inplace update is incompleted", LastTransitionTime: metav1.Now(),
		}},
	}

	// Use a custom patchBodyFunc that returns empty (no changes)
	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control: inplaceupdate.NewInPlaceUpdateControl(fakeClient, func(opts inplaceupdate.InPlaceUpdateOptions) (string, error) {
			return "", nil // No patch needed
		}),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := runClaimInplaceUpdate(ctx, handler, pod, box, newStatus)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	// control.Update returns !changed → return true, nil
	if !result {
		t.Error("Expected result true (no changes), got false")
	}
	// The adapter leaves Ready to syncStatusFromPod, so the gate stays closed here.
	assertInplaceConditions(t, newStatus, false, agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded)
	update := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
	require.Equal(t, "in-place update completed successfully", update.Message)
	ready := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady))
	require.Equal(t, "inplace update is incompleted", ready.Message)
}

func TestHandleInPlaceUpdateCommon_MetadataOnlyChange(t *testing.T) {
	// A completed metadata-only write records success without touching Ready.
	ctx := context.Background()

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "nginx:latest",
		}},
	}

	box := buildMatchingHashBox("test-sandbox", "default", podSpec)
	// Add labels to the sandbox template that the pod does not have.
	// This is a metadata-only change — hash-immutable-part is unaffected.
	box.Spec.Template.Labels = map[string]string{
		"app": "test-app",
	}

	// Compute the new revision hash (includes labels)
	hash, _ := HashSandbox(box)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-revision",
			},
		},
		Spec: podSpec, // Same image and resources as box template
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: hash,
	}

	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := runClaimInplaceUpdate(ctx, handler, pod, box, newStatus)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result {
		t.Error("Expected result true (metadata patched directly), got false")
	}

	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
	require.Equal(t, agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, cond.Reason)
	require.Equal(t, "in-place update completed successfully", cond.Message)
	// Ready is owned by syncStatusFromPod, not by the adapter.
	require.Nil(t, utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady)))

	// Verify the pod was actually patched: template hash label should match new revision
	updatedPod := &corev1.Pod{}
	if err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-pod"}, updatedPod); err != nil {
		t.Fatalf("Failed to get updated pod: %v", err)
	}
	if updatedPod.Labels[agentsv1alpha1.PodLabelTemplateHash] != hash {
		t.Errorf("Expected pod template hash to be %s, got %s", hash, updatedPod.Labels[agentsv1alpha1.PodLabelTemplateHash])
	}
	if updatedPod.Labels["app"] != "test-app" {
		t.Errorf("Expected pod label app=test-app, got %s", updatedPod.Labels["app"])
	}
}

func TestHandleInPlaceUpdateCommon_MetadataOnlyChangeWriteFailed(t *testing.T) {
	// Metadata-only write failures also record conditions, gate Ready, and return the full error for retries.
	invalid := func(detail string, fields ...string) error {
		status := metav1.Status{Reason: metav1.StatusReasonInvalid, Code: 422, Message: detail,
			Details: &metav1.StatusDetails{}}
		for _, name := range fields {
			status.Details.Causes = append(status.Details.Causes, metav1.StatusCause{
				Type: metav1.CauseTypeFieldValueInvalid, Field: name, Message: detail,
			})
		}
		return &apierrors.StatusError{ErrStatus: status}
	}
	conflict := func(detail string) error {
		return apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "test-pod", fmt.Errorf("%s", detail))
	}
	forbidden := func(detail string) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "test-pod", fmt.Errorf("%s", detail))
	}
	networkError := func(detail string, timeout bool) error {
		return &net.OpError{Op: "write", Net: "tcp", Err: &net.DNSError{Err: detail, Name: detail, IsTimeout: timeout}}
	}
	const prefix = "cannot deliver in-place update: "
	tests := []struct {
		name                 string
		first, second        error
		message, nextMessage string
	}{
		{name: "conflict ignores resourceVersion", first: conflict("resourceVersion=1"), second: conflict("resourceVersion=2"), message: prefix + "Conflict (HTTP 409)"},
		{name: "forbidden ignores dynamic details", first: forbidden("diff at time=1"), second: forbidden("diff at time=2"), message: prefix + "Forbidden (HTTP 403)"},
		{name: "unauthorized", first: apierrors.NewUnauthorized("request=1"), second: apierrors.NewUnauthorized("request=2"), message: prefix + "Unauthorized (HTTP 401)"},
		{name: "bad request", first: apierrors.NewBadRequest("diff=1"), second: apierrors.NewBadRequest("diff=2"), message: prefix + "BadRequest (HTTP 400)"},
		{name: "invalid sorts and deduplicates fields", first: invalid("PodSpec diff=1", "spec.volumes", "spec.containers", "spec.volumes"), second: invalid("PodSpec diff=2", "spec.containers", "spec.volumes"), message: prefix + "Invalid (HTTP 422); fields: spec.containers, spec.volumes"},
		{name: "throttling ignores retryAfter", first: apierrors.NewTooManyRequests("request=1", 1), second: apierrors.NewTooManyRequests("request=2", 30), message: prefix + "TooManyRequests (HTTP 429)"},
		{name: "wrapped resize failure", first: fmt.Errorf("request=1: %w", &inplaceupdate.ResizeNotSupportedError{Err: forbidden("diff=1")}), second: fmt.Errorf("request=2: %w", &inplaceupdate.ResizeNotSupportedError{Err: forbidden("diff=2")}), message: prefix + "in-place pod resize not supported: Forbidden (HTTP 403)"},
		{name: "timeout preserves error chain", first: context.DeadlineExceeded, second: fmt.Errorf("request=2: %w", context.DeadlineExceeded), message: prefix + "timeout"},
		{name: "canceled preserves error chain", first: context.Canceled, second: fmt.Errorf("request=2: %w", context.Canceled), message: prefix + "context canceled"},
		{name: "network timeout ignores endpoints", first: networkError("endpoint=1", true), second: networkError("endpoint=2", true), message: prefix + "network timeout"},
		{name: "network error ignores endpoints", first: networkError("endpoint=1", false), second: networkError("endpoint=2", false), message: prefix + "network error"},
		{name: "unknown error never copies raw message", first: fmt.Errorf("unexpected diff=1"), second: fmt.Errorf("unexpected diff=2"), message: prefix + "unexpected error (see controller logs for details)"},
		{name: "API status without reason or code", first: &apierrors.StatusError{ErrStatus: metav1.Status{Message: "diff=1"}}, second: &apierrors.StatusError{ErrStatus: metav1.Status{Message: "diff=2"}}, message: prefix + "API error"},
		{name: "changed error category emits again", first: conflict("resourceVersion=1"), second: forbidden("diff=2"), message: prefix + "Conflict (HTTP 409)", nextMessage: prefix + "Forbidden (HTTP 403)"},
		{name: "changed field emits again", first: invalid("diff=1", "spec.containers"), second: invalid("diff=2", "spec.volumes"), message: prefix + "Invalid (HTTP 422); fields: spec.containers", nextMessage: prefix + "Invalid (HTTP 422); fields: spec.volumes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, clientgoscheme.AddToScheme(scheme))
			require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
			podSpec := corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "nginx:latest"}}}
			box := buildMatchingHashBox("test-sandbox", "default", podSpec)
			box.Spec.Template.Labels = map[string]string{"app": "test-app"}
			hash, _ := HashSandbox(box)
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod", Namespace: "default",
					Labels: map[string]string{agentsv1alpha1.PodLabelTemplateHash: "old-revision"},
					// Retain an unfinished image record so a recovered write remains InProgress.
					Annotations: map[string]string{inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"old-revision","updateImages":true,"lastContainerStatuses":{"main":{"targetImage":"nginx:latest"}}}`},
				},
				Spec: podSpec,
			}
			var patchErr error
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).
				WithInterceptorFuncs(interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if patchErr != nil {
							return patchErr
						}
						return c.Patch(ctx, obj, patch, opts...)
					},
				}).Build()
			newStatus := &agentsv1alpha1.SandboxStatus{UpdateRevision: hash}
			fakeRecorder := record.NewFakeRecorder(10)
			handler := &MockInPlaceUpdateHandler{
				control:  inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
				recorder: fakeRecorder, logger: logr.Discard(),
			}

			for attempt, cause := range []error{tt.first, tt.second} {
				// Start each round from a copy of the persisted status to verify deduplication across reconciles.
				if attempt > 0 {
					box.Status = *newStatus.DeepCopy()
					newStatus = box.Status.DeepCopy()
				}
				before := newStatus.DeepCopy()
				patchErr = cause
				done, err := runClaimInplaceUpdate(t.Context(), handler, pod, box, newStatus)
				require.ErrorIs(t, err, cause)
				require.ErrorContains(t, err, cause.Error())
				require.False(t, done)
				assertInplaceConditions(t, newStatus, false, agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating)
				require.False(t, isInplaceUpdateTerminal(newStatus))
				cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
				message := tt.message
				if attempt > 0 && tt.nextMessage != "" {
					message = tt.nextMessage
				}
				require.Equal(t, message, cond.Message)
				ready := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady))
				require.Equal(t, agentsv1alpha1.SandboxReadyReasonInplaceUpdating, ready.Reason)
				if attempt == 0 || tt.nextMessage != "" {
					require.Len(t, fakeRecorder.Events, 1)
					require.Contains(t, <-fakeRecorder.Events, "Warning InplaceUpdateFailed "+message)
				} else {
					require.Equal(t, before, newStatus, "identical summaries must not produce a status patch difference")
					require.Empty(t, fakeRecorder.Events)
				}
				if attempt > 0 {
					previous := utils.GetSandboxCondition(before, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
					require.Equal(t, previous.LastTransitionTime, cond.LastTransitionTime)
				}
			}

			// Clear the summary after recovery; report the same error category again if it recurs.
			patchErr = nil
			done, err := runClaimInplaceUpdate(t.Context(), handler, pod, box, newStatus)
			require.NoError(t, err)
			require.False(t, done)
			require.Empty(t, utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionInplaceUpdate)).Message)
			require.Empty(t, fakeRecorder.Events)
			box.Status = *newStatus.DeepCopy()
			newStatus = box.Status.DeepCopy()
			patchErr = tt.first
			done, err = runClaimInplaceUpdate(t.Context(), handler, pod, box, newStatus)
			// The delivered revision only observes progress; it must not retry the successful write.
			require.NoError(t, err)
			require.False(t, done)
			require.Empty(t, fakeRecorder.Events)
			box.Spec.Template.Labels["app"] = "retry-app"
			newStatus.UpdateRevision, _ = HashSandbox(box)
			done, err = runClaimInplaceUpdate(t.Context(), handler, pod, box, newStatus)
			require.ErrorIs(t, err, patchErr)
			require.False(t, done)
			require.Len(t, fakeRecorder.Events, 1)
			require.Contains(t, <-fakeRecorder.Events, tt.message)

			// Simulate calculateStatus clearing the condition for a new revision; the old Box.Status must not suppress the first event.
			box.Status = *newStatus.DeepCopy()
			newStatus = box.Status.DeepCopy()
			box.Spec.Template.Labels["app"] = "next-app"
			newStatus.UpdateRevision, _ = HashSandbox(box)
			require.NotEqual(t, box.Status.UpdateRevision, newStatus.UpdateRevision)
			utils.RemoveSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
			done, err = runClaimInplaceUpdate(t.Context(), handler, pod, box, newStatus)
			require.ErrorIs(t, err, patchErr)
			require.False(t, done)
			require.Len(t, fakeRecorder.Events, 1)
			require.Contains(t, <-fakeRecorder.Events, tt.message)
		})
	}
}

func TestIsMetadataOnlyChange(t *testing.T) {
	tests := []struct {
		name     string
		pod      *corev1.Pod
		box      *agentsv1alpha1.Sandbox
		expected bool
	}{
		{
			name: "identical image and resources",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "main",
						Image: "nginx:latest",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("100m"),
							},
						},
					}},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name:  "main",
									Image: "nginx:latest",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU: resource.MustParse("100m"),
										},
									},
								}},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "pod has extra injected resources (subset match)",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "main",
						Image: "nginx:latest",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:              resource.MustParse("100m"),
								corev1.ResourceMemory:           resource.MustParse("128Mi"),
								corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("500m"),
							},
						},
					}},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name:  "main",
									Image: "nginx:latest",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("100m"),
											corev1.ResourceMemory: resource.MustParse("128Mi"),
										},
									},
								}},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "image differs",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "main",
						Image: "nginx:old",
					}},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name:  "main",
									Image: "nginx:new",
								}},
							},
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "template resource value differs",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "main",
						Image: "nginx:latest",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("50m"),
							},
						},
					}},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name:  "main",
									Image: "nginx:latest",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU: resource.MustParse("100m"),
										},
									},
								}},
							},
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "template resource missing from pod",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "main",
						Image: "nginx:latest",
					}},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name:  "main",
									Image: "nginx:latest",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU: resource.MustParse("100m"),
										},
									},
								}},
							},
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "nil template returns false",
			pod:  &corev1.Pod{},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isMetadataOnlyChange(tt.pod, tt.box)
			if got != tt.expected {
				t.Errorf("isMetadataOnlyChange() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestIsInplaceUpdateTerminal(t *testing.T) {
	tests := []struct {
		name     string
		status   *agentsv1alpha1.SandboxStatus
		expected bool
	}{
		{
			name:     "nil condition returns false",
			status:   &agentsv1alpha1.SandboxStatus{},
			expected: false,
		},
		{
			name: "Failed reason returns true",
			status: &agentsv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{{
					Type:   string(agentsv1alpha1.SandboxConditionInplaceUpdate),
					Status: metav1.ConditionFalse,
					Reason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed,
				}},
			},
			expected: true,
		},
		{
			name: "Succeeded reason returns true",
			status: &agentsv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{{
					Type:   string(agentsv1alpha1.SandboxConditionInplaceUpdate),
					Status: metav1.ConditionTrue,
					Reason: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded,
				}},
			},
			expected: true,
		},
		{
			name: "InplaceUpdating reason returns false",
			status: &agentsv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{{
					Type:   string(agentsv1alpha1.SandboxConditionInplaceUpdate),
					Status: metav1.ConditionFalse,
					Reason: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
				}},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isInplaceUpdateTerminal(tt.status)
			if result != tt.expected {
				t.Errorf("isInplaceUpdateTerminal() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// The tests below verify that a corrected latest target can supersede an
// unfinished earlier round for both SUO and Claim callers.

func TestInplaceEngine_ImagePullFailureAcceptsCorrectedTarget(t *testing.T) {
	// A bad image does not block delivering a new target; the same Pod continues
	// to be used to complete the in-place remediation.
	ctx := context.Background()

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{{Name: "main", Image: "nginx:bad"}},
	}
	fixedPodSpec := corev1.PodSpec{
		Containers: []corev1.Container{{Name: "main", Image: "nginx:fixed"}},
	}
	box := buildMatchingHashBox("test-sandbox", "default", fixedPodSpec)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels:    map[string]string{agentsv1alpha1.PodLabelTemplateHash: "old-revision"},
			Annotations: map[string]string{
				inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"prev-revision","updateTimestamp":"2024-01-01T00:00:00Z","updateImages":true,"lastContainerStatuses":{"main":{"imageID":"docker://sha256:old"}}}`,
			},
		},
		Spec: podSpec,
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:    "main",
				ImageID: "docker://sha256:old", // unchanged: round in flight
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "ImagePullBackOff", Message: "Back-off pulling image \"nginx:bad\"",
				}},
			}},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	control := inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc)

	step, err := handleInPlaceUpdateCommon(ctx, control, nil, pod, box, "new-revision")
	require.NoError(t, err)
	require.Equal(t, inplaceUpdateStepInProgress, step)

	updated := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-pod"}, updated))
	require.Equal(t, "new-revision", updated.Labels[agentsv1alpha1.PodLabelTemplateHash])
	require.Equal(t, "nginx:fixed", updated.Spec.Containers[0].Image)
	require.Equal(t, pod.UID, updated.UID)
	state, stateErr := inplaceupdate.GetPodInPlaceUpdateState(updated)
	require.NoError(t, stateErr)
	require.NotNil(t, state)
	require.Equal(t, "new-revision", state.Revision)
	require.Equal(t, "docker://sha256:old", state.LastContainerStatuses["main"].ImageID)
}

func TestInplaceEngine_ClaimAcceptsRepeatedUpdate(t *testing.T) {
	// Claim follows the same latest-target-wins policy as SUO when a Pod still
	// records an earlier in-place update.
	ctx := context.Background()

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	box := buildMatchingHashBox("test-sandbox", "default", corev1.PodSpec{
		Containers: []corev1.Container{{Name: "main", Image: "nginx:fixed"}},
	})
	newPod := func(imageID string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
				Labels:    map[string]string{agentsv1alpha1.PodLabelTemplateHash: "old-revision"},
				Annotations: map[string]string{
					inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"prev-revision","updateTimestamp":"2024-01-01T00:00:00Z","updateImages":true,"lastContainerStatuses":{"main":{"imageID":"docker://sha256:old"}}}`,
				},
			},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "nginx:bad"}}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "main", ImageID: imageID}}},
		}
	}

	for _, imageID := range []string{"docker://sha256:old", "docker://sha256:new"} {
		t.Run(imageID, func(t *testing.T) {
			pod := newPod(imageID)
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
			control := inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc)

			step, err := handleInPlaceUpdateCommon(ctx, control, nil, pod, box, "new-revision")
			require.NoError(t, err)
			require.Equal(t, inplaceUpdateStepInProgress, step)

			stored := &corev1.Pod{}
			require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), stored))
			require.Equal(t, "new-revision", stored.Labels[agentsv1alpha1.PodLabelTemplateHash])
			require.Equal(t, "nginx:fixed", stored.Spec.Containers[0].Image)
		})
	}
}

func TestInplaceEngineSteps(t *testing.T) {
	tests := []struct {
		name, kind, writeError string
		step                   inplaceUpdateStepResult
		class                  inplaceupdate.ErrorClass
		hasError, terminal     bool
		expectPodVersion       bool
		readyAfterPatch        corev1.ConditionStatus
	}{
		{name: "metadata success", kind: "metadata", step: inplaceUpdateStepSucceeded},
		{name: "metadata returns newly unready pod", kind: "metadata", readyAfterPatch: corev1.ConditionFalse, step: inplaceUpdateStepSucceeded},
		{name: "metadata returns newly ready pod", kind: "metadata", readyAfterPatch: corev1.ConditionTrue, step: inplaceUpdateStepSucceeded},
		{name: "pending image returns newly unready pod", kind: "image", readyAfterPatch: corev1.ConditionFalse, step: inplaceUpdateStepInProgress, expectPodVersion: true},
		{name: "no write needed", kind: "noop", step: inplaceUpdateStepSucceeded},
		{name: "image delivered", kind: "image", step: inplaceUpdateStepInProgress, expectPodVersion: true},
		{name: "image already applied after write", kind: "image-applied", step: inplaceUpdateStepSucceeded, expectPodVersion: true},
		{name: "resize delivered", kind: "resize", step: inplaceUpdateStepInProgress, expectPodVersion: true},
		{name: "resources and image delivered", kind: "combined", step: inplaceUpdateStepInProgress, expectPodVersion: true},
		{name: "resize fails observation after write", kind: "resize-written-infeasible", step: inplaceUpdateStepFailed, class: inplaceupdate.InplaceUpdateResizeFailed, hasError: true, terminal: true, expectPodVersion: true},
		{name: "metadata with pending image keeps observing", kind: "metadata-pending-image", step: inplaceUpdateStepInProgress},
		{name: "metadata with pending resources and image keeps observing", kind: "metadata-pending-combined", step: inplaceUpdateStepInProgress},
		{name: "untracked", kind: "untracked", step: inplaceUpdateStepFailed, class: inplaceupdate.InplaceUpdateInvalidPodState, hasError: true, terminal: true},
		{name: "injected init container unchanged", kind: "init-injected", step: inplaceUpdateStepSucceeded},
		{name: "unsupported", kind: "unsupported", step: inplaceUpdateStepFailed, class: inplaceupdate.InplaceUpdateUnsupportedChange, hasError: true, terminal: true},
		{name: "corrupted matching revision", kind: "corrupted", step: inplaceUpdateStepFailed, class: inplaceupdate.InplaceUpdateInvalidPodState, hasError: true, terminal: true},
		{name: "corrupted old revision", kind: "corrupted-old", step: inplaceUpdateStepFailed, class: inplaceupdate.InplaceUpdateInvalidPodState, hasError: true, terminal: true},
		{name: "C1 QoS rejection", kind: "qos", step: inplaceUpdateStepFailed, class: inplaceupdate.InplaceUpdateResizeRejected, hasError: true, terminal: true},
		{name: "C2 resize conflict", kind: "resize", writeError: "resize-conflict", step: inplaceUpdateStepInProgress, class: inplaceupdate.InplaceUpdateFailed, hasError: true},
		{name: "C3 resize success patch conflict", kind: "resize", writeError: "conflict", step: inplaceUpdateStepInProgress, class: inplaceupdate.InplaceUpdateFailed, hasError: true},
		{name: "C4 resource wait", kind: "resource-wait", step: inplaceUpdateStepInProgress},
		{name: "C5 image wait", kind: "image-wait", step: inplaceUpdateStepInProgress},
		{name: "C6 applied not ready", kind: "not-ready", step: inplaceUpdateStepSucceeded},
		{name: "C7 applied and ready", kind: "ready", step: inplaceUpdateStepSucceeded},
		{name: "metadata conflict", kind: "metadata", writeError: "conflict", step: inplaceUpdateStepInProgress, class: inplaceupdate.InplaceUpdateFailed, hasError: true},
		{name: "metadata timeout", kind: "metadata", writeError: "timeout", step: inplaceUpdateStepInProgress, class: inplaceupdate.InplaceUpdateFailed, hasError: true},
		{name: "metadata unknown error", kind: "metadata", writeError: "unknown", step: inplaceUpdateStepInProgress, class: inplaceupdate.InplaceUpdateFailed, hasError: true},
		{name: "metadata forbidden", kind: "metadata", writeError: "forbidden", step: inplaceUpdateStepInProgress, class: inplaceupdate.InplaceUpdateFailed, hasError: true, terminal: true},
		{name: "image patch forbidden", kind: "image", writeError: "forbidden", step: inplaceUpdateStepInProgress, class: inplaceupdate.InplaceUpdateFailed, hasError: true, terminal: true},
		{name: "resize unsupported", kind: "resize", writeError: "resize-unsupported", step: inplaceUpdateStepInProgress, class: inplaceupdate.InplaceUpdateFailed, hasError: true, terminal: true},
		{name: "resize infeasible", kind: "resize-infeasible", step: inplaceUpdateStepFailed, class: inplaceupdate.InplaceUpdateResizeFailed, hasError: true, terminal: true},
		{name: "resize deferred", kind: "resize-deferred", step: inplaceUpdateStepFailed, class: inplaceupdate.InplaceUpdateResizeFailed, hasError: true, terminal: true},
		{name: "resize apply error", kind: "resize-error", step: inplaceUpdateStepFailed, class: inplaceupdate.InplaceUpdateResizeFailed, hasError: true, terminal: true},
		{name: "apply failure", kind: "apply-failure", step: inplaceUpdateStepInProgress},
		// A metadata-only write on top of an unfinished resize must not report
		// success before the earlier round is observed complete.
		{name: "metadata with pending resize keeps observing", kind: "metadata-pending", step: inplaceUpdateStepInProgress},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "matrix-pod", Namespace: "default", UID: "preserved", Generation: 2,
				Labels: map[string]string{agentsv1alpha1.PodLabelTemplateHash: "target"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "nginx:1", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
				}}}},
				Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
			}
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", Image: "nginx:1", ImageID: "same-image-id",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, Resources: pod.Spec.Containers[0].Resources.DeepCopy()}}
			box := buildMatchingHashBox("matrix-box", "default", *pod.Spec.DeepCopy())
			switch tt.kind {
			case "init-injected":
				pod.Spec.InitContainers = []corev1.Container{{Name: "init", Image: "busybox:1"}}
				box = buildMatchingHashBox("matrix-box", "default", *pod.Spec.DeepCopy())
				pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{Name: "injected", Image: "busybox:1"})
			case "untracked":
				delete(pod.Labels, agentsv1alpha1.PodLabelTemplateHash)
			case "unsupported":
				box.Spec.Template.Spec.Containers[0].Command = []string{"changed"}
			case "corrupted", "corrupted-old":
				pod.Annotations = map[string]string{inplaceupdate.PodAnnotationInPlaceUpdateStateKey: "{broken"}
				if tt.kind == "corrupted-old" {
					pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = "old"
				}
			case "qos":
				pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = "old"
				box.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("500m")
			case "resize", "combined", "resize-written-infeasible":
				box.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("200m")
				pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = "old"
				if tt.kind == "combined" {
					box.Spec.Template.Spec.Containers[0].Image = "nginx:2"
				} else if tt.kind == "resize-written-infeasible" {
					pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
						Type: corev1.PodResizePending, Status: corev1.ConditionTrue, Reason: corev1.PodReasonInfeasible,
					})
				}
			case "metadata", "metadata-pending", "metadata-pending-image", "metadata-pending-combined", "noop":
				pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = "old"
				if tt.kind == "metadata-pending" || tt.kind == "metadata-pending-combined" {
					pod.Annotations = map[string]string{inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"old","updateResources":true}`}
					pod.Status.ContainerStatuses[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("50m")
				}
				if tt.kind == "metadata-pending-image" || tt.kind == "metadata-pending-combined" {
					pod.Annotations = map[string]string{inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"old","updateImages":true,"lastContainerStatuses":{"main":{"targetImage":"nginx:1"}}}`}
					if tt.kind == "metadata-pending-combined" {
						pod.Annotations[inplaceupdate.PodAnnotationInPlaceUpdateStateKey] = `{"revision":"old","updateImages":true,"updateResources":true,"lastContainerStatuses":{"main":{"targetImage":"nginx:1"}}}`
					}
					pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
				}
			case "image", "image-applied":
				pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = "old"
				box.Spec.Template.Spec.Containers[0].Image = "nginx:2"
				if tt.kind == "image-applied" {
					pod.Status.ContainerStatuses[0].Image = "nginx:2"
				}
			case "not-ready":
				pod.Status.Conditions[0].Status = corev1.ConditionFalse
			case "image-wait", "apply-failure":
				pod.Status.Conditions[0].Status = corev1.ConditionFalse
				pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}
				pod.Annotations = map[string]string{inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"target","updateImages":true,"lastContainerStatuses":{"main":{"imageID":"same-image-id","targetImage":"nginx:1"}}}`}
				if tt.kind == "apply-failure" {
					pod.Status.ContainerStatuses[0].State.Waiting.Reason = "InvalidImageName"
				}
			case "resource-wait", "resize-infeasible", "resize-deferred", "resize-error":
				pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("200m")
				box.Spec.Template.Spec = *pod.Spec.DeepCopy()
				pod.Annotations = map[string]string{inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"target","updateResources":true}`}
				if tt.kind != "resource-wait" {
					condition := corev1.PodCondition{Type: corev1.PodResizePending, Status: corev1.ConditionTrue, Reason: corev1.PodReasonInfeasible, Message: "resize rejected"}
					if tt.kind == "resize-deferred" {
						condition.Reason = corev1.PodReasonDeferred
					} else if tt.kind == "resize-error" {
						condition.Type, condition.Reason = corev1.PodResizeInProgress, corev1.PodReasonError
					}
					pod.Status.Conditions = append(pod.Status.Conditions, condition)
				}
			}
			if tt.readyAfterPatch == corev1.ConditionTrue {
				pod.Status.Conditions[0].Status = corev1.ConditionFalse
			}
			scheme := runtime.NewScheme()
			require.NoError(t, clientgoscheme.AddToScheme(scheme))
			base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.Pod{}).WithObjects(pod).Build()
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
			original := pod.DeepCopy()
			originalAlias := *pod
			ResourceVersionExpectations.Delete(pod)
			t.Cleanup(func() { ResourceVersionExpectations.Delete(pod) })
			var beforeFinalPatch, writtenPod *corev1.Pod
			var injected error
			if tt.writeError != "" {
				injected = apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, pod.Name, fmt.Errorf("version changed"))
				switch tt.writeError {
				case "forbidden":
					injected = apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, pod.Name, fmt.Errorf("denied"))
				case "timeout":
					injected = context.DeadlineExceeded
				case "unknown":
					injected = fmt.Errorf("unknown write outcome")
				case "resize-unsupported":
					injected = &inplaceupdate.ResizeNotSupportedError{Err: fmt.Errorf("resize disabled")}
				}
			}
			failWrites := true
			patches, resizes := 0, 0
			wrapped := interceptor.NewClient(base, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					beforeFinalPatch = obj.(*corev1.Pod).DeepCopy()
					if failWrites && injected != nil && tt.writeError != "resize-conflict" && patch.Type() == types.StrategicMergePatchType {
						return injected
					}
					if tt.readyAfterPatch != "" {
						latest := &corev1.Pod{}
						require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), latest))
						latest.Status.Conditions[0].Status = tt.readyAfterPatch
						latest.Status.PodIP = "10.0.0.2"
						require.NoError(t, c.Status().Update(ctx, latest))
					}
					if err := c.Patch(ctx, obj, patch, opts...); err != nil {
						return err
					}
					writtenPod = obj.(*corev1.Pod)
					return nil
				},
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					require.Equal(t, "resize", sub)
					resizes++
					if failWrites && (tt.writeError == "resize-conflict" || tt.writeError == "resize-unsupported") {
						return injected
					}
					// The fake client does not implement kubelet resize; only apply the
					// actually generated resource patch, without simulating that it took effect.
					return c.Patch(ctx, obj, patch)
				},
			})
			patchFunc := inplaceupdate.DefaultGeneratePatchBodyFunc
			if tt.kind == "noop" {
				patchFunc = func(inplaceupdate.InPlaceUpdateOptions) (string, error) { return "", nil }
			}
			control := inplaceupdate.NewInPlaceUpdateControl(wrapped, patchFunc)
			before := box.DeepCopy()
			step, err := handleInPlaceUpdateCommon(t.Context(), control, nil, pod, box, "target")
			require.Equal(t, before, box, "the engine must not read or write Sandbox conditions")
			require.Equal(t, tt.step, step)
			if tt.hasError {
				require.Error(t, err)
				require.Equal(t, tt.class, inplaceupdate.ClassifyError(err))
				require.Equal(t, tt.terminal, isTerminalInplaceError(err))
				if injected != nil {
					require.ErrorIs(t, err, injected)
				}
			} else {
				require.NoError(t, err)
			}
			if writtenPod != nil {
				require.Equal(t, writtenPod, pod, "callers must receive the final written Pod even when observation fails or remains in progress")
				written := writtenPod.DeepCopy()
				writtenPod.Labels["response-only"] = "changed"
				writtenPod.Status.Conditions[0].Message = "response-only"
				require.Equal(t, written, pod, "the updated object must not share nested fields with the write response")
			} else {
				require.Equal(t, original, pod, "do not update the caller object when the write did not complete successfully")
			}
			require.Equal(t, original, &originalAlias, "updating the caller object must not mutate nested fields in the original object")
			if tt.readyAfterPatch != "" {
				require.Equal(t, tt.readyAfterPatch, utils.GetPodCondition(&pod.Status, corev1.PodReady).Status)
				require.Equal(t, "10.0.0.2", pod.Status.PodIP)
			}
			isSatisfied, _ := ResourceVersionExpectations.IsSatisfied(original)
			require.Equal(t, !tt.expectPodVersion, isSatisfied)
			if tt.expectPodVersion {
				require.NotNil(t, beforeFinalPatch)
				isSatisfied, _ = ResourceVersionExpectations.IsSatisfied(beforeFinalPatch)
				require.False(t, isSatisfied, "wait for the final written version, not the intermediate resize version")
				delivered := &corev1.Pod{}
				require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(pod), delivered))
				isSatisfied, _ = ResourceVersionExpectations.IsSatisfied(delivered)
				require.True(t, isSatisfied, "the final written version must satisfy the expectation")
			}
			if tt.kind == "resize" && tt.writeError == "conflict" {
				// The resize took effect on the spec but the finishing patch failed; the
				// retry only needs the metadata finalisation.
				current := &corev1.Pod{}
				require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(pod), current))
				require.Equal(t, box.Spec.Template.Spec.Containers[0].Resources, current.Spec.Containers[0].Resources)
				require.Equal(t, original.Status, current.Status)
				require.Equal(t, "old", current.Labels[agentsv1alpha1.PodLabelTemplateHash])
				failWrites = false
				step, err := handleInPlaceUpdateCommon(t.Context(), control, nil, current, box, "target")
				require.NoError(t, err)
				require.Equal(t, inplaceUpdateStepInProgress, step)
				require.Equal(t, 1, resizes)
				require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(pod), current))
				require.Equal(t, "target", current.Labels[agentsv1alpha1.PodLabelTemplateHash])
				require.Equal(t, original.Status, current.Status)
				state, stateErr := inplaceupdate.GetPodInPlaceUpdateState(current)
				require.NoError(t, stateErr)
				require.NotNil(t, state)
				require.True(t, state.UpdateResources)
				// Metadata/state repair is not completion until kubelet applies the resources.
				current.Status.ContainerStatuses[0].Resources = current.Spec.Containers[0].Resources.DeepCopy()
				require.NoError(t, base.Status().Update(t.Context(), current))
				step, err = handleInPlaceUpdateCommon(t.Context(), control, nil, current, box, "target")
				require.NoError(t, err)
				require.Equal(t, inplaceUpdateStepSucceeded, step)
				require.Equal(t, 1, resizes)
				require.Equal(t, 2, patches)
			}
			switch tt.kind {
			case "untracked", "unsupported", "corrupted", "corrupted-old", "qos":
				require.Zero(t, patches)
				require.Zero(t, resizes)
			}
			if strings.HasPrefix(tt.kind, "metadata-pending") {
				require.Equal(t, 1, patches)
				require.Zero(t, resizes)
			}
			stored := &corev1.Pod{}
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(pod), stored))
			require.Equal(t, original.UID, stored.UID)
			if strings.HasPrefix(tt.kind, "metadata-pending") {
				previous, stateErr := inplaceupdate.GetPodInPlaceUpdateState(original)
				require.NoError(t, stateErr)
				require.NotNil(t, previous)
				state, stateErr := inplaceupdate.GetPodInPlaceUpdateState(stored)
				require.NoError(t, stateErr)
				require.NotNil(t, state)
				// Recovery may rewrite the revision and timestamp but must retain unfinished observations.
				require.Equal(t, previous.UpdateImages, state.UpdateImages)
				require.Equal(t, previous.UpdateResources, state.UpdateResources)
				if previous.UpdateImages {
					require.Equal(t, previous.LastContainerStatuses, state.LastContainerStatuses)
				} else {
					require.Empty(t, state.LastContainerStatuses)
				}
				if previous.UpdateResources {
					require.Equal(t, "target", state.Revision)
					require.False(t, state.UpdateTimestamp.IsZero())
				} else {
					require.Equal(t, previous.Revision, state.Revision)
				}
			}
			if tt.kind == "combined" {
				state, stateErr := inplaceupdate.GetPodInPlaceUpdateState(stored)
				require.NoError(t, stateErr)
				require.NotNil(t, state)
				require.True(t, state.UpdateImages)
				require.True(t, state.UpdateResources)
				stored.Status.ContainerStatuses[0].Resources = stored.Spec.Containers[0].Resources.DeepCopy()
				step, err = handleInPlaceUpdateCommon(t.Context(), control, nil, stored, box, "target")
				require.NoError(t, err)
				require.Equal(t, inplaceUpdateStepInProgress, step, "keep waiting when resources are applied but the image update remains incomplete")
				require.Equal(t, 1, patches)
				require.Equal(t, 1, resizes)
			}
		})
	}
}

func TestInplaceErrorPolicy(t *testing.T) {
	tests := []struct {
		name     string
		cause    error
		class    inplaceupdate.ErrorClass
		terminal bool
	}{
		{"conflict", apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "pod", fmt.Errorf("changed")), inplaceupdate.InplaceUpdateFailed, false},
		{"throttled", apierrors.NewTooManyRequests("slow down", 1), inplaceupdate.InplaceUpdateFailed, false},
		{"timeout", context.DeadlineExceeded, inplaceupdate.InplaceUpdateFailed, false},
		{"unknown", fmt.Errorf("unknown write result"), inplaceupdate.InplaceUpdateFailed, false},
		{"unauthorized", apierrors.NewUnauthorized("denied"), inplaceupdate.InplaceUpdateFailed, true},
		{"bad request", apierrors.NewBadRequest("invalid request"), inplaceupdate.InplaceUpdateFailed, true},
		{"unsupported", apierrors.NewMethodNotSupported(schema.GroupResource{Resource: "pods"}, "patch"), inplaceupdate.InplaceUpdateFailed, true},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "pod", nil), inplaceupdate.InplaceUpdateFailed, true},
		{"resize unsupported", &inplaceupdate.ResizeNotSupportedError{Err: fmt.Errorf("unsupported")}, inplaceupdate.InplaceUpdateFailed, true},
		{"resize infeasible", inplaceupdate.NewError(inplaceupdate.InplaceUpdateResizeFailed, "infeasible", nil), inplaceupdate.InplaceUpdateResizeFailed, true},
		{"resize failure with unsupported text", inplaceupdate.NewError(inplaceupdate.InplaceUpdateResizeFailed, "in-place pod resize not supported", nil), inplaceupdate.InplaceUpdateResizeFailed, true},
		{"invalid pod state", inplaceupdate.NewError(inplaceupdate.InplaceUpdateInvalidPodState, "invalid state", nil), inplaceupdate.InplaceUpdateInvalidPodState, true},
		{"unsupported change", inplaceupdate.NewError(inplaceupdate.InplaceUpdateUnsupportedChange, "immutable change", nil), inplaceupdate.InplaceUpdateUnsupportedChange, true},
		{"QoS rejected", inplaceupdate.NewError(inplaceupdate.InplaceUpdateResizeRejected, "QoS changed", nil), inplaceupdate.InplaceUpdateResizeRejected, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := fmt.Errorf("outer context: %w", inplaceupdate.NewError(inplaceupdate.ClassifyError(tt.cause), "delivery", tt.cause))
			require.ErrorIs(t, err, tt.cause)
			require.Equal(t, tt.class, inplaceupdate.ClassifyError(err))
			require.Equal(t, tt.terminal, isTerminalInplaceError(err))
			require.Equal(t, tt.cause, inplaceUnderlyingError(err))
		})
	}
}
