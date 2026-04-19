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
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsapiv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// buildTestScheme builds a runtime scheme containing both the agents/v1alpha1
// types and corev1 types used by the in-place update tests.
func buildTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme, err := agentsapiv1alpha1.SchemeBuilder.Build()
	if err != nil {
		t.Fatalf("Failed to build scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("Failed to add corev1 to scheme: %v", err)
	}
	return scheme
}

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

func TestInPlaceUpdateControl_Update_ImageChange(t *testing.T) {
	scheme := buildTestScheme(t)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "old"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "container1",
				Image: "nginx:latest",
			}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:    "container1",
				ImageID: "docker.io/nginx:latest",
			}},
		},
	}
	box := &agentsapiv1alpha1.Sandbox{
		Spec: agentsapiv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{"app": "new"},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "container1",
							Image: "nginx:1.20",
						}},
					},
				},
			},
		},
	}

	progressCount := 0
	ctrl := NewInPlaceUpdateControl(
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build(),
		nil,
	)
	progressed, err := ctrl.Update(context.Background(), InPlaceUpdateOptions{
		Box:        box,
		Pod:        pod,
		Revision:   "rev-1",
		OnProgress: func() { progressCount++ },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !progressed {
		t.Fatalf("expected progressed=true")
	}
	if progressCount != 1 {
		t.Fatalf("expected exactly 1 progress callback (image patch only), got %d", progressCount)
	}

	updated := &corev1.Pod{}
	if err := ctrl.Get(context.Background(),
		types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, updated); err != nil {
		t.Fatalf("get updated pod: %v", err)
	}
	if updated.Spec.Containers[0].Image != "nginx:1.20" {
		t.Fatalf("expected image patched to nginx:1.20, got %q", updated.Spec.Containers[0].Image)
	}
	if updated.Labels[agentsapiv1alpha1.PodLabelTemplateHash] != "rev-1" {
		t.Fatalf("expected pod-template-hash label=rev-1, got %q",
			updated.Labels[agentsapiv1alpha1.PodLabelTemplateHash])
	}
	if updated.Labels["app"] != "new" {
		t.Fatalf("expected app label patched to new, got %q", updated.Labels["app"])
	}
	stateStr, ok := updated.Annotations[PodAnnotationInPlaceUpdateStateKey]
	if !ok || stateStr == "" {
		t.Fatalf("expected in-place update state annotation to be set")
	}
	state := &InPlaceUpdateState{}
	if err := json.Unmarshal([]byte(stateStr), state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.Revision != "rev-1" || !state.UpdateImages || state.UpdateResources {
		t.Fatalf("unexpected state: %+v", state)
	}
}

func TestInPlaceUpdateControl_Update_NoChange(t *testing.T) {
	scheme := buildTestScheme(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p",
			Namespace: "default",
			Labels: map[string]string{
				agentsapiv1alpha1.PodLabelTemplateHash: "rev",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c1", Image: "img:1"}},
		},
	}
	box := &agentsapiv1alpha1.Sandbox{
		Spec: agentsapiv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c1", Image: "img:1"}},
					},
				},
			},
		},
	}
	ctrl := NewInPlaceUpdateControl(
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build(),
		nil,
	)
	progressed, err := ctrl.Update(context.Background(), InPlaceUpdateOptions{
		Box:      box,
		Pod:      pod,
		Revision: "rev",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if progressed {
		t.Fatalf("expected progressed=false when no spec changes")
	}
}

func TestInPlaceUpdateControl_Update_MetadataOnlyLabels(t *testing.T) {
	scheme := buildTestScheme(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p-label-only",
			Namespace: "default",
			Labels: map[string]string{
				agentsapiv1alpha1.PodLabelTemplateHash: "old-rev",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c1", Image: "img:1"}},
		},
	}
	box := &agentsapiv1alpha1.Sandbox{
		Spec: agentsapiv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{"custom-label-1": "value1"},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c1", Image: "img:1"}},
					},
				},
			},
		},
	}
	ctrl := NewInPlaceUpdateControl(
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build(),
		nil,
	)
	progressed, err := ctrl.Update(context.Background(), InPlaceUpdateOptions{
		Box:      box,
		Pod:      pod,
		Revision: "new-rev",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !progressed {
		t.Fatalf("expected progressed=true for metadata-only labels patch")
	}

	updated := &corev1.Pod{}
	if err := ctrl.Get(context.Background(), types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, updated); err != nil {
		t.Fatalf("get updated pod: %v", err)
	}
	if updated.Labels["custom-label-1"] != "value1" {
		t.Fatalf("expected custom-label-1=value1 on pod, got labels: %v", updated.Labels)
	}
	if updated.Labels[agentsapiv1alpha1.PodLabelTemplateHash] != "new-rev" {
		t.Fatalf("expected pod-template-hash=new-rev, got %q", updated.Labels[agentsapiv1alpha1.PodLabelTemplateHash])
	}
}

func TestInPlaceUpdateControl_Update_ResizeViaSubresource(t *testing.T) {
	scheme := buildTestScheme(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "c",
				Image: "img:1",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				},
			}},
		},
	}
	box := &agentsapiv1alpha1.Sandbox{
		Spec: agentsapiv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "c",
							Image: "img:1",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
							},
						}},
					},
				},
			},
		},
	}

	resizeCalls := 0
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	wrapped := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if sub != "resize" {
				return c.SubResource(sub).Update(ctx, obj, opts...)
			}
			resizeCalls++
			body := obj
			updateOpts := &client.SubResourceUpdateOptions{}
			updateOpts.ApplyOptions(opts)
			if updateOpts.SubResourceBody != nil {
				body = updateOpts.SubResourceBody
			}
			resizePod, ok := body.(*corev1.Pod)
			if !ok {
				return fmt.Errorf("expected *corev1.Pod, got %T", body)
			}
			existing := &corev1.Pod{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), existing); err != nil {
				return err
			}
			for i, container := range existing.Spec.Containers {
				for _, rc := range resizePod.Spec.Containers {
					if rc.Name == container.Name {
						existing.Spec.Containers[i].Resources = rc.Resources
					}
				}
			}
			return c.Update(ctx, existing)
		},
	})

	progressCount := 0
	ctrl := NewInPlaceUpdateControl(wrapped, nil)
	progressed, err := ctrl.Update(context.Background(), InPlaceUpdateOptions{
		Box:        box,
		Pod:        pod,
		Revision:   "rev",
		OnProgress: func() { progressCount++ },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !progressed {
		t.Fatalf("expected progressed=true")
	}
	if resizeCalls != 1 {
		t.Fatalf("expected exactly one resize subresource call, got %d", resizeCalls)
	}
	// One callback for the metadata patch (UpdateResources state) + one for the resize call.
	if progressCount != 2 {
		t.Fatalf("expected 2 progress callbacks (patch + resize), got %d", progressCount)
	}
	if ctrl.useDirectResourcePatch.Load() {
		t.Fatalf("useDirectResourcePatch should remain false when resize subresource works")
	}

	updated := &corev1.Pod{}
	if err := ctrl.Get(context.Background(),
		types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, updated); err != nil {
		t.Fatalf("get updated pod: %v", err)
	}
	cpuReq := updated.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if cpuReq.MilliValue() != 500 {
		t.Fatalf("expected cpu request=500m, got %dm", cpuReq.MilliValue())
	}
}

func TestInPlaceUpdateControl_Update_ResizeFallbackToDirectPatch(t *testing.T) {
	scheme := buildTestScheme(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "c",
				Image: "img:1",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				},
			}},
		},
	}
	box := &agentsapiv1alpha1.Sandbox{
		Spec: agentsapiv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "c",
							Image: "img:1",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("750m")},
							},
						}},
					},
				},
			},
		},
	}

	subCalls := 0
	patchCalls := 0
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	wrapped := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if sub == "resize" {
				subCalls++
				return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, obj.GetName())
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			patchCalls++
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	ctrl := NewInPlaceUpdateControl(wrapped, nil)
	progressed, err := ctrl.Update(context.Background(), InPlaceUpdateOptions{
		Box:      box,
		Pod:      pod,
		Revision: "rev",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !progressed {
		t.Fatalf("expected progressed=true")
	}
	if subCalls != 1 {
		t.Fatalf("expected exactly one resize subresource probe, got %d", subCalls)
	}
	if !ctrl.useDirectResourcePatch.Load() {
		t.Fatalf("expected useDirectResourcePatch=true after 404 fallback")
	}
	// Expect the metadata patch + the direct resource patch fallback to be observed.
	if patchCalls < 2 {
		t.Fatalf("expected at least 2 Patch calls (metadata + resource), got %d", patchCalls)
	}

	// Second invocation: the cached flag should make us skip the subresource probe entirely.
	subCallsBefore := subCalls
	pod2 := pod.DeepCopy()
	pod2.Name = "p2"
	pod2.ResourceVersion = ""
	if err := ctrl.Create(context.Background(), pod2); err != nil {
		t.Fatalf("seed second pod: %v", err)
	}
	if _, err := ctrl.Update(context.Background(), InPlaceUpdateOptions{
		Box:      box,
		Pod:      pod2,
		Revision: "rev",
	}); err != nil {
		t.Fatalf("second update: %v", err)
	}
	if subCalls != subCallsBefore {
		t.Fatalf("expected subresource probe to be cached (no extra calls), got %d more",
			subCalls-subCallsBefore)
	}
}

func TestInPlaceUpdateControl_Update_ResizeNotSupported(t *testing.T) {
	scheme := buildTestScheme(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "c",
				Image: "img:1",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				},
			}},
		},
	}
	box := &agentsapiv1alpha1.Sandbox{
		Spec: agentsapiv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "c",
							Image: "img:1",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("750m")},
							},
						}},
					},
				},
			},
		},
	}

	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	wrapped := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if sub == "resize" {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, obj.GetName())
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			// Allow the metadata patch (which contains "metadata") to succeed
			// but reject the spec-only resource patch fallback so we can
			// observe the ResizeNotSupportedError path.
			data, _ := patch.Data(obj)
			if !strings.Contains(string(data), `"metadata"`) {
				return apierrors.NewBadRequest("InPlacePodVerticalScaling feature gate disabled")
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	ctrl := NewInPlaceUpdateControl(wrapped, nil)
	_, err := ctrl.Update(context.Background(), InPlaceUpdateOptions{
		Box:      box,
		Pod:      pod,
		Revision: "rev",
	})
	if err == nil {
		t.Fatalf("expected error for unsupported resize")
	}
	var resizeErr *ResizeNotSupportedError
	if !errors.As(err, &resizeErr) {
		t.Fatalf("expected ResizeNotSupportedError, got %T: %v", err, err)
	}
	if resizeErr.Unwrap() == nil {
		t.Fatalf("expected wrapped error to be non-nil")
	}
	if !strings.Contains(resizeErr.Error(), "in-place pod resize not supported") {
		t.Fatalf("unexpected error message: %v", resizeErr.Error())
	}
}

func TestInPlaceUpdateControl_Update_ResizeSubresourceServerError(t *testing.T) {
	scheme := buildTestScheme(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "c",
				Image: "img:1",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				},
			}},
		},
	}
	box := &agentsapiv1alpha1.Sandbox{
		Spec: agentsapiv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "c",
							Image: "img:1",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("750m")},
							},
						}},
					},
				},
			},
		},
	}

	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	wrapped := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if sub == "resize" {
				return apierrors.NewInternalError(fmt.Errorf("etcd timeout"))
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	ctrl := NewInPlaceUpdateControl(wrapped, nil)

	progressed, err := ctrl.Update(context.Background(), InPlaceUpdateOptions{
		Box:      box,
		Pod:      pod,
		Revision: "rev",
	})
	if err == nil {
		t.Fatalf("expected non-nil error from failing resize subresource")
	}
	// progressed must be true because the metadata patch already happened.
	if !progressed {
		t.Fatalf("expected progressed=true after the metadata patch even when resize fails")
	}
	// The non-NotFound error must NOT trigger the fallback flag.
	if ctrl.useDirectResourcePatch.Load() {
		t.Fatalf("non-NotFound error should not flip the direct-patch flag")
	}
	// And it must NOT be wrapped as ResizeNotSupportedError (that's reserved
	// for the direct-patch fallback failure).
	var resizeErr *ResizeNotSupportedError
	if errors.As(err, &resizeErr) {
		t.Fatalf("did not expect ResizeNotSupportedError from subresource server error")
	}
}

func TestInPlaceUpdateControl_Update_ResizeConflictRetrySucceeds(t *testing.T) {
	scheme := buildTestScheme(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "c",
				Image: "img:1",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				},
			}},
		},
	}
	box := &agentsapiv1alpha1.Sandbox{
		Spec: agentsapiv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "c",
							Image: "img:1",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("750m")},
							},
						}},
					},
				},
			},
		},
	}

	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	resizeAttempts := 0
	wrapped := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if sub != "resize" {
				return c.SubResource(sub).Update(ctx, obj, opts...)
			}
			resizeAttempts++
			if resizeAttempts == 1 {
				// First attempt: bump the pod's resourceVersion behind our
				// back so the controller observes a conflict and re-reads
				// the latest object.
				latest := &corev1.Pod{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(obj), latest); err != nil {
					return err
				}
				latest.Annotations = map[string]string{"bumped": "true"}
				if err := c.Update(ctx, latest); err != nil {
					return err
				}
				return apierrors.NewConflict(
					schema.GroupResource{Resource: "pods"}, obj.GetName(),
					fmt.Errorf("the object has been modified"),
				)
			}
			// Subsequent attempts succeed and apply the resize.
			body := obj
			updateOpts := &client.SubResourceUpdateOptions{}
			updateOpts.ApplyOptions(opts)
			if updateOpts.SubResourceBody != nil {
				body = updateOpts.SubResourceBody
			}
			rp := body.(*corev1.Pod)
			existing := &corev1.Pod{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), existing); err != nil {
				return err
			}
			for i, container := range existing.Spec.Containers {
				for _, rc := range rp.Spec.Containers {
					if rc.Name == container.Name {
						existing.Spec.Containers[i].Resources = rc.Resources
					}
				}
			}
			return c.Update(ctx, existing)
		},
	})
	ctrl := NewInPlaceUpdateControl(wrapped, nil)

	progressed, err := ctrl.Update(context.Background(), InPlaceUpdateOptions{
		Box:      box,
		Pod:      pod,
		Revision: "rev",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !progressed {
		t.Fatalf("expected progressed=true")
	}
	if resizeAttempts < 2 {
		t.Fatalf("expected the conflict to trigger at least one retry, got %d attempts", resizeAttempts)
	}

	updated := &corev1.Pod{}
	if err := ctrl.Get(context.Background(),
		types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, updated); err != nil {
		t.Fatalf("get updated pod: %v", err)
	}
	cpuReq := updated.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if cpuReq.MilliValue() != 750 {
		t.Fatalf("expected cpu request=750m after retry, got %dm", cpuReq.MilliValue())
	}
}

func TestInPlaceUpdateControl_Update_ResizeConflictRetryNoLongerNeeded(t *testing.T) {
	scheme := buildTestScheme(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "c",
				Image: "img:1",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				},
			}},
		},
	}
	box := &agentsapiv1alpha1.Sandbox{
		Spec: agentsapiv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "c",
							Image: "img:1",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("750m")},
							},
						}},
					},
				},
			},
		},
	}

	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	resizeAttempts := 0
	wrapped := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if sub != "resize" {
				return c.SubResource(sub).Update(ctx, obj, opts...)
			}
			resizeAttempts++
			// Simulate that another actor has already applied the resize, so
			// recomputing resizeBody from the latest pod returns nil and the
			// retry loop should exit successfully without a re-attempt.
			latest := &corev1.Pod{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), latest); err != nil {
				return err
			}
			latest.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("750m"),
			}
			if err := c.Update(ctx, latest); err != nil {
				return err
			}
			return apierrors.NewConflict(
				schema.GroupResource{Resource: "pods"}, obj.GetName(),
				fmt.Errorf("the object has been modified"),
			)
		},
	})
	ctrl := NewInPlaceUpdateControl(wrapped, nil)

	progressed, err := ctrl.Update(context.Background(), InPlaceUpdateOptions{
		Box:      box,
		Pod:      pod,
		Revision: "rev",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !progressed {
		t.Fatalf("expected progressed=true (metadata patch already applied)")
	}
	if resizeAttempts != 1 {
		t.Fatalf("expected exactly 1 resize attempt (no retry needed when resize is no-op), got %d",
			resizeAttempts)
	}
}

func TestInPlaceUpdateControl_PatchPodResources_Conflict(t *testing.T) {
	scheme := buildTestScheme(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c"}},
		},
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	wrapped := interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			return apierrors.NewConflict(
				schema.GroupResource{Resource: "pods"}, obj.GetName(),
				fmt.Errorf("the object has been modified"),
			)
		},
	})
	ctrl := NewInPlaceUpdateControl(wrapped, nil)

	resizeBody := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "c",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				},
			}},
		},
	}
	err := ctrl.patchPodResources(context.Background(), klog.NewKlogr(), pod, resizeBody)
	if err == nil {
		t.Fatalf("expected error from conflicted patch")
	}
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected a Conflict error to be propagated unwrapped, got %T: %v", err, err)
	}
	var resizeErr *ResizeNotSupportedError
	if errors.As(err, &resizeErr) {
		t.Fatalf("Conflict must NOT be wrapped as ResizeNotSupportedError")
	}
}

func TestDefaultGeneratePatchBodyFunc_PodHasContainerNotInTemplate(t *testing.T) {
	// Ensures the `continue` branch when a pod container is absent from the
	// template is exercised.
	body := DefaultGeneratePatchBodyFunc(InPlaceUpdateOptions{
		Box: &agentsapiv1alpha1.Sandbox{
			Spec: agentsapiv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "main", Image: "img:1"}},
						},
					},
				},
			},
		},
		Pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "p",
				Namespace: "default",
				Labels: map[string]string{
					agentsapiv1alpha1.PodLabelTemplateHash: "rev",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "main", Image: "img:1"},
					{Name: "sidecar-not-in-template", Image: "side:1"},
				},
			},
		},
		Revision: "rev",
	})
	if body != "" {
		t.Fatalf("expected empty body when only an off-template sidecar is present, got: %s", body)
	}
}

func TestInPlaceUpdateControl_Update_PatchError(t *testing.T) {
	scheme := buildTestScheme(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "img:1"}},
		},
	}
	box := &agentsapiv1alpha1.Sandbox{
		Spec: agentsapiv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "img:2"}},
					},
				},
			},
		},
	}

	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	wrapped := interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			return apierrors.NewServiceUnavailable("temporary outage")
		},
	})
	ctrl := NewInPlaceUpdateControl(wrapped, nil)
	progressed, err := ctrl.Update(context.Background(), InPlaceUpdateOptions{
		Box:      box,
		Pod:      pod,
		Revision: "rev",
	})
	if err == nil {
		t.Fatalf("expected error from failing patch")
	}
	if progressed {
		t.Fatalf("expected progressed=false on initial patch failure")
	}
}

func TestInPlaceUpdateControl_Update_CustomPatchFunc(t *testing.T) {
	scheme := buildTestScheme(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "img:1"}},
		},
	}
	box := &agentsapiv1alpha1.Sandbox{
		Spec: agentsapiv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "img:1"}},
					},
				},
			},
		},
	}

	called := 0
	custom := func(opts InPlaceUpdateOptions) string {
		called++
		return `{"metadata":{"annotations":{"custom":"yes"}}}`
	}
	ctrl := NewInPlaceUpdateControl(
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build(),
		custom,
	)
	progressed, err := ctrl.Update(context.Background(), InPlaceUpdateOptions{
		Box:      box,
		Pod:      pod,
		Revision: "rev",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !progressed {
		t.Fatalf("expected progressed=true")
	}
	if called == 0 {
		t.Fatalf("expected custom patch func to be invoked")
	}
	updated := &corev1.Pod{}
	if err := ctrl.Get(context.Background(),
		types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, updated); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if updated.Annotations["custom"] != "yes" {
		t.Fatalf("expected custom annotation to be applied, got %v", updated.Annotations)
	}
}

func TestNewInPlaceUpdateControl(t *testing.T) {
	scheme := buildTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	ctrl := NewInPlaceUpdateControl(c, nil)
	if ctrl == nil {
		t.Fatalf("expected non-nil control")
	}
	if ctrl.Client != c {
		t.Fatalf("expected client to be wired through")
	}
	if ctrl.generatePatchBodyFunc != nil {
		t.Fatalf("expected nil patch func when none provided")
	}

	customCalled := false
	custom := func(opts InPlaceUpdateOptions) string {
		customCalled = true
		return ""
	}
	ctrl2 := NewInPlaceUpdateControl(c, custom)
	if ctrl2.generatePatchBody(InPlaceUpdateOptions{}) != "" {
		t.Fatalf("expected empty body from custom func")
	}
	if !customCalled {
		t.Fatalf("expected custom patch func to be called via generatePatchBody dispatcher")
	}
}

func TestResizeNotSupportedError(t *testing.T) {
	inner := fmt.Errorf("boom")
	err := &ResizeNotSupportedError{Err: inner}
	if !strings.Contains(err.Error(), "in-place pod resize not supported") {
		t.Fatalf("unexpected message: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected wrapped message to be included: %s", err.Error())
	}
	if !errors.Is(err, inner) {
		t.Fatalf("expected errors.Is to match wrapped error")
	}
	if err.Unwrap() != inner {
		t.Fatalf("Unwrap() returned %v, want %v", err.Unwrap(), inner)
	}
}

func TestBuildResourcePatch(t *testing.T) {
	body := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "c1",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("250m"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
				{Name: "c2"},
			},
		},
	}
	patch := buildResourcePatch(body)
	if patch == "" {
		t.Fatalf("expected non-empty patch")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(patch), &parsed); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	spec, ok := parsed["spec"].(map[string]any)
	if !ok {
		t.Fatalf("expected spec in patch, got %v", parsed)
	}
	containers, ok := spec["containers"].([]any)
	if !ok {
		t.Fatalf("expected containers slice, got %v", spec)
	}
	if len(containers) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(containers))
	}
	if _, exists := parsed["metadata"]; exists {
		t.Fatalf("resource patch should not include metadata")
	}
}

func TestDefaultGenerateResizeSubresourceBody_NilTemplateAndNoChange(t *testing.T) {
	if got := DefaultGenerateResizeSubresourceBody(InPlaceUpdateOptions{
		Box: &agentsapiv1alpha1.Sandbox{},
		Pod: &corev1.Pod{},
	}); got != nil {
		t.Fatalf("expected nil body when template is nil, got %+v", got)
	}

	identical := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
	}
	body := DefaultGenerateResizeSubresourceBody(InPlaceUpdateOptions{
		Box: &agentsapiv1alpha1.Sandbox{
			Spec: agentsapiv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "c", Resources: identical},
								{Name: "absent-from-pod", Resources: identical},
							},
						},
					},
				},
			},
		},
		Pod: &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "c", Resources: identical},
					{Name: "extra", Resources: corev1.ResourceRequirements{}},
				},
			},
		},
	})
	if body != nil {
		t.Fatalf("expected nil body when no resource changes, got %+v", body)
	}
}

func TestDefaultGeneratePatchBodyFunc_NoChange(t *testing.T) {
	got := DefaultGeneratePatchBodyFunc(InPlaceUpdateOptions{
		Box: &agentsapiv1alpha1.Sandbox{
			Spec: agentsapiv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "c", Image: "img:1"}},
						},
					},
				},
			},
		},
		Pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "p",
				Namespace: "default",
				Labels: map[string]string{
					agentsapiv1alpha1.PodLabelTemplateHash: "r",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "c", Image: "img:1"}},
			},
		},
		Revision: "r",
	})
	if got != "" {
		t.Fatalf("expected empty patch body when nothing changed, got %s", got)
	}
}

func TestDefaultGeneratePatchBodyFunc_ImageOnly(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p",
			Namespace: "default",
			Labels: map[string]string{
				"keep":    "me",
				"app":     "old",
				"already": "set",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "old"}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "c", ImageID: "old-id"},
			},
		},
	}
	box := &agentsapiv1alpha1.Sandbox{
		Spec: agentsapiv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsapiv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app":     "new",
							"already": "set",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "new"}},
					},
				},
			},
		},
	}

	body := DefaultGeneratePatchBodyFunc(InPlaceUpdateOptions{
		Box:      box,
		Pod:      pod,
		Revision: "rev-img",
	})
	if body == "" {
		t.Fatalf("expected non-empty body for image change")
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	metadata, _ := decoded["metadata"].(map[string]any)
	labels, _ := metadata["labels"].(map[string]any)
	if labels["app"] != "new" {
		t.Fatalf("expected app label updated, got %v", labels)
	}
	if _, exists := labels["already"]; exists {
		t.Fatalf("labels already in sync should not be patched again")
	}
	if labels[agentsapiv1alpha1.PodLabelTemplateHash] != "rev-img" {
		t.Fatalf("expected template hash label, got %v", labels)
	}

	annotations, _ := metadata["annotations"].(map[string]any)
	stateRaw, _ := annotations[PodAnnotationInPlaceUpdateStateKey].(string)
	state := &InPlaceUpdateState{}
	if err := json.Unmarshal([]byte(stateRaw), state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if !state.UpdateImages || state.UpdateResources {
		t.Fatalf("expected only updateImages=true, got %+v", state)
	}
	if state.LastContainerStatuses["c"].ImageID != "old-id" {
		t.Fatalf("expected previous image id captured, got %+v", state.LastContainerStatuses)
	}
}

func TestProcessResourceListAndGetQOSResources(t *testing.T) {
	// Sum non-zero CPU values across two containers; verify zero/unsupported
	// resources are skipped and that the second container's CPU is added to
	// the existing entry rather than overwriting it.
	list := corev1.ResourceList{}
	processResourceList(list, corev1.ResourceList{
		corev1.ResourceCPU:              resource.MustParse("100m"),
		corev1.ResourceMemory:           resource.MustParse("0"),
		corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
	})
	processResourceList(list, corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("250m"),
		corev1.ResourceMemory: resource.MustParse("128Mi"),
	})
	cpu := list[corev1.ResourceCPU]
	if cpu.MilliValue() != 350 {
		t.Fatalf("expected accumulated cpu = 350m, got %dm", cpu.MilliValue())
	}
	if _, ok := list[corev1.ResourceEphemeralStorage]; ok {
		t.Fatalf("unsupported resource should not be tracked")
	}

	qos := getQOSResources(corev1.ResourceList{
		corev1.ResourceCPU:              resource.MustParse("100m"),
		corev1.ResourceMemory:           resource.MustParse("0"),
		corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
	})
	if !qos[corev1.ResourceCPU] {
		t.Fatalf("expected cpu marked")
	}
	if qos[corev1.ResourceMemory] {
		t.Fatalf("zero memory should be skipped")
	}
	if qos[corev1.ResourceEphemeralStorage] {
		t.Fatalf("unsupported resource should be skipped")
	}
}

func TestIsResourceListCovered(t *testing.T) {
	expected := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}

	if !isResourceListCovered(corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("64Mi"),
	}, expected) {
		t.Fatalf("expected actual=expected (with extra) to be covered")
	}
	if isResourceListCovered(corev1.ResourceList{}, expected) {
		t.Fatalf("missing key should not be covered")
	}
	if isResourceListCovered(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("99m")}, expected) {
		t.Fatalf("smaller value should not be covered")
	}
}

func TestIsPodResourceResizeCompleted(t *testing.T) {
	tmpl := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
	}
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Resources: tmpl}},
		},
	}

	if isPodResourceResizeCompleted(pod) {
		t.Fatalf("expected not completed when status is empty")
	}

	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "c"}}
	if isPodResourceResizeCompleted(pod) {
		t.Fatalf("expected not completed when status.resources is nil")
	}

	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "c",
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
			Limits:   tmpl.Limits,
		},
	}}
	if isPodResourceResizeCompleted(pod) {
		t.Fatalf("expected not completed when requests do not match")
	}

	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "c",
		Resources: &corev1.ResourceRequirements{
			Requests: tmpl.Requests,
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
		},
	}}
	if isPodResourceResizeCompleted(pod) {
		t.Fatalf("expected not completed when limits do not match")
	}

	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:      "c",
		Resources: &tmpl,
	}}
	if !isPodResourceResizeCompleted(pod) {
		t.Fatalf("expected completed when spec/status resources match")
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

func TestDefaultGeneratePatchBodyFunc_ExtensionAnnotations(t *testing.T) {
	opts := InPlaceUpdateOptions{
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
		Revision: "rev-ext",
		ExtensionAnnotations: map[string]string{
			"custom.annotation/key1": "value1",
			"custom.annotation/key2": "value2",
		},
	}

	patchBody := DefaultGeneratePatchBodyFunc(opts)
	if patchBody == "" {
		t.Fatalf("expected non-empty patch body for extension annotations")
	}

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

	// Verify extension annotations are present
	if annotations["custom.annotation/key1"] != "value1" {
		t.Errorf("Expected extension annotation custom.annotation/key1=value1, got %v", annotations["custom.annotation/key1"])
	}
	if annotations["custom.annotation/key2"] != "value2" {
		t.Errorf("Expected extension annotation custom.annotation/key2=value2, got %v", annotations["custom.annotation/key2"])
	}

	// Verify inplace update state annotation still exists
	stateStr, ok := annotations[PodAnnotationInPlaceUpdateStateKey].(string)
	if !ok {
		t.Fatalf("Annotations should have inplace update state")
	}

	var state InPlaceUpdateState
	if err := json.Unmarshal([]byte(stateStr), &state); err != nil {
		t.Fatalf("Failed to unmarshal state: %v", err)
	}

	if state.Revision != "rev-ext" {
		t.Errorf("Expected revision rev-ext, got %s", state.Revision)
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
