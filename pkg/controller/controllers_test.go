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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/sandboxmetricsgc"
	"github.com/openkruise/agents/pkg/features"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
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

// TestSetupWithManager_MetricsGCRegistrationError verifies the error-wrapping
// branch at controllers.go line 42: when metricsGC.SetupWithManager(m) returns
// a non-nil error, SetupWithManager wraps it with the "sandbox-metrics-gc:"
// prefix and returns immediately, before any other sub-controller Add runs.
//
// Triggering mechanism: controller-runtime maintains a process-wide
// usedNames set (see sigs.k8s.io/controller-runtime/pkg/controller.checkName)
// that rejects duplicate Named(...) registrations across all managers in the
// same process. By pre-seeding "sandbox-metrics-gc" into that set on an
// isolated seed manager, the subsequent SetupWithManager call is guaranteed
// to fail at the metricsGC.SetupWithManager step regardless of whether other
// tests in this binary already ran.
//
// We deliberately ignore the seed registration's error: if a previous test
// (e.g., TestSetupWithManager_AllControllersEarlyReturn) already populated
// usedNames, the seed call here will itself fail, but the global state is
// still in the desired "name already taken" condition. Either path leaves
// "sandbox-metrics-gc" in usedNames, which is the only invariant the
// assertion below depends on.
func TestSetupWithManager_MetricsGCRegistrationError(t *testing.T) {
	// Reset state then deterministically seed "sandbox-metrics-gc" into the
	// global controller-runtime usedNames set so this test does not depend on
	// any prior test having registered that name.
	resetControllerRuntimeUsedNames()
	seedControllerName("sandbox-metrics-gc")

	tests := []struct {
		name        string
		deps        Deps
		expectError string
	}{
		{
			name:        "duplicate metricsGC registration returns wrapped error",
			deps:        Deps{MetricsGCOptions: sandboxmetricsgc.Options{Workers: 1, ChannelBuffer: 8}},
			expectError: "sandbox-metrics-gc: ",
		},
		{
			name:        "zero-value Deps still surfaces metricsGC error wrap",
			deps:        Deps{},
			expectError: "sandbox-metrics-gc: ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := newControllersTestManager(t)
			err := SetupWithManager(mgr, tt.deps)
			if tt.expectError == "" {
				if err != nil {
					t.Errorf("SetupWithManager: unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("SetupWithManager: expected error containing %q, got nil", tt.expectError)
			}
			if !strings.Contains(err.Error(), tt.expectError) {
				t.Errorf("SetupWithManager: error = %q, want containing %q", err.Error(), tt.expectError)
			}
			// Also assert the wrapped error preserves the underlying
			// controller-runtime collision message — proves we got here
			// via the registration-collision path and not, say, an early
			// validation error.
			if !strings.Contains(err.Error(), "already exists") {
				t.Errorf("SetupWithManager: error = %q, expected to contain controller-runtime collision phrase 'already exists'", err.Error())
			}
		})
	}
}

// TestSetupWithManager_SubControllerErrors covers the five remaining error
// branches in SetupWithManager (sandbox / sandboxset / sandboxclaim /
// sandboxupdateops / securitytokenrefresh). Each branch is gated by
// discovery.DiscoverGVK plus, in some cases, a feature gate; under a stub
// REST config those guards normally early-return nil and the wrapped error
// path is never executed.
//
// To exercise the wrapping consistently, this test installs a FakeDiscovery
// into the package-private client.defaultGenericClient slot (via go:linkname)
// and configures feature gates so the targeted sub-controller proceeds past
// its discovery / feature-gate guard while earlier sub-controllers either
// register cleanly or short-circuit at their own guards.
//
// For most subtests, the targeted sub-controller is forced into its error
// branch by pre-seeding controller-runtime's process-wide usedNames set with
// that controller's Name() value, so the subsequent Complete(...) call
// collides with the canonical "controller with name X already exists" error.
// SetupWithManager then wraps that error with the expected per-controller
// prefix, which the assertion below pins.
//
// The sandboxclaim subtest uses a different mechanism: sandboxclaim.Add
// constructs infracache.NewCache(mgr) before it ever attempts a
// controller-runtime registration, and that NewCache call fails on its own
// under our stub REST config (it tries to dial the apiserver). So merely
// passing the discovery+gate guard is enough to exercise the
// "sandboxclaim: %w" wrap, and no controller-name seeding is required.
//
// The sandboxupdateops and securitytokenrefresh subtests disable
// SandboxClaimGate so sandboxclaim.Add early-returns nil at its feature-gate
// check without touching the cache (avoiding both the slow discovery retry
// and the cache failure path that would otherwise short-circuit the chain).
//
// The subtests share a process but reset state at the top of each one, so
// ordering between them is irrelevant.
func TestSetupWithManager_SubControllerErrors(t *testing.T) {
	// Snapshot the original feature-gate values so we can restore them at
	// the end of the parent test, regardless of any intermediate subtest
	// flips. The defaults are Sandbox/SandboxSet/SandboxClaim=true and
	// SecurityIdentityProvider=false.
	prevGates := map[string]bool{
		string(features.SandboxGate):                  utilfeature.DefaultFeatureGate.Enabled(features.SandboxGate),
		string(features.SandboxSetGate):               utilfeature.DefaultFeatureGate.Enabled(features.SandboxSetGate),
		string(features.SandboxClaimGate):             utilfeature.DefaultFeatureGate.Enabled(features.SandboxClaimGate),
		string(features.SecurityIdentityProviderGate): utilfeature.DefaultFeatureGate.Enabled(features.SecurityIdentityProviderGate),
	}
	t.Cleanup(func() {
		_ = utilfeature.DefaultMutableFeatureGate.SetFromMap(prevGates)
	})

	// allOn enables every gate consulted by SetupWithManager's sub-controllers,
	// claimOff is identical except SandboxClaimGate=false (used to bypass
	// sandboxclaim.Add cheaply for tests that target a later sub-controller).
	allOn := map[string]bool{
		string(features.SandboxGate):                  true,
		string(features.SandboxSetGate):               true,
		string(features.SandboxClaimGate):             true,
		string(features.SecurityIdentityProviderGate): true,
	}
	claimOff := map[string]bool{
		string(features.SandboxGate):                  true,
		string(features.SandboxSetGate):               true,
		string(features.SandboxClaimGate):             false,
		string(features.SecurityIdentityProviderGate): true,
	}

	tests := []struct {
		name           string
		gates          map[string]bool // feature gates to apply for this subtest
		seedName       string          // controller-runtime Name() to pre-seed and force a collision ("" = skip)
		expectError    string          // wrapped prefix from controllers.go
		expectContains string          // additional substring expected somewhere in err.Error()
	}{
		{
			name:           "sandbox add error gets sandbox: prefix",
			gates:          allOn,
			seedName:       "sandbox-controller",
			expectError:    "sandbox: ",
			expectContains: "sandbox-controller",
		},
		{
			name:           "sandboxset add error gets sandboxset: prefix",
			gates:          allOn,
			seedName:       "sandboxset-controller",
			expectError:    "sandboxset: ",
			expectContains: "sandboxset-controller",
		},
		{
			// NewCache fails naturally on no apiserver, so no seed is needed.
			name:           "sandboxclaim add error gets sandboxclaim: prefix",
			gates:          allOn,
			seedName:       "",
			expectError:    "sandboxclaim: ",
			expectContains: "",
		},
		{
			name:           "sandboxupdateops add error gets sandboxupdateops: prefix",
			gates:          claimOff,
			seedName:       "sandboxupdateops-controller",
			expectError:    "sandboxupdateops: ",
			expectContains: "sandboxupdateops-controller",
		},
		{
			name:           "securitytokenrefresh add error gets securitytokenrefresh: prefix",
			gates:          claimOff,
			seedName:       "security-token-refresh",
			expectError:    "securitytokenrefresh: ",
			expectContains: "security-token-refresh",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := utilfeature.DefaultMutableFeatureGate.SetFromMap(tt.gates); err != nil {
				t.Fatalf("set feature gates: %v", err)
			}

			// Advertise every Kind so any sub-controller whose gate is on
			// passes its discovery guard. Sub-controllers whose gate is
			// off short-circuit before discovery is consulted, so we do
			// not need to vary the kind list per subtest.
			prev := installFakeGenericClient()
			t.Cleanup(func() { restoreGenericClient(prev) })

			resetControllerRuntimeUsedNames()
			if tt.seedName != "" {
				seedControllerName(tt.seedName)
			}

			mgr := newControllersTestManager(t)
			err := SetupWithManager(mgr, Deps{MetricsGCOptions: sandboxmetricsgc.Options{Workers: 1, ChannelBuffer: 8}})
			if tt.expectError == "" {
				if err != nil {
					t.Errorf("SetupWithManager: unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("SetupWithManager: expected error containing %q, got nil", tt.expectError)
			}
			if !strings.HasPrefix(err.Error(), tt.expectError) {
				t.Errorf("SetupWithManager: error = %q, want prefix %q", err.Error(), tt.expectError)
			}
			if tt.expectContains != "" && !strings.Contains(err.Error(), tt.expectContains) {
				t.Errorf("SetupWithManager: error = %q, want substring %q", err.Error(), tt.expectContains)
			}
		})
	}
}

