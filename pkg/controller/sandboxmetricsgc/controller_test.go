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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
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

func TestSetupWithManager_Compiles(_ *testing.T) {
	// Compile-time assertion: NewReconciler returns *Reconciler which has
	// SetupWithManager(ctrl.Manager) error. If this stops compiling the
	// wiring broke.
	var _ func(ctrl.Manager) error = (*Reconciler)(nil).SetupWithManager
}
