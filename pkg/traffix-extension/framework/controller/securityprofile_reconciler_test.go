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

package controller

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/traffix-extension/framework/configstore"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return s
}

// TestReconcile_CreateOrUpdate adds a profile to the fake API server and
// verifies the reconciler upserts it into the in-memory store.
func TestReconcile_CreateOrUpdate(t *testing.T) {
	prof := &v1alpha1.SecurityProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(prof).Build()
	store := configstore.NewStore()

	r := &SecurityProfileReconciler{Reader: c, Store: store}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "p", Namespace: "ns"},
	}); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if got, ok := store.ProfileGet(types.NamespacedName{Name: "p", Namespace: "ns"}); !ok || got.Profile.Name != "p" {
		t.Errorf("expected profile in store, got %v ok=%v", got, ok)
	}
}

// TestReconcile_NotFound_DeletesFromStore removes the profile from the
// fake client and verifies the reconciler deletes it from the store.
func TestReconcile_NotFound_DeletesFromStore(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	store := configstore.NewStore()
	store.ProfileSet(&v1alpha1.SecurityProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "stale", Namespace: "ns"},
	})

	r := &SecurityProfileReconciler{Reader: c, Store: store}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "stale", Namespace: "ns"},
	}); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if _, ok := store.ProfileGet(types.NamespacedName{Name: "stale", Namespace: "ns"}); ok {
		t.Errorf("expected profile to be deleted from store")
	}
}

// fakeErrReader makes Get return a non-NotFound error to exercise the
// "transient error" branch.
type fakeErrReader struct{ client.Reader }

func (fakeErrReader) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return errors.New("boom")
}

func (fakeErrReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return nil
}

func (fakeErrReader) RESTMapper() meta.RESTMapper { return nil }

func (fakeErrReader) Scheme() *runtime.Scheme { return runtime.NewScheme() }

func (fakeErrReader) GroupVersionKindFor(_ runtime.Object) (schema.GroupVersionKind, error) {
	return schema.GroupVersionKind{}, nil
}

func (fakeErrReader) IsObjectNamespaced(_ runtime.Object) (bool, error) { return true, nil }

func TestReconcile_NonNotFoundError_BubblesUp(t *testing.T) {
	r := &SecurityProfileReconciler{Reader: fakeErrReader{}, Store: configstore.NewStore()}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "x", Namespace: "ns"},
	})
	if err == nil || apierrors.IsNotFound(err) {
		t.Errorf("expected transient error, got %v", err)
	}
}
