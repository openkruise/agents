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

	"github.com/stretchr/testify/require"
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

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// buildTestScheme builds a runtime scheme containing both the agents/v1alpha1
// types and corev1 types used by the in-place update tests.
func buildTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme, err := agentsv1alpha1.SchemeBuilder.Build()
	if err != nil {
		t.Fatalf("Failed to build scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("Failed to add corev1 to scheme: %v", err)
	}
	return scheme
}

func mustGetInPlaceUpdateStateFromPod(t *testing.T, pod *corev1.Pod) *InPlaceUpdateState {
	t.Helper()
	raw := pod.Annotations[PodAnnotationInPlaceUpdateStateKey]
	if raw == "" {
		t.Fatalf("expected inplace update state annotation on pod %s/%s", pod.Namespace, pod.Name)
	}
	state := &InPlaceUpdateState{}
	if err := json.Unmarshal([]byte(raw), state); err != nil {
		t.Fatalf("decode inplace update state: %v", err)
	}
	return state
}

func applyResizeSubresourcePatch(ctx context.Context, c client.Client, obj client.Object, patch client.Patch) error {
	data, err := patch.Data(obj)
	if err != nil {
		return err
	}
	resizePatch := struct {
		Spec struct {
			Containers []corev1.Container `json:"containers"`
		} `json:"spec"`
	}{}
	if err := json.Unmarshal(data, &resizePatch); err != nil {
		return err
	}

	existing := &corev1.Pod{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), existing); err != nil {
		return err
	}
	for i := range existing.Spec.Containers {
		for _, patchContainer := range resizePatch.Spec.Containers {
			if patchContainer.Name != existing.Spec.Containers[i].Name {
				continue
			}
			// The pods/resize subresource applies a strategic merge patch:
			// resources.requests and resources.limits are merged key-wise,
			// preserving unrelated entries such as ephemeral-storage or
			// system-injected resources on the live pod.
			resources := &existing.Spec.Containers[i].Resources
			for name, quantity := range patchContainer.Resources.Requests {
				if resources.Requests == nil {
					resources.Requests = corev1.ResourceList{}
				}
				resources.Requests[name] = quantity
			}
			for name, quantity := range patchContainer.Resources.Limits {
				if resources.Limits == nil {
					resources.Limits = corev1.ResourceList{}
				}
				resources.Limits[name] = quantity
			}
		}
	}
	// Real controller-runtime refreshes the passed-in object with the server
	// response (including the new resourceVersion) after a subresource patch;
	// write back to obj here too, so a later optimistic-lock metadata patch by
	// the caller can read the latest version.
	if err := c.Update(ctx, existing); err != nil {
		return err
	}
	if pod, ok := obj.(*corev1.Pod); ok {
		existing.DeepCopyInto(pod)
	}
	return nil
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
	box := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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

	ctrl := NewInPlaceUpdateControl(
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build(),
		nil,
	)
	progressed, err := ctrl.Update(context.Background(), InPlaceUpdateOptions{
		Box:      box,
		Pod:      pod,
		Revision: "rev-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !progressed {
		t.Fatalf("expected progressed=true")
	}

	updated := &corev1.Pod{}
	if err := ctrl.Get(context.Background(),
		types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, updated); err != nil {
		t.Fatalf("get updated pod: %v", err)
	}
	if updated.Spec.Containers[0].Image != "nginx:1.20" {
		t.Fatalf("expected image patched to nginx:1.20, got %q", updated.Spec.Containers[0].Image)
	}
	if updated.Labels[agentsv1alpha1.PodLabelTemplateHash] != "rev-1" {
		t.Fatalf("expected pod-template-hash label=rev-1, got %q",
			updated.Labels[agentsv1alpha1.PodLabelTemplateHash])
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
				agentsv1alpha1.PodLabelTemplateHash: "rev",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c1", Image: "img:1"}},
		},
	}
	box := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
				agentsv1alpha1.PodLabelTemplateHash: "old-rev",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c1", Image: "img:1"}},
		},
	}
	box := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
	if updated.Labels[agentsv1alpha1.PodLabelTemplateHash] != "new-rev" {
		t.Fatalf("expected pod-template-hash=new-rev, got %q", updated.Labels[agentsv1alpha1.PodLabelTemplateHash])
	}
}

func TestInPlaceUpdateControl_Update_ResizeViaSubresource(t *testing.T) {
	scheme := buildTestScheme(t)
	injectedResource := corev1.ResourceName("example.com/injected-resource")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "c",
				Image: "img:1",
				ResizePolicy: []corev1.ContainerResizePolicy{
					{
						ResourceName:  corev1.ResourceMemory,
						RestartPolicy: corev1.RestartContainer,
					},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:              resource.MustParse("100m"),
						corev1.ResourceEphemeralStorage: resource.MustParse("30Gi"),
						injectedResource:                resource.MustParse("1"),
					},
					Limits: corev1.ResourceList{
						injectedResource: resource.MustParse("1"),
					},
				},
			}},
		},
	}
	box := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if sub != "resize" {
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			}
			resizeCalls++
			data, err := patch.Data(obj)
			if err != nil {
				return err
			}
			if strings.Contains(string(data), "resizePolicy") {
				return fmt.Errorf("resize subresource patch must not include resizePolicy: %s", string(data))
			}
			return applyResizeSubresourcePatch(ctx, c, obj, patch)
		},
	})

	ctrl := NewInPlaceUpdateControl(wrapped, nil)
	require.NoError(t, ctrl.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
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
	if resizeCalls != 1 {
		t.Fatalf("expected exactly one resize subresource call, got %d", resizeCalls)
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
	if got := updated.Spec.Containers[0].Resources.Requests[corev1.ResourceEphemeralStorage]; got.Cmp(resource.MustParse("30Gi")) != 0 {
		t.Fatalf("expected ephemeral-storage request to be preserved, got %s", got.String())
	}
	if got := updated.Spec.Containers[0].Resources.Requests[injectedResource]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Fatalf("expected injected request to be preserved, got %s", got.String())
	}
	if got := updated.Spec.Containers[0].Resources.Limits[injectedResource]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Fatalf("expected injected limit to be preserved, got %s", got.String())
	}
	state := mustGetInPlaceUpdateStateFromPod(t, updated)
	if !state.UpdateResources {
		t.Fatalf("expected UpdateResources=true after resize, got %+v", state)
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
	box := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if sub == "resize" {
				subCalls++
				return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, obj.GetName())
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			patchCalls++
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	ctrl := NewInPlaceUpdateControl(wrapped, nil)
	require.NoError(t, ctrl.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
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
	box := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if sub == "resize" {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, obj.GetName())
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			// The intent patch may succeed; the resource write is identified by spec
			// fields and does not depend on whether metadata is carried.
			data, err := patch.Data(obj)
			if err != nil {
				return err
			}
			if strings.Contains(string(data), `"spec"`) {
				return apierrors.NewMethodNotSupported(schema.GroupResource{Resource: "pods"}, "resize")
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	ctrl := NewInPlaceUpdateControl(wrapped, nil)
	require.NoError(t, ctrl.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
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
	box := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if sub == "resize" {
				return apierrors.NewInternalError(fmt.Errorf("etcd timeout"))
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	ctrl := NewInPlaceUpdateControl(wrapped, nil)
	require.NoError(t, ctrl.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))

	progressed, err := ctrl.Update(context.Background(), InPlaceUpdateOptions{
		Box:      box,
		Pod:      pod,
		Revision: "rev",
	})
	if err == nil {
		t.Fatalf("expected non-nil error from failing resize subresource")
	}
	// A failed resize still returns incomplete, but the resource intent
	// persisted earlier must be preserved.
	if progressed {
		t.Fatalf("expected progressed=false when resize fails before patch")
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
	box := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if sub != "resize" {
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
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
			return applyResizeSubresourcePatch(ctx, c, obj, patch)
		},
	})
	ctrl := NewInPlaceUpdateControl(wrapped, nil)
	require.NoError(t, ctrl.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))

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
	state := mustGetInPlaceUpdateStateFromPod(t, updated)
	if !state.UpdateResources {
		t.Fatalf("state.UpdateResources is false")
	}
}

func TestInPlaceUpdateControl_Update_ResizeConflictRetryNoLongerNeeded(t *testing.T) {
	for name, mode := range map[string]UpdateMode{"compatibility": CompatibilityMode, "target": TargetConvergenceMode} {
		t.Run(name, func(t *testing.T) {
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
			box := &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object,
					patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if sub != "resize" {
						return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
					}
					resizeAttempts++
					// Simulate that another actor has already applied the resize, so
					// recomputing resize containers from the latest pod returns nil and the
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
			require.NoError(t, ctrl.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))

			progressed, err := ctrl.Update(context.Background(), InPlaceUpdateOptions{
				Mode:     mode,
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

			updated := &corev1.Pod{}
			if err := ctrl.Get(context.Background(),
				types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, updated); err != nil {
				t.Fatalf("get updated pod: %v", err)
			}
			if updated.Labels[agentsv1alpha1.PodLabelTemplateHash] != "rev" {
				t.Fatalf("expected pod-template-hash=rev, got %q", updated.Labels[agentsv1alpha1.PodLabelTemplateHash])
			}
			state, err := GetPodInPlaceUpdateState(updated)
			if err != nil {
				t.Fatalf("get inplace update state failed: %v", err)
			}
			if mode == TargetConvergenceMode {
				require.NotNil(t, state)
				require.True(t, state.UpdateResources)
				completed, err := mode.IsInplaceUpdateCompleted(t.Context(), updated, state)
				require.NoError(t, err)
				require.False(t, completed, "spec 已写入，但 status 尚未确认资源生效")
				updated.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "c", Resources: updated.Spec.Containers[0].Resources.DeepCopy(),
				}}
				completed, err = mode.IsInplaceUpdateCompleted(t.Context(), updated, state)
				require.NoError(t, err)
				require.True(t, completed)
			} else {
				require.Nil(t, state, "兼容模式保留 Conflict 重算后无需 resize 的原有结果")
				completed, err := IsInplaceUpdateCompleted(t.Context(), updated, state)
				require.NoError(t, err)
				require.True(t, completed)
			}

			cpuReq := updated.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
			if cpuReq.MilliValue() != 750 {
				t.Fatalf("expected cpu request=750m after conflict handoff, got %dm", cpuReq.MilliValue())
			}
		})
	}
}

func TestInPlaceUpdateControl_Update_ResizeConflictGetFails(t *testing.T) {
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
	box := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
	getFailErr := fmt.Errorf("simulated get failure after conflict")
	wrapped := interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if sub != "resize" {
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			}
			// Always return conflict to trigger the retry path
			return apierrors.NewConflict(
				schema.GroupResource{Resource: "pods"}, obj.GetName(),
				fmt.Errorf("the object has been modified"),
			)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			// Allow the first Get (for the pod object itself before resize),
			// but fail on subsequent Gets during conflict retry
			if _, ok := obj.(*corev1.Pod); ok {
				// Check if this is a retry Get (pod already exists in store)
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				// Return error on re-Get during conflict handling
				return getFailErr
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	ctrl := NewInPlaceUpdateControl(wrapped, nil)
	require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))

	_, err := ctrl.Update(context.Background(), InPlaceUpdateOptions{
		Box:      box,
		Pod:      pod,
		Revision: "rev",
	})
	if err == nil {
		t.Fatal("expected error from Get during conflict retry, got nil")
	}
	if !strings.Contains(err.Error(), "simulated get failure after conflict") {
		t.Fatalf("unexpected error: %v", err)
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
	require.NoError(t, ctrl.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
	resizeContainers := []corev1.Container{{
		Name: "c",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
		},
	}}
	err := ctrl.patchPodResources(context.Background(), klog.NewKlogr(), pod, resizeContainers, CompatibilityMode)
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
	body, err := DefaultGeneratePatchBodyFunc(InPlaceUpdateOptions{
		Box: &agentsv1alpha1.Sandbox{
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
					agentsv1alpha1.PodLabelTemplateHash: "rev",
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
	require.NoError(t, err)
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
	box := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
	require.NoError(t, ctrl.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
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
	box := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "img:1"}},
					},
				},
			},
		},
	}

	called := 0
	custom := func(opts InPlaceUpdateOptions) (string, error) {
		called++
		return `{"metadata":{"annotations":{"custom":"yes"}}}`, nil
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
	custom := func(opts InPlaceUpdateOptions) (string, error) {
		customCalled = true
		return "", nil
	}
	ctrl2 := NewInPlaceUpdateControl(c, custom)
	body, err := ctrl2.generatePatchBody(InPlaceUpdateOptions{})
	require.NoError(t, err)
	if body != "" {
		t.Fatalf("expected empty body from custom func")
	}
	if !customCalled {
		t.Fatalf("expected custom patch func to be called via generatePatchBody dispatcher")
	}
}

func TestImageTargetCompletion(t *testing.T) {
	tests := []struct {
		name         string
		image        string
		target       string
		running      bool
		waiting      string
		wantDone     bool
		wantTerminal bool
	}{
		{name: "rollback may keep original image ID", image: "nginx:1", target: "nginx:1", running: true, wantDone: true},
		{name: "normalized target", image: "docker.io/library/nginx:latest", target: "nginx", running: true, wantDone: true},
		{name: "old running image", image: "nginx:1", target: "nginx:2", running: true},
		{name: "target not running", image: "nginx:2", target: "nginx:2"},
		{name: "pull backoff waits", image: "nginx:2", target: "nginx:2", waiting: "ImagePullBackOff"},
		{name: "old invalid image ignored", image: "INVALID", target: "nginx:2", waiting: "InvalidImageName"},
		{name: "current invalid image fails", image: "INVALID", target: "INVALID", waiting: "InvalidImageName", wantTerminal: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &InPlaceUpdateState{UpdateImages: true, LastContainerStatuses: map[string]InPlaceUpdateContainerStatus{
				"main": {ImageID: "same-id", TargetImage: tt.target},
			}}
			status := corev1.ContainerStatus{Name: "main", Image: tt.image, ImageID: "same-id"}
			if tt.running {
				status.State.Running = &corev1.ContainerStateRunning{}
			}
			if tt.waiting != "" {
				status.State.Waiting = &corev1.ContainerStateWaiting{Reason: tt.waiting}
			}
			pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{status}}}
			compatDone, compatErr := CompatibilityMode.IsInplaceUpdateCompleted(t.Context(), pod, state)
			require.NoError(t, compatErr)
			require.False(t, compatDone, "兼容模式仍要求 ImageID 发生变化")
			done, err := TargetConvergenceMode.IsInplaceUpdateCompleted(t.Context(), pod, state)
			require.Equal(t, tt.wantDone, done)
			if tt.wantTerminal {
				var imageErr *ImagePullFailedError
				require.ErrorAs(t, err, &imageErr)
			} else {
				require.NoError(t, err)
			}
		})
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
	containers := []corev1.Container{
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
	}
	patch := buildResourcePatch(containers)
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
	patchContainers, ok := spec["containers"].([]any)
	if !ok {
		t.Fatalf("expected containers slice, got %v", spec)
	}
	if len(patchContainers) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(patchContainers))
	}
	if _, exists := parsed["metadata"]; exists {
		t.Fatalf("resource patch should not include metadata")
	}
	if _, exists := spec["initContainers"]; exists {
		t.Fatalf("resource patch should not include initContainers")
	}
}

func TestDefaultBuildResizeContainers_NilTemplateAndNoChange(t *testing.T) {
	if got := DefaultBuildResizeContainers(InPlaceUpdateOptions{
		Box: &agentsv1alpha1.Sandbox{},
		Pod: &corev1.Pod{},
	}); got != nil {
		t.Fatalf("expected nil resize containers when template is nil, got %+v", got)
	}

	identical := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
	}
	resizeContainers := DefaultBuildResizeContainers(InPlaceUpdateOptions{
		Box: &agentsv1alpha1.Sandbox{
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
	if resizeContainers != nil {
		t.Fatalf("expected nil resize containers when no resource changes, got %+v", resizeContainers)
	}
}

func TestDefaultGeneratePatchBodyFunc_NoChange(t *testing.T) {
	got, err := DefaultGeneratePatchBodyFunc(InPlaceUpdateOptions{
		Box: &agentsv1alpha1.Sandbox{
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
					agentsv1alpha1.PodLabelTemplateHash: "r",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "c", Image: "img:1"}},
			},
		},
		Revision: "r",
	})
	require.NoError(t, err)
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
	box := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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

	for _, tt := range []struct {
		name        string
		mode        UpdateMode
		targetImage string
	}{
		{name: "compatibility", mode: CompatibilityMode},
		{name: "target", mode: TargetConvergenceMode, targetImage: "new"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body, err := DefaultGeneratePatchBodyFunc(InPlaceUpdateOptions{
				Box:      box,
				Pod:      pod,
				Revision: "rev-img",
				Mode:     tt.mode,
			})
			require.NoError(t, err)
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
			if labels[agentsv1alpha1.PodLabelTemplateHash] != "rev-img" {
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
			require.Equal(t, tt.targetImage, state.LastContainerStatuses["c"].TargetImage)
		})
	}
}

func TestDefaultBuildResizeContainers_IgnoresInjectedOrEquivalentResources(t *testing.T) {
	tests := []struct {
		name              string
		templateResources corev1.ResourceRequirements
		podResources      corev1.ResourceRequirements
	}{
		{
			name: "extra ephemeral storage does not trigger resource update",
			templateResources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("500m"),
				},
			},
			podResources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:              resource.MustParse("500m"),
					corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
				},
			},
		},
		{
			name: "equivalent cpu quantity does not trigger resource update",
			templateResources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1000m"),
				},
			},
			podResources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resizeContainers := DefaultBuildResizeContainers(InPlaceUpdateOptions{
				Box: &agentsv1alpha1.Sandbox{
					Spec: agentsv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:      "main",
											Resources: tt.templateResources,
										},
									},
								},
							},
						},
					},
				},
				Pod: &corev1.Pod{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:      "main",
								Resources: tt.podResources,
							},
						},
					},
				},
			})
			if resizeContainers != nil {
				t.Fatalf("expected no resize containers, got %+v", resizeContainers)
			}
		})
	}
}

func TestDefaultGeneratePatchBodyFunc_ImageOnlyWithInjectedResource(t *testing.T) {
	const revision = "rev-current"
	body, err := DefaultGeneratePatchBodyFunc(InPlaceUpdateOptions{
		Box: &agentsv1alpha1.Sandbox{
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name:  "main",
								Image: "img:2",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU: resource.MustParse("500m"),
									},
								},
							}},
						},
					},
				},
			},
		},
		Pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					agentsv1alpha1.PodLabelTemplateHash: revision,
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "main",
					Image: "img:1",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:              resource.MustParse("500m"),
							corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
						},
					},
				}},
			},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:    "main",
					ImageID: "img:1@sha256:old",
				}},
			},
		},
		Revision: revision,
	})
	require.NoError(t, err)
	if body == "" {
		t.Fatalf("expected non-empty patch body")
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	metadata, _ := decoded["metadata"].(map[string]any)
	annotations, _ := metadata["annotations"].(map[string]any)
	stateRaw, ok := annotations[PodAnnotationInPlaceUpdateStateKey].(string)
	if !ok || stateRaw == "" {
		t.Fatalf("expected inplace update state annotation, got %v", annotations)
	}
	state := &InPlaceUpdateState{}
	if err := json.Unmarshal([]byte(stateRaw), state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if !state.UpdateImages || state.UpdateResources {
		t.Fatalf("expected only UpdateImages=true, got %+v", state)
	}
}

func TestDefaultGeneratePatchBodyFunc_UsesExplicitResourceUpdateIntent(t *testing.T) {
	const revision = "rev-current"
	box := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "main",
							Image: "img:1",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("1"),
								},
							},
						}},
					},
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: revision,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "main",
				Image: "img:1",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("500m"),
					},
				},
			}},
		},
	}

	withoutIntent, err := DefaultGeneratePatchBodyFunc(InPlaceUpdateOptions{
		Box:      box,
		Pod:      pod,
		Revision: revision,
	})
	require.NoError(t, err)
	if withoutIntent != "" {
		t.Fatalf("expected resource differences alone not to generate a patch, got %s", withoutIntent)
	}

	postResizePod := pod.DeepCopy()
	postResizePod.Spec.Containers[0].Resources = box.Spec.Template.Spec.Containers[0].Resources
	withIntent, err := DefaultGeneratePatchBodyFunc(InPlaceUpdateOptions{
		Box:                    box,
		Pod:                    postResizePod,
		Revision:               revision,
		ResourceUpdateRequired: true,
	})
	require.NoError(t, err)
	if withIntent == "" {
		t.Fatalf("expected explicit resource update intent to generate a patch")
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(withIntent), &decoded); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	metadata, _ := decoded["metadata"].(map[string]any)
	annotations, _ := metadata["annotations"].(map[string]any)
	stateRaw, ok := annotations[PodAnnotationInPlaceUpdateStateKey].(string)
	if !ok || stateRaw == "" {
		t.Fatalf("expected inplace update state annotation, got %v", annotations)
	}
	state := &InPlaceUpdateState{}
	if err := json.Unmarshal([]byte(stateRaw), state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if state.UpdateImages || !state.UpdateResources {
		t.Fatalf("expected only UpdateResources=true, got %+v", state)
	}
	if _, exists := decoded["spec"]; exists {
		t.Fatalf("resource update intent patch should not contain spec, got %s", withIntent)
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

	tests := []struct {
		name       string
		pod        *corev1.Pod
		expectDone bool
		compatOnly bool
	}{
		{
			name: "no container statuses",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Resources: tmpl}},
				},
			},
			expectDone: false,
		},
		{
			name: "container status exists but resources is nil",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Resources: tmpl}},
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{{Name: "c"}},
				},
			},
			expectDone: false,
		},
		{
			name:       "resource decrease waits only in target mode",
			compatOnly: true,
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Resources: tmpl}}},
				Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
					Name: "c", Resources: &corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
						Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
					},
				}}},
			},
			expectDone: false,
		},
		{
			name: "requests do not match",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Resources: tmpl}},
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{{
						Name: "c",
						Resources: &corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
							Limits:   tmpl.Limits,
						},
					}},
				},
			},
			expectDone: false,
		},
		{
			name: "limits do not match",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Resources: tmpl}},
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{{
						Name: "c",
						Resources: &corev1.ResourceRequirements{
							Requests: tmpl.Requests,
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
						},
					}},
				},
			},
			expectDone: false,
		},
		{
			name: "spec and status resources match",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Resources: tmpl}},
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{{
						Name:      "c",
						Resources: &tmpl,
					}},
				},
			},
			expectDone: true,
		},
		{
			name: "spec has more resource types than status",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "c",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:              resource.MustParse("500m"),
								corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
							},
							Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
						},
					}},
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{{
						Name:      "c",
						Resources: &tmpl, // Only CPU, missing ephemeral-storage
					}},
				},
			},
			expectDone: false,
		},
		{
			name: "status has more resource types than spec",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Resources: tmpl}},
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{{
						Name: "c",
						Resources: &corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:              resource.MustParse("500m"),
								corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
							},
							Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
						},
					}},
				},
			},
			expectDone: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expectDone || tt.compatOnly, isPodResourceResizeCompleted(tt.pod))
			require.Equal(t, tt.expectDone, TargetConvergenceMode.isPodResourceResizeCompleted(tt.pod))
		})
	}
}

func TestIsInplaceUpdateCompleted(t *testing.T) {
	tests := []struct {
		name              string
		pod               *corev1.Pod
		expectedCompleted bool
		expectTerminalErr bool
		expectParseErr    bool
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
			expectParseErr:    true,
			expectedCompleted: true,
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
		{
			name: "image pull backoff on tracked container keeps waiting",
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
							ImageID: "image123", // Same as old image ID: round in flight
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason:  "ImagePullBackOff",
									Message: "Back-off pulling image",
								},
							},
						},
					},
				},
			},
			expectedCompleted: false,
			expectTerminalErr: false,
		},
		{
			name: "image pull failure on untracked container is not terminal",
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
							ImageID: "image123", // Same as old image ID: round in flight
						},
						{
							Name:    "sidecar", // Not tracked by the round
							ImageID: "image789",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason: "ErrImagePull",
								},
							},
						},
					},
				},
			},
			expectedCompleted: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, parseErr := GetPodInPlaceUpdateState(tt.pod)
			if tt.expectParseErr {
				require.Error(t, parseErr)
				return
			}
			require.NoError(t, parseErr)
			completed, terminalErr := IsInplaceUpdateCompleted(context.TODO(), tt.pod, state)
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
	const revision = "rev-resource-only"
	opts := InPlaceUpdateOptions{
		Box: &agentsv1alpha1.Sandbox{
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
				Labels: map[string]string{
					agentsv1alpha1.PodLabelTemplateHash: revision,
				},
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
		Revision: revision,
	}

	resizeContainers := DefaultBuildResizeContainers(opts)
	if len(resizeContainers) != 1 {
		t.Fatalf("expected one resize container, got %+v", resizeContainers)
	}
	got := resizeContainers[0].Resources.Requests[corev1.ResourceCPU]
	if got.MilliValue() != 1000 {
		t.Fatalf("expected cpu request=1000m, got=%dm", got.MilliValue())
	}

	postResizePod := opts.Pod.DeepCopy()
	postResizePod.Spec.Containers[0].Resources = resizeContainers[0].Resources
	opts.Pod = postResizePod
	opts.ResourceUpdateRequired = true

	patchBody, err := DefaultGeneratePatchBodyFunc(opts)
	require.NoError(t, err)
	if patchBody == "" {
		t.Fatalf("expected patch body for resource-only update")
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(patchBody), &decoded); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	if _, exists := decoded["spec"]; exists {
		t.Fatalf("resource-only metadata patch should not contain spec, got: %s", patchBody)
	}
	metadata, _ := decoded["metadata"].(map[string]any)
	annotations, _ := metadata["annotations"].(map[string]any)
	stateRaw, ok := annotations[PodAnnotationInPlaceUpdateStateKey].(string)
	if !ok || stateRaw == "" {
		t.Fatalf("expected inplace update state annotation, got %v", annotations)
	}
	state := &InPlaceUpdateState{}
	if err := json.Unmarshal([]byte(stateRaw), state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if state.UpdateImages || !state.UpdateResources {
		t.Fatalf("expected only UpdateResources=true, got %+v", state)
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

	completed, terminalErr := IsInplaceUpdateCompleted(context.Background(), pod, state)
	if completed {
		t.Fatalf("expected incomplete while PodResizeInProgress is true")
	}
	if terminalErr != nil {
		t.Fatalf("unexpected terminal error: %v", terminalErr)
	}

	pod.Status.Conditions = nil
	completed, terminalErr = IsInplaceUpdateCompleted(context.Background(), pod, state)
	if completed {
		t.Fatalf("expected incomplete when no resize signal and no applied resources")
	}
	if terminalErr != nil {
		t.Fatalf("unexpected terminal error: %v", terminalErr)
	}

	pod.Status.Resize = corev1.PodResizeStatusInProgress
	completed, terminalErr = IsInplaceUpdateCompleted(context.Background(), pod, state)
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
	completed, terminalErr = IsInplaceUpdateCompleted(context.Background(), pod, state)
	if completed {
		t.Fatalf("expected incomplete when only resize signals exist but resources not applied")
	}
	if terminalErr != nil {
		t.Fatalf("unexpected terminal error: %v", terminalErr)
	}

	// An Infeasible resize terminates this round.
	pod.Status.Resize = ""
	pod.Status.Conditions = []corev1.PodCondition{
		{
			Type: corev1.PodResizePending, Status: corev1.ConditionTrue,
			Reason: corev1.PodReasonInfeasible, Message: "insufficient cpu",
		},
	}
	completed, terminalErr = IsInplaceUpdateCompleted(context.Background(), pod, state)
	if completed {
		t.Fatalf("expected incomplete when resize is infeasible")
	}
	if terminalErr == nil {
		t.Fatalf("expected terminal error for infeasible resize")
	}
	if !strings.Contains(terminalErr.Error(), "infeasible") {
		t.Fatalf("expected error containing 'infeasible', got: %v", terminalErr)
	}

	// Deferred 在兼容模式中仍报错；仅目标收敛模式继续等待。
	pod.Status.Resize = ""
	pod.Status.Conditions = []corev1.PodCondition{
		{
			Type:    corev1.PodResizePending,
			Status:  corev1.ConditionTrue,
			Reason:  corev1.PodReasonDeferred,
			Message: "node resources temporarily insufficient",
		},
	}
	completed, terminalErr = IsInplaceUpdateCompleted(context.Background(), pod, state)
	if completed {
		t.Fatalf("expected incomplete when resize is deferred")
	}
	require.ErrorContains(t, terminalErr, "deferred")
	completed, terminalErr = TargetConvergenceMode.IsInplaceUpdateCompleted(t.Context(), pod, state)
	require.False(t, completed)
	require.NoError(t, terminalErr)

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
	completed, terminalErr = IsInplaceUpdateCompleted(context.Background(), pod, state)
	if !completed {
		t.Fatalf("expected completed when resources are applied to container status")
	}
	if terminalErr != nil {
		t.Fatalf("unexpected terminal error: %v", terminalErr)
	}
	// 同一份超配 status：兼容模式已满足目标，SUO 必须等待降配生效。
	pod.Status.ContainerStatuses[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("1500m")
	completed, terminalErr = IsInplaceUpdateCompleted(t.Context(), pod, state)
	require.NoError(t, terminalErr)
	require.True(t, completed)
	completed, terminalErr = TargetConvergenceMode.IsInplaceUpdateCompleted(t.Context(), pod, state)
	require.NoError(t, terminalErr)
	require.False(t, completed)
	box := &agentsv1alpha1.Sandbox{Spec: agentsv1alpha1.SandboxSpec{
		EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
			Template: &corev1.PodTemplateSpec{Spec: *pod.Spec.DeepCopy()},
		},
	}}
	pod.Spec.Containers[0].Resources = *pod.Status.ContainerStatuses[0].Resources.DeepCopy()
	require.Empty(t, DefaultBuildResizeContainers(InPlaceUpdateOptions{Box: box, Pod: pod}))
	require.Len(t, DefaultBuildResizeContainers(InPlaceUpdateOptions{Box: box, Pod: pod, Mode: TargetConvergenceMode}), 1)
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
			wantErr: false,
		},
		{
			name: "deprecated Resize field is Deferred",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Resize: corev1.PodResizeStatusDeferred,
				},
			},
			wantErr: false,
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
			err := TargetConvergenceMode.checkPodResizeInfeasible(tt.pod)
			if tt.wantErr {
				var resizeErr *ResizeInfeasibleError
				require.ErrorAs(t, err, &resizeErr)
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errSubstr)
				}
				if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.errSubstr)) {
					t.Fatalf("expected error containing %q, got: %v", tt.errSubstr, err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			compatErr := checkPodResizeInfeasible(tt.pod)
			if tt.wantErr || strings.Contains(tt.name, "Deferred") {
				var resizeErr *ResizeInfeasibleError
				require.ErrorAs(t, compatErr, &resizeErr)
				require.ErrorAs(t, fmt.Errorf("observation failed: %w", compatErr), &resizeErr)
				if tt.wantErr {
					require.EqualError(t, compatErr, err.Error())
				} else {
					require.Contains(t, compatErr.Error(), "deferred")
				}
			} else {
				require.NoError(t, compatErr)
			}
		})
	}
}

func Test_checkPodImagePullFailed(t *testing.T) {
	state := &InPlaceUpdateState{
		LastContainerStatuses: map[string]InPlaceUpdateContainerStatus{
			"main": {ImageID: "docker://sha256:old"},
		},
	}
	tests := []struct {
		name      string
		pod       *corev1.Pod
		wantErr   bool
		errSubstr string
	}{
		{
			name: "no waiting state - no error",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{Name: "main"},
					},
				},
			},
		},
		{
			name: "tracked container InvalidImageName",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name: "main", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "InvalidImageName"}},
			}}}},
			wantErr: true, errSubstr: "InvalidImageName",
		},
		{
			name: "tracked container ErrImageNeverPull",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name: "main", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImageNeverPull"}},
			}}}},
			wantErr: true, errSubstr: "ErrImageNeverPull",
		},
		{
			name: "tracked container ErrImagePull",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "main",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason:  "ErrImagePull",
									Message: "manifest unknown",
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "tracked container ImagePullBackOff",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "main",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason: "ImagePullBackOff",
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "tracked container waiting for another reason - no error",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "main",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason: "ContainerCreating",
								},
							},
						},
					},
				},
			},
		},
		{
			name: "untracked container pull failure - no error",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "sidecar",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason: "ImagePullBackOff",
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkPodImagePullFailed(tt.pod, state)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errSubstr)
				}
				var pullErr *ImagePullFailedError
				if !errors.As(err, &pullErr) {
					t.Fatalf("expected *ImagePullFailedError, got %T: %v", err, err)
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
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
		Box: &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-sandbox",
				Namespace: "default",
			},
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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

	patchBody, err := DefaultGeneratePatchBodyFunc(opts)
	require.NoError(t, err)
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

// TestDefaultGeneratePatchBodyFunc_TemplateAnnotations verifies that annotations
// declared on the sandbox template are propagated to the pod, mirroring the
// existing template-label propagation behavior.
func TestDefaultGeneratePatchBodyFunc_TemplateAnnotations(t *testing.T) {
	tests := []struct {
		name                string
		templateAnnotations map[string]string
		podAnnotations      map[string]string
		expectPatched       map[string]string
		expectNotPatched    []string
	}{
		{
			name:                "new template annotation is patched to pod",
			templateAnnotations: map[string]string{"ansm.alibabacloud.com/dns-zone": "zone-a"},
			podAnnotations:      nil,
			expectPatched:       map[string]string{"ansm.alibabacloud.com/dns-zone": "zone-a"},
		},
		{
			name:                "changed template annotation overrides pod value",
			templateAnnotations: map[string]string{"ansm.alibabacloud.com/dns-zone": "zone-b"},
			podAnnotations:      map[string]string{"ansm.alibabacloud.com/dns-zone": "zone-a"},
			expectPatched:       map[string]string{"ansm.alibabacloud.com/dns-zone": "zone-b"},
		},
		{
			name:                "annotation already in sync is not patched again",
			templateAnnotations: map[string]string{"ansm.alibabacloud.com/dns-zone": "zone-a"},
			podAnnotations:      map[string]string{"ansm.alibabacloud.com/dns-zone": "zone-a"},
			expectNotPatched:    []string{"ansm.alibabacloud.com/dns-zone"},
		},
		{
			name: "multiple template annotations propagate together",
			templateAnnotations: map[string]string{
				"ansm.alibabacloud.com/dns-zone":           "zone-a",
				"ansm.alibabacloud.com/networkservicerule": "rule-1",
			},
			podAnnotations: nil,
			expectPatched: map[string]string{
				"ansm.alibabacloud.com/dns-zone":           "zone-a",
				"ansm.alibabacloud.com/networkservicerule": "rule-1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			box := &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Annotations: tt.templateAnnotations},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "c", Image: "img:1"}},
							},
						},
					},
				},
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "p",
					Namespace:   "default",
					Annotations: tt.podAnnotations,
					// Same revision so no image/resource/hash change is produced;
					// only annotation propagation should drive the patch.
					Labels: map[string]string{agentsv1alpha1.PodLabelTemplateHash: "rev-annot"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "img:1"}},
				},
			}

			body, err := DefaultGeneratePatchBodyFunc(InPlaceUpdateOptions{
				Box:      box,
				Pod:      pod,
				Revision: "rev-annot",
			})
			require.NoError(t, err)

			annotations := map[string]any{}
			if body != "" {
				var decoded map[string]any
				if err := json.Unmarshal([]byte(body), &decoded); err != nil {
					t.Fatalf("unmarshal patch: %v", err)
				}
				if metadata, ok := decoded["metadata"].(map[string]any); ok {
					if a, ok := metadata["annotations"].(map[string]any); ok {
						annotations = a
					}
				}
			}

			for k, v := range tt.expectPatched {
				if annotations[k] != v {
					t.Errorf("expected annotation %s=%s, got %v", k, v, annotations[k])
				}
			}
			for _, k := range tt.expectNotPatched {
				if _, exists := annotations[k]; exists {
					t.Errorf("annotation %s already in sync should not be patched again", k)
				}
			}
		})
	}
}

func TestCheckResizeQoSChange(t *testing.T) {
	tests := []struct {
		name          string
		box           *agentsv1alpha1.Sandbox
		pod           *corev1.Pod
		wantOrig      corev1.PodQOSClass
		wantUpdated   corev1.PodQOSClass
		wantChanged   bool
		compatUpdated corev1.PodQOSClass
	}{
		{
			name: "no QoS change - Burstable stays Burstable",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
			name: "image-only update preserves injected Guaranteed resources",
			box: &agentsv1alpha1.Sandbox{Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "img:2"}}}},
				},
			}},
			pod: &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "main", Image: "img:1", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
				},
			}}}},
			wantOrig: corev1.PodQOSGuaranteed, wantUpdated: corev1.PodQOSGuaranteed,
			compatUpdated: corev1.PodQOSBestEffort,
		},
		{
			name: "nil template - no change",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{},
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
			orig, updated, changed := TargetConvergenceMode.CheckResizeQoSChange(tt.box, tt.pod)
			compatOrig, compatUpdated, compatChanged := CheckResizeQoSChange(tt.box, tt.pod)
			wantCompat := tt.wantUpdated
			if tt.compatUpdated != "" {
				wantCompat = tt.compatUpdated
			}
			require.Equal(t, tt.wantOrig, compatOrig)
			require.Equal(t, wantCompat, compatUpdated)
			require.Equal(t, compatOrig != wantCompat, compatChanged)
			if changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tt.wantChanged)
			}
			if tt.box.Spec.Template != nil {
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

func TestCheckMemoryDownscale(t *testing.T) {
	boxWithMemory := func(req, lim string) *agentsv1alpha1.Sandbox {
		return &agentsv1alpha1.Sandbox{
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name: "main",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(req)},
									Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(lim)},
								},
							}},
						},
					},
				},
			},
		}
	}
	podWithMemory := func(req, lim string) *corev1.Pod {
		return &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(req)},
						Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(lim)},
					},
				}},
			},
		}
	}

	tests := []struct {
		name        string
		box         *agentsv1alpha1.Sandbox
		pod         *corev1.Pod
		expectError string
	}{
		{
			name:        "lower memory request rejected",
			box:         boxWithMemory("128Mi", "512Mi"),
			pod:         podWithMemory("256Mi", "512Mi"),
			expectError: "target memory request 128Mi must not be lower than the current value 256Mi",
		},
		{
			name:        "lower memory limit rejected",
			box:         boxWithMemory("256Mi", "256Mi"),
			pod:         podWithMemory("256Mi", "512Mi"),
			expectError: "target memory limit 256Mi must not be lower than the current value 512Mi",
		},
		{
			name: "upscale accepted",
			box:  boxWithMemory("512Mi", "1Gi"),
			pod:  podWithMemory("256Mi", "512Mi"),
		},
		{
			name: "equal accepted",
			box:  boxWithMemory("256Mi", "512Mi"),
			pod:  podWithMemory("256Mi", "512Mi"),
		},
		{
			name: "container not in template ignored",
			box:  boxWithMemory("128Mi", "512Mi"),
			pod: &corev1.Pod{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "other",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
					},
				}},
			}},
		},
		{
			name: "nil template accepted",
			box:  &agentsv1alpha1.Sandbox{},
			pod:  podWithMemory("256Mi", "512Mi"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckMemoryDownscale(tt.box, tt.pod)
			if tt.expectError != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.expectError)
				}
				if !strings.Contains(err.Error(), tt.expectError) {
					t.Fatalf("expected error containing %q, got %v", tt.expectError, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
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

func TestDefaultBuildResizeContainers_MinimalFields(t *testing.T) {
	tests := []struct {
		name                   string
		box                    *agentsv1alpha1.Sandbox
		pod                    *corev1.Pod
		expectNil              bool
		expectContainerCount   int
		verifyContainerMinimal bool
	}{
		{
			name: "resize containers have no Image field",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "main",
										Image: "nginx:2.0",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU: resource.MustParse("500m"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", ResourceVersion: "100"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "nginx:1.0",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("100m"),
								},
							},
						},
					},
				},
			},
			expectNil:              false,
			expectContainerCount:   1,
			verifyContainerMinimal: true,
		},
		{
			name: "initContainers are ignored by resize containers",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "main",
										Image: "nginx:2.0",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU: resource.MustParse("500m"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", ResourceVersion: "100"},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{
							Name:  "init-db",
							Image: "init-img:1.0",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("50m"),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "data", MountPath: "/data"},
							},
						},
						{
							Name:  "init-sidecar",
							Image: "sidecar-img:1.0",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
							},
							Command: []string{"sh", "-c", "echo hello"},
						},
					},
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "nginx:1.0",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("100m"),
								},
							},
						},
					},
				},
			},
			expectNil:              false,
			expectContainerCount:   1,
			verifyContainerMinimal: true,
		},
		{
			name: "multiple containers all minimal",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "web",
										Image: "web:2.0",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
										},
									},
									{
										Name:  "worker",
										Image: "worker:2.0",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("400m")},
										},
									},
								},
							},
						},
					},
				},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", ResourceVersion: "200"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "web",
							Image: "web:1.0",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
							},
							VolumeMounts: []corev1.VolumeMount{{Name: "vol", MountPath: "/app"}},
						},
						{
							Name:  "worker",
							Image: "worker:1.0",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
							},
							Command: []string{"/bin/worker"},
						},
					},
				},
			},
			expectNil:              false,
			expectContainerCount:   2,
			verifyContainerMinimal: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resizeContainers := DefaultBuildResizeContainers(InPlaceUpdateOptions{
				Box: tt.box,
				Pod: tt.pod,
			})
			if tt.expectNil {
				if resizeContainers != nil {
					t.Fatalf("expected nil resize containers, got %+v", resizeContainers)
				}
				return
			}
			if len(resizeContainers) == 0 {
				t.Fatalf("expected non-empty resize containers")
			}
			if len(resizeContainers) != tt.expectContainerCount {
				t.Fatalf("expected %d containers, got %d", tt.expectContainerCount, len(resizeContainers))
			}

			if tt.verifyContainerMinimal {
				for i, c := range resizeContainers {
					if c.Image != "" {
						t.Errorf("container[%d] %q should have empty Image, got %q", i, c.Name, c.Image)
					}
					if c.Name == "" {
						t.Errorf("container[%d] should have a Name", i)
					}
					if len(c.VolumeMounts) > 0 {
						t.Errorf("container[%d] %q should have no VolumeMounts", i, c.Name)
					}
					if len(c.Command) > 0 {
						t.Errorf("container[%d] %q should have no Command", i, c.Name)
					}
				}
			}

		})
	}
}

func TestInPlaceUpdateControl_Update_ResizeBeforePatch(t *testing.T) {
	for _, tt := range []struct {
		name  string
		mode  UpdateMode
		order []string
	}{
		{name: "compatibility", mode: CompatibilityMode, order: []string{"resize", "patch"}},
		{name: "target", mode: TargetConvergenceMode, order: []string{"patch", "resize", "patch"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scheme := buildTestScheme(t)
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p-order", Namespace: "default"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "c",
						Image: "img:1",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
						},
					}},
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{Name: "c", ImageID: "img:1@sha256:abc"},
					},
				},
			}
			box := &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name:  "c",
									Image: "img:2",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
									},
								}},
							},
						},
					},
				},
			}

			// Record the order of intent persistence, resize, image, and metadata
			// finalization.
			var callOrder []string
			base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
			wrapped := interceptor.NewClient(base, interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object,
					patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if sub == "resize" {
						callOrder = append(callOrder, "resize")
						// Simulate a successful resize by applying resources
						return applyResizeSubresourcePatch(ctx, c, obj, patch)
					}
					return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
					opts ...client.PatchOption) error {
					callOrder = append(callOrder, "patch")
					return c.Patch(ctx, obj, patch, opts...)
				},
			})

			ctrl := NewInPlaceUpdateControl(wrapped, nil)
			require.NoError(t, ctrl.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
			progressed, err := ctrl.Update(context.Background(), InPlaceUpdateOptions{
				Box:      box,
				Pod:      pod,
				Revision: "rev-order",
				Mode:     tt.mode,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !progressed {
				t.Fatalf("expected progressed=true")
			}

			require.Equal(t, tt.order, callOrder)
		})
	}
}

func TestResourceListContains(t *testing.T) {
	tests := []struct {
		name    string
		actual  corev1.ResourceList
		desired corev1.ResourceList
		expect  bool
	}{
		{
			name:    "both nil",
			actual:  nil,
			desired: nil,
			expect:  true,
		},
		{
			name: "desired nil actual not nil",
			actual: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
			desired: nil,
			expect:  true,
		},
		{
			name:   "desired not nil actual nil",
			actual: nil,
			desired: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1"),
			},
			expect: false,
		},
		{
			name: "exact match",
			actual: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
			desired: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
			expect: true,
		},
		{
			name: "actual has extra resources",
			actual: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("1"),
				corev1.ResourceMemory:           resource.MustParse("1Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("30Gi"),
			},
			desired: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
			expect: true,
		},
		{
			name: "different unit same value cpu",
			actual: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1"),
			},
			desired: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1000m"),
			},
			expect: true,
		},
		{
			name: "different unit same value memory",
			actual: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
			desired: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1024Mi"),
			},
			expect: true,
		},
		{
			name: "different value",
			actual: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1"),
			},
			desired: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("2"),
			},
			expect: false,
		},
		{
			name: "desired has resource actual missing",
			actual: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
			desired: corev1.ResourceList{
				corev1.ResourceCPU:                    resource.MustParse("1"),
				corev1.ResourceMemory:                 resource.MustParse("1Gi"),
				corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
			},
			expect: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isResourceListCovered(tt.actual, tt.desired)
			if got != tt.expect {
				t.Errorf("isResourceListCovered() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestIsResourceSatisfied(t *testing.T) {
	tests := []struct {
		name    string
		desired corev1.ResourceRequirements
		actual  corev1.ResourceRequirements
		expect  bool
	}{
		{
			name: "identical resources",
			desired: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
			},
			actual: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
			},
			expect: true,
		},
		{
			name: "pod has extra ephemeral-storage in requests",
			desired: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
			actual: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:              resource.MustParse("1"),
					corev1.ResourceMemory:           resource.MustParse("1Gi"),
					corev1.ResourceEphemeralStorage: resource.MustParse("30Gi"),
				},
			},
			expect: true,
		},
		{
			name: "different cpu unit same value",
			desired: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1000m"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1000m"),
				},
			},
			actual: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				},
			},
			expect: true,
		},
		{
			name: "cpu actually different",
			desired: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("2"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("2"),
				},
			},
			actual: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				},
			},
			expect: false,
		},
		{
			name: "limits differ",
			desired: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				},
			},
			actual: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				},
			},
			expect: false,
		},
		{
			name:    "both empty",
			desired: corev1.ResourceRequirements{},
			actual:  corev1.ResourceRequirements{},
			expect:  true,
		},
		{
			name: "desired has limits actual missing limits",
			desired: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				},
			},
			actual: corev1.ResourceRequirements{},
			expect: false,
		},
		{
			name: "pod has extra limits sandbox fewer",
			desired: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
			actual: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:                    resource.MustParse("1"),
					corev1.ResourceMemory:                 resource.MustParse("1Gi"),
					corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
				},
			},
			expect: true,
		},
		{
			name: "actual requests exceed desired",
			desired: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				},
			},
			actual: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
				},
			},
			expect: true,
		},
		{
			name: "actual limits exceed desired but requests equal",
			desired: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("2"),
				},
			},
			actual: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("4"),
				},
			},
			expect: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsResourceSatisfied(tt.desired, tt.actual)
			if got != tt.expect {
				t.Errorf("IsResourceSatisfied() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestResourcesExactlyEqual(t *testing.T) {
	tests := []struct {
		name    string
		desired corev1.ResourceRequirements
		actual  corev1.ResourceRequirements
		expect  bool
	}{
		{
			name: "identical resources",
			desired: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
			},
			actual: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
			},
			expect: true,
		},
		{
			name: "different cpu unit same value",
			desired: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1000m"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1000m"),
				},
			},
			actual: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				},
			},
			expect: true,
		},
		{
			name: "actual requests exceed desired",
			desired: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				},
			},
			actual: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
				},
			},
			expect: false,
		},
		{
			name: "actual limits exceed desired but requests equal",
			desired: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("2"),
				},
			},
			actual: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("4"),
				},
			},
			expect: false,
		},
		{
			name: "pod has extra ephemeral-storage in requests",
			desired: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
			actual: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:              resource.MustParse("1"),
					corev1.ResourceMemory:           resource.MustParse("1Gi"),
					corev1.ResourceEphemeralStorage: resource.MustParse("30Gi"),
				},
			},
			expect: true,
		},
		{
			name: "actual cpu less than desired",
			desired: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("2"),
				},
			},
			actual: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				},
			},
			expect: false,
		},
		{
			name:    "both empty",
			desired: corev1.ResourceRequirements{},
			actual:  corev1.ResourceRequirements{},
			expect:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResourcesExactlyEqual(tt.desired, tt.actual)
			if got != tt.expect {
				t.Errorf("ResourcesExactlyEqual() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestDefaultBuildResizeContainers_NoResizeWhenExtraOrUnitDiff(t *testing.T) {
	tests := []struct {
		name string
		box  *agentsv1alpha1.Sandbox
		pod  *corev1.Pod
	}{
		{
			name: "should NOT resize when pod has extra ephemeral-storage",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "main",
										Image: "busybox",
										Resources: corev1.ResourceRequirements{
											Limits: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("1"),
												corev1.ResourceMemory: resource.MustParse("1Gi"),
											},
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("1"),
												corev1.ResourceMemory: resource.MustParse("1Gi"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "default", ResourceVersion: "1"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "busybox",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:              resource.MustParse("1"),
									corev1.ResourceMemory:           resource.MustParse("1Gi"),
									corev1.ResourceEphemeralStorage: resource.MustParse("30Gi"),
								},
							},
						},
					},
				},
			},
		},
		{
			name: "should NOT resize when units differ but values equal",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "main",
										Image: "busybox",
										Resources: corev1.ResourceRequirements{
											Limits: corev1.ResourceList{
												corev1.ResourceCPU: resource.MustParse("1000m"),
											},
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
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Namespace: "default", ResourceVersion: "1"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "busybox",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("1"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("1"),
								},
							},
						},
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DefaultBuildResizeContainers(InPlaceUpdateOptions{
				Box: tt.box,
				Pod: tt.pod,
			})
			if result != nil {
				t.Errorf("expected nil (no resize needed), but got resize containers")
			}
		})
	}
}

func TestDefaultBuildResizeContainers_OnlyIncludesDesiredResources(t *testing.T) {
	injectedResource := corev1.ResourceName("test-resource")
	tests := []struct {
		name string
		box  *agentsv1alpha1.Sandbox
		pod  *corev1.Pod
	}{
		{
			name: "preserve ephemeral-storage while updating cpu/memory",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name: "main",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("1"),
												corev1.ResourceMemory: resource.MustParse("2Gi"),
											},
											Limits: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("1"),
												corev1.ResourceMemory: resource.MustParse("2Gi"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "main",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:              resource.MustParse("500m"),
									corev1.ResourceMemory:           resource.MustParse("2Gi"),
									corev1.ResourceEphemeralStorage: resource.MustParse("30Gi"),
									injectedResource:                resource.MustParse("1"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("2Gi"),
									injectedResource:      resource.MustParse("1"),
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DefaultBuildResizeContainers(InPlaceUpdateOptions{
				Box: tt.box,
				Pod: tt.pod,
			})
			if len(result) == 0 {
				t.Fatalf("expected non-zero resize containers, but got zero")
			}
			c := result[0]

			if _, ok := c.Resources.Requests[corev1.ResourceEphemeralStorage]; ok {
				t.Errorf("expected resize containers to omit ephemeral-storage request")
			}
			if _, ok := c.Resources.Requests[injectedResource]; ok {
				t.Errorf("expected resize containers to omit injected request")
			}
			if _, ok := c.Resources.Limits[injectedResource]; ok {
				t.Errorf("expected resize containers to omit injected limit")
			}

			gotReqCPU, ok := c.Resources.Requests[corev1.ResourceCPU]
			if !ok {
				t.Errorf("expected cpu in Requests, but it was missing")
			} else if gotReqCPU.Cmp(resource.MustParse("1")) != 0 {
				t.Errorf("Requests cpu = %s, want 1", gotReqCPU.String())
			}

			gotLimCPU, ok := c.Resources.Limits[corev1.ResourceCPU]
			if !ok {
				t.Errorf("expected cpu in Limits, but it was missing")
			} else if gotLimCPU.Cmp(resource.MustParse("1")) != 0 {
				t.Errorf("Limits cpu = %s, want 1", gotLimCPU.String())
			}

			gotLimMem, ok := c.Resources.Limits[corev1.ResourceMemory]
			if !ok {
				t.Errorf("expected memory in Limits, but it was missing")
			} else if gotLimMem.Cmp(resource.MustParse("2Gi")) != 0 {
				t.Errorf("Limits memory = %s, want 2Gi", gotLimMem.String())
			}
		})
	}
}
