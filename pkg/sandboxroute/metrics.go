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

package sandboxroute

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Surface identifies the component that owns a route Store.
type Surface string

const (
	SurfaceManager Surface = "manager"
	SurfaceGateway Surface = "gateway"
)

// Operation identifies a route mutation operation.
type Operation string

const (
	OperationUpsert Operation = "upsert"
	OperationDelete Operation = "delete"
)

// EventResult identifies the result of a route mutation event.
type EventResult string

const (
	EventResultApplied        EventResult = "applied"
	EventResultIgnored        EventResult = "ignored"
	EventResultInvalid        EventResult = "invalid"
	EventResultCollision      EventResult = "collision"
	EventResultRepairRequired EventResult = "repair_required"
)

// RepairResult identifies an aggregate targeted repair outcome.
type RepairResult string

const (
	RepairResultEnqueued        RepairResult = "enqueued"
	RepairResultSuccess         RepairResult = "success"
	RepairResultGetError        RepairResult = "get_error"
	RepairResultProjectionError RepairResult = "projection_error"
	RepairResultStale           RepairResult = "stale"
)

var (
	routeLegacyFallbackTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_route_legacy_fallback_total",
			Help: "Total successful legacy delete fallbacks that removed an ID-only route.",
		},
		[]string{"surface"},
	)
	routeEventTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_route_event_total",
			Help: "Total route mutation events by shape, operation, and result.",
		},
		[]string{"surface", "shape", "operation", "result"},
	)
	routeRecords = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_route_records",
			Help: "Current route records by shape.",
		},
		[]string{"surface", "shape"},
	)
	routeRepairTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_route_repair_total",
			Help: "Total targeted route repairs by result.",
		},
		[]string{"surface", "result"},
	)
	routeRepairQueueDepth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_route_repair_queue_depth",
			Help: "Current number of queued targeted route repairs.",
		},
		[]string{"surface"},
	)
	routeRepairRetriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_route_repair_retries_total",
			Help: "Total targeted route repair retries.",
		},
		[]string{"surface"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		routeLegacyFallbackTotal,
		routeEventTotal,
		routeRecords,
		routeRepairTotal,
		routeRepairQueueDepth,
		routeRepairRetriesTotal,
	)
}

func recordLegacyDeleteFallback(surface Surface) {
	routeLegacyFallbackTotal.WithLabelValues(string(surface)).Inc()
}

func recordEvent(surface Surface, shape Shape, operation Operation, result EventResult) {
	routeEventTotal.WithLabelValues(string(surface), string(shape), string(operation), string(result)).Inc()
}

func setRecords(surface Surface, shape Shape, count int) {
	if count >= 0 {
		routeRecords.WithLabelValues(string(surface), string(shape)).Set(float64(count))
	}
}

func recordRepair(surface Surface, result RepairResult) {
	routeRepairTotal.WithLabelValues(string(surface), string(result)).Inc()
}

func setRepairQueueDepth(surface Surface, count int) {
	if count >= 0 {
		routeRepairQueueDepth.WithLabelValues(string(surface)).Set(float64(count))
	}
}

func recordRepairRetry(surface Surface) {
	routeRepairRetriesTotal.WithLabelValues(string(surface)).Inc()
}
