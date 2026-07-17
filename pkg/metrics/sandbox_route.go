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

package metrics

import "github.com/prometheus/client_golang/prometheus"

const (
	// RouteSurface* label values for sandbox_route_* metrics (aligned with sandboxroute.Surface).
	RouteSurfaceManager = "manager"
	RouteSurfaceGateway = "gateway"

	// RouteRecordShape* label values retained on sandbox_route_records.
	RouteRecordShapeIDOnly    = "id_only"
	RouteRecordShapeCollision = "collision"
)

var (
	sandboxRouteLegacyFallbackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandbox_route_legacy_fallback_total",
		Help: "Total successful legacy delete fallbacks that removed an ID-only route.",
	}, []string{"surface"})
	sandboxRouteInvalidTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandbox_route_invalid_total",
		Help: "Total invalid route mutations by serving surface.",
	}, []string{"surface"})
	sandboxRouteRecords = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sandbox_route_records",
		Help: "Current compatibility and collision route records by shape.",
	}, []string{"surface", "shape"})
	sandboxRouteRepairQueueDepth = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sandbox_route_repair_queue_depth",
		Help: "Current number of queued targeted route repairs.",
	}, []string{"surface"})
)

// RecordSandboxRouteLegacyFallback records one successful legacy delete fallback.
func RecordSandboxRouteLegacyFallback(surface string) {
	if !supportedSandboxRouteSurface(surface) {
		return
	}
	sandboxRouteLegacyFallbackTotal.WithLabelValues(surface).Inc()
}

// RecordSandboxRouteInvalid records one invalid route mutation.
func RecordSandboxRouteInvalid(surface string) {
	if !supportedSandboxRouteSurface(surface) {
		return
	}
	sandboxRouteInvalidTotal.WithLabelValues(surface).Inc()
}

// SetSandboxRouteRecords sets the current number of records for one retained shape.
func SetSandboxRouteRecords(surface, shape string, count int) {
	if supportedSandboxRouteSurface(surface) &&
		allowedLabel(shape, RouteRecordShapeIDOnly, RouteRecordShapeCollision) &&
		count >= 0 {
		sandboxRouteRecords.WithLabelValues(surface, shape).Set(float64(count))
	}
}

// SetSandboxRouteRepairQueueDepth sets the current targeted repair queue depth.
func SetSandboxRouteRepairQueueDepth(surface string, count int) {
	if supportedSandboxRouteSurface(surface) && count >= 0 {
		sandboxRouteRepairQueueDepth.WithLabelValues(surface).Set(float64(count))
	}
}

func supportedSandboxRouteSurface(surface string) bool {
	return allowedLabel(surface, RouteSurfaceManager, RouteSurfaceGateway)
}
