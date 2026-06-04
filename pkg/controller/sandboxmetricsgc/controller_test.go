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
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
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
