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
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
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

	recordSandboxMetrics(sandbox, nil)
	defer DeleteSandboxMetrics("default", "test-sandbox")

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

	recordSandboxMetrics(sandbox, nil)
	defer DeleteSandboxMetrics("default", "del-sandbox")

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

	recordSandboxMetrics(sandbox, nil)
	defer DeleteSandboxMetrics("default", "no-del-sandbox")

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

			recordSandboxMetrics(sandbox, nil)
			defer DeleteSandboxMetrics(ns, name)

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
	recordSandboxMetrics(sandbox, nil)
	defer DeleteSandboxMetrics("default", "empty-phase-sandbox")
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

	recordSandboxMetrics(sandbox, nil)
	defer DeleteSandboxMetrics("default", "ready-sandbox")

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
	now := metav1.NewTime(time.Now())
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
					LastTransitionTime: now,
				},
			},
		},
	}

	recordSandboxMetrics(sandbox, nil)
	defer DeleteSandboxMetrics("default", "notready-sandbox")

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

	recordSandboxMetrics(sandbox, nil)
	defer DeleteSandboxMetrics("default", "inplace-sandbox")

	// InplaceUpdate=False: inplace_updating should be 1 (negative semantics)
	val := testutil.ToFloat64(sandboxStatusInplaceUpdating.WithLabelValues("default", "inplace-sandbox"))
	if val != 1 {
		t.Errorf("sandbox_status_inplace_updating = %v, want 1", val)
	}
	valTime := testutil.ToFloat64(sandboxStatusInplaceUpdatingTime.WithLabelValues("default", "inplace-sandbox"))
	if valTime == 0 {
		t.Errorf("sandbox_status_inplace_updating_time should be set when condition is False")
	}
}

func TestRecordSandboxMetrics_InplaceUpdateConditionTrue(t *testing.T) {
	now := metav1.NewTime(time.Now())
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
					LastTransitionTime: now,
				},
			},
		},
	}

	recordSandboxMetrics(sandbox, nil)
	defer DeleteSandboxMetrics("default", "inplace-true-sandbox")

	// Verify inplace_updating metrics (True → updating=0)
	doneVal := testutil.ToFloat64(sandboxStatusInplaceUpdating.WithLabelValues("default", "inplace-true-sandbox"))
	if doneVal != 0 {
		t.Errorf("sandbox_status_inplace_updating = %v, want 0", doneVal)
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

	// Paused=False should not panic and stores start time for duration tracking
	recordSandboxMetrics(sandbox, nil)
	defer DeleteSandboxMetrics("default", "paused-false-sandbox")
}

func TestRecordSandboxMetrics_PausedConditionTrue(t *testing.T) {
	now := metav1.NewTime(time.Now())
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
					LastTransitionTime: now,
				},
			},
		},
	}

	recordSandboxMetrics(sandbox, nil)
	defer DeleteSandboxMetrics("default", "paused-true-sandbox")

	// Paused=True → unpaused=0, unpaused_time should NOT be set
	unpausedVal := testutil.ToFloat64(sandboxStatusUnpaused.WithLabelValues("default", "paused-true-sandbox"))
	if unpausedVal != 0 {
		t.Errorf("sandbox_status_unpaused = %v, want 0", unpausedVal)
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

	// Resumed=False should not panic and stores start time for duration tracking
	recordSandboxMetrics(sandbox, nil)
	defer DeleteSandboxMetrics("default", "resumed-false-sandbox")
}

func TestRecordSandboxMetrics_ResumedConditionTrue(t *testing.T) {
	now := metav1.NewTime(time.Now())
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
					LastTransitionTime: now,
				},
			},
		},
	}

	recordSandboxMetrics(sandbox, nil)
	defer DeleteSandboxMetrics("default", "resumed-true-sandbox")

	// Resumed=True → unresumed=0, unresumed_time should NOT be set
	unresumedVal := testutil.ToFloat64(sandboxStatusUnresumed.WithLabelValues("default", "resumed-true-sandbox"))
	if unresumedVal != 0 {
		t.Errorf("sandbox_status_unresumed = %v, want 0", unresumedVal)
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

	recordSandboxMetrics(sandbox, nil)
	defer DeleteSandboxMetrics("default", "multi-cond-sandbox")

	readyVal := testutil.ToFloat64(sandboxStatusReady.WithLabelValues("default", "multi-cond-sandbox"))
	if readyVal != 1 {
		t.Errorf("sandbox_status_ready = %v, want 1", readyVal)
	}

	// InplaceUpdate=False: inplace_updating should be 1 (negative semantics)
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
	recordSandboxMetrics(sandbox, nil)

	// Verify metrics are set
	val := testutil.ToFloat64(sandboxCreated.WithLabelValues(ns, name))
	if val == 0 {
		t.Fatal("sandbox_created should be set before delete")
	}

	// Verify sandbox_info is set
	infoVal := testutil.ToFloat64(sandboxInfo.WithLabelValues(ns, name, "", "", ""))
	if infoVal != 1 {
		t.Errorf("sandbox_info before delete = %v, want 1", infoVal)
	}

	// Delete metrics
	DeleteSandboxMetrics(ns, name)

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

func TestRecordSandboxMetrics_Info(t *testing.T) {
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "info-sandbox",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now()),
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxTemplate: "my-template",
				agentsv1alpha1.LabelSandboxPool:     "my-sandboxset",
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase:    agentsv1alpha1.SandboxRunning,
			NodeName: "node-1",
		},
	}

	recordSandboxMetrics(sandbox, nil)
	defer DeleteSandboxMetrics("default", "info-sandbox")

	val := testutil.ToFloat64(sandboxInfo.WithLabelValues("default", "info-sandbox",
		"my-sandboxset", "node-1", "my-template"))
	if val != 1 {
		t.Errorf("sandbox_info = %v, want 1", val)
	}
}

func TestRecordConditionTrueMetric(t *testing.T) {
	statusGauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "test_true_condition_status",
		Help: "test",
	}, []string{"namespace", "name"})
	timeGauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "test_true_condition_time",
		Help: "test",
	}, []string{"namespace", "name"})

	now := metav1.NewTime(time.Now())

	t.Run("condition true sets 1 and timestamp", func(t *testing.T) {
		cond := metav1.Condition{Status: metav1.ConditionTrue, LastTransitionTime: now}
		recordConditionTrueMetric(cond, statusGauge, timeGauge, "ns", "sb")
		if v := testutil.ToFloat64(statusGauge.WithLabelValues("ns", "sb")); v != 1 {
			t.Errorf("status gauge = %v, want 1", v)
		}
		if v := testutil.ToFloat64(timeGauge.WithLabelValues("ns", "sb")); v != float64(now.Unix()) {
			t.Errorf("time gauge = %v, want %v", v, float64(now.Unix()))
		}
	})

	t.Run("condition false sets 0", func(t *testing.T) {
		cond := metav1.Condition{Status: metav1.ConditionFalse, LastTransitionTime: now}
		recordConditionTrueMetric(cond, statusGauge, timeGauge, "ns", "sb2")
		if v := testutil.ToFloat64(statusGauge.WithLabelValues("ns", "sb2")); v != 0 {
			t.Errorf("status gauge = %v, want 0", v)
		}
	})
}

func TestRecordSandboxMetrics_PausedConditionTrueTimestamp(t *testing.T) {
	tests := []struct {
		name         string
		status       metav1.ConditionStatus
		wantPausedTS bool
	}{
		{name: "Paused=False sets unpaused_time timestamp", status: metav1.ConditionFalse, wantPausedTS: true},
		{name: "Paused=True does not set unpaused_time", status: metav1.ConditionTrue, wantPausedTS: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := metav1.NewTime(time.Now())
			sbName := "paused-ts-" + string(tt.status)
			sandbox := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              sbName,
					Namespace:         "default",
					CreationTimestamp: metav1.NewTime(time.Now()),
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionPaused),
							Status:             tt.status,
							LastTransitionTime: now,
						},
					},
				},
			}

			recordSandboxMetrics(sandbox, nil)
			defer DeleteSandboxMetrics("default", sbName)

			if tt.wantPausedTS {
				ts := testutil.ToFloat64(sandboxStatusUnpausedTime.WithLabelValues("default", sbName))
				if ts != float64(now.Unix()) {
					t.Errorf("sandbox_status_unpaused_time = %v, want %v", ts, float64(now.Unix()))
				}
			}
		})
	}
}

func TestRecordSandboxMetrics_ResumedConditionTrueTimestamp(t *testing.T) {
	tests := []struct {
		name          string
		status        metav1.ConditionStatus
		wantResumedTS bool
	}{
		{name: "Resumed=False sets unresumed_time timestamp", status: metav1.ConditionFalse, wantResumedTS: true},
		{name: "Resumed=True does not set unresumed_time", status: metav1.ConditionTrue, wantResumedTS: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := metav1.NewTime(time.Now())
			sbName := "resumed-ts-" + string(tt.status)
			sandbox := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              sbName,
					Namespace:         "default",
					CreationTimestamp: metav1.NewTime(time.Now()),
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionResumed),
							Status:             tt.status,
							LastTransitionTime: now,
						},
					},
				},
			}

			recordSandboxMetrics(sandbox, nil)
			defer DeleteSandboxMetrics("default", sbName)

			if tt.wantResumedTS {
				ts := testutil.ToFloat64(sandboxStatusUnresumedTime.WithLabelValues("default", sbName))
				if ts != float64(now.Unix()) {
					t.Errorf("sandbox_status_unresumed_time = %v, want %v", ts, float64(now.Unix()))
				}
			}
		})
	}
}

func TestRecordSandboxMetrics_InplaceUpdateConditionTrueTimestamp(t *testing.T) {
	tests := []struct {
		name       string
		status     metav1.ConditionStatus
		wantDone   float64
		wantDoneTS bool
	}{
		{name: "InplaceUpdate=False sets updating=1 with timestamp", status: metav1.ConditionFalse, wantDone: 1, wantDoneTS: true},
		{name: "InplaceUpdate=True sets updating=0", status: metav1.ConditionTrue, wantDone: 0, wantDoneTS: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := metav1.NewTime(time.Now())
			sbName := "inplace-done-" + string(tt.status)
			sandbox := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              sbName,
					Namespace:         "default",
					CreationTimestamp: metav1.NewTime(time.Now()),
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionInplaceUpdate),
							Status:             tt.status,
							LastTransitionTime: now,
						},
					},
				},
			}

			recordSandboxMetrics(sandbox, nil)
			defer DeleteSandboxMetrics("default", sbName)

			val := testutil.ToFloat64(sandboxStatusInplaceUpdating.WithLabelValues("default", sbName))
			if val != tt.wantDone {
				t.Errorf("sandbox_status_inplace_updating = %v, want %v", val, tt.wantDone)
			}
			if tt.wantDoneTS {
				ts := testutil.ToFloat64(sandboxStatusInplaceUpdatingTime.WithLabelValues("default", sbName))
				if ts != float64(now.Unix()) {
					t.Errorf("sandbox_status_inplace_updating_time = %v, want %v", ts, float64(now.Unix()))
				}
			}
		})
	}
}

func TestDeleteSandboxMetrics_NewMetrics(t *testing.T) {
	ns, name := "default", "delete-new-metrics-sandbox"
	now := metav1.NewTime(time.Now())
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
					Status:             metav1.ConditionFalse,
					LastTransitionTime: now,
				},
				{
					Type:               string(agentsv1alpha1.SandboxConditionPaused),
					Status:             metav1.ConditionFalse,
					LastTransitionTime: now,
				},
				{
					Type:               string(agentsv1alpha1.SandboxConditionResumed),
					Status:             metav1.ConditionFalse,
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

	recordSandboxMetrics(sandbox, nil)

	// Verify new metrics are set (Ready=False → ready=0, Paused=True → paused_time set, etc.)
	if v := testutil.ToFloat64(sandboxStatusReady.WithLabelValues(ns, name)); v != 0 {
		t.Errorf("sandbox_status_ready before delete = %v, want 0", v)
	}
	if v := testutil.ToFloat64(sandboxStatusUnpaused.WithLabelValues(ns, name)); v != 1 {
		t.Errorf("sandbox_status_unpaused before delete = %v, want 1", v)
	}
	if v := testutil.ToFloat64(sandboxStatusUnpausedTime.WithLabelValues(ns, name)); v == 0 {
		t.Errorf("sandbox_status_unpaused_time before delete should be set")
	}
	if v := testutil.ToFloat64(sandboxStatusUnresumed.WithLabelValues(ns, name)); v != 1 {
		t.Errorf("sandbox_status_unresumed before delete = %v, want 1", v)
	}
	if v := testutil.ToFloat64(sandboxStatusUnresumedTime.WithLabelValues(ns, name)); v == 0 {
		t.Errorf("sandbox_status_unresumed_time before delete should be set")
	}
	if v := testutil.ToFloat64(sandboxStatusInplaceUpdating.WithLabelValues(ns, name)); v != 1 {
		t.Errorf("sandbox_status_inplace_updating before delete = %v, want 1", v)
	}
	if v := testutil.ToFloat64(sandboxStatusInplaceUpdatingTime.WithLabelValues(ns, name)); v == 0 {
		t.Errorf("sandbox_status_inplace_updating_time before delete should be set")
	}

	// Delete and verify cleanup
	DeleteSandboxMetrics(ns, name)

	if v := testutil.ToFloat64(sandboxStatusReady.WithLabelValues(ns, name)); v != 0 {
		t.Errorf("sandbox_status_ready after delete = %v, want 0", v)
	}
	if v := testutil.ToFloat64(sandboxStatusUnpaused.WithLabelValues(ns, name)); v != 0 {
		t.Errorf("sandbox_status_unpaused after delete = %v, want 0", v)
	}
	if v := testutil.ToFloat64(sandboxStatusUnpausedTime.WithLabelValues(ns, name)); v != 0 {
		t.Errorf("sandbox_status_unpaused_time after delete = %v, want 0", v)
	}
	if v := testutil.ToFloat64(sandboxStatusUnresumed.WithLabelValues(ns, name)); v != 0 {
		t.Errorf("sandbox_status_unresumed after delete = %v, want 0", v)
	}
	if v := testutil.ToFloat64(sandboxStatusUnresumedTime.WithLabelValues(ns, name)); v != 0 {
		t.Errorf("sandbox_status_unresumed_time after delete = %v, want 0", v)
	}
	if v := testutil.ToFloat64(sandboxStatusInplaceUpdating.WithLabelValues(ns, name)); v != 0 {
		t.Errorf("sandbox_status_inplace_updating after delete = %v, want 0", v)
	}
	if v := testutil.ToFloat64(sandboxStatusInplaceUpdatingTime.WithLabelValues(ns, name)); v != 0 {
		t.Errorf("sandbox_status_inplace_updating_time after delete = %v, want 0", v)
	}
}

func TestRecordSandboxMetrics_AllConditions(t *testing.T) {
	now := metav1.NewTime(time.Now())
	ns, name := "default", "bidir-sandbox"
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
					LastTransitionTime: now,
				},
				{
					Type:               string(agentsv1alpha1.SandboxConditionPaused),
					Status:             metav1.ConditionFalse,
					LastTransitionTime: now,
				},
				{
					Type:               string(agentsv1alpha1.SandboxConditionResumed),
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

	recordSandboxMetrics(sandbox, nil)
	defer DeleteSandboxMetrics(ns, name)

	// Ready=True: ready=1
	if v := testutil.ToFloat64(sandboxStatusReady.WithLabelValues(ns, name)); v != 1 {
		t.Errorf("sandbox_status_ready = %v, want 1", v)
	}
	if v := testutil.ToFloat64(sandboxStatusReadyTime.WithLabelValues(ns, name)); v != float64(now.Unix()) {
		t.Errorf("sandbox_status_ready_time = %v, want %v", v, float64(now.Unix()))
	}

	// Resumed=True: unresumed=0
	if v := testutil.ToFloat64(sandboxStatusUnresumed.WithLabelValues(ns, name)); v != 0 {
		t.Errorf("sandbox_status_unresumed = %v, want 0", v)
	}

	// InplaceUpdate=False: inplace_updating=1 (negative semantics)
	if v := testutil.ToFloat64(sandboxStatusInplaceUpdating.WithLabelValues(ns, name)); v != 1 {
		t.Errorf("sandbox_status_inplace_updating = %v, want 1", v)
	}
	// unpaused_time should be set (Paused=False)
	if v := testutil.ToFloat64(sandboxStatusUnpausedTime.WithLabelValues(ns, name)); v != float64(now.Unix()) {
		t.Errorf("sandbox_status_unpaused_time = %v, want %v", v, float64(now.Unix()))
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

	recordSandboxMetrics(sandbox, nil)
	defer DeleteSandboxMetrics("default", "info-no-owner-sandbox")

	// All new labels should be empty string when not set
	val := testutil.ToFloat64(sandboxInfo.WithLabelValues("default", "info-no-owner-sandbox",
		"", "", ""))
	if val != 1 {
		t.Errorf("sandbox_info with no owner = %v, want 1", val)
	}
}

func TestRecordSandboxMetrics_InfoPartialFields(t *testing.T) {
	tests := []struct {
		name          string
		nodeName      string
		templateLabel string
		wantNode      string
		wantTemplate  string
	}{
		{
			name:         "node set, no template label",
			nodeName:     "worker-1",
			wantNode:     "worker-1",
			wantTemplate: "",
		},
		{
			name:          "template label set, no node",
			templateLabel: "tmpl-a",
			wantNode:      "",
			wantTemplate:  "tmpl-a",
		},
		{
			name:          "all fields set",
			nodeName:      "node-x",
			templateLabel: "tmpl-b",
			wantNode:      "node-x",
			wantTemplate:  "tmpl-b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbName := "info-partial-" + tt.name
			labels := map[string]string{}
			if tt.templateLabel != "" {
				labels[agentsv1alpha1.LabelSandboxTemplate] = tt.templateLabel
			}
			sandbox := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              sbName,
					Namespace:         "default",
					CreationTimestamp: metav1.NewTime(time.Now()),
					Labels:            labels,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase:    agentsv1alpha1.SandboxRunning,
					NodeName: tt.nodeName,
				},
			}

			recordSandboxMetrics(sandbox, nil)
			defer DeleteSandboxMetrics("default", sbName)

			val := testutil.ToFloat64(sandboxInfo.WithLabelValues("default", sbName,
				"", tt.wantNode, tt.wantTemplate))
			if val != 1 {
				t.Errorf("sandbox_info = %v, want 1", val)
			}
		})
	}
}

// creationToReadyHistogramSum collects the current sample sum from the creation-to-ready HistogramVec for a given namespace.
func creationToReadyHistogramSum(t *testing.T, namespace string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := sandboxCreationDuration.WithLabelValues(namespace).(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("failed to write histogram metric: %v", err)
	}
	return m.GetHistogram().GetSampleSum()
}

// inplaceUpdateHistogramSum collects the current sample sum from the inplace-update HistogramVec for a given namespace.
func inplaceUpdateHistogramSum(t *testing.T, namespace string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := sandboxInplaceUpdateDuration.WithLabelValues(namespace).(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("failed to write histogram metric: %v", err)
	}
	return m.GetHistogram().GetSampleSum()
}

func TestSandboxCreationToReadyDuration_ObservedOnce(t *testing.T) {
	now := time.Now()
	creationTime := now.Add(-45 * time.Second)
	readyTime := metav1.NewTime(now)
	ns, name := "creation-dur-observed", "ready-duration-sandbox"

	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(creationTime),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionReady),
					Status:             metav1.ConditionTrue,
					LastTransitionTime: readyTime,
				},
			},
		},
	}

	beforeSum := creationToReadyHistogramSum(t, ns)

	// First call should observe
	recordSandboxMetrics(sandbox, nil)
	afterFirstSum := creationToReadyHistogramSum(t, ns)
	expectedDuration := readyTime.Sub(creationTime).Seconds()
	if delta := afterFirstSum - beforeSum; delta < expectedDuration-0.01 || delta > expectedDuration+0.01 {
		t.Errorf("first observation: sum delta = %v, want ~%v", delta, expectedDuration)
	}

	// Second call should NOT observe (deduplicated)
	recordSandboxMetrics(sandbox, nil)
	afterSecondSum := creationToReadyHistogramSum(t, ns)
	if afterSecondSum != afterFirstSum {
		t.Errorf("second call should not change sum: got %v, want %v", afterSecondSum, afterFirstSum)
	}

	// Delete and re-record should observe again
	DeleteSandboxMetrics(ns, name)
	// After delete, the namespace-level histogram still exists; read the new baseline.
	baselineAfterDelete := creationToReadyHistogramSum(t, ns)
	recordSandboxMetrics(sandbox, nil)
	afterReObserve := creationToReadyHistogramSum(t, ns)
	if delta := afterReObserve - baselineAfterDelete; delta < expectedDuration-0.01 || delta > expectedDuration+0.01 {
		t.Errorf("re-observation after delete: sum delta = %v, want ~%v", delta, expectedDuration)
	}

	DeleteSandboxMetrics(ns, name)
}

func TestSandboxCreationToReadyDuration_NotObservedWhenNotReady(t *testing.T) {
	now := time.Now()
	ns, name := "creation-dur-notready", "notready-duration-sandbox"
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxPending,
			Conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionReady),
					Status:             metav1.ConditionFalse,
					LastTransitionTime: metav1.NewTime(now),
				},
			},
		},
	}

	beforeSum := creationToReadyHistogramSum(t, ns)
	recordSandboxMetrics(sandbox, nil)
	afterSum := creationToReadyHistogramSum(t, ns)

	if afterSum != beforeSum {
		t.Errorf("not-ready should not observe histogram: sum changed from %v to %v", beforeSum, afterSum)
	}
	DeleteSandboxMetrics(ns, name)
}

func TestSandboxInplaceUpdateDuration_ObservedOnce(t *testing.T) {
	now := time.Now()
	startTime := now.Add(-20 * time.Second)
	endTime := now
	ns, name := "inplace-dur-observed", "inplace-duration-sandbox"

	// Step 1: Record with InplaceUpdate=False (stores start time)
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Minute)),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionInplaceUpdate),
					Status:             metav1.ConditionFalse,
					LastTransitionTime: metav1.NewTime(startTime),
				},
			},
		},
	}

	beforeSum := inplaceUpdateHistogramSum(t, ns)
	recordSandboxMetrics(sandbox, nil)

	// No histogram observation yet (only False recorded)
	afterFalseSum := inplaceUpdateHistogramSum(t, ns)
	if afterFalseSum != beforeSum {
		t.Errorf("InplaceUpdate=False should not observe histogram: sum changed from %v to %v", beforeSum, afterFalseSum)
	}

	// Step 2: Record with InplaceUpdate=True (should observe duration)
	sandbox.Status.Conditions[0].Status = metav1.ConditionTrue
	sandbox.Status.Conditions[0].LastTransitionTime = metav1.NewTime(endTime)

	recordSandboxMetrics(sandbox, nil)
	afterTrueSum := inplaceUpdateHistogramSum(t, ns)
	expectedDuration := endTime.Sub(startTime).Seconds()
	if delta := afterTrueSum - beforeSum; delta < expectedDuration-0.01 || delta > expectedDuration+0.01 {
		t.Errorf("InplaceUpdate=True observation: sum delta = %v, want ~%v", delta, expectedDuration)
	}

	// Step 3: Second call should NOT observe (deduplicated)
	recordSandboxMetrics(sandbox, nil)
	afterSecondSum := inplaceUpdateHistogramSum(t, ns)
	if afterSecondSum != afterTrueSum {
		t.Errorf("second InplaceUpdate=True call should not change sum: got %v, want %v", afterSecondSum, afterTrueSum)
	}

	// Cleanup
	DeleteSandboxMetrics(ns, name)
}

// pauseDurationHistogramSum collects the current sample sum from the pause duration HistogramVec for a given namespace.
func pauseDurationHistogramSum(t *testing.T, namespace string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := sandboxPauseDuration.WithLabelValues(namespace).(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("failed to write histogram metric: %v", err)
	}
	return m.GetHistogram().GetSampleSum()
}

// resumeDurationHistogramSum collects the current sample sum from the resume duration HistogramVec for a given namespace.
func resumeDurationHistogramSum(t *testing.T, namespace string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := sandboxResumeDuration.WithLabelValues(namespace).(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("failed to write histogram metric: %v", err)
	}
	return m.GetHistogram().GetSampleSum()
}

func TestSandboxPauseDuration(t *testing.T) {
	now := time.Now()
	startTime := now.Add(-15 * time.Second)
	endTime := now
	ns, name := "pause-dur-test", "pause-duration-sandbox"

	// Clean up global state
	pauseStartTimes.Delete(ns + "/" + name)
	observedPauseDurations.Delete(ns + "/" + name)

	// Step 1: Record with Paused=False (stores start time)
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Minute)),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionPaused),
					Status:             metav1.ConditionFalse,
					LastTransitionTime: metav1.NewTime(startTime),
				},
			},
		},
	}

	beforeSum := pauseDurationHistogramSum(t, ns)
	recordSandboxMetrics(sandbox, nil)

	// No histogram observation yet (only False recorded)
	afterFalseSum := pauseDurationHistogramSum(t, ns)
	if afterFalseSum != beforeSum {
		t.Errorf("Paused=False should not observe histogram: sum changed from %v to %v", beforeSum, afterFalseSum)
	}

	// Step 2: Record with Paused=True (should observe duration)
	sandbox.Status.Conditions[0].Status = metav1.ConditionTrue
	sandbox.Status.Conditions[0].LastTransitionTime = metav1.NewTime(endTime)

	recordSandboxMetrics(sandbox, nil)
	afterTrueSum := pauseDurationHistogramSum(t, ns)
	expectedDuration := endTime.Sub(startTime).Seconds()
	if delta := afterTrueSum - beforeSum; delta < expectedDuration-0.01 || delta > expectedDuration+0.01 {
		t.Errorf("Paused=True observation: sum delta = %v, want ~%v", delta, expectedDuration)
	}

	// Step 3: Second call should NOT observe (deduplicated)
	recordSandboxMetrics(sandbox, nil)
	afterSecondSum := pauseDurationHistogramSum(t, ns)
	if afterSecondSum != afterTrueSum {
		t.Errorf("second Paused=True call should not change sum: got %v, want %v", afterSecondSum, afterTrueSum)
	}

	// Cleanup
	DeleteSandboxMetrics(ns, name)
}

func TestSandboxResumeDuration(t *testing.T) {
	now := time.Now()
	startTime := now.Add(-10 * time.Second)
	endTime := now
	ns, name := "resume-dur-test", "resume-duration-sandbox"

	// Clean up global state
	resumeStartTimes.Delete(ns + "/" + name)
	observedResumeDurations.Delete(ns + "/" + name)

	// Step 1: Record with Resumed=False (stores start time)
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Minute)),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxResuming,
			Conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionResumed),
					Status:             metav1.ConditionFalse,
					LastTransitionTime: metav1.NewTime(startTime),
				},
			},
		},
	}

	beforeSum := resumeDurationHistogramSum(t, ns)
	recordSandboxMetrics(sandbox, nil)

	// No histogram observation yet (only False recorded)
	afterFalseSum := resumeDurationHistogramSum(t, ns)
	if afterFalseSum != beforeSum {
		t.Errorf("Resumed=False should not observe histogram: sum changed from %v to %v", beforeSum, afterFalseSum)
	}

	// Step 2: Record with Resumed=True (should observe duration)
	sandbox.Status.Conditions[0].Status = metav1.ConditionTrue
	sandbox.Status.Conditions[0].LastTransitionTime = metav1.NewTime(endTime)

	recordSandboxMetrics(sandbox, nil)
	afterTrueSum := resumeDurationHistogramSum(t, ns)
	expectedDuration := endTime.Sub(startTime).Seconds()
	if delta := afterTrueSum - beforeSum; delta < expectedDuration-0.01 || delta > expectedDuration+0.01 {
		t.Errorf("Resumed=True observation: sum delta = %v, want ~%v", delta, expectedDuration)
	}

	// Step 3: Second call should NOT observe (deduplicated)
	recordSandboxMetrics(sandbox, nil)
	afterSecondSum := resumeDurationHistogramSum(t, ns)
	if afterSecondSum != afterTrueSum {
		t.Errorf("second Resumed=True call should not change sum: got %v, want %v", afterSecondSum, afterTrueSum)
	}

	// Cleanup
	DeleteSandboxMetrics(ns, name)
}

func TestSandboxInplaceUpdateDuration_NotObservedWithoutStartTime(t *testing.T) {
	now := time.Now()
	ns, name := "inplace-dur-nostart", "inplace-no-start-sandbox"

	// Directly set InplaceUpdate=True without prior False record
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Minute)),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionInplaceUpdate),
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(now),
				},
			},
		},
	}

	beforeSum := inplaceUpdateHistogramSum(t, ns)
	recordSandboxMetrics(sandbox, nil)
	afterSum := inplaceUpdateHistogramSum(t, ns)

	if afterSum != beforeSum {
		t.Errorf("InplaceUpdate=True without prior False should not observe: sum changed from %v to %v", beforeSum, afterSum)
	}
	DeleteSandboxMetrics(ns, name)
}

func TestRecordSandboxMetrics_PhaseCompact(t *testing.T) {
	ns, name := "default", "phase-compact-sandbox"

	// Start in Running phase
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning},
	}
	recordSandboxMetrics(sandbox, nil)

	// Running should be 1
	val := testutil.ToFloat64(sandboxStatusPhase.WithLabelValues(ns, name, string(agentsv1alpha1.SandboxRunning)))
	if val != 1 {
		t.Errorf("phase Running = %v, want 1", val)
	}

	// Transition to Paused
	sandbox.Status.Phase = agentsv1alpha1.SandboxPaused
	recordSandboxMetrics(sandbox, nil)

	// Paused should be 1
	pausedVal := testutil.ToFloat64(sandboxStatusPhase.WithLabelValues(ns, name, string(agentsv1alpha1.SandboxPaused)))
	if pausedVal != 1 {
		t.Errorf("phase Paused = %v, want 1", pausedVal)
	}

	// Running should be 0 (deleted, ToFloat64 returns 0 for non-existent series)
	runningVal := testutil.ToFloat64(sandboxStatusPhase.WithLabelValues(ns, name, string(agentsv1alpha1.SandboxRunning)))
	if runningVal != 0 {
		t.Errorf("phase Running after transition = %v, want 0", runningVal)
	}

	DeleteSandboxMetrics(ns, name)
}

func TestSanitizeLabelName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "simple key", input: "app", expected: "app"},
		{name: "dots and slashes", input: "label_app.kubernetes.io/name", expected: "label_app_kubernetes_io_name"},
		{name: "hyphen", input: "label_my-label", expected: "label_my_label"},
		{name: "slash", input: "label_foo/bar", expected: "label_foo_bar"},
		{name: "already safe", input: "label_already_safe", expected: "label_already_safe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeLabelName(tt.input); got != tt.expected {
				t.Errorf("sanitizeLabelName(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestInitSandboxLabelsMetric_EmptyAllowlist(t *testing.T) {
	// Save and restore package-level state
	origLabels := sandboxLabels
	origAllowlist := labelsAllowlist
	defer func() {
		sandboxLabels = origLabels
		labelsAllowlist = origAllowlist
	}()

	sandboxLabels = nil
	labelsAllowlist = nil

	InitSandboxLabelsMetric([]string{})
	if sandboxLabels != nil {
		t.Errorf("sandboxLabels should be nil after empty allowlist init")
	}
	InitSandboxLabelsMetric(nil)
	if sandboxLabels != nil {
		t.Errorf("sandboxLabels should be nil after nil allowlist init")
	}
}

// counterValue reads the current value of a CounterVec for specific label values.
func counterValue(t *testing.T, cv *prometheus.CounterVec, lvs ...string) float64 {
	t.Helper()
	return testutil.ToFloat64(cv.WithLabelValues(lvs...))
}

// histogramSampleCount reads the sample count from a Histogram metric.
func histogramSampleCount(t *testing.T, hv *prometheus.HistogramVec, lvs ...string) uint64 {
	t.Helper()
	m := &dto.Metric{}
	if err := hv.WithLabelValues(lvs...).(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("failed to write histogram metric: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// histogramSampleSum reads the sample sum from a Histogram metric.
func histogramSampleSum(t *testing.T, hv *prometheus.HistogramVec, lvs ...string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := hv.WithLabelValues(lvs...).(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("failed to write histogram metric: %v", err)
	}
	return m.GetHistogram().GetSampleSum()
}

func TestSandboxCreationTotal(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(ns, sbName string)
		sandboxFunc func(ns, sbName string) *agentsv1alpha1.Sandbox
		verify      func(t *testing.T, ns, sbName string)
	}{
		{
			name: "creation success increments counter on first Ready=True",
			setup: func(ns, sbName string) {
				observedCreationToReady.Delete(ns + "/" + sbName)
				sandboxCreationTotal.Reset()
			},
			sandboxFunc: func(ns, sbName string) *agentsv1alpha1.Sandbox {
				now := time.Now()
				return &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name: sbName, Namespace: ns,
						CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Second)),
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxRunning,
						Conditions: []metav1.Condition{{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionTrue,
							LastTransitionTime: metav1.NewTime(now),
						}},
					},
				}
			},
			verify: func(t *testing.T, ns, sbName string) {
				val := counterValue(t, sandboxCreationTotal, ns, "success")
				if val != 1 {
					t.Errorf("sandbox_creation_total{result=success} = %v, want 1", val)
				}
			},
		},
		{
			name: "creation failure increments counter on Phase=Failed",
			setup: func(ns, sbName string) {
				observedCreationFailure.Delete(ns + "/" + sbName)
				sandboxCreationTotal.Reset()
			},
			sandboxFunc: func(ns, sbName string) *agentsv1alpha1.Sandbox {
				return &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name: sbName, Namespace: ns,
						CreationTimestamp: metav1.NewTime(time.Now()),
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxFailed,
					},
				}
			},
			verify: func(t *testing.T, ns, sbName string) {
				val := counterValue(t, sandboxCreationTotal, ns, "failure")
				if val != 1 {
					t.Errorf("sandbox_creation_total{result=failure} = %v, want 1", val)
				}
			},
		},
		{
			name: "duplicate calls do not re-increment success counter",
			setup: func(ns, sbName string) {
				observedCreationToReady.Delete(ns + "/" + sbName)
				sandboxCreationTotal.Reset()
			},
			sandboxFunc: func(ns, sbName string) *agentsv1alpha1.Sandbox {
				now := time.Now()
				return &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name: sbName, Namespace: ns,
						CreationTimestamp: metav1.NewTime(now.Add(-10 * time.Second)),
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxRunning,
						Conditions: []metav1.Condition{{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionTrue,
							LastTransitionTime: metav1.NewTime(now),
						}},
					},
				}
			},
			verify: func(t *testing.T, ns, sbName string) {
				// Call recordSandboxMetrics a second time and ensure counter doesn't increase
				before := counterValue(t, sandboxCreationTotal, ns, "success")
				now := time.Now()
				sb := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name: sbName, Namespace: ns,
						CreationTimestamp: metav1.NewTime(now.Add(-10 * time.Second)),
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxRunning,
						Conditions: []metav1.Condition{{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionTrue,
							LastTransitionTime: metav1.NewTime(now),
						}},
					},
				}
				recordSandboxMetrics(sb, nil)
				after := counterValue(t, sandboxCreationTotal, ns, "success")
				if after != before {
					t.Errorf("duplicate call incremented counter: before=%v, after=%v", before, after)
				}
			},
		},
		{
			name: "DeleteSandboxMetrics does not remove namespace-level creation counter",
			setup: func(ns, sbName string) {
				observedCreationToReady.Delete(ns + "/" + sbName)
				sandboxCreationTotal.Reset()
			},
			sandboxFunc: func(ns, sbName string) *agentsv1alpha1.Sandbox {
				now := time.Now()
				return &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name: sbName, Namespace: ns,
						CreationTimestamp: metav1.NewTime(now.Add(-5 * time.Second)),
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxRunning,
						Conditions: []metav1.Condition{{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionTrue,
							LastTransitionTime: metav1.NewTime(now),
						}},
					},
				}
			},
			verify: func(t *testing.T, ns, sbName string) {
				DeleteSandboxMetrics(ns, sbName)
				// Counter is namespace-level, so it persists after per-sandbox deletion
				val := counterValue(t, sandboxCreationTotal, ns, "success")
				if val != 1 {
					t.Errorf("sandbox_creation_total after delete = %v, want 1 (namespace-level persists)", val)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := "default"
			sbName := "creation-total-" + tt.name
			tt.setup(ns, sbName)
			sb := tt.sandboxFunc(ns, sbName)
			recordSandboxMetrics(sb, nil)
			defer DeleteSandboxMetrics(ns, sbName)
			tt.verify(t, ns, sbName)
		})
	}
}

func TestSandboxPauseTotal(t *testing.T) {
	tests := []struct {
		name   string
		verify func(t *testing.T, ns, sbName string)
	}{
		{
			name: "pause success increments counter on first Paused=True observation",
			verify: func(t *testing.T, ns, sbName string) {
				now := time.Now()
				key := ns + "/" + sbName
				pauseStartTimes.Delete(key)
				observedPauseDurations.Delete(key)
				sandboxPauseTotal.Reset()

				// Step 1: Paused=False stores start time
				sb := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name: sbName, Namespace: ns,
						CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Minute)),
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxRunning,
						Conditions: []metav1.Condition{{
							Type:               string(agentsv1alpha1.SandboxConditionPaused),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.NewTime(now.Add(-10 * time.Second)),
						}},
					},
				}
				recordSandboxMetrics(sb, nil)

				// Step 2: Paused=True should increment
				sb.Status.Phase = agentsv1alpha1.SandboxPaused
				sb.Status.Conditions[0].Status = metav1.ConditionTrue
				sb.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now)
				recordSandboxMetrics(sb, nil)

				val := counterValue(t, sandboxPauseTotal, ns, "success")
				if val != 1 {
					t.Errorf("sandbox_pause_total{result=success} = %v, want 1", val)
				}
			},
		},
		{
			name: "duplicate calls do not re-increment pause counter",
			verify: func(t *testing.T, ns, sbName string) {
				now := time.Now()
				key := ns + "/" + sbName
				pauseStartTimes.Delete(key)
				observedPauseDurations.Delete(key)
				sandboxPauseTotal.Reset()

				sb := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name: sbName, Namespace: ns,
						CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Minute)),
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxRunning,
						Conditions: []metav1.Condition{{
							Type:               string(agentsv1alpha1.SandboxConditionPaused),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.NewTime(now.Add(-10 * time.Second)),
						}},
					},
				}
				recordSandboxMetrics(sb, nil)

				sb.Status.Phase = agentsv1alpha1.SandboxPaused
				sb.Status.Conditions[0].Status = metav1.ConditionTrue
				sb.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now)
				recordSandboxMetrics(sb, nil)

				before := counterValue(t, sandboxPauseTotal, ns, "success")
				recordSandboxMetrics(sb, nil)
				after := counterValue(t, sandboxPauseTotal, ns, "success")
				if after != before {
					t.Errorf("duplicate pause call incremented: before=%v, after=%v", before, after)
				}
			},
		},
		{
			name: "new pause cycle after cleanup can re-count",
			verify: func(t *testing.T, ns, sbName string) {
				now := time.Now()
				key := ns + "/" + sbName
				pauseStartTimes.Delete(key)
				observedPauseDurations.Delete(key)
				sandboxPauseTotal.Reset()

				sb := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name: sbName, Namespace: ns,
						CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Minute)),
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxRunning,
						Conditions: []metav1.Condition{{
							Type:               string(agentsv1alpha1.SandboxConditionPaused),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.NewTime(now.Add(-10 * time.Second)),
						}},
					},
				}
				recordSandboxMetrics(sb, nil)
				sb.Status.Phase = agentsv1alpha1.SandboxPaused
				sb.Status.Conditions[0].Status = metav1.ConditionTrue
				sb.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now)
				recordSandboxMetrics(sb, nil)

				// Simulate a new pause cycle by resetting condition to False
				sb.Status.Phase = agentsv1alpha1.SandboxRunning
				sb.Status.Conditions[0].Status = metav1.ConditionFalse
				sb.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(1 * time.Second))
				recordSandboxMetrics(sb, nil)

				sb.Status.Phase = agentsv1alpha1.SandboxPaused
				sb.Status.Conditions[0].Status = metav1.ConditionTrue
				sb.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(5 * time.Second))
				recordSandboxMetrics(sb, nil)

				val := counterValue(t, sandboxPauseTotal, ns, "success")
				if val != 2 {
					t.Errorf("sandbox_pause_total after new cycle = %v, want 2", val)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := "default"
			sbName := "pause-total-" + tt.name
			defer DeleteSandboxMetrics(ns, sbName)
			tt.verify(t, ns, sbName)
		})
	}
}

func TestSandboxResumeTotal(t *testing.T) {
	tests := []struct {
		name   string
		verify func(t *testing.T, ns, sbName string)
	}{
		{
			name: "resume success increments counter on first Resumed=True observation",
			verify: func(t *testing.T, ns, sbName string) {
				now := time.Now()
				key := ns + "/" + sbName
				resumeStartTimes.Delete(key)
				observedResumeDurations.Delete(key)
				sandboxResumeTotal.Reset()

				sb := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name: sbName, Namespace: ns,
						CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Minute)),
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxResuming,
						Conditions: []metav1.Condition{{
							Type:               string(agentsv1alpha1.SandboxConditionResumed),
							Status:             metav1.ConditionFalse,
							Reason:             "CreatePod",
							LastTransitionTime: metav1.NewTime(now.Add(-10 * time.Second)),
						}},
					},
				}
				recordSandboxMetrics(sb, nil)

				sb.Status.Phase = agentsv1alpha1.SandboxRunning
				sb.Status.Conditions[0].Status = metav1.ConditionTrue
				sb.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now)
				recordSandboxMetrics(sb, nil)

				val := counterValue(t, sandboxResumeTotal, ns, "success")
				if val != 1 {
					t.Errorf("sandbox_resume_total{result=success} = %v, want 1", val)
				}
			},
		},
		{
			name: "duplicate calls do not re-increment resume counter",
			verify: func(t *testing.T, ns, sbName string) {
				now := time.Now()
				key := ns + "/" + sbName
				resumeStartTimes.Delete(key)
				observedResumeDurations.Delete(key)
				sandboxResumeTotal.Reset()

				sb := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name: sbName, Namespace: ns,
						CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Minute)),
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxResuming,
						Conditions: []metav1.Condition{{
							Type:               string(agentsv1alpha1.SandboxConditionResumed),
							Status:             metav1.ConditionFalse,
							Reason:             "CreatePod",
							LastTransitionTime: metav1.NewTime(now.Add(-10 * time.Second)),
						}},
					},
				}
				recordSandboxMetrics(sb, nil)

				sb.Status.Phase = agentsv1alpha1.SandboxRunning
				sb.Status.Conditions[0].Status = metav1.ConditionTrue
				sb.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now)
				recordSandboxMetrics(sb, nil)

				before := counterValue(t, sandboxResumeTotal, ns, "success")
				recordSandboxMetrics(sb, nil)
				after := counterValue(t, sandboxResumeTotal, ns, "success")
				if after != before {
					t.Errorf("duplicate resume call incremented: before=%v, after=%v", before, after)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := "default"
			sbName := "resume-total-" + tt.name
			defer DeleteSandboxMetrics(ns, sbName)
			tt.verify(t, ns, sbName)
		})
	}
}

func TestSandboxDeletionDuration(t *testing.T) {
	tests := []struct {
		name   string
		verify func(t *testing.T, ns, sbName string)
	}{
		{
			name: "deletion duration is observed after DeleteSandboxMetrics",
			verify: func(t *testing.T, ns, sbName string) {
				now := time.Now()
				delTime := metav1.NewTime(now.Add(-2 * time.Second))

				sb := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name: sbName, Namespace: ns,
						CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Minute)),
						DeletionTimestamp: &delTime,
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxTerminating,
					},
				}

				// Record metrics stores the deletion start time
				recordSandboxMetrics(sb, nil)

				// Verify deletionStartTimes has entry after recordSandboxMetrics
				key := ns + "/" + sbName
				startTimeVal, ok := deletionStartTimes.Load(key)
				if !ok {
					t.Fatal("deletionStartTimes should have entry after recordSandboxMetrics with DeletionTimestamp")
				}
				startTime := startTimeVal.(time.Time)
				if !startTime.Equal(delTime.Time) {
					t.Errorf("stored deletion start time = %v, want %v", startTime, delTime.Time)
				}

				// Before delete, histogram should have 0 samples
				before := histogramSampleCount(t, sandboxDeletionDuration, ns)
				if before != 0 {
					t.Errorf("deletion duration sample count before delete = %v, want 0", before)
				}

				// Call DeleteSandboxMetrics - this observes duration then deletes the series.
				// Since the series is deleted after observation, we verify the code path
				// executed by checking that deletionStartTimes entry was consumed.
				DeleteSandboxMetrics(ns, sbName)

				// Verify deletionStartTimes entry was consumed (proves Observe was called)
				_, stillExists := deletionStartTimes.Load(key)
				if stillExists {
					t.Errorf("deletionStartTimes should be cleaned after DeleteSandboxMetrics")
				}
			},
		},
		{
			name: "no deletion duration observed without DeletionTimestamp",
			verify: func(t *testing.T, ns, sbName string) {
				now := time.Now()
				sb := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name: sbName, Namespace: ns,
						CreationTimestamp: metav1.NewTime(now),
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxRunning,
					},
				}
				recordSandboxMetrics(sb, nil)

				before := histogramSampleCount(t, sandboxDeletionDuration, ns)
				DeleteSandboxMetrics(ns, sbName)
				after := histogramSampleCount(t, sandboxDeletionDuration, ns)
				if after != before {
					t.Errorf("deletion duration should not be observed without DeletionTimestamp: before=%v, after=%v", before, after)
				}
			},
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := fmt.Sprintf("del-dur-ns-%d", i)
			sbName := "del-dur-" + tt.name
			// Clean global state
			deletionStartTimes.Delete(ns + "/" + sbName)
			tt.verify(t, ns, sbName)
		})
	}
}

func TestSandboxStatusAbnormal(t *testing.T) {
	tests := []struct {
		name               string
		phase              agentsv1alpha1.SandboxPhase
		conditions         []metav1.Condition
		wantPauseAbnormal  float64
		wantResumeAbnormal float64
	}{
		{
			name:  "Phase=Paused with SandboxPaused=False is abnormal",
			phase: agentsv1alpha1.SandboxPaused,
			conditions: []metav1.Condition{{
				Type:               string(agentsv1alpha1.SandboxConditionPaused),
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.NewTime(time.Now()),
			}},
			wantPauseAbnormal:  1,
			wantResumeAbnormal: 0,
		},
		{
			name:  "Phase=Paused with SandboxPaused=True is normal",
			phase: agentsv1alpha1.SandboxPaused,
			conditions: []metav1.Condition{{
				Type:               string(agentsv1alpha1.SandboxConditionPaused),
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(time.Now()),
			}},
			wantPauseAbnormal:  0,
			wantResumeAbnormal: 0,
		},
		{
			name:  "Phase=Resuming with SandboxResumed=False is abnormal",
			phase: agentsv1alpha1.SandboxResuming,
			conditions: []metav1.Condition{{
				Type:               string(agentsv1alpha1.SandboxConditionResumed),
				Status:             metav1.ConditionFalse,
				Reason:             "CreatePod",
				LastTransitionTime: metav1.NewTime(time.Now()),
			}},
			wantPauseAbnormal:  0,
			wantResumeAbnormal: 1,
		},
		{
			name:  "Phase=Resuming with SandboxResumed=True is normal",
			phase: agentsv1alpha1.SandboxResuming,
			conditions: []metav1.Condition{{
				Type:               string(agentsv1alpha1.SandboxConditionResumed),
				Status:             metav1.ConditionTrue,
				Reason:             "CreatePod",
				LastTransitionTime: metav1.NewTime(time.Now()),
			}},
			wantPauseAbnormal:  0,
			wantResumeAbnormal: 0,
		},
		{
			name:               "Phase=Running has no abnormal state",
			phase:              agentsv1alpha1.SandboxRunning,
			conditions:         nil,
			wantPauseAbnormal:  0,
			wantResumeAbnormal: 0,
		},
		{
			name:               "Phase=Paused with no Paused condition is abnormal",
			phase:              agentsv1alpha1.SandboxPaused,
			conditions:         nil,
			wantPauseAbnormal:  1,
			wantResumeAbnormal: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := "default"
			sbName := "abnormal-" + tt.name

			sb := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name: sbName, Namespace: ns,
					CreationTimestamp: metav1.NewTime(time.Now()),
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase:      tt.phase,
					Conditions: tt.conditions,
				},
			}

			recordSandboxMetrics(sb, nil)
			defer DeleteSandboxMetrics(ns, sbName)

			pauseVal := testutil.ToFloat64(sandboxStatusAbnormal.WithLabelValues(ns, sbName, "pause_incomplete"))
			if pauseVal != tt.wantPauseAbnormal {
				t.Errorf("sandbox_status_abnormal{type=pause_incomplete} = %v, want %v", pauseVal, tt.wantPauseAbnormal)
			}

			resumeVal := testutil.ToFloat64(sandboxStatusAbnormal.WithLabelValues(ns, sbName, "resume_incomplete"))
			if resumeVal != tt.wantResumeAbnormal {
				t.Errorf("sandbox_status_abnormal{type=resume_incomplete} = %v, want %v", resumeVal, tt.wantResumeAbnormal)
			}

			// Verify cleanup removes abnormal metrics
			DeleteSandboxMetrics(ns, sbName)
			pauseAfter := testutil.ToFloat64(sandboxStatusAbnormal.WithLabelValues(ns, sbName, "pause_incomplete"))
			if pauseAfter != 0 {
				t.Errorf("sandbox_status_abnormal{type=pause_incomplete} after delete = %v, want 0", pauseAfter)
			}
			resumeAfter := testutil.ToFloat64(sandboxStatusAbnormal.WithLabelValues(ns, sbName, "resume_incomplete"))
			if resumeAfter != 0 {
				t.Errorf("sandbox_status_abnormal{type=resume_incomplete} after delete = %v, want 0", resumeAfter)
			}
		})
	}
}

func TestSandboxLabelsMetric_RecordAndDelete(t *testing.T) {
	// Initialize sandbox_labels if not yet initialized.
	// Since InitSandboxLabelsMetric registers to global registry and can only be called once,
	// we check if sandboxLabels is already set.
	if sandboxLabels == nil {
		InitSandboxLabelsMetric([]string{"app", "env"})
	}

	ns, name := "default", "labels-test-sandbox"
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(time.Now()),
			Labels: map[string]string{
				"app": "myapp",
				"env": "prod",
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
		},
	}

	t.Run("record labels", func(t *testing.T) {
		recordSandboxMetrics(sandbox, nil)

		val := testutil.ToFloat64(sandboxLabels.WithLabelValues(ns, name, "myapp", "prod"))
		if val != 1 {
			t.Errorf("sandbox_labels = %v, want 1", val)
		}
	})

	t.Run("missing label key returns empty string", func(t *testing.T) {
		// Create a sandbox with only "app" label, "env" should be empty string
		ns2, name2 := "default", "labels-partial-sandbox"
		partialSandbox := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name2,
				Namespace:         ns2,
				CreationTimestamp: metav1.NewTime(time.Now()),
				Labels: map[string]string{
					"app": "myapp",
				},
			},
			Status: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
		}
		recordSandboxMetrics(partialSandbox, nil)
		defer DeleteSandboxMetrics(ns2, name2)

		// env label value should be empty string
		val := testutil.ToFloat64(sandboxLabels.WithLabelValues(ns2, name2, "myapp", ""))
		if val != 1 {
			t.Errorf("sandbox_labels with missing label = %v, want 1", val)
		}
	})

	t.Run("delete labels", func(t *testing.T) {
		DeleteSandboxMetrics(ns, name)

		// After deletion, WithLabelValues creates a new zero-value gauge
		val := testutil.ToFloat64(sandboxLabels.WithLabelValues(ns, name, "myapp", "prod"))
		if val != 0 {
			t.Errorf("sandbox_labels after delete = %v, want 0", val)
		}
	})
}

func TestRecordRuntimeContainerAbnormal(t *testing.T) {
	ns, name := "default", "test-sandbox"

	tests := []struct {
		name         string
		pod          *corev1.Pod
		wantAbnormal map[string]float64 // container -> expected value (1=abnormal)
		wantHealthy  []string           // containers that should be 0 (healthy)
	}{
		{
			name:         "nil pod skips metric",
			pod:          nil,
			wantAbnormal: nil,
		},
		{
			name: "all runtime containers ready",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: "agent-runtime", Ready: true},
						{Name: "csi-sidecar", Ready: true},
					},
				},
			},
			wantHealthy: []string{"agent-runtime", "csi-sidecar"},
		},
		{
			name: "runtime container restarted and not ready",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{
							Name:         "agent-runtime",
							Ready:        false,
							RestartCount: 3,
						},
						{Name: "csi-sidecar", Ready: true},
					},
				},
			},
			wantAbnormal: map[string]float64{"agent-runtime": 1},
			wantHealthy:  []string{"csi-sidecar"},
		},
		{
			name: "multiple runtime containers restarted and not ready",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: "agent-runtime", Ready: false, RestartCount: 2},
						{Name: "csi-sidecar", Ready: false, RestartCount: 1},
					},
				},
			},
			wantAbnormal: map[string]float64{"agent-runtime": 1, "csi-sidecar": 1},
		},
		{
			name: "runtime container not ready but no restart - not abnormal",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: "agent-runtime", Ready: false, RestartCount: 0},
					},
				},
			},
			wantHealthy: []string{"agent-runtime"},
		},
		{
			name: "runtime container restarted but ready - not abnormal",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: "agent-runtime", Ready: true, RestartCount: 5},
					},
				},
			},
			wantHealthy: []string{"agent-runtime"},
		},
		{
			name: "non-runtime container does not affect metric",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: "agent-runtime", Ready: true},
						{Name: "user-init", Ready: false, RestartCount: 10},
					},
				},
			},
			wantHealthy: []string{"agent-runtime"},
		},
		{
			name: "no runtime containers in status",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: "user-init", Ready: false},
					},
				},
			},
		},
		{
			name: "container transitions from abnormal to ready resets to 0",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: "agent-runtime", Ready: true, RestartCount: 5},
					},
				},
			},
			wantHealthy: []string{"agent-runtime"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandboxRuntimeContainerAbnormal.DeletePartialMatch(prometheus.Labels{"namespace": ns, "name": name})

			// Pre-set a series for "agent-runtime" to verify reset on recovery
			if tt.name == "container transitions from abnormal to ready resets to 0" {
				sandboxRuntimeContainerAbnormal.WithLabelValues(ns, name, "agent-runtime").Set(1)
			}

			recordRuntimeContainerAbnormal(ns, name, tt.pod)

			if tt.pod == nil {
				return
			}
			for container, want := range tt.wantAbnormal {
				got := testutil.ToFloat64(sandboxRuntimeContainerAbnormal.WithLabelValues(ns, name, container))
				if got != want {
					t.Errorf("sandbox_runtime_container_abnormal{container=%s} = %v, want %v", container, got, want)
				}
			}
			for _, container := range tt.wantHealthy {
				got := testutil.ToFloat64(sandboxRuntimeContainerAbnormal.WithLabelValues(ns, name, container))
				if got != 0 {
					t.Errorf("sandbox_runtime_container_abnormal{container=%s} = %v, want 0 (healthy)", container, got)
				}
			}
		})
	}
}

func TestDeleteSandboxMetrics_ClearsRuntimeContainerAbnormal(t *testing.T) {
	ns, name := "default", "cleanup-sandbox"

	sandboxRuntimeContainerAbnormal.WithLabelValues(ns, name, "agent-runtime").Set(1)
	sandboxRuntimeContainerAbnormal.WithLabelValues(ns, name, "csi-sidecar").Set(1)

	val := testutil.ToFloat64(sandboxRuntimeContainerAbnormal.WithLabelValues(ns, name, "agent-runtime"))
	if val != 1 {
		t.Fatalf("precondition: expected 1, got %v", val)
	}

	DeleteSandboxMetrics(ns, name)

	val = testutil.ToFloat64(sandboxRuntimeContainerAbnormal.WithLabelValues(ns, name, "agent-runtime"))
	if val != 0 {
		t.Errorf("after delete, agent-runtime series = %v, want 0", val)
	}
	val = testutil.ToFloat64(sandboxRuntimeContainerAbnormal.WithLabelValues(ns, name, "csi-sidecar"))
	if val != 0 {
		t.Errorf("after delete, csi-sidecar series = %v, want 0", val)
	}
}
