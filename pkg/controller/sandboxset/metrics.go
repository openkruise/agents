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

package sandboxset

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// SandboxSetReplicas tracks the total number of replicas in each SandboxSet
	SandboxSetReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandboxset_replicas",
			Help: "Current number of replicas in the SandboxSet",
		},
		[]string{"namespace", "name"},
	)

	// SandboxSetAvailableReplicas tracks the number of available replicas in each SandboxSet
	SandboxSetAvailableReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandboxset_available_replicas",
			Help: "Current number of available replicas in the SandboxSet",
		},
		[]string{"namespace", "name"},
	)

	// SandboxSetDesiredReplicas tracks the desired number of replicas in each SandboxSet
	SandboxSetDesiredReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandboxset_desired_replicas",
			Help: "Desired number of replicas in the SandboxSet",
		},
		[]string{"namespace", "name"},
	)
)

func init() {
	// Register custom metrics with the global prometheus registry
	metrics.Registry.MustRegister(SandboxSetReplicas, SandboxSetAvailableReplicas, SandboxSetDesiredReplicas)
}
