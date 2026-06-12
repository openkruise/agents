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
	"github.com/openkruise/agents/pkg/controller/sandboxmetricsgc"
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

// TestSetupWithManager_AllControllersEarlyReturn verifies that
// SetupWithManager walks every sub-controller Add in sequence and returns nil.
//
// Semantics covered here:
//   - The sandbox-metrics-gc reconciler's SetupWithManager runs unconditionally
//     (it has no discovery guard) and registers itself on the manager.
//   - The remaining sub-controllers (sandbox, sandboxset, sandboxclaim,
//     sandboxupdateops, securitytokenrefresh) each perform a discovery guard
//     inside their own Add func and early-return with nil when their target
//     GVK is not discoverable. In this test the manager is built against a
//     stub REST config with no real apiserver, so the discovery client is
//     unavailable and every guarded Add takes the early-return branch.
//
// This exercises every statement in SetupWithManager (including the
// unconditional metricsGC registration) and ensures a nil error propagates
// correctly. Coverage of the post-discovery path in each guarded controller's
// Add (constructing the reconciler and calling SetupWithManager) is exercised
// separately in each controller's own test package.
//
// NOTE: Only one table entry registers a controller because controller-runtime
// uses a global prometheus registry keyed by controller name.  Registering
// "sandbox-metrics-gc" twice in the same process causes a collision.
func TestSetupWithManager_AllControllersEarlyReturn(t *testing.T) {
	tests := []struct {
		name        string
		deps        Deps
		expectError string
	}{
		{
			name: "explicit options with no discovery client returns nil",
			deps: Deps{MetricsGCOptions: sandboxmetricsgc.Options{
				Workers:       4,
				ChannelBuffer: 1000,
			}},
			expectError: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use a fresh manager per sub-test so each call to SetupWithManager
			// starts with a clean controller registry. The metricsGC reconciler
			// registers unconditionally; the other sub-controllers early-return
			// because client.GetGenericClient() is nil here, so
			// discovery.DiscoverGVK always returns false.
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

// TestSetupWithManager_ZeroValueOptions verifies that a zero-value Options
// (relying on NewReconciler defaults) results in a valid Reconciler being
// created.  We verify this at the unit level without registering the controller
// to avoid the global prometheus registry collision tested above.
func TestSetupWithManager_ZeroValueOptions(t *testing.T) {
	opts := sandboxmetricsgc.Options{}
	r := sandboxmetricsgc.NewReconciler(opts)
	if r == nil {
		t.Fatal("NewReconciler returned nil for zero-value Options")
	}
}
