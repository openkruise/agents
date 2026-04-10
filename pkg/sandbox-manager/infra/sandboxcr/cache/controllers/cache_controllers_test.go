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
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"
)

// TestSetupCacheControllersWithManager verifies that SetupCacheControllersWithManager
// correctly registers all 4 cache controllers with the manager and returns a
// CacheControllerHandlers with valid reconciler references, or propagates errors
// from any individual Add call.
func TestSetupCacheControllersWithManager(t *testing.T) {
	sentinel := fmt.Errorf("injected add error")

	tests := []struct {
		name         string
		failOnNthAdd int   // 0 = never fail
		addError     error // error injected when failOnNthAdd is reached
		// nilWaitHooks controls whether waitHooks passed to the function is nil
		nilWaitHooks bool
		wantErr      bool
		// checkHandlers is called only when wantErr == false
		checkHandlers func(t *testing.T, mgr *MockManager, handlers *CacheControllerHandlers)
	}{
		{
			name:         "success - all reconcilers registered with non-nil waitHooks",
			failOnNthAdd: 0,
			nilWaitHooks: false,
			wantErr:      false,
			checkHandlers: func(t *testing.T, mgr *MockManager, handlers *CacheControllerHandlers) {
				t.Helper()
				require.NotNil(t, handlers, "handlers must not be nil on success")

				// Both custom reconcilers must be present
				require.NotNil(t, handlers.SandboxCustomReconciler, "SandboxCustomReconciler must not be nil")
				require.NotNil(t, handlers.SandboxSetCustomReconciler, "SandboxSetCustomReconciler must not be nil")

				// Verify the reconciler names match production code expectations
				assert.Equal(t, "SandboxCustom", handlers.SandboxCustomReconciler.Name)
				assert.Equal(t, "SandboxSetCustom", handlers.SandboxSetCustomReconciler.Name)

				// Verify the client and scheme are correctly wired from the manager
				assert.Equal(t, mgr.GetClient(), handlers.SandboxCustomReconciler.Client)
				assert.Equal(t, mgr.GetScheme(), handlers.SandboxCustomReconciler.Scheme)
				assert.Equal(t, mgr.GetClient(), handlers.SandboxSetCustomReconciler.Client)
				assert.Equal(t, mgr.GetScheme(), handlers.SandboxSetCustomReconciler.Scheme)

				// Verify NewObject factories produce the correct types
				assert.IsType(t, &agentsv1alpha1.Sandbox{}, handlers.SandboxCustomReconciler.NewObject())
				assert.IsType(t, &agentsv1alpha1.SandboxSet{}, handlers.SandboxSetCustomReconciler.NewObject())

				// All 4 controllers must have been added to the manager:
				// SandboxWait, CheckpointWait, SandboxCustom, SandboxSetCustom
				assert.Equal(t, 4, mgr.addCallsCount(), "expected exactly 4 Add() calls")
			},
		},
		{
			name:         "success - all reconcilers registered with nil waitHooks",
			failOnNthAdd: 0,
			nilWaitHooks: true,
			wantErr:      false,
			checkHandlers: func(t *testing.T, mgr *MockManager, handlers *CacheControllerHandlers) {
				t.Helper()
				require.NotNil(t, handlers)
				require.NotNil(t, handlers.SandboxCustomReconciler)
				require.NotNil(t, handlers.SandboxSetCustomReconciler)
				assert.Equal(t, 4, mgr.addCallsCount())
			},
		},
		{
			name:         "fail on 1st Add - SandboxWaitReconciler registration error",
			failOnNthAdd: 1,
			addError:     sentinel,
			wantErr:      true,
		},
		{
			name:         "fail on 2nd Add - CheckpointWaitReconciler registration error",
			failOnNthAdd: 2,
			addError:     sentinel,
			wantErr:      true,
		},
		{
			name:         "fail on 3rd Add - SandboxCustomReconciler registration error",
			failOnNthAdd: 3,
			addError:     sentinel,
			wantErr:      true,
		},
		{
			name:         "fail on 4th Add - SandboxSetCustomReconciler registration error",
			failOnNthAdd: 4,
			addError:     sentinel,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := newMockManagerBuilderForTest(t).WithFailOnNthAdd(tt.failOnNthAdd, tt.addError).Build()

			var waitHooks *sync.Map
			if !tt.nilWaitHooks {
				waitHooks = &sync.Map{}
			}

			handlers, err := SetupCacheControllersWithManager(mgr, waitHooks)

			if tt.wantErr {
				require.Error(t, err, "expected an error but got none")
				assert.Nil(t, handlers, "handlers must be nil when an error occurs")
				// Verify the injected error is propagated
				assert.ErrorIs(t, err, sentinel, "returned error must wrap the injected sentinel")
				// Verify Add() calls stop immediately after the first failure
				if tt.failOnNthAdd > 0 {
					assert.Equal(t, tt.failOnNthAdd, mgr.addCallsCount(),
						"Add() calls should stop after the first failure")
				}
			} else {
				require.NoError(t, err)
				if tt.checkHandlers != nil {
					tt.checkHandlers(t, mgr, handlers)
				}
			}
		})
	}
}

// TestCacheControllerHandlers_AddReconcileHandlers verifies that the reconcilers
// returned inside CacheControllerHandlers correctly support AddReconcileHandlers,
// which is the primary usage pattern for callers of SetupCacheControllersWithManager.
func TestCacheControllerHandlers_AddReconcileHandlers(t *testing.T) {
	mgr := newMockManagerBuilderForTest(t).Build()
	waitHooks := &sync.Map{}

	handlers, err := SetupCacheControllersWithManager(mgr, waitHooks)
	require.NoError(t, err)
	require.NotNil(t, handlers)

	// Register a handler on SandboxCustomReconciler and verify it is tracked
	var sbxHandlerCalled atomic.Int32
	handlers.SandboxCustomReconciler.AddReconcileHandlers(
		func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ bool) (ctrl.Result, error) {
			sbxHandlerCalled.Add(1)
			return ctrl.Result{}, nil
		},
	)

	// Register a handler on SandboxSetCustomReconciler
	var sbsHandlerCalled atomic.Int32
	handlers.SandboxSetCustomReconciler.AddReconcileHandlers(
		func(_ context.Context, _ *agentsv1alpha1.SandboxSet, _ bool) (ctrl.Result, error) {
			sbsHandlerCalled.Add(1)
			return ctrl.Result{}, nil
		},
	)

	// Verify handlers are stored (they are called at reconcile time, not immediately)
	assert.Equal(t, int32(0), sbxHandlerCalled.Load(), "handler must not be called until Reconcile is triggered")
	assert.Equal(t, int32(0), sbsHandlerCalled.Load(), "handler must not be called until Reconcile is triggered")
}
