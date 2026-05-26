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

package metricsasync

import (
	"github.com/prometheus/client_golang/prometheus"
)

// collectors holds the self-observability metric vectors for a Pool.
// They are scoped to a configurable subsystem name so tests can use
// unique names and avoid panics from duplicate registration.
type collectors struct {
	queueDepth     *prometheus.GaugeVec
	processedTotal *prometheus.CounterVec
	duration       *prometheus.HistogramVec
	latency        *prometheus.HistogramVec
	dropped        *prometheus.CounterVec
}

func newCollectors(subsystem string) *collectors {
	return &collectors{
		queueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Subsystem: subsystem,
			Name:      "queue_depth",
			Help:      "Current depth of the metric async cleanup queue per kind",
		}, []string{"kind"}),
		processedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      "processed_total",
			Help:      "Total cleanup tasks processed by the metric async pool",
		}, []string{"kind", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Subsystem: subsystem,
			Name:      "duration_seconds",
			Help:      "Time spent inside the cleanup function",
			Buckets:   prometheus.ExponentialBuckets(0.001, 2, 12), // 1ms -> ~4s
		}, []string{"kind"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Subsystem: subsystem,
			Name:      "latency_seconds",
			Help:      "Time from Enqueue to start of cleanup function",
			Buckets:   prometheus.ExponentialBuckets(0.001, 2, 15), // 1ms -> ~32s
		}, []string{"kind"}),
		dropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      "dropped_total",
			Help:      "Tasks dropped without execution (unregistered, queue_full, drain_timeout)",
		}, []string{"kind", "reason"}),
	}
}

// register attaches the collectors to the supplied Registerer. Errors
// from prometheus (e.g., AlreadyRegisteredError) are returned unchanged
// so the caller can decide whether to ignore them.
func (c *collectors) register(reg prometheus.Registerer) error {
	for _, col := range []prometheus.Collector{c.queueDepth, c.processedTotal, c.duration, c.latency, c.dropped} {
		if err := reg.Register(col); err != nil {
			return err
		}
	}
	return nil
}
