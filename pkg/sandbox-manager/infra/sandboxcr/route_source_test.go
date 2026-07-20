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

package sandboxcr

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/cache/controllers"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

func TestRouteSandboxSourceRegisterEventHandler(t *testing.T) {
	handlerErr := errors.New("route handler failed")
	tests := []struct {
		name        string
		withSandbox bool
		handlerErr  error
	}{
		{name: "present sandbox", withSandbox: true},
		{name: "not found sandbox"},
		{name: "handler error is returned to reconciler", withSandbox: true, handlerErr: handlerErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []client.Object
			if tt.withSandbox {
				objects = append(objects, routeSourceSandbox())
			}
			managerCache, apiReader, err := cachetest.NewTestCache(t, objects...)
			require.NoError(t, err)
			source := &routeSandboxSource{cache: managerCache, apiReader: apiReader}

			var gotKey types.NamespacedName
			var gotSandbox infra.Sandbox
			require.NoError(t, source.RegisterEventHandler(func(_ context.Context, key types.NamespacedName, sandbox infra.Sandbox) error {
				gotKey = key
				gotSandbox = sandbox
				return tt.handlerErr
			}))

			_, err = managerCache.GetSandboxController().Reconcile(t.Context(), ctrl.Request{
				NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "sandbox-a"},
			})
			if tt.handlerErr != nil {
				assert.ErrorIs(t, err, tt.handlerErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, types.NamespacedName{Namespace: "team-a", Name: "sandbox-a"}, gotKey)
			if tt.withSandbox {
				require.NotNil(t, gotSandbox)
				assert.Equal(t, "10.0.0.1", gotSandbox.GetPodIP())
			} else {
				assert.Nil(t, gotSandbox)
			}
		})
	}
}

func TestRouteSandboxSourceObserve(t *testing.T) {
	managerCache, apiReader, err := cachetest.NewTestCache(t, routeSourceSandbox())
	require.NoError(t, err)
	key := types.NamespacedName{Namespace: "team-a", Name: "sandbox-a"}

	t.Run("present", func(t *testing.T) {
		source := &routeSandboxSource{cache: managerCache, apiReader: apiReader}
		sandbox, err := source.Observe(t.Context(), key)
		require.NoError(t, err)
		require.NotNil(t, sandbox)
		assert.Equal(t, "10.0.0.1", sandbox.GetPodIP())
	})

	t.Run("not found", func(t *testing.T) {
		source := &routeSandboxSource{cache: managerCache, apiReader: apiReader}
		missing := types.NamespacedName{Namespace: "team-a", Name: "missing"}
		sandbox, err := source.Observe(t.Context(), missing)
		require.NoError(t, err)
		assert.Nil(t, sandbox)
	})

	t.Run("reader error is preserved", func(t *testing.T) {
		readErr := errors.New("direct read failed")
		source := &routeSandboxSource{cache: managerCache, apiReader: routeSourceErrorReader{err: readErr}}
		_, err := source.Observe(t.Context(), key)
		require.Error(t, err)
		assert.ErrorIs(t, err, readErr)
	})
}

func TestRouteSandboxSourceRegistrationValidation(t *testing.T) {
	managerCache, apiReader, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	handler := func(context.Context, types.NamespacedName, infra.Sandbox) error { return nil }

	t.Run("nil handler", func(t *testing.T) {
		assert.Error(t, (&routeSandboxSource{cache: managerCache, apiReader: apiReader}).RegisterEventHandler(nil))
	})
	t.Run("missing cache", func(t *testing.T) {
		assert.Error(t, (&routeSandboxSource{apiReader: apiReader}).RegisterEventHandler(handler))
	})
	t.Run("missing controller capability", func(t *testing.T) {
		source := &routeSandboxSource{cache: providerWithoutSandboxController{Provider: managerCache}, apiReader: apiReader}
		assert.Error(t, source.RegisterEventHandler(handler))
	})
	t.Run("nil controller", func(t *testing.T) {
		source := &routeSandboxSource{cache: nilSandboxControllerProvider{Provider: managerCache}, apiReader: apiReader}
		assert.Error(t, source.RegisterEventHandler(handler))
	})
	t.Run("API reader is not required", func(t *testing.T) {
		assert.NoError(t, (&routeSandboxSource{cache: managerCache}).RegisterEventHandler(handler))
	})
}

type providerWithoutSandboxController struct {
	cache.Provider
}

type nilSandboxControllerProvider struct {
	cache.Provider
}

func (nilSandboxControllerProvider) GetSandboxController() *controllers.CacheSandboxCustomReconciler {
	return nil
}

type routeSourceErrorReader struct {
	err error
}

func (r routeSourceErrorReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}

func (r routeSourceErrorReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return r.err
}

func routeSourceSandbox() *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "team-a",
			Name:            "sandbox-a",
			UID:             "uid-a",
			ResourceVersion: "10",
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase:   agentsv1alpha1.SandboxRunning,
			PodInfo: agentsv1alpha1.PodInfo{PodIP: "10.0.0.1"},
		},
	}
}
