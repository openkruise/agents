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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	sandboxctrl "github.com/openkruise/agents/pkg/controller/sandbox"
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

func TestReconcile_DeletesSandboxMetricSeries(t *testing.T) {
	// This test verifies the Reconciler's single piece of behaviour: it must
	// invoke sandbox.DeleteSandboxMetrics for the requested (ns, name). We
	// assert side-effects on a known series instead of mocking the call, so
	// the test fails for the right reason if the wiring breaks.
	tests := []struct {
		name string
		ns   string
		obj  string
	}{
		{name: "basic delete clears created gauge", ns: "default", obj: "gc-victim-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              tt.obj,
					Namespace:         tt.ns,
					CreationTimestamp: metav1.Now(),
				},
				Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning},
			}
			sandboxctrl.RecordSandboxMetricsForTest(sb)

			r := NewReconciler(Options{})
			_, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: tt.ns, Name: tt.obj},
			})
			if err != nil {
				t.Fatalf("Reconcile returned err: %v", err)
			}
			if got := sandboxctrl.CreatedGaugeValueForTest(tt.ns, tt.obj); got != 0 {
				t.Errorf("created gauge after Reconcile = %v, want 0", got)
			}
		})
	}
}
