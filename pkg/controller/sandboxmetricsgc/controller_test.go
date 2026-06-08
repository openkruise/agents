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

package sandboxmetricsgc

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestEnqueue_NonBlockingDropOnFullChannel(t *testing.T) {
	tests := []struct {
		name       string
		bufferSize int
		enqueues   int
		wantDrops  float64
	}{
		{name: "fills exactly to capacity, no drops", bufferSize: 2, enqueues: 2, wantDrops: 0},
		{name: "third enqueue drops", bufferSize: 2, enqueues: 3, wantDrops: 1},
		{name: "many overflow", bufferSize: 1, enqueues: 5, wantDrops: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			droppedTotal.Reset()
			r := NewReconciler(Options{ChannelBuffer: tt.bufferSize})
			for i := 0; i < tt.enqueues; i++ {
				r.Enqueue("ns", "sb")
			}
			got := testutil.ToFloat64(droppedTotal.WithLabelValues("channel_full"))
			if got != tt.wantDrops {
				t.Errorf("drops = %v, want %v", got, tt.wantDrops)
			}
		})
	}
}

func TestReconcile_ReturnsOK(t *testing.T) {
	// The contract Reconcile must satisfy is "call DeleteSandboxMetrics and
	// return (Result{}, nil)". DeleteSandboxMetrics' correctness is verified
	// by tests in pkg/controller/sandbox/metrics_test.go; here we only check
	// that the wrapper returns cleanly and never errors regardless of whether
	// the target series exists.
	tests := []struct {
		name string
		ns   string
		obj  string
	}{
		{name: "delete missing series is a no-op", ns: "default", obj: "never-existed"},
		{name: "empty namespace name still returns ok", ns: "", obj: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewReconciler(Options{})
			res, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: tt.ns, Name: tt.obj},
			})
			if err != nil {
				t.Fatalf("Reconcile returned err: %v", err)
			}
			if res != (ctrl.Result{}) {
				t.Errorf("Reconcile returned %+v, want zero Result", res)
			}
		})
	}
}

// newTestManager creates a real controller-runtime Manager backed by a stub
// REST config. The manager is never started, so no apiserver is needed — only
// construction and controller registration are exercised.
func newTestManager(t *testing.T) ctrl.Manager {
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

func TestSetupWithManager_RegistersWithoutError(t *testing.T) {
	// Only one registration per process is possible due to the global controller
	// name uniqueness check in controller-runtime. We exercise the full
	// SetupWithManager call path once, which is sufficient for coverage.
	mgr := newTestManager(t)
	r := NewReconciler(Options{})
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("SetupWithManager: unexpected error: %v", err)
	}
}

func TestNewReconciler_AppliesDefaults(t *testing.T) {
	tests := []struct {
		name          string
		opts          Options
		wantWorkers   int
		wantBufferCap int
	}{
		{
			name:          "zero options gets both defaults",
			opts:          Options{},
			wantWorkers:   defaultWorkers,
			wantBufferCap: defaultChannelBuffer,
		},
		{
			name:          "negative workers clamped to default",
			opts:          Options{Workers: -3, ChannelBuffer: 100},
			wantWorkers:   defaultWorkers,
			wantBufferCap: 100,
		},
		{
			name:          "negative buffer clamped to default",
			opts:          Options{Workers: 4, ChannelBuffer: -1},
			wantWorkers:   4,
			wantBufferCap: defaultChannelBuffer,
		},
		{
			name:          "explicit values pass through",
			opts:          Options{Workers: 2, ChannelBuffer: 16},
			wantWorkers:   2,
			wantBufferCap: 16,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewReconciler(tt.opts)
			if r.workers != tt.wantWorkers {
				t.Errorf("workers = %d, want %d", r.workers, tt.wantWorkers)
			}
			if got := cap(r.eventChan); got != tt.wantBufferCap {
				t.Errorf("eventChan cap = %d, want %d", got, tt.wantBufferCap)
			}
		})
	}
}
