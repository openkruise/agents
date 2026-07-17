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
	SandboxIDAssignmentResultSuccess = "success"
	SandboxIDAssignmentResultFailure = "failure"

	// LegacyResolutionSurface* label values for sandbox_id_legacy_resolution_total.
	LegacyResolutionSurfaceE2B     = "e2b"
	LegacyResolutionSurfaceGateway = "gateway"

	// CollisionSurface* label values for sandbox_id_collision_total.
	CollisionSurfaceCache        = "cache"
	CollisionSurfaceManagerRoute = "manager_route"
	CollisionSurfaceGatewayRoute = "gateway_route"
)

var (
	sandboxIDLegacyResolutionTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandbox_id_legacy_resolution_total",
		Help: "Total legacy Sandbox ID resolutions by serving surface.",
	}, []string{"surface"})
	sandboxIDAssignmentTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandbox_id_assignment_total",
		Help: "Total short Sandbox ID assignments by result.",
	}, []string{"result"})
	sandboxIDCollisionTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandbox_id_collision_total",
		Help: "Total Sandbox ID collisions by detection surface.",
	}, []string{"surface"})
)

// RecordSandboxIDLegacyResolution records one legacy Sandbox ID resolution.
func RecordSandboxIDLegacyResolution(surface string) {
	if !allowedLabel(surface, LegacyResolutionSurfaceE2B, LegacyResolutionSurfaceGateway) {
		return
	}
	sandboxIDLegacyResolutionTotal.WithLabelValues(surface).Inc()
}

// RecordSandboxIDAssignment records one short Sandbox ID assignment result.
func RecordSandboxIDAssignment(result string) {
	if !allowedLabel(result, SandboxIDAssignmentResultSuccess, SandboxIDAssignmentResultFailure) {
		return
	}
	sandboxIDAssignmentTotal.WithLabelValues(result).Inc()
}

// RecordSandboxIDCollision records one ambiguous Sandbox ID collision.
func RecordSandboxIDCollision(surface string) {
	if !allowedLabel(surface, CollisionSurfaceCache, CollisionSurfaceManagerRoute, CollisionSurfaceGatewayRoute) {
		return
	}
	sandboxIDCollisionTotal.WithLabelValues(surface).Inc()
}
