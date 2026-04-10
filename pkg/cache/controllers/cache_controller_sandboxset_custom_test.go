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

package controllers

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestCacheSandboxSetCustomReconciler_Reconcile(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))

	nsName := types.NamespacedName{Namespace: "default", Name: "test-sandboxset"}

	tests := []struct {
		name             string
		objects          []client.Object
		registerHandlers func(r *CustomReconciler[*agentsv1alpha1.SandboxSet]) (updateCalled, deleteCalled *atomic.Int32)
		expectErr        bool
		expectUpdate     int32
		expectDelete     int32
	}{
		{
			name: "no handlers registered",
			objects: []client.Object{
				&agentsv1alpha1.SandboxSet{
					ObjectMeta: metav1.ObjectMeta{Name: "test-sandboxset", Namespace: "default"},
				},
			},
			registerHandlers: nil,
			expectErr:        false,
		},
		{
			name: "handler registered and object exists",
			objects: []client.Object{
				&agentsv1alpha1.SandboxSet{
					ObjectMeta: metav1.ObjectMeta{Name: "test-sandboxset", Namespace: "default"},
				},
			},
			registerHandlers: func(r *CustomReconciler[*agentsv1alpha1.SandboxSet]) (updateCalled, deleteCalled *atomic.Int32) {
				updateCalled = &atomic.Int32{}
				deleteCalled = &atomic.Int32{}
				r.AddReconcileHandlers(func(ctx context.Context, obj *agentsv1alpha1.SandboxSet, notFound bool) (ctrl.Result, error) {
					if notFound {
						deleteCalled.Add(1)
					} else {
						updateCalled.Add(1)
					}
					return ctrl.Result{}, nil
				})
				return
			},
			expectErr:    false,
			expectUpdate: 1,
			expectDelete: 0,
		},
		{
			name:    "handler registered and object not found",
			objects: nil,
			registerHandlers: func(r *CustomReconciler[*agentsv1alpha1.SandboxSet]) (updateCalled, deleteCalled *atomic.Int32) {
				updateCalled = &atomic.Int32{}
				deleteCalled = &atomic.Int32{}
				r.AddReconcileHandlers(func(ctx context.Context, obj *agentsv1alpha1.SandboxSet, notFound bool) (ctrl.Result, error) {
					if notFound {
						deleteCalled.Add(1)
					} else {
						updateCalled.Add(1)
					}
					return ctrl.Result{}, nil
				})
				return
			},
			expectErr:    false,
			expectUpdate: 0,
			expectDelete: 1,
		},
		{
			name: "multiple handlers registered",
			objects: []client.Object{
				&agentsv1alpha1.SandboxSet{
					ObjectMeta: metav1.ObjectMeta{Name: "test-sandboxset", Namespace: "default"},
				},
			},
			registerHandlers: func(r *CustomReconciler[*agentsv1alpha1.SandboxSet]) (updateCalled, deleteCalled *atomic.Int32) {
				updateCalled = &atomic.Int32{}
				deleteCalled = &atomic.Int32{}
				for i := 0; i < 3; i++ {
					r.AddReconcileHandlers(func(ctx context.Context, obj *agentsv1alpha1.SandboxSet, notFound bool) (ctrl.Result, error) {
						if notFound {
							deleteCalled.Add(1)
						} else {
							updateCalled.Add(1)
						}
						return ctrl.Result{}, nil
					})
				}
				return
			},
			expectErr:    false,
			expectUpdate: 3,
			expectDelete: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if len(tt.objects) > 0 {
				builder = builder.WithObjects(tt.objects...)
			}
			fakeClient := builder.Build()

			r := &CacheSandboxSetCustomReconciler{
				CustomReconciler: CustomReconciler[*agentsv1alpha1.SandboxSet]{
					Client:    fakeClient,
					NewObject: NewSandboxSet,
				},
			}

			var updateCalled, deleteCalled *atomic.Int32
			if tt.registerHandlers != nil {
				updateCalled, deleteCalled = tt.registerHandlers(&r.CustomReconciler)
			}

			result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nsName})
			if tt.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, ctrl.Result{}, result)

			if tt.registerHandlers != nil {
				assert.Equal(t, tt.expectUpdate, updateCalled.Load(), "UpdateFunc call count mismatch")
				assert.Equal(t, tt.expectDelete, deleteCalled.Load(), "DeleteFunc call count mismatch")
			}
		})
	}
}
