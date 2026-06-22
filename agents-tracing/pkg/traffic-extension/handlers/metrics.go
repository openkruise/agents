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

package handlers

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var unauthenticatedRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "traffic_extension_unauthenticated_requests_total",
	Help: "Total number of requests that could not be matched to a SecurityProfile due to missing pod identity or corrupted labels.",
}, []string{"reason", "outcome"})

func init() {
	metrics.Registry.MustRegister(unauthenticatedRequestsTotal)
}
