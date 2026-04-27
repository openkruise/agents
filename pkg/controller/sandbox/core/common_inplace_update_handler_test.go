package core

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
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
			result, err := handleInPlaceUpdateCommon(ctx, handler, tc.pod, tc.box, tc.newStatus)

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
	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)

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

	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)
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

	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)
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

	// newStatus already has InplaceUpdate: Failed from a previous reconcile
	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "target-revision",
		Conditions: []metav1.Condition{
			{
				Type:    string(agentsv1alpha1.SandboxConditionInplaceUpdate),
				Status:  metav1.ConditionFalse,
				Reason:  agentsv1alpha1.SandboxInplaceUpdateReasonFailed,
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

	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result {
		t.Fatal("Expected result true (done), got false")
	}

	for _, cond := range newStatus.Conditions {
		if cond.Type == string(agentsv1alpha1.SandboxConditionInplaceUpdate) {
			if cond.Reason != agentsv1alpha1.SandboxInplaceUpdateReasonFailed {
				t.Errorf("Expected InplaceUpdate condition to remain Failed, got %s", cond.Reason)
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
	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)

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

	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)
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

	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)
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

	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)
	if err == nil {
		t.Fatal("Expected error from malformed annotation, got nil")
	}
	if result {
		t.Error("Expected result false on error, got true")
	}
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

	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result {
		t.Error("Expected result true (completed), got false")
	}
}

func TestHandleInPlaceUpdateCommon_StateNotNilNotCompletedTerminalErr(t *testing.T) {
	// state != nil, resize pending infeasible → not completed, terminalErr != nil
	// → log and return false, nil
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

	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if result {
		t.Error("Expected result false (not completed), got true")
	}
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
		UpdateRevision: "new-revision",
	}

	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)
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
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	// control.Update should succeed (image patch), returns changed=true → return false, nil
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

	// Use a custom patchBodyFunc that returns empty (no changes)
	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control: inplaceupdate.NewInPlaceUpdateControl(fakeClient, func(opts inplaceupdate.InPlaceUpdateOptions) string {
			return "" // No patch needed
		}),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	// control.Update returns !changed → return true, nil
	if !result {
		t.Error("Expected result true (no changes), got false")
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
