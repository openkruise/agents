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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
)

func TestCacheSandboxWaitReconciler_Reconcile(t *testing.T) {
	obj := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sbx"}}
	for _, tt := range []struct {
		name                           string
		registered, present, satisfied bool
	}{
		{name: "no hook", present: true},
		{name: "unfinished", registered: true, present: true},
		{name: "observed completion", registered: true, present: true, satisfied: true},
		{name: "confirmed deletion", registered: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fc := newFakeClientWithScheme(t)
			if tt.present {
				require.NoError(t, fc.Create(t.Context(), obj.DeepCopy()))
			}
			var hooks sync.Map
			r := WaitReconciler[*agentsv1alpha1.Sandbox]{Client: fc, waitHooks: &hooks, NewObject: NewSandbox}
			var result <-chan error
			if tt.registered {
				result = startTestWait(t, &hooks, obj, fc, cacheutils.WaitActionWaitReady, func(snapshot *agentsv1alpha1.Sandbox) (bool, error) {
					require.Equal(t, obj.GetName(), snapshot.GetName())
					return tt.satisfied, nil
				})
			}
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(obj)})
			require.NoError(t, err)
			if !tt.registered || tt.present && !tt.satisfied {
				select {
				case err := <-result:
					t.Fatalf("unexpected wait result: %v", err)
				default:
				}
				return
			}
			select {
			case err := <-result:
				if tt.present {
					require.NoError(t, err)
				} else {
					require.True(t, apierrors.IsNotFound(err))
				}
			case <-time.After(time.Second):
				t.Fatal("waiter did not finish")
			}
		})
	}
}
