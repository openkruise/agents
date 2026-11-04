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

package sandbox_manager

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestOperationMetricsOmitNamespace(t *testing.T) {
	collectors := []prometheus.Collector{
		sandboxClaimTotal,
		sandboxClaimRetries,
		sandboxRouteSyncDuration,
		sandboxRouteSyncTotal,
	}
	for _, collector := range collectors {
		assertCollectorOmitsNamespace(t, collector)
	}
}

func assertCollectorOmitsNamespace(t *testing.T, collector prometheus.Collector) {
	t.Helper()
	ch := make(chan *prometheus.Desc, 4)
	collector.Describe(ch)
	close(ch)
	n := 0
	for desc := range ch {
		n++
		text := desc.String()
		if strings.Contains(text, "namespace") {
			t.Errorf("metric still labeled by namespace: %s", text)
		}
	}
	if n == 0 {
		t.Fatal("collector described no metrics")
	}
}
