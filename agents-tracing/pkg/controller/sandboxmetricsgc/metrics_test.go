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

func TestDroppedCounter_IncrementsByReason(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   float64
	}{
		{name: "channel_full reason recorded", reason: "channel_full", want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			droppedTotal.Reset()
			droppedTotal.WithLabelValues(tt.reason).Inc()
			got := testutil.ToFloat64(droppedTotal.WithLabelValues(tt.reason))
			if got != tt.want {
				t.Errorf("droppedTotal{reason=%s} = %v, want %v", tt.reason, got, tt.want)
			}
		})
	}
}
