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
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	sandboxctrl "github.com/openkruise/agents/pkg/controller/sandbox"
)

// newControllersTestManager creates a real controller-runtime Manager backed
// by a stub REST config.  The manager is never started; we only exercise the
// controller registration wiring, so no running apiserver is required.
func newControllersTestManager(t *testing.T) ctrl.Manager {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add clientgo scheme: %v", err)
	}
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add agents scheme: %v", err)
	}
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:0"}, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr
}

// nopEnqueuer satisfies sandbox.Enqueuer without side effects.
type nopEnqueuer struct{}

func (n *nopEnqueuer) Enqueue(_, _ string) {}

// TestSetupWithManager_AllControllersEarlyReturn verifies that
// SetupWithManager walks every sub-controller Add in sequence and returns nil
// when no discovery client is registered (all controllers report GVK-not-found
// and return early).  This exercises every statement in SetupWithManager and
// ensures that a nil error propagates correctly.
//
// The discovery early-return is not a limitation of the test: in production,
// the controller Add functions perform the same discovery guard.  Coverage of
// the post-discovery path in each controller's Add (constructing the reconciler
// and calling SetupWithManager) is exercised separately in each controller's
// own test package.
func TestSetupWithManager_AllControllersEarlyReturn(t *testing.T) {
	tests := []struct {
		name        string
		deps        Deps
		expectError string
	}{
		{
			name:        "nil enqueuer with no discovery client returns nil",
			deps:        Deps{MetricsCleanup: nil},
			expectError: "",
		},
		{
			name:        "noop enqueuer with no discovery client returns nil",
			deps:        Deps{MetricsCleanup: &nopEnqueuer{}},
			expectError: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use a fresh manager per sub-test so each call to SetupWithManager
			// starts with a clean controller registry.  In this environment,
			// client.GetGenericClient() is nil, so discovery.DiscoverGVK always
			// returns false and every sub-controller Add returns early with nil.
			mgr := newControllersTestManager(t)
			err := SetupWithManager(mgr, tt.deps)
			if tt.expectError == "" {
				if err != nil {
					t.Errorf("SetupWithManager: unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("SetupWithManager: expected error containing %q, got nil", tt.expectError)
				} else if err.Error() != tt.expectError {
					t.Errorf("SetupWithManager: error = %q, want containing %q", err.Error(), tt.expectError)
				}
			}
		})
	}
}

// compile-time check that nopEnqueuer satisfies sandboxctrl.Enqueuer.
var _ sandboxctrl.Enqueuer = (*nopEnqueuer)(nil)
