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

package sandbox

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/sandbox/core"
)

// newSandboxTestManager creates a real controller-runtime Manager backed by a
// stub REST config. The manager is never started; we only exercise controller
// registration, so no running apiserver is required.
func newSandboxTestManager(t *testing.T) ctrl.Manager {
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

// noopEnqueuer satisfies Enqueuer without side effects.
type noopEnqueuer struct{}

func (n *noopEnqueuer) Enqueue(_, _ string) {}

// TestSandboxReconciler_SetupWithManager verifies that SetupWithManager
// registers the sandbox controller with the manager without error. This
// exercises the wiring path (For/Watches/Named/Complete chain) against a real
// controller-runtime manager backed by a stub REST config; the manager is
// never started so no apiserver is needed.
func TestSandboxReconciler_SetupWithManager(t *testing.T) {
	mgr := newSandboxTestManager(t)

	r := &SandboxReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		controls: core.NewSandboxControl(core.SandboxControlArgs{
			Client:    mgr.GetClient(),
			APIReader: mgr.GetAPIReader(),
			Recorder:  mgr.GetEventRecorderFor("sandbox"),
		}),
		metricsCleanup: &noopEnqueuer{},
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("SandboxReconciler.SetupWithManager: unexpected error: %v", err)
	}
}
