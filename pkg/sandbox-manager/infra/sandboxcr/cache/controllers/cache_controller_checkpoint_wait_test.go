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
	"sync"
	"testing"

	cacheutils "github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr/cache/utils"
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

func TestCacheCheckpointWaitReconciler_Reconcile(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))

	nsName := types.NamespacedName{Namespace: "default", Name: "test-checkpoint"}
	// Use waitHookKey format: *v1alpha1.Checkpoint/namespace/name
	waitHookKey := "*v1alpha1.Checkpoint/default/test-checkpoint"

	tests := []struct {
		name           string
		objects        []client.Object
		waitHooks      *sync.Map
		setupWaitHooks func(m *sync.Map)
		expectErr      bool
		expectDone     bool
	}{
		{
			name:      "waitHooks is nil",
			objects:   nil,
			waitHooks: nil,
			expectErr: false,
		},
		{
			name: "waitHooks has no matching entry",
			objects: []client.Object{
				&agentsv1alpha1.Checkpoint{
					ObjectMeta: metav1.ObjectMeta{Name: "test-checkpoint", Namespace: "default"},
				},
			},
			waitHooks: &sync.Map{},
			expectErr: false,
		},
		{
			name: "waitHooks has entry and checker satisfied",
			objects: []client.Object{
				&agentsv1alpha1.Checkpoint{
					ObjectMeta: metav1.ObjectMeta{Name: "test-checkpoint", Namespace: "default"},
				},
			},
			waitHooks: &sync.Map{},
			setupWaitHooks: func(m *sync.Map) {
				m.Store(waitHookKey, cacheutils.NewWaitEntry[*agentsv1alpha1.Checkpoint](
					context.Background(),
					cacheutils.WaitActionCheckpoint,
					func(cp *agentsv1alpha1.Checkpoint) (bool, error) {
						return true, nil
					},
				))
			},
			expectErr:  false,
			expectDone: true,
		},
		{
			name: "waitHooks has entry but checker not satisfied",
			objects: []client.Object{
				&agentsv1alpha1.Checkpoint{
					ObjectMeta: metav1.ObjectMeta{Name: "test-checkpoint", Namespace: "default"},
				},
			},
			waitHooks: &sync.Map{},
			setupWaitHooks: func(m *sync.Map) {
				m.Store(waitHookKey, cacheutils.NewWaitEntry[*agentsv1alpha1.Checkpoint](
					context.Background(),
					cacheutils.WaitActionCheckpoint,
					func(cp *agentsv1alpha1.Checkpoint) (bool, error) {
						return false, nil
					},
				))
			},
			expectErr:  false,
			expectDone: false,
		},
		{
			name:      "object not found with waitHook - done channel closed on delete",
			objects:   nil,
			waitHooks: &sync.Map{},
			setupWaitHooks: func(m *sync.Map) {
				m.Store(waitHookKey, cacheutils.NewWaitEntry[*agentsv1alpha1.Checkpoint](
					context.Background(),
					cacheutils.WaitActionCheckpoint,
					func(cp *agentsv1alpha1.Checkpoint) (bool, error) {
						return false, nil
					},
				))
			},
			expectErr:  false,
			expectDone: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if len(tt.objects) > 0 {
				builder = builder.WithObjects(tt.objects...)
			}
			fakeClient := builder.Build()

			var hooks *sync.Map
			if tt.waitHooks != nil {
				hooks = tt.waitHooks
				if tt.setupWaitHooks != nil {
					tt.setupWaitHooks(hooks)
				}
			}

			r := &CacheCheckpointWaitReconciler{
				WaitReconciler: WaitReconciler[*agentsv1alpha1.Checkpoint]{
					Client:    fakeClient,
					Scheme:    scheme,
					waitHooks: hooks,
					NewObject: NewCheckpoint,
				},
			}

			result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nsName})
			if tt.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, ctrl.Result{}, result)

			if hooks != nil {
				value, ok := hooks.Load(waitHookKey)
				if ok {
					entry := value.(*cacheutils.WaitEntry[*agentsv1alpha1.Checkpoint])
					select {
					case <-entry.Done():
						if !tt.expectDone {
							t.Error("done channel was closed but expected open")
						}
					default:
						if tt.expectDone {
							t.Error("done channel was open but expected closed")
						}
					}
				}
			}
		})
	}
}
