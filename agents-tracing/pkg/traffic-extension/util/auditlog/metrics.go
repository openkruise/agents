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

package auditlog

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// AuditLogDroppedTotal counts audit log entries discarded because the
// buffered worker's input channel was full. Operators can watch this to
// decide whether to raise --audit-log-buffer-size.
var AuditLogDroppedTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "traffic_extension_audit_log_dropped_total",
	Help: "Total number of audit log entries dropped because the in-memory buffer was full.",
})

func init() {
	metrics.Registry.MustRegister(AuditLogDroppedTotal)
}
