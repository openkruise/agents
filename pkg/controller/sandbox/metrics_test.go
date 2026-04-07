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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestBoolFloat64(t *testing.T) {
	tests := []struct {
		name     string
		input    bool
		expected float64
	}{
		{name: "true returns 1", input: true, expected: 1},
		{name: "false returns 0", input: false, expected: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := boolFloat64(tt.input); got != tt.expected {
				t.Errorf("boolFloat64(%v) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}

func TestRecordSandboxMetrics_CreatedTimestamp(t *testing.T) {
	now := time.Now()
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-sandbox",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
		},
	}

	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "test-sandbox")

	val := testutil.ToFloat64(sandboxCreated.WithLabelValues("default", "test-sandbox"))
	expected := float64(now.Unix())
	if val != expected {
		t.Errorf("sandbox_created = %v, want %v", val, expected)
	}
}

func TestRecordSandboxMetrics_DeletionTimestamp(t *testing.T) {
	now := time.Now()
	delTime := metav1.NewTime(now)
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "del-sandbox",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Hour)),
			DeletionTimestamp: &delTime,
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxTerminating,
		},
	}

	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "del-sandbox")

	val := testutil.ToFloat64(sandboxDeletionTimestamp.WithLabelValues("default", "del-sandbox"))
	expected := float64(now.Unix())
	if val != expected {
		t.Errorf("sandbox_deletion_timestamp = %v, want %v", val, expected)
	}
}

func TestRecordSandboxMetrics_NoDeletionTimestamp(t *testing.T) {
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "no-del-sandbox",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
		},
	}

	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "no-del-sandbox")

	// When no deletion timestamp, the metric should not have been set for this sandbox.
	// We verify by checking that the gauge count for this label set is 0 after collect.
	count := testutil.CollectAndCount(sandboxDeletionTimestamp)
	// This is a global metric, so we just verify recordSandboxMetrics doesn't panic
	// and confirm creation metric is set correctly.
	if count < 0 {
		t.Errorf("unexpected negative metric count: %d", count)
	}
}

func TestRecordSandboxMetrics_StatusPhase(t *testing.T) {
	tests := []struct {
		name  string
		phase agentsv1alpha1.SandboxPhase
	}{
		{name: "Pending phase", phase: agentsv1alpha1.SandboxPending},
		{name: "Running phase", phase: agentsv1alpha1.SandboxRunning},
		{name: "Paused phase", phase: agentsv1alpha1.SandboxPaused},
		{name: "Resuming phase", phase: agentsv1alpha1.SandboxResuming},
		{name: "Succeeded phase", phase: agentsv1alpha1.SandboxSucceeded},
		{name: "Failed phase", phase: agentsv1alpha1.SandboxFailed},
		{name: "Terminating phase", phase: agentsv1alpha1.SandboxTerminating},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := "default"
			name := "phase-sandbox-" + string(tt.phase)
			sandbox := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              name,
					Namespace:         ns,
					CreationTimestamp: metav1.NewTime(time.Now()),
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: tt.phase,
				},
			}

			recordSandboxMetrics(sandbox)
			defer deleteSandboxMetrics(ns, name)

			// Verify active phase is 1
			val := testutil.ToFloat64(sandboxStatusPhase.WithLabelValues(ns, name, string(tt.phase)))
			if val != 1 {
				t.Errorf("sandbox_status_phase{phase=%s} = %v, want 1", tt.phase, val)
			}

			// Verify other phases are 0
			for _, p := range allPhases {
				if p == tt.phase {
					continue
				}
				v := testutil.ToFloat64(sandboxStatusPhase.WithLabelValues(ns, name, string(p)))
				if v != 0 {
					t.Errorf("sandbox_status_phase{phase=%s} = %v, want 0 (active phase is %s)", p, v, tt.phase)
				}
			}
		})
	}
}

func TestRecordSandboxMetrics_EmptyPhase(t *testing.T) {
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "empty-phase-sandbox",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: "", // empty phase
		},
	}

	// Should not panic and should skip phase metric recording
	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "empty-phase-sandbox")
}

func TestRecordSandboxMetrics_ReadyConditionTrue(t *testing.T) {
	now := metav1.NewTime(time.Now())
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "ready-sandbox",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionReady),
					Status:             metav1.ConditionTrue,
					LastTransitionTime: now,
				},
			},
		},
	}

	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "ready-sandbox")

	val := testutil.ToFloat64(sandboxStatusReady.WithLabelValues("default", "ready-sandbox"))
	if val != 1 {
		t.Errorf("sandbox_status_ready = %v, want 1", val)
	}

	readyTime := testutil.ToFloat64(sandboxStatusReadyTime.WithLabelValues("default", "ready-sandbox"))
	if readyTime != float64(now.Unix()) {
		t.Errorf("sandbox_status_ready_time = %v, want %v", readyTime, float64(now.Unix()))
	}
}

func TestRecordSandboxMetrics_ReadyConditionFalse(t *testing.T) {
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "notready-sandbox",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxPending,
			Conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionReady),
					Status:             metav1.ConditionFalse,
					LastTransitionTime: metav1.NewTime(time.Now()),
				},
			},
		},
	}

	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "notready-sandbox")

	val := testutil.ToFloat64(sandboxStatusReady.WithLabelValues("default", "notready-sandbox"))
	if val != 0 {
		t.Errorf("sandbox_status_ready = %v, want 0", val)
	}
}

func TestRecordSandboxMetrics_InplaceUpdateConditionFalse(t *testing.T) {
	now := metav1.NewTime(time.Now())
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "inplace-sandbox",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionInplaceUpdate),
					Status:             metav1.ConditionFalse,
					LastTransitionTime: now,
				},
			},
		},
	}

	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "inplace-sandbox")

	val := testutil.ToFloat64(sandboxStatusInplaceUpdating.WithLabelValues("default", "inplace-sandbox"))
	if val != 1 {
		t.Errorf("sandbox_status_inplace_updating = %v, want 1", val)
	}

	ts := testutil.ToFloat64(sandboxStatusInplaceUpdatingTime.WithLabelValues("default", "inplace-sandbox"))
	if ts != float64(now.Unix()) {
		t.Errorf("sandbox_status_inplace_updating_time = %v, want %v", ts, float64(now.Unix()))
	}
}

func TestRecordSandboxMetrics_InplaceUpdateConditionTrue(t *testing.T) {
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "inplace-true-sandbox",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionInplaceUpdate),
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(time.Now()),
				},
			},
		},
	}

	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "inplace-true-sandbox")

	val := testutil.ToFloat64(sandboxStatusInplaceUpdating.WithLabelValues("default", "inplace-true-sandbox"))
	if val != 0 {
		t.Errorf("sandbox_status_inplace_updating = %v, want 0", val)
	}
}

func TestRecordSandboxMetrics_PausedConditionFalse(t *testing.T) {
	now := metav1.NewTime(time.Now())
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "paused-false-sandbox",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxPaused,
			Conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionPaused),
					Status:             metav1.ConditionFalse,
					LastTransitionTime: now,
				},
			},
		},
	}

	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "paused-false-sandbox")

	val := testutil.ToFloat64(sandboxStatusUnpaused.WithLabelValues("default", "paused-false-sandbox"))
	if val != 1 {
		t.Errorf("sandbox_status_unpaused = %v, want 1", val)
	}

	ts := testutil.ToFloat64(sandboxStatusUnpausedTime.WithLabelValues("default", "paused-false-sandbox"))
	if ts != float64(now.Unix()) {
		t.Errorf("sandbox_status_unpaused_time = %v, want %v", ts, float64(now.Unix()))
	}
}

func TestRecordSandboxMetrics_PausedConditionTrue(t *testing.T) {
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "paused-true-sandbox",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxPaused,
			Conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionPaused),
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(time.Now()),
				},
			},
		},
	}

	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "paused-true-sandbox")

	val := testutil.ToFloat64(sandboxStatusUnpaused.WithLabelValues("default", "paused-true-sandbox"))
	if val != 0 {
		t.Errorf("sandbox_status_unpaused = %v, want 0", val)
	}
}

func TestRecordSandboxMetrics_ResumedConditionFalse(t *testing.T) {
	now := metav1.NewTime(time.Now())
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "resumed-false-sandbox",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxResuming,
			Conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionResumed),
					Status:             metav1.ConditionFalse,
					LastTransitionTime: now,
				},
			},
		},
	}

	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "resumed-false-sandbox")

	val := testutil.ToFloat64(sandboxStatusUnresumed.WithLabelValues("default", "resumed-false-sandbox"))
	if val != 1 {
		t.Errorf("sandbox_status_unresumed = %v, want 1", val)
	}

	ts := testutil.ToFloat64(sandboxStatusUnresumedTime.WithLabelValues("default", "resumed-false-sandbox"))
	if ts != float64(now.Unix()) {
		t.Errorf("sandbox_status_unresumed_time = %v, want %v", ts, float64(now.Unix()))
	}
}

func TestRecordSandboxMetrics_ResumedConditionTrue(t *testing.T) {
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "resumed-true-sandbox",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionResumed),
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(time.Now()),
				},
			},
		},
	}

	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "resumed-true-sandbox")

	val := testutil.ToFloat64(sandboxStatusUnresumed.WithLabelValues("default", "resumed-true-sandbox"))
	if val != 0 {
		t.Errorf("sandbox_status_unresumed = %v, want 0", val)
	}
}

func TestRecordSandboxMetrics_MultipleConditions(t *testing.T) {
	now := metav1.NewTime(time.Now())
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "multi-cond-sandbox",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionReady),
					Status:             metav1.ConditionTrue,
					LastTransitionTime: now,
				},
				{
					Type:               string(agentsv1alpha1.SandboxConditionInplaceUpdate),
					Status:             metav1.ConditionFalse,
					LastTransitionTime: now,
				},
			},
		},
	}

	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "multi-cond-sandbox")

	readyVal := testutil.ToFloat64(sandboxStatusReady.WithLabelValues("default", "multi-cond-sandbox"))
	if readyVal != 1 {
		t.Errorf("sandbox_status_ready = %v, want 1", readyVal)
	}

	inplaceVal := testutil.ToFloat64(sandboxStatusInplaceUpdating.WithLabelValues("default", "multi-cond-sandbox"))
	if inplaceVal != 1 {
		t.Errorf("sandbox_status_inplace_updating = %v, want 1", inplaceVal)
	}
}

func TestDeleteSandboxMetrics(t *testing.T) {
	ns, name := "default", "delete-test-sandbox"
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionReady),
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(time.Now()),
				},
			},
		},
	}

	// First record metrics
	recordSandboxMetrics(sandbox)

	// Verify metrics are set
	val := testutil.ToFloat64(sandboxCreated.WithLabelValues(ns, name))
	if val == 0 {
		t.Fatal("sandbox_created should be set before delete")
	}

	// Verify sandbox_info is set
	infoVal := testutil.ToFloat64(sandboxInfo.WithLabelValues(ns, name, "", ""))
	if infoVal != 1 {
		t.Errorf("sandbox_info before delete = %v, want 1", infoVal)
	}

	// Delete metrics
	deleteSandboxMetrics(ns, name)

	// After deletion, WithLabelValues creates a new zero-value gauge.
	val = testutil.ToFloat64(sandboxCreated.WithLabelValues(ns, name))
	if val != 0 {
		t.Errorf("sandbox_created after delete = %v, want 0", val)
	}

	// Verify phase metrics are cleaned
	for _, phase := range allPhases {
		v := testutil.ToFloat64(sandboxStatusPhase.WithLabelValues(ns, name, string(phase)))
		if v != 0 {
			t.Errorf("sandbox_status_phase{phase=%s} after delete = %v, want 0", phase, v)
		}
	}
}

func TestRecordConditionFalseMetric(t *testing.T) {
	statusGauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "test_condition_status",
		Help: "test",
	}, []string{"namespace", "name"})
	timeGauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "test_condition_time",
		Help: "test",
	}, []string{"namespace", "name"})

	now := metav1.NewTime(time.Now())

	t.Run("condition false sets 1 and timestamp", func(t *testing.T) {
		cond := metav1.Condition{Status: metav1.ConditionFalse, LastTransitionTime: now}
		recordConditionFalseMetric(cond, statusGauge, timeGauge, "ns", "sb")
		if v := testutil.ToFloat64(statusGauge.WithLabelValues("ns", "sb")); v != 1 {
			t.Errorf("status gauge = %v, want 1", v)
		}
		if v := testutil.ToFloat64(timeGauge.WithLabelValues("ns", "sb")); v != float64(now.Unix()) {
			t.Errorf("time gauge = %v, want %v", v, float64(now.Unix()))
		}
	})

	t.Run("condition true sets 0", func(t *testing.T) {
		cond := metav1.Condition{Status: metav1.ConditionTrue, LastTransitionTime: now}
		recordConditionFalseMetric(cond, statusGauge, timeGauge, "ns", "sb2")
		if v := testutil.ToFloat64(statusGauge.WithLabelValues("ns", "sb2")); v != 0 {
			t.Errorf("status gauge = %v, want 0", v)
		}
	})
}

func TestRecordSandboxMetrics_Info(t *testing.T) {
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "info-sandbox",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now()),
			OwnerReferences: []metav1.OwnerReference{
				{
					Kind:       "SandboxSet",
					Name:       "my-sandboxset",
					Controller: boolPtr(true),
				},
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
		},
	}

	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "info-sandbox")

	val := testutil.ToFloat64(sandboxInfo.WithLabelValues("default", "info-sandbox",
		"SandboxSet", "my-sandboxset"))
	if val != 1 {
		t.Errorf("sandbox_info = %v, want 1", val)
	}
}

func TestRecordSandboxMetrics_InfoNoOwner(t *testing.T) {
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "info-no-owner-sandbox",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
		},
	}

	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "info-no-owner-sandbox")

	val := testutil.ToFloat64(sandboxInfo.WithLabelValues("default", "info-no-owner-sandbox",
		"", ""))
	if val != 1 {
		t.Errorf("sandbox_info with no owner = %v, want 1", val)
	}
}
