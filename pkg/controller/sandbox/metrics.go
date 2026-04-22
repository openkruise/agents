/*
Copyright 2025.

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

package sandbox

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

var (
	// sandboxInfo records sandbox metadata as metric labels.
	sandboxInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_info",
			Help: "Information about the sandbox",
		},
		[]string{"namespace", "name", "created_by_kind", "created_by_name"},
	)

	// sandboxCreated records the creation timestamp of a sandbox.
	sandboxCreated = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_created",
			Help: "Unix creation timestamp of the sandbox",
		},
		[]string{"namespace", "name"},
	)

	// sandboxDeletionTimestamp records the deletion timestamp of a sandbox.
	sandboxDeletionTimestamp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_deletion_timestamp",
			Help: "Unix deletion timestamp of the sandbox",
		},
		[]string{"namespace", "name"},
	)

	// sandboxStatusPhase represents the current phase of a sandbox.
	// Each phase is a separate label value with gauge value 1 for the active phase.
	sandboxStatusPhase = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_status_phase",
			Help: "The current phase of the sandbox (1 for active phase)",
		},
		[]string{"namespace", "name", "phase"},
	)

	// sandboxStatusReady indicates whether the sandbox is in Ready condition (1=ready, 0=not ready).
	sandboxStatusReady = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_status_ready",
			Help: "Whether the sandbox is in Ready condition (1 for true, 0 for false)",
		},
		[]string{"namespace", "name"},
	)

	// sandboxStatusReadyTime records the timestamp when the sandbox became Ready.
	sandboxStatusReadyTime = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_status_ready_time",
			Help: "Unix timestamp when the sandbox last transitioned to Ready",
		},
		[]string{"namespace", "name"},
	)

	// sandboxStatusInplaceUpdating indicates whether the sandbox inplace update condition is False
	// (1 when InplaceUpdate condition status is False, similar to kube_pod_status_unschedulable).
	sandboxStatusInplaceUpdating = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_status_inplace_updating",
			Help: "Whether the sandbox InplaceUpdate condition is False (1 for False, 0 otherwise)",
		},
		[]string{"namespace", "name"},
	)

	// sandboxStatusInplaceUpdatingTime records the timestamp when InplaceUpdate condition became False,
	// similar to kube_pod_status_unscheduled_time.
	sandboxStatusInplaceUpdatingTime = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_status_inplace_updating_time",
			Help: "Unix timestamp when the sandbox InplaceUpdate condition transitioned to False",
		},
		[]string{"namespace", "name"},
	)

	// sandboxStatusUnpaused indicates whether the sandbox paused condition is False
	// (1 when SandboxPaused condition status is False, similar to kube_pod_status_unschedulable).
	sandboxStatusUnpaused = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_status_unpaused",
			Help: "Whether the sandbox SandboxPaused condition is False (1 for False, 0 otherwise)",
		},
		[]string{"namespace", "name"},
	)

	// sandboxStatusUnpausedTime records the timestamp when SandboxPaused condition became False,
	// similar to kube_pod_status_unscheduled_time.
	sandboxStatusUnpausedTime = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_status_unpaused_time",
			Help: "Unix timestamp when the sandbox SandboxPaused condition transitioned to False",
		},
		[]string{"namespace", "name"},
	)

	// sandboxStatusUnresumed indicates whether the sandbox resumed condition is False
	// (1 when SandboxResumed condition status is False, similar to kube_pod_status_unschedulable).
	sandboxStatusUnresumed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_status_unresumed",
			Help: "Whether the sandbox SandboxResumed condition is False (1 for False, 0 otherwise)",
		},
		[]string{"namespace", "name"},
	)

	// sandboxStatusUnresumedTime records the timestamp when SandboxResumed condition became False,
	// similar to kube_pod_status_unscheduled_time.
	sandboxStatusUnresumedTime = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_status_unresumed_time",
			Help: "Unix timestamp when the sandbox SandboxResumed condition transitioned to False",
		},
		[]string{"namespace", "name"},
	)

	// sandboxStatusNotReady indicates whether the sandbox is in NotReady condition
	// (1 when Ready condition status is False, 0 otherwise).
	sandboxStatusNotReady = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_status_not_ready",
			Help: "Whether the sandbox Ready condition is False (1 for False, 0 otherwise)",
		},
		[]string{"namespace", "name"},
	)

	// sandboxStatusNotReadyTime records the timestamp when Ready condition became False.
	sandboxStatusNotReadyTime = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_status_not_ready_time",
			Help: "Unix timestamp when the sandbox Ready condition transitioned to False",
		},
		[]string{"namespace", "name"},
	)

	// sandboxStatusPaused indicates whether the sandbox paused condition is True
	// (1 when SandboxPaused condition status is True, 0 otherwise).
	sandboxStatusPaused = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_status_paused",
			Help: "Whether the sandbox SandboxPaused condition is True (1 for True, 0 otherwise)",
		},
		[]string{"namespace", "name"},
	)

	// sandboxStatusPausedTime records the timestamp when SandboxPaused condition became True.
	sandboxStatusPausedTime = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_status_paused_time",
			Help: "Unix timestamp when the sandbox SandboxPaused condition transitioned to True",
		},
		[]string{"namespace", "name"},
	)

	// sandboxStatusResumed indicates whether the sandbox resumed condition is True
	// (1 when SandboxResumed condition status is True, 0 otherwise).
	sandboxStatusResumed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_status_resumed",
			Help: "Whether the sandbox SandboxResumed condition is True (1 for True, 0 otherwise)",
		},
		[]string{"namespace", "name"},
	)

	// sandboxStatusResumedTime records the timestamp when SandboxResumed condition became True.
	sandboxStatusResumedTime = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_status_resumed_time",
			Help: "Unix timestamp when the sandbox SandboxResumed condition transitioned to True",
		},
		[]string{"namespace", "name"},
	)

	// sandboxStatusInplaceUpdateDone indicates whether the sandbox inplace update condition is True
	// (1 when InplaceUpdate condition status is True, 0 otherwise).
	sandboxStatusInplaceUpdateDone = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_status_inplace_update_done",
			Help: "Whether the sandbox InplaceUpdate condition is True (1 for True, 0 otherwise)",
		},
		[]string{"namespace", "name"},
	)

	// sandboxStatusInplaceUpdateDoneTime records the timestamp when InplaceUpdate condition became True.
	sandboxStatusInplaceUpdateDoneTime = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_status_inplace_update_done_time",
			Help: "Unix timestamp when the sandbox InplaceUpdate condition transitioned to True",
		},
		[]string{"namespace", "name"},
	)

	// sandbox_creation_to_ready_duration_seconds
	sandboxCreationToReadyDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandbox_creation_to_ready_duration_seconds",
			Help:    "Duration from sandbox creation to Ready condition in seconds",
			Buckets: []float64{1, 2, 5, 10, 20, 30, 60, 120, 300, 600},
		},
	)

	// sandbox_inplace_update_duration_seconds
	sandboxInplaceUpdateDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandbox_inplace_update_duration_seconds",
			Help:    "Duration of in-place update operations from start to completion in seconds",
			Buckets: []float64{1, 2, 5, 10, 20, 30, 60, 120, 300, 600},
		},
	)

	// allPhases enumerates all possible sandbox phases for metric cleanup.
	allPhases = []agentsv1alpha1.SandboxPhase{
		agentsv1alpha1.SandboxPending,
		agentsv1alpha1.SandboxRunning,
		agentsv1alpha1.SandboxPaused,
		agentsv1alpha1.SandboxResuming,
		agentsv1alpha1.SandboxSucceeded,
		agentsv1alpha1.SandboxFailed,
		agentsv1alpha1.SandboxTerminating,
	}
)

// observedCreationToReady tracks which sandboxes have had their creation-to-ready
// duration observed, preventing duplicate histogram observations on re-reconcile.
var observedCreationToReady sync.Map

// inplaceUpdateStartTimes tracks the start time of in-place update operations
// (when InplaceUpdate condition transitions to False).
var inplaceUpdateStartTimes sync.Map

// observedInplaceUpdateDurations tracks which sandboxes have had their in-place update
// duration observed, preventing duplicate histogram observations.
var observedInplaceUpdateDurations sync.Map

func init() {
	metrics.Registry.MustRegister(
		sandboxCreated,
		sandboxDeletionTimestamp,
		sandboxStatusPhase,
		sandboxStatusReady,
		sandboxStatusReadyTime,
		sandboxStatusInplaceUpdating,
		sandboxStatusInplaceUpdatingTime,
		sandboxStatusUnpaused,
		sandboxStatusUnpausedTime,
		sandboxStatusUnresumed,
		sandboxStatusUnresumedTime,
		sandboxStatusNotReady,
		sandboxStatusNotReadyTime,
		sandboxStatusPaused,
		sandboxStatusPausedTime,
		sandboxStatusResumed,
		sandboxStatusResumedTime,
		sandboxStatusInplaceUpdateDone,
		sandboxStatusInplaceUpdateDoneTime,
		sandboxInfo,
		sandboxCreationToReadyDuration,
		sandboxInplaceUpdateDuration,
	)
}

// boolFloat64 converts a boolean to a float64 value (1.0 for true, 0.0 for false),
// following the kube-state-metrics convention.
func boolFloat64(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// recordConditionFalseMetric records a pair of condition metrics following the kube-state-metrics pattern:
// the status gauge is set to 1 when the condition is False, 0 otherwise;
// the time gauge records the transition timestamp when the condition is False.
func recordConditionFalseMetric(condition metav1.Condition, statusGauge, timeGauge *prometheus.GaugeVec, namespace, name string) {
	if condition.Status == metav1.ConditionFalse {
		statusGauge.WithLabelValues(namespace, name).Set(1)
		timeGauge.WithLabelValues(namespace, name).Set(float64(condition.LastTransitionTime.Unix()))
	} else {
		statusGauge.WithLabelValues(namespace, name).Set(0)
	}
}

// recordConditionTrueMetric records a pair of condition metrics following the kube-state-metrics pattern:
// the status gauge is set to 1 when the condition is True, 0 otherwise;
// the time gauge records the transition timestamp when the condition is True.
func recordConditionTrueMetric(condition metav1.Condition, statusGauge, timeGauge *prometheus.GaugeVec, namespace, name string) {
	if condition.Status == metav1.ConditionTrue {
		statusGauge.WithLabelValues(namespace, name).Set(1)
		timeGauge.WithLabelValues(namespace, name).Set(float64(condition.LastTransitionTime.Unix()))
	} else {
		statusGauge.WithLabelValues(namespace, name).Set(0)
	}
}

// recordSandboxMetrics updates all sandbox lifecycle metrics based on the current sandbox state.
func recordSandboxMetrics(sandbox *agentsv1alpha1.Sandbox) {
	namespace := sandbox.Namespace
	name := sandbox.Name

	// sandbox_info: sandbox metadata
	var createdByKind, createdByName string
	if controller := metav1.GetControllerOfNoCopy(sandbox); controller != nil {
		createdByKind = controller.Kind
		createdByName = controller.Name
	}
	sandboxInfo.WithLabelValues(namespace, name, createdByKind, createdByName).Set(1)

	// sandbox_created: creation timestamp
	sandboxCreated.WithLabelValues(namespace, name).Set(float64(sandbox.CreationTimestamp.Unix()))

	// sandbox_deletion_timestamp
	if sandbox.DeletionTimestamp != nil {
		sandboxDeletionTimestamp.WithLabelValues(namespace, name).Set(float64(sandbox.DeletionTimestamp.Unix()))
	}

	// sandbox_status_phase: following kube_pod_status_phase pattern,
	// skip if phase is empty, otherwise set 1 for current phase and 0 for all others.
	currentPhase := sandbox.Status.Phase
	if currentPhase != "" {
		for _, p := range allPhases {
			sandboxStatusPhase.WithLabelValues(namespace, name, string(p)).Set(boolFloat64(currentPhase == p))
		}
	}

	// Process conditions
	for _, condition := range sandbox.Status.Conditions {
		switch agentsv1alpha1.SandboxConditionType(condition.Type) {
		case agentsv1alpha1.SandboxConditionReady:
			isReady := condition.Status == metav1.ConditionTrue
			sandboxStatusReady.WithLabelValues(namespace, name).Set(boolFloat64(isReady))
			if isReady {
				sandboxStatusReadyTime.WithLabelValues(namespace, name).Set(float64(condition.LastTransitionTime.Unix()))
			}
			recordConditionFalseMetric(condition, sandboxStatusNotReady, sandboxStatusNotReadyTime, namespace, name)

			// Observe creation-to-ready duration histogram (once per sandbox)
			if isReady {
				key := namespace + "/" + name
				if _, loaded := observedCreationToReady.LoadOrStore(key, true); !loaded {
					duration := condition.LastTransitionTime.Sub(sandbox.CreationTimestamp.Time)
					sandboxCreationToReadyDuration.Observe(duration.Seconds())
				}
			}

		case agentsv1alpha1.SandboxConditionInplaceUpdate:
			recordConditionFalseMetric(condition, sandboxStatusInplaceUpdating, sandboxStatusInplaceUpdatingTime, namespace, name)
			recordConditionTrueMetric(condition, sandboxStatusInplaceUpdateDone, sandboxStatusInplaceUpdateDoneTime, namespace, name)

			key := namespace + "/" + name
			if condition.Status == metav1.ConditionFalse {
				// Store the start time when in-place update begins
				inplaceUpdateStartTimes.Store(key, condition.LastTransitionTime.Time)
			} else if condition.Status == metav1.ConditionTrue {
				// Observe duration when in-place update completes (once)
				if startTime, ok := inplaceUpdateStartTimes.Load(key); ok {
					if _, observed := observedInplaceUpdateDurations.LoadOrStore(key, true); !observed {
						duration := condition.LastTransitionTime.Sub(startTime.(time.Time))
						sandboxInplaceUpdateDuration.Observe(duration.Seconds())
					}
				}
			}

		case agentsv1alpha1.SandboxConditionPaused:
			recordConditionFalseMetric(condition, sandboxStatusUnpaused, sandboxStatusUnpausedTime, namespace, name)
			recordConditionTrueMetric(condition, sandboxStatusPaused, sandboxStatusPausedTime, namespace, name)

		case agentsv1alpha1.SandboxConditionResumed:
			recordConditionFalseMetric(condition, sandboxStatusUnresumed, sandboxStatusUnresumedTime, namespace, name)
			recordConditionTrueMetric(condition, sandboxStatusResumed, sandboxStatusResumedTime, namespace, name)
		}
	}
}

// deleteSandboxMetrics removes all metrics for a sandbox that has been deleted.
func deleteSandboxMetrics(namespace, name string) {
	sandboxInfo.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "name": name})
	sandboxCreated.DeleteLabelValues(namespace, name)
	sandboxDeletionTimestamp.DeleteLabelValues(namespace, name)
	for _, phase := range allPhases {
		sandboxStatusPhase.DeleteLabelValues(namespace, name, string(phase))
	}
	sandboxStatusReady.DeleteLabelValues(namespace, name)
	sandboxStatusReadyTime.DeleteLabelValues(namespace, name)
	sandboxStatusInplaceUpdating.DeleteLabelValues(namespace, name)
	sandboxStatusInplaceUpdatingTime.DeleteLabelValues(namespace, name)
	sandboxStatusUnpaused.DeleteLabelValues(namespace, name)
	sandboxStatusUnpausedTime.DeleteLabelValues(namespace, name)
	sandboxStatusUnresumed.DeleteLabelValues(namespace, name)
	sandboxStatusUnresumedTime.DeleteLabelValues(namespace, name)
	sandboxStatusNotReady.DeleteLabelValues(namespace, name)
	sandboxStatusNotReadyTime.DeleteLabelValues(namespace, name)
	sandboxStatusPaused.DeleteLabelValues(namespace, name)
	sandboxStatusPausedTime.DeleteLabelValues(namespace, name)
	sandboxStatusResumed.DeleteLabelValues(namespace, name)
	sandboxStatusResumedTime.DeleteLabelValues(namespace, name)
	sandboxStatusInplaceUpdateDone.DeleteLabelValues(namespace, name)
	sandboxStatusInplaceUpdateDoneTime.DeleteLabelValues(namespace, name)

	key := namespace + "/" + name
	observedCreationToReady.Delete(key)
	inplaceUpdateStartTimes.Delete(key)
	observedInplaceUpdateDurations.Delete(key)
}
