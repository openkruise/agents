package proxy

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// RoutesTotal tracks the current number of routes in the proxy routing table.
	RoutesTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "sandboxmanager_routes_total",
			Help: "Total number of routes currently managed",
		},
	)

	// PeersTotal tracks the current number of peers connected.
	PeersTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "sandboxmanager_peers_total",
			Help: "Total number of peers currently connected",
		},
	)
)

func init() {
	metrics.Registry.MustRegister(RoutesTotal, PeersTotal)
}
