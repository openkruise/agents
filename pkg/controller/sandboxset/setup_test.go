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

package sandboxset

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// newSandboxSetTestManager creates a real controller-runtime Manager backed by
// a stub REST config. The manager is never started; we only exercise controller
// registration wiring, so no running apiserver is required.
func newSandboxSetTestManager(t *testing.T) ctrl.Manager {
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

// TestReconciler_SetupWithManager verifies that SetupWithManager registers the
// sandboxset controller with a real controller-runtime manager without error.
// The manager is never started, so no apiserver is needed.
func TestReconciler_SetupWithManager(t *testing.T) {
	mgr := newSandboxSetTestManager(t)
	r := &Reconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("Reconciler.SetupWithManager: unexpected error: %v", err)
	}
}
