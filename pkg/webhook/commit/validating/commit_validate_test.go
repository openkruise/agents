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

package validating

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func newTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	return scheme
}

func TestValidateCommit(t *testing.T) {
	runningPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "workspace"},
				{Name: "sidecar"},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	pendingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "workspace"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	now := metav1.Now()
	deletingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "deleting-pod", Namespace: "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{"test"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "workspace"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	tests := []struct {
		name      string
		commit    *agentsv1alpha1.Commit
		objects   []runtime.Object
		wantErrs  int
		wantField string // check one of the error fields if wantErrs > 0
	}{
		{
			name: "valid commit",
			commit: &agentsv1alpha1.Commit{
				ObjectMeta: metav1.ObjectMeta{Name: "ok", Namespace: "default"},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:       "my-pod",
					ContainerName: "workspace",
					Image:         "registry.example.com/team/env:v1",
				},
			},
			objects:  []runtime.Object{runningPod},
			wantErrs: 0,
		},
		{
			name: "invalid image - no tag",
			commit: &agentsv1alpha1.Commit{
				ObjectMeta: metav1.ObjectMeta{Name: "no-tag", Namespace: "default"},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:       "my-pod",
					ContainerName: "workspace",
					Image:         "registry.example.com/team/env",
				},
			},
			objects:   []runtime.Object{runningPod},
			wantErrs:  1,
			wantField: "spec.image",
		},
		{
			name: "invalid image - malformed",
			commit: &agentsv1alpha1.Commit{
				ObjectMeta: metav1.ObjectMeta{Name: "bad-img", Namespace: "default"},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:       "my-pod",
					ContainerName: "workspace",
					Image:         "INVALID:::image",
				},
			},
			objects:   []runtime.Object{runningPod},
			wantErrs:  1,
			wantField: "spec.image",
		},
		{
			name: "pod not found",
			commit: &agentsv1alpha1.Commit{
				ObjectMeta: metav1.ObjectMeta{Name: "no-pod", Namespace: "default"},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:       "nonexistent",
					ContainerName: "workspace",
					Image:         "registry.example.com/env:v1",
				},
			},
			objects:   []runtime.Object{},
			wantErrs:  1,
			wantField: "spec.podName",
		},
		{
			name: "pod not running",
			commit: &agentsv1alpha1.Commit{
				ObjectMeta: metav1.ObjectMeta{Name: "pending", Namespace: "default"},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:       "pending-pod",
					ContainerName: "workspace",
					Image:         "registry.example.com/env:v1",
				},
			},
			objects:   []runtime.Object{pendingPod},
			wantErrs:  1,
			wantField: "spec.podName",
		},
		{
			name: "pod being deleted",
			commit: &agentsv1alpha1.Commit{
				ObjectMeta: metav1.ObjectMeta{Name: "deleting", Namespace: "default"},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:       "deleting-pod",
					ContainerName: "workspace",
					Image:         "registry.example.com/env:v1",
				},
			},
			objects:   []runtime.Object{deletingPod},
			wantErrs:  1,
			wantField: "spec.podName",
		},
		{
			name: "container not found",
			commit: &agentsv1alpha1.Commit{
				ObjectMeta: metav1.ObjectMeta{Name: "no-container", Namespace: "default"},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:       "my-pod",
					ContainerName: "nonexistent",
					Image:         "registry.example.com/env:v1",
				},
			},
			objects:   []runtime.Object{runningPod},
			wantErrs:  1,
			wantField: "spec.containerName",
		},
		{
			name: "negative TTL",
			commit: &agentsv1alpha1.Commit{
				ObjectMeta: metav1.ObjectMeta{Name: "neg-ttl", Namespace: "default"},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:       "my-pod",
					ContainerName: "workspace",
					Image:         "registry.example.com/env:v1",
					Ttl:           &metav1.Duration{Duration: -1},
				},
			},
			objects:   []runtime.Object{runningPod},
			wantErrs:  1,
			wantField: "spec.ttl",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newTestScheme()
			builder := fake.NewClientBuilder().WithScheme(scheme)
			for _, obj := range tt.objects {
				builder = builder.WithRuntimeObjects(obj)
			}
			fc := builder.Build()
			h := &CommitValidatingHandler{Client: fc}

			ctx := t.Context()
			errs := h.validateCommit(ctx, tt.commit, field.NewPath("spec"))

			if len(errs) != tt.wantErrs {
				t.Errorf("got %d errors, want %d: %v", len(errs), tt.wantErrs, errs)
			}
			if tt.wantErrs > 0 && tt.wantField != "" {
				found := false
				for _, e := range errs {
					if e.Field == tt.wantField {
						found = true
					}
				}
				if !found {
					t.Errorf("expected error on field %s, got fields: %v", tt.wantField, errs)
				}
			}
		})
	}
}

func TestPath(t *testing.T) {
	h := &CommitValidatingHandler{}
	if got := h.Path(); got != "/validate-commit" {
		t.Errorf("expected /validate-commit, got %s", got)
	}
}

func TestEnabled(t *testing.T) {
	// Without a discovery client set up, DiscoverGVK will return false
	h := &CommitValidatingHandler{}
	// Just verify it doesn't panic and returns a bool
	_ = h.Enabled()
}

func TestHandle(t *testing.T) {
	scheme := newTestScheme()

	runningPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "workspace"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	tests := []struct {
		name       string
		rawObject  []byte
		objects    []runtime.Object
		expectCode int32
	}{
		{
			name:       "decode error - invalid JSON",
			rawObject:  []byte(`{not valid json`),
			objects:    nil,
			expectCode: http.StatusBadRequest,
		},
		{
			name: "valid commit - allowed",
			rawObject: mustMarshal(&agentsv1alpha1.Commit{
				TypeMeta:   metav1.TypeMeta{APIVersion: agentsv1alpha1.SchemeGroupVersion.String(), Kind: "Commit"},
				ObjectMeta: metav1.ObjectMeta{Name: "ok", Namespace: "default"},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:       "my-pod",
					ContainerName: "workspace",
					Image:         "registry.example.com/env:v1",
				},
			}),
			objects:    []runtime.Object{runningPod},
			expectCode: 0, // 0 means Allowed (no error code)
		},
		{
			name: "invalid commit - missing pod",
			rawObject: mustMarshal(&agentsv1alpha1.Commit{
				TypeMeta:   metav1.TypeMeta{APIVersion: agentsv1alpha1.SchemeGroupVersion.String(), Kind: "Commit"},
				ObjectMeta: metav1.ObjectMeta{Name: "no-pod", Namespace: "default"},
				Spec: agentsv1alpha1.CommitSpec{
					PodName:       "nonexistent",
					ContainerName: "workspace",
					Image:         "registry.example.com/env:v1",
				},
			}),
			objects:    nil,
			expectCode: http.StatusUnprocessableEntity,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme)
			for _, obj := range tt.objects {
				builder = builder.WithRuntimeObjects(obj)
			}
			fc := builder.Build()

			decoder := admission.NewDecoder(scheme)
			h := &CommitValidatingHandler{Client: fc, Decoder: decoder}

			req := admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					Object: runtime.RawExtension{Raw: tt.rawObject},
				},
			}

			resp := h.Handle(context.TODO(), req)

			if tt.expectCode == 0 {
				// Expect allowed
				if !resp.Allowed {
					t.Errorf("expected allowed, got denied: %v", resp.Result)
				}
			} else {
				// Expect denied with specific code
				if resp.Allowed {
					t.Error("expected denied, got allowed")
				}
				if resp.Result == nil {
					t.Fatal("expected Result to be set")
				}
				if resp.Result.Code != tt.expectCode {
					t.Errorf("expected code %d, got %d", tt.expectCode, resp.Result.Code)
				}
			}
		})
	}
}

func mustMarshal(obj interface{}) []byte {
	data, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	return data
}
