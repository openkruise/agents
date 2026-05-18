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

package peers

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

func TestRecordPeerAlive_AndDead(t *testing.T) {
	const node = "test-node-1"

	// Reset both gauges for this node so we get a clean view.
	peerStateGauge.DeleteLabelValues(node, peerStateAlive)
	peerStateGauge.DeleteLabelValues(node, peerStateDead)

	recordPeerAlive(node)
	if v := testutil.ToFloat64(peerStateGauge.WithLabelValues(node, peerStateAlive)); v != 1 {
		t.Errorf("after recordPeerAlive: alive gauge = %v, want 1", v)
	}
	if v := testutil.ToFloat64(peerStateGauge.WithLabelValues(node, peerStateDead)); v != 0 {
		t.Errorf("after recordPeerAlive: dead gauge = %v, want 0", v)
	}

	recordPeerDead(node)
	if v := testutil.ToFloat64(peerStateGauge.WithLabelValues(node, peerStateAlive)); v != 0 {
		t.Errorf("after recordPeerDead: alive gauge = %v, want 0", v)
	}
	if v := testutil.ToFloat64(peerStateGauge.WithLabelValues(node, peerStateDead)); v != 1 {
		t.Errorf("after recordPeerDead: dead gauge = %v, want 1", v)
	}
}

func TestRecordPeerAlive_DoesNotAffectOtherNodes(t *testing.T) {
	const a = "node-a-isolation"
	const b = "node-b-isolation"

	peerStateGauge.DeleteLabelValues(a, peerStateAlive)
	peerStateGauge.DeleteLabelValues(b, peerStateAlive)

	recordPeerAlive(a)
	if v := testutil.ToFloat64(peerStateGauge.WithLabelValues(b, peerStateAlive)); v != 0 {
		t.Errorf("recordPeerAlive(%q) should not touch node %q's alive gauge, got %v", a, b, v)
	}
}

func TestObservePeerJoinDuration(t *testing.T) {
	dtoMetric := &dto.Metric{}
	if err := peerJoinDuration.Write(dtoMetric); err != nil {
		t.Fatalf("histogram Write failed before: %v", err)
	}
	before := dtoMetric.GetHistogram().GetSampleCount()

	observePeerJoinDuration(0.42)

	dtoMetric = &dto.Metric{}
	if err := peerJoinDuration.Write(dtoMetric); err != nil {
		t.Fatalf("histogram Write failed after: %v", err)
	}
	after := dtoMetric.GetHistogram().GetSampleCount()

	if after != before+1 {
		t.Errorf("histogram sample count = %d after one observation, want %d", after, before+1)
	}
}
