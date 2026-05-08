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
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	peerStateAlive = "alive"
	peerStateDead  = "dead"
)

var (
	// sandbox_peer_state is a per-(node,state) gauge that reports 1 for the
	// peer's current state and 0 for other states. Today only "alive" and
	// "dead" are tracked via the memberlist event delegate; "suspect" would
	// require periodic polling of memberlist.Members() and is left for a
	// follow-up.
	peerStateGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_peer_state",
			Help: "Current memberlist peer state (1 for active state, 0 otherwise). state ∈ {alive, dead}.",
		},
		[]string{"node", "state"},
	)

	// sandbox_peer_join_duration_seconds is observed once per local node, the
	// first time another peer joins the cluster after Start(). It captures the
	// wall-clock time from local memberlist startup to first observed peer.
	peerJoinDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandbox_peer_join_duration_seconds",
			Help:    "Duration from local memberlist Start() to first observed peer join in seconds.",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 12), // 100ms -> ~6.8m
		},
	)
)

func init() {
	metrics.Registry.MustRegister(peerStateGauge, peerJoinDuration)
}

// recordPeerAlive flips the gauge to mark `node` alive and clears its dead bit.
func recordPeerAlive(node string) {
	peerStateGauge.WithLabelValues(node, peerStateAlive).Set(1)
	peerStateGauge.WithLabelValues(node, peerStateDead).Set(0)
}

// recordPeerDead flips the gauge to mark `node` dead and clears its alive bit.
func recordPeerDead(node string) {
	peerStateGauge.WithLabelValues(node, peerStateAlive).Set(0)
	peerStateGauge.WithLabelValues(node, peerStateDead).Set(1)
}

// observePeerJoinDuration records the time-to-first-peer for the local node.
// Callers should ensure this is invoked at most once per process via sync.Once.
func observePeerJoinDuration(seconds float64) {
	peerJoinDuration.Observe(seconds)
}
