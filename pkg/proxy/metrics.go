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

package proxy

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// routesTotal tracks the current number of routes in the proxy routing table.
	routesTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "sandbox_routes_total",
			Help: "Current number of routes in the proxy routing table",
		},
	)

	// peersTotal tracks the current number of connected peer nodes.
	peersTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "sandbox_peers_total",
			Help: "Current number of connected peer nodes",
		},
	)
)

func init() {
	metrics.Registry.MustRegister(routesTotal, peersTotal)
}
