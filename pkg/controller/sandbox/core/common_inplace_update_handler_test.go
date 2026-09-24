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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
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

// Create test event recorder
func createTestRecorder() record.EventRecorder {
	scheme := runtime.NewScheme()
	agentsv1alpha1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)
	return record.NewFakeRecorder(100)
}

func assertInplaceConditions(t *testing.T, status *agentsv1alpha1.SandboxStatus, ready bool, reason string) {
	t.Helper()
	update := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
	require.NotNil(t, update)
	require.Equal(t, reason, update.Reason)
	if reason == agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded {
		require.Equal(t, metav1.ConditionTrue, update.Status)
		require.Empty(t, update.Message)
	} else {
		require.Equal(t, metav1.ConditionFalse, update.Status)
		require.NotEmpty(t, update.Message)
	}
	condition := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionReady))
	require.NotNil(t, condition)
	if ready {
		require.Equal(t, metav1.ConditionTrue, condition.Status)
		require.Equal(t, agentsv1alpha1.SandboxReadyReasonPodReady, condition.Reason)
		require.Empty(t, condition.Message)
	} else {
		require.Equal(t, metav1.ConditionFalse, condition.Status)
		require.Equal(t, agentsv1alpha1.SandboxReadyReasonInplaceUpdating, condition.Reason)
	}
}

func TestHandleInPlaceUpdateCommon(t *testing.T) {
	// Test cases definition
	testCases := []struct {
		name           string
		pod            *corev1.Pod
		box            *agentsv1alpha1.Sandbox
		newStatus      *agentsv1alpha1.SandboxStatus
		setupHandler   func(pod *corev1.Pod) *commonControl
		expectedResult bool
		expectError    bool
		checkStatus    func(t *testing.T, status *agentsv1alpha1.SandboxStatus)
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
			setupHandler: func(pod *corev1.Pod) *commonControl {
				recorder := createTestRecorder()
				return &commonControl{
					inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
					recorder:             recorder,
				}
			},
			expectedResult: true,
			expectError:    false,
			checkStatus: func(t *testing.T, status *agentsv1alpha1.SandboxStatus) {
				assertInplaceConditions(t, status, false, agentsv1alpha1.SandboxInplaceUpdateReasonFailed)
			},
			description: "When Pod has no template hash label, should return true immediately",
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
			setupHandler: func(pod *corev1.Pod) *commonControl {
				recorder := createTestRecorder()
				return &commonControl{
					inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
					recorder:             recorder,
				}
			},
			expectedResult: true,
			expectError:    false,
			checkStatus: func(t *testing.T, status *agentsv1alpha1.SandboxStatus) {
				cond := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
				require.NotNil(t, cond)
				require.Equal(t, metav1.ConditionFalse, cond.Status)
				require.Equal(t, agentsv1alpha1.SandboxInplaceUpdateReasonFailed, cond.Reason)
				require.NotEmpty(t, cond.Message)
			},
			description: "When hash mismatch occurs, should return true",
		},
		{
			name: "missing SandboxHashImmutablePart annotation should skip hash check",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.PodLabelTemplateHash: "some-hash",
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
				Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-sandbox",
					Namespace:   "default",
					Annotations: map[string]string{}, // no immutable-field hash record
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
			setupHandler: func(pod *corev1.Pod) *commonControl {
				scheme := runtime.NewScheme()
				_ = clientgoscheme.AddToScheme(scheme)
				_ = agentsv1alpha1.AddToScheme(scheme)

				// 使用与观察对象一致的 Pod，验证 metadata 更新确实被下发。
				fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

				recorder := createTestRecorder()
				return &commonControl{
					inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
					recorder:             recorder,
				}
			},
			expectedResult: true,
			expectError:    false,
			checkStatus: func(t *testing.T, status *agentsv1alpha1.SandboxStatus) {
				assertInplaceConditions(t, status, true, agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded)
			},
			description: "When SandboxHashImmutablePart annotation is missing, hash check should be skipped and continue processing",
		},
		{
			name: "revision consistent and update completed should return true",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						agentsv1alpha1.PodLabelTemplateHash: "test-revision", // Matches newStatus.UpdateRevision
					},
				},
				Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container", Image: "nginx:latest"}}},
				Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
			},
			box: &agentsv1alpha1.Sandbox{
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
			setupHandler: func(pod *corev1.Pod) *commonControl {
				recorder := createTestRecorder()
				return &commonControl{
					inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
					recorder:             recorder,
				}
			},
			expectedResult: true,
			expectError:    false,
			checkStatus: func(t *testing.T, status *agentsv1alpha1.SandboxStatus) {
				assertInplaceConditions(t, status, true, agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded)
			},
			description: "When revision is consistent and update completed, should return true",
		},
	}

	// Execute test cases
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Set up test context
			ctx := context.Background()

			// Create handler
			handler := tc.setupHandler(tc.pod)

			// Execute function
			result, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: tc.pod, Box: tc.box, NewStatus: tc.newStatus})

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

			if tc.checkStatus != nil {
				tc.checkStatus(t, tc.newStatus)
			}
		})
	}
}

func TestHandleInPlaceUpdateCommon_UnsupportedChangeWithPreviousState(t *testing.T) {
	// 不可变字段校验拒绝当前目标，不受旧更新记录影响。
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
	handler := &commonControl{
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder:             recorder,
	}

	// Execute function
	result, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})

	// Verify result
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	require.True(t, result)
	assertInplaceConditions(t, newStatus, false, agentsv1alpha1.SandboxInplaceUpdateReasonFailed)
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
	handler := &commonControl{
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder:             recorder,
	}

	result, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result {
		t.Error("Expected result true (done, no requeue), got false")
	}

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
	handler := &commonControl{
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder:             recorder,
	}

	result, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
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

func TestHandleInPlaceUpdateCommon_UnsupportedResizeReason(t *testing.T) {
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
			data, _ := patch.Data(obj)
			// A direct resource patch carries spec.containers[].resources; when the
			// cluster has not enabled the InPlacePodVerticalScaling feature, the server
			// returns Invalid(422) for that field update, and production code treats it
			// as an unsupported resize. Metadata/status patches carry no spec and pass.
			if strings.Contains(string(data), `"spec"`) {
				return apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, obj.GetName(), nil)
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	newStatus := &agentsv1alpha1.SandboxStatus{UpdateRevision: "new-revision"}
	handler := &commonControl{
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(wrapped, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder:             createTestRecorder(),
	}

	result, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result {
		t.Fatal("Expected result true (unsupported resize is terminal), got false")
	}

	var cond *metav1.Condition
	for i := range newStatus.Conditions {
		if newStatus.Conditions[i].Type == string(agentsv1alpha1.SandboxConditionInplaceUpdate) {
			cond = &newStatus.Conditions[i]
			break
		}
	}
	if cond == nil {
		t.Fatal("InplaceUpdate condition not found in status")
	}
	if cond.Reason != agentsv1alpha1.SandboxInplaceUpdateReasonUnsupportedResize {
		t.Errorf("Expected reason %s, got %s", agentsv1alpha1.SandboxInplaceUpdateReasonUnsupportedResize, cond.Reason)
	}
	if !strings.Contains(cond.Message, "in-place pod resize not supported") {
		t.Errorf("Expected unsupported resize message, got %q", cond.Message)
	}
}

func TestHandleInPlaceUpdateCommon_ResizeInfeasibleFailFast(t *testing.T) {
	ctx := context.Background()

	// A resource adjustment for the current generation is explicitly rejected by
	// kubelet, so this round should terminate.
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
					Type:               corev1.PodResizePending,
					Status:             corev1.ConditionTrue,
					Reason:             corev1.PodReasonInfeasible,
					Message:            "insufficient cpu on node",
					ObservedGeneration: 2,
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
	pod.Generation = 2

	recorder := createTestRecorder()
	handler := &commonControl{
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder:             recorder,
	}

	result, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result {
		t.Fatal("Expected result true (done, fail-fast), got false")
	}

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

	// Current generation, same revision has already failed and must not be
	// overwritten by an old configuration taking effect.
	box.Status.UpdateRevision = "target-revision"
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
	handler := &commonControl{
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder:             recorder,
	}

	result, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
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

func TestHandleInPlaceUpdateCommon_UnsupportedChangeWithoutPreviousState(t *testing.T) {
	// 没有旧记录时，不可变字段校验仍须终止不支持的更新。
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
	handler := &commonControl{
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder:             recorder,
	}

	// Execute function
	result, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})

	// Verify result
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	require.True(t, result)
	assertInplaceConditions(t, newStatus, false, agentsv1alpha1.SandboxInplaceUpdateReasonFailed)
}

// buildMatchingHashBox creates a sandbox with correct hash for the given podSpec
// 使原地更新通过不可变字段哈希校验。
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
	// Claim adapter 同时确认配置生效与 Pod Ready，再写入成功状态。
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
			// no in-place update state record; the configuration already took effect.
		},
		Spec:   podSpec,
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "target-rev",
	}

	recorder := createTestRecorder()
	handler := &commonControl{
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder:             recorder,
	}

	result, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
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
	// 已有 Succeeded 仍重新观察 Pod；配置生效且 Ready 时保持成功。
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
	handler := &commonControl{
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder:             recorder,
	}

	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	done, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
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
				// Container creation and image pull backoff both keep waiting, without
				// automatically concluding a terminal failure.
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason:  "ContainerCreating",
					Message: `pulling image "nginx:latest"`,
				}},
			}},
		},
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "target-rev",
	}

	recorder := createTestRecorder()
	handler := &commonControl{
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder:             recorder,
	}

	result, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if result {
		t.Fatal("Expected result false (still in progress), got true")
	}
	assertInplaceConditions(t, newStatus, false, agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating)
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
	handler := &commonControl{
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder:             recorder,
	}

	result, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	require.NoError(t, err)
	require.True(t, result)
	assertInplaceConditions(t, newStatus, false, agentsv1alpha1.SandboxInplaceUpdateReasonFailed)
}

func TestHandleInPlaceUpdateCommon_StateNotNilCompleted(t *testing.T) {
	// 旧更新已完成后，Claim 直接下发新目标，并等待其生效。
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
			Annotations: map[string]string{
				// Previous update state without updateImages/updateResources → completed=true
				inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"prev-revision","updateTimestamp":"2024-01-01T00:00:00Z"}`,
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
	handler := &commonControl{
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder:             recorder,
	}

	result, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	require.False(t, result)
	assertInplaceConditions(t, newStatus, false, agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating)
	stored := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), stored))
	require.Equal(t, newPodSpec, stored.Spec)
	require.Equal(t, "new-revision", stored.Labels[agentsv1alpha1.PodLabelTemplateHash])
	assertNewInplaceUpdateRoundStarted(t, ctx, fakeClient)
}

// assertNewInplaceUpdateRoundStarted 检查新一轮 Pod 更新记录已写入。
func assertNewInplaceUpdateRoundStarted(t *testing.T, ctx context.Context, c client.Client) {
	t.Helper()

	updated := &corev1.Pod{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-pod"}, updated); err != nil {
		t.Fatalf("Failed to get pod: %v", err)
	}
	state, err := inplaceupdate.GetPodInPlaceUpdateState(updated)
	if err != nil {
		t.Fatalf("Failed to parse state annotation: %v", err)
	}
	if state == nil {
		t.Fatal("Expected state annotation to be present")
	}
	if state.Revision != "new-revision" {
		t.Errorf("Expected state annotation rebuilt with revision new-revision, got %s", state.Revision)
	}
}

func TestHandleInPlaceUpdateCommon_StateNotNilNotCompletedTerminalErr(t *testing.T) {
	// 旧资源更新未完成时，Claim 仍可下发新的镜像目标。
	ctx := context.Background()

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

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
	// Corrected template: image change only, resources unchanged so the new
	// round is a plain image update rather than another resize attempt.
	fixedPodSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "nginx:fixed",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2000m")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2000m")},
			},
		}},
	}

	box := buildMatchingHashBox("test-sandbox", "default", fixedPodSpec)

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
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:    "main",
				ImageID: "docker://sha256:latest",
			}},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
	}

	recorder := createTestRecorder()
	handler := &commonControl{
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder:             recorder,
	}

	result, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	require.False(t, result)
	assertInplaceConditions(t, newStatus, false, agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating)
	stored := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(pod), stored))
	require.True(t, apiequality.Semantic.DeepEqual(fixedPodSpec, stored.Spec), "Claim 已下发修正目标；资源数量按语义比较")
	require.Equal(t, "new-revision", stored.Labels[agentsv1alpha1.PodLabelTemplateHash])
	assertNewInplaceUpdateRoundStarted(t, ctx, fakeClient)
}

func TestHandleInPlaceUpdateCommon_ImagePullFailureAcceptsCorrectedTarget(t *testing.T) {
	// A bad image does not block delivering a new target; the same Pod continues
	// to be used to complete the in-place remediation.
	ctx := context.Background()

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "nginx:bad",
		}},
	}
	fixedPodSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "nginx:fixed",
		}},
	}

	box := buildMatchingHashBox("test-sandbox", "default", fixedPodSpec)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-revision",
			},
			Annotations: map[string]string{
				inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"prev-revision","updateTimestamp":"2024-01-01T00:00:00Z","updateImages":true,"lastContainerStatuses":{"main":{"imageID":"docker://sha256:old"}}}`,
			},
		},
		Spec: podSpec,
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:    "main",
				ImageID: "docker://sha256:old", // unchanged: round in flight
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason:  "ImagePullBackOff",
						Message: "Back-off pulling image \"nginx:bad\"",
					},
				},
			}},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
	}

	recorder := createTestRecorder()
	handler := &commonControl{
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder:             recorder,
	}

	result, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if result {
		t.Error("Expected result false (corrected target in progress), got true")
	}

	assertInplaceConditions(t, newStatus, false, agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating)

	// Claim 的新目标已经下发，旧镜像失败不阻止修正目标。
	updated := &corev1.Pod{}
	if err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-pod"}, updated); err != nil {
		t.Fatalf("Failed to get pod: %v", err)
	}
	require.Equal(t, "new-revision", updated.Labels[agentsv1alpha1.PodLabelTemplateHash])
	require.Equal(t, "nginx:fixed", updated.Spec.Containers[0].Image)
	require.Equal(t, pod.UID, updated.UID)
	state, stateErr := inplaceupdate.GetPodInPlaceUpdateState(updated)
	if stateErr != nil {
		t.Fatalf("Failed to parse state annotation: %v", stateErr)
	}
	require.NotNil(t, state)
	require.Equal(t, "new-revision", state.Revision)
	require.Equal(t, "docker://sha256:old", state.LastContainerStatuses["main"].ImageID)
}

func TestHandleInPlaceUpdateCommon_ImagePullBackoffWaits(t *testing.T) {
	// Pull backoff is still a wait; the Claim timeout is handled by the existing
	// call chain.
	ctx := context.Background()

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "nginx:bad",
		}},
	}

	box := buildMatchingHashBox("test-sandbox", "default", podSpec)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "same-revision",
			},
			Annotations: map[string]string{
				inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"same-revision","updateTimestamp":"2024-01-01T00:00:00Z","updateImages":true,"lastContainerStatuses":{"main":{"imageID":"docker://sha256:old"}}}`,
			},
		},
		Spec: podSpec,
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:    "main",
				ImageID: "docker://sha256:old", // unchanged: round in flight
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason:  "ImagePullBackOff",
						Message: "Back-off pulling image \"nginx:bad\"",
					},
				},
			}},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "same-revision",
	}

	recorder := createTestRecorder()
	handler := &commonControl{
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder:             recorder,
	}

	result, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	require.False(t, result)

	assertInplaceConditions(t, newStatus, false, agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating)
	condition := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
	require.Contains(t, condition.Message, "ImagePullBackOff")
}

func TestHandleInPlaceUpdateCommon_StateNotNilNotCompletedNoTerminalErr(t *testing.T) {
	// state != nil, resource update in progress but no terminal error
	// → not completed, return false, nil
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
		UpdateRevision: "old-revision",
	}

	recorder := createTestRecorder()
	handler := &commonControl{
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder:             recorder,
	}

	result, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if result {
		t.Error("Expected result false (resource resize in progress), got true")
	}
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
	handler := &commonControl{
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder:             recorder,
	}

	result, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
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
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}

	// Use a callback that returns an empty patch to verify there is no
	// configuration change.
	recorder := createTestRecorder()
	handler := &commonControl{
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, func(opts inplaceupdate.InPlaceUpdateOptions) (string, error) {
			return "", nil // no patch needed
		}),
		recorder: recorder,
	}

	result, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	// control.Update returns !changed → return true, nil
	if !result {
		t.Error("Expected result true (no changes), got false")
	}
}

func TestHandleInPlaceUpdateCommon_MetadataOnlyChange(t *testing.T) {
	// metadata-only 保留快速路径；Pod Ready 时同时报告更新成功与可用。
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
		Spec:   podSpec, // image and resources match the template.
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: hash,
	}

	recorder := createTestRecorder()
	handler := &commonControl{
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder:             recorder,
	}

	result, err := handler.handleInplaceUpdateSandbox(ctx, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result {
		t.Error("Expected result true (metadata patched directly), got false")
	}

	assertInplaceConditions(t, newStatus, true, agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded)

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

func TestInplaceStepAndClaimState(t *testing.T) {
	tests := []struct {
		name, kind, writeError string
		step                   inplaceUpdateStepResult
		class                  inplaceErrorClass
		hasError, terminal     bool
		claimDone, claimError  bool
		claimReady             bool
		claimReason            string
		existingReason         string
		sameRound              bool
	}{
		{name: "untracked", kind: "untracked", step: inplaceUpdateStepInProgress, class: inplaceClassUntrackedPod, hasError: true, terminal: true, claimDone: true, claimReady: true, claimReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed},
		{name: "untracked not ready", kind: "untracked-not-ready", step: inplaceUpdateStepInProgress, class: inplaceClassUntrackedPod, hasError: true, terminal: true, claimDone: true, claimReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed},
		{name: "injected init container unchanged", kind: "init-injected", step: inplaceUpdateStepSucceeded, claimDone: true, claimReady: true, claimReason: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded},
		{name: "unsupported", kind: "unsupported", step: inplaceUpdateStepInProgress, class: inplaceClassUnsupportedChange, hasError: true, terminal: true, claimDone: true, claimReady: true, claimReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed},
		{name: "corrupted matching revision", kind: "corrupted", step: inplaceUpdateStepInProgress, class: inplaceClassStateCorrupted, hasError: true, terminal: true, claimDone: true, claimReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed},
		{name: "corrupted old revision", kind: "corrupted-old", step: inplaceUpdateStepInProgress, class: inplaceClassStateCorrupted, hasError: true, terminal: true, claimDone: true, claimReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed},
		{name: "C1 QoS rejection", kind: "qos", step: inplaceUpdateStepInProgress, class: inplaceClassQoSRejected, hasError: true, terminal: true, claimDone: true, claimReady: true, claimReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed},
		{name: "C2 resize conflict", kind: "resize", writeError: "resize-conflict", step: inplaceUpdateStepPatchDelivered, class: inplaceClassUpdateFailed, hasError: true, claimError: true},
		{name: "C3 resize success patch conflict", kind: "resize", writeError: "conflict", step: inplaceUpdateStepPatchDelivered, class: inplaceClassUpdateFailed, hasError: true, claimError: true},
		{name: "C4 resource wait", kind: "resource-wait", step: inplaceUpdateStepPatchDelivered, claimReady: true},
		{name: "C5 image wait", kind: "image-wait", step: inplaceUpdateStepPatchDelivered},
		{name: "C6 applied not ready", kind: "not-ready", step: inplaceUpdateStepSucceeded},
		{name: "C7 applied and ready", kind: "ready", step: inplaceUpdateStepSucceeded, claimDone: true, claimReady: true, claimReason: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded},
		{name: "metadata conflict", kind: "metadata", writeError: "conflict", step: inplaceUpdateStepPatchDelivered, class: inplaceClassUpdateFailed, hasError: true, claimError: true},
		{name: "metadata timeout", kind: "metadata", writeError: "timeout", step: inplaceUpdateStepPatchDelivered, class: inplaceClassUpdateFailed, hasError: true, claimError: true},
		{name: "metadata unknown error", kind: "metadata", writeError: "unknown", step: inplaceUpdateStepPatchDelivered, class: inplaceClassUpdateFailed, hasError: true, claimError: true},
		{name: "metadata forbidden", kind: "metadata", writeError: "forbidden", step: inplaceUpdateStepPatchDelivered, class: inplaceClassUpdateFailed, hasError: true, terminal: true, claimDone: true, claimReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed},
		{name: "image patch forbidden", kind: "image", writeError: "forbidden", step: inplaceUpdateStepPatchDelivered, class: inplaceClassUpdateFailed, hasError: true, terminal: true, claimDone: true, claimReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed},
		{name: "resize unsupported", kind: "resize", writeError: "resize-unsupported", step: inplaceUpdateStepPatchDelivered, class: inplaceClassUpdateFailed, hasError: true, terminal: true, claimDone: true, claimReason: agentsv1alpha1.SandboxInplaceUpdateReasonUnsupportedResize},
		{name: "resize infeasible", kind: "resize-infeasible", step: inplaceUpdateStepPatchDelivered, class: inplaceClassUpdateFailed, hasError: true, terminal: true, claimDone: true, claimReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed},
		{name: "resize deferred", kind: "resize-deferred", step: inplaceUpdateStepPatchDelivered, class: inplaceClassUpdateFailed, hasError: true, terminal: true, claimDone: true, claimReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed},
		{name: "resize apply error", kind: "resize-error", step: inplaceUpdateStepPatchDelivered, class: inplaceClassUpdateFailed, hasError: true, terminal: true, claimDone: true, claimReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed},
		{name: "apply failure", kind: "apply-failure", step: inplaceUpdateStepPatchDelivered},
		{name: "old terminal succeeded", kind: "terminal", existingReason: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, step: inplaceUpdateStepPatchDelivered, claimReady: true},
		{name: "old terminal failed", kind: "terminal", existingReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed, step: inplaceUpdateStepPatchDelivered, claimReady: true},
		{name: "old terminal unsupported resize", kind: "terminal", existingReason: agentsv1alpha1.SandboxInplaceUpdateReasonUnsupportedResize, step: inplaceUpdateStepPatchDelivered, claimReady: true},
		{name: "same round failure stays terminal", kind: "terminal", sameRound: true, existingReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed, step: inplaceUpdateStepPatchDelivered, claimDone: true, claimReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed},
		{name: "same round unsupported stays terminal", kind: "terminal", sameRound: true, existingReason: agentsv1alpha1.SandboxInplaceUpdateReasonUnsupportedResize, step: inplaceUpdateStepPatchDelivered, claimDone: true, claimReason: agentsv1alpha1.SandboxInplaceUpdateReasonUnsupportedResize},
		{name: "same generation new revision ignores old failure", kind: "terminal-revision", existingReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed, step: inplaceUpdateStepPatchDelivered, claimReady: true},
		// proposal 已记录的保留行为：metadata 快速路径不观察旧资源更新。
		{name: "metadata with pending resize skips observation", kind: "metadata-pending", step: inplaceUpdateStepSucceeded, claimDone: true, claimReady: true, claimReason: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded},
	}
	for _, tt := range tests {
		for _, caller := range []string{"engine", "claim"} {
			t.Run(tt.name+"/"+caller, func(t *testing.T) {
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
				box.Generation = 3
				switch tt.kind {
				case "init-injected":
					pod.Spec.InitContainers = []corev1.Container{{Name: "init", Image: "busybox:1"}}
					box = buildMatchingHashBox("matrix-box", "default", *pod.Spec.DeepCopy())
					box.Generation = 3
					pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{Name: "injected", Image: "busybox:1"})
				case "untracked", "untracked-not-ready":
					delete(pod.Labels, agentsv1alpha1.PodLabelTemplateHash)
					if tt.kind == "untracked-not-ready" {
						pod.Status.Conditions[0].Status = corev1.ConditionFalse
					}
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
				case "resize":
					box.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("200m")
					pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = "old"
				case "metadata", "metadata-pending":
					pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = "old"
					if tt.kind == "metadata-pending" {
						pod.Annotations = map[string]string{inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"old","updateResources":true}`}
						pod.Status.ContainerStatuses[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("50m")
					}
				case "terminal", "terminal-revision":
					box.Status.Conditions = []metav1.Condition{{
						Type: string(agentsv1alpha1.SandboxConditionInplaceUpdate), Status: metav1.ConditionFalse,
						Reason: tt.existingReason, Message: "preserved terminal state",
					}}
					if tt.existingReason == agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded {
						box.Status.Conditions[0].Status = metav1.ConditionTrue
					}
					pod.Annotations = map[string]string{inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"target","updateResources":true}`}
					pod.Status.ContainerStatuses[0].Resources = nil
					box.Status.UpdateRevision = "target"
					if tt.sameRound || tt.kind == "terminal-revision" {
						box.Status.Conditions[0].ObservedGeneration = box.Generation
					}
					if tt.kind == "terminal-revision" {
						box.Status.UpdateRevision = "previous"
					}
				case "image":
					pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = "old"
					box.Spec.Template.Spec.Containers[0].Image = "nginx:2"
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
				scheme := runtime.NewScheme()
				require.NoError(t, clientgoscheme.AddToScheme(scheme))
				base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
				require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
				original := pod.DeepCopy()
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
						if failWrites && injected != nil && tt.writeError != "resize-conflict" && patch.Type() == types.StrategicMergePatchType {
							return injected
						}
						return c.Patch(ctx, obj, patch, opts...)
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
				control := inplaceupdate.NewInPlaceUpdateControl(wrapped, inplaceupdate.DefaultGeneratePatchBodyFunc)
				if caller == "engine" {
					before := box.DeepCopy()
					step, err := handleInPlaceUpdateCommon(t.Context(), control, pod, box, "target")
					require.Equal(t, before, box, "引擎不应读写 Sandbox Condition")
					require.Equal(t, tt.step, step)
					if tt.hasError {
						require.Error(t, err)
						require.Equal(t, tt.class, classifyInplaceError(err))
						require.Equal(t, tt.terminal, isTerminalInplaceError(err))
						if injected != nil {
							require.ErrorIs(t, err, injected)
						}
					} else {
						require.NoError(t, err)
					}
				} else {
					recorder := record.NewFakeRecorder(20)
					handler := &commonControl{inplaceUpdateControl: control, recorder: recorder}
					status := box.Status.DeepCopy()
					status.Phase, status.UpdateRevision = agentsv1alpha1.SandboxRunning, "target"
					done, err := handler.handleInplaceUpdateSandbox(t.Context(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: status})
					require.Equal(t, tt.claimDone, done)
					if tt.claimError {
						require.Error(t, err)
						if injected != nil {
							require.ErrorIs(t, err, injected)
						}
					} else {
						require.NoError(t, err)
					}
					reason := tt.claimReason
					if reason == "" {
						reason = agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating
					}
					assertInplaceConditions(t, status, tt.claimReady, reason)
					cond := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
					require.Equal(t, box.Generation, cond.ObservedGeneration)
					require.Equal(t, agentsv1alpha1.SandboxRunning, status.Phase)
					if tt.claimDone && reason != agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded {
						events := len(recorder.Events)
						if tt.sameRound {
							require.Zero(t, events)
						} else {
							require.Equal(t, 1, events)
						}
						box.Status = *status.DeepCopy()
						done, err = handler.handleInplaceUpdateSandbox(t.Context(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: status})
						require.NoError(t, err)
						require.True(t, done)
						assertInplaceConditions(t, status, tt.claimReady, reason)
						require.Len(t, recorder.Events, events, "同代失败重入不重复发送事件")
					}
				}
				if caller == "engine" && tt.kind == "resize" && tt.writeError == "conflict" {
					// C3 保留已知风险：metadata 收尾成功后走快速路径；修复方案待 proposal 确认。
					current := &corev1.Pod{}
					require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(pod), current))
					require.Equal(t, box.Spec.Template.Spec.Containers[0].Resources, current.Spec.Containers[0].Resources)
					require.Equal(t, original.Status, current.Status)
					require.Equal(t, "old", current.Labels[agentsv1alpha1.PodLabelTemplateHash])
					failWrites = false
					step, err := handleInPlaceUpdateCommon(t.Context(), control, current, box, "target")
					require.NoError(t, err)
					require.Equal(t, inplaceUpdateStepSucceeded, step)
					require.Equal(t, 1, resizes)
					require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(pod), current))
					require.Equal(t, "target", current.Labels[agentsv1alpha1.PodLabelTemplateHash])
					require.Equal(t, original.Status, current.Status)
				}
				require.Equal(t, original, pod)
				if tt.step == inplaceUpdateStepInProgress || tt.kind == "terminal" {
					require.Zero(t, patches)
					require.Zero(t, resizes)
				}
				if caller == "engine" && tt.kind == "metadata-pending" {
					require.Equal(t, 1, patches)
					require.Zero(t, resizes)
				}
				stored := &corev1.Pod{}
				require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(pod), stored))
				require.Equal(t, original.UID, stored.UID)
			})
		}
	}
}

func TestInplaceErrorPolicy(t *testing.T) {
	tests := []struct {
		name     string
		cause    error
		terminal bool
	}{
		{"conflict", apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "pod", fmt.Errorf("changed")), false},
		{"throttled", apierrors.NewTooManyRequests("slow down", 1), false},
		{"timeout", context.DeadlineExceeded, false},
		{"unknown", fmt.Errorf("unknown write result"), false},
		{"unauthorized", apierrors.NewUnauthorized("denied"), true},
		{"bad request", apierrors.NewBadRequest("invalid request"), true},
		{"unsupported", apierrors.NewMethodNotSupported(schema.GroupResource{Resource: "pods"}, "patch"), true},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "pod", nil), true},
		{"resize unsupported", &inplaceupdate.ResizeNotSupportedError{Err: fmt.Errorf("unsupported")}, true},
		{"resize infeasible", &inplaceupdate.ResizeInfeasibleError{Message: "infeasible"}, true},
		{"image failure", &inplaceupdate.ImagePullFailedError{Reason: "InvalidImageName"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := fmt.Errorf("outer context: %w", wrapInplaceError(inplaceClassUpdateFailed, "delivery", tt.cause))
			require.ErrorIs(t, err, tt.cause)
			require.Equal(t, inplaceClassUpdateFailed, classifyInplaceError(err))
			require.Equal(t, tt.terminal, isTerminalInplaceError(err))
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
			name: "template resource lower than pod (resize down)",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "main",
						Image: "nginx:latest",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
						}},
					},
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
											corev1.ResourceCPU: resource.MustParse("250m"),
										},
										Limits: corev1.ResourceList{
											corev1.ResourceCPU: resource.MustParse("500m"),
										},
									}},
								},
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
			require.Equal(t, tt.expected, isMetadataOnlyChange(tt.pod, tt.box))
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
			name: "UnsupportedResize reason returns true",
			status: &agentsv1alpha1.SandboxStatus{Conditions: []metav1.Condition{{
				Type:   string(agentsv1alpha1.SandboxConditionInplaceUpdate),
				Status: metav1.ConditionFalse, Reason: agentsv1alpha1.SandboxInplaceUpdateReasonUnsupportedResize,
			}}},
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
