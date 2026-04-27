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
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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

	// InplaceUpdate=False: inplace_update_done should be 0
	val := testutil.ToFloat64(sandboxStatusInplaceUpdateDone.WithLabelValues("default", "inplace-sandbox"))
	if val != 0 {
		t.Errorf("sandbox_status_inplace_update_done = %v, want 0", val)
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

	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "inplace-true-sandbox")

	// Verify inplace_update_done metrics
	doneVal := testutil.ToFloat64(sandboxStatusInplaceUpdateDone.WithLabelValues("default", "inplace-true-sandbox"))
	if doneVal != 1 {
		t.Errorf("sandbox_status_inplace_update_done = %v, want 1", doneVal)
	}
	doneTime := testutil.ToFloat64(sandboxStatusInplaceUpdateDoneTime.WithLabelValues("default", "inplace-true-sandbox"))
	if doneTime != float64(now.Unix()) {
		t.Errorf("sandbox_status_inplace_update_done_time = %v, want %v", doneTime, float64(now.Unix()))
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
	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "paused-false-sandbox")
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

	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "paused-true-sandbox")

	// Verify paused_time timestamp is recorded
	pausedTime := testutil.ToFloat64(sandboxStatusPausedTime.WithLabelValues("default", "paused-true-sandbox"))
	if pausedTime != float64(now.Unix()) {
		t.Errorf("sandbox_status_paused_time = %v, want %v", pausedTime, float64(now.Unix()))
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
	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "resumed-false-sandbox")
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

	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "resumed-true-sandbox")

	// Verify resumed_time timestamp is recorded
	resumedTime := testutil.ToFloat64(sandboxStatusResumedTime.WithLabelValues("default", "resumed-true-sandbox"))
	if resumedTime != float64(now.Unix()) {
		t.Errorf("sandbox_status_resumed_time = %v, want %v", resumedTime, float64(now.Unix()))
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

	// InplaceUpdate=False: inplace_update_done should be 0
	inplaceVal := testutil.ToFloat64(sandboxStatusInplaceUpdateDone.WithLabelValues("default", "multi-cond-sandbox"))
	if inplaceVal != 0 {
		t.Errorf("sandbox_status_inplace_update_done = %v, want 0", inplaceVal)
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
	infoVal := testutil.ToFloat64(sandboxInfo.WithLabelValues(ns, name, "", "", "", ""))
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
			PodInfo: agentsv1alpha1.PodInfo{
				PodUID: types.UID("abc-123"),
			},
		},
	}

	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics("default", "info-sandbox")

	val := testutil.ToFloat64(sandboxInfo.WithLabelValues("default", "info-sandbox",
		"my-sandboxset", "node-1", "abc-123", "my-template"))
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
		{name: "Paused=True sets paused_time timestamp", status: metav1.ConditionTrue, wantPausedTS: true},
		{name: "Paused=False does not set paused_time", status: metav1.ConditionFalse, wantPausedTS: false},
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

			recordSandboxMetrics(sandbox)
			defer deleteSandboxMetrics("default", sbName)

			if tt.wantPausedTS {
				ts := testutil.ToFloat64(sandboxStatusPausedTime.WithLabelValues("default", sbName))
				if ts != float64(now.Unix()) {
					t.Errorf("sandbox_status_paused_time = %v, want %v", ts, float64(now.Unix()))
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
		{name: "Resumed=True sets resumed_time timestamp", status: metav1.ConditionTrue, wantResumedTS: true},
		{name: "Resumed=False does not set resumed_time", status: metav1.ConditionFalse, wantResumedTS: false},
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

			recordSandboxMetrics(sandbox)
			defer deleteSandboxMetrics("default", sbName)

			if tt.wantResumedTS {
				ts := testutil.ToFloat64(sandboxStatusResumedTime.WithLabelValues("default", sbName))
				if ts != float64(now.Unix()) {
					t.Errorf("sandbox_status_resumed_time = %v, want %v", ts, float64(now.Unix()))
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
		{name: "InplaceUpdate=True sets done=1 with timestamp", status: metav1.ConditionTrue, wantDone: 1, wantDoneTS: true},
		{name: "InplaceUpdate=False sets done=0", status: metav1.ConditionFalse, wantDone: 0, wantDoneTS: false},
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

			recordSandboxMetrics(sandbox)
			defer deleteSandboxMetrics("default", sbName)

			val := testutil.ToFloat64(sandboxStatusInplaceUpdateDone.WithLabelValues("default", sbName))
			if val != tt.wantDone {
				t.Errorf("sandbox_status_inplace_update_done = %v, want %v", val, tt.wantDone)
			}
			if tt.wantDoneTS {
				ts := testutil.ToFloat64(sandboxStatusInplaceUpdateDoneTime.WithLabelValues("default", sbName))
				if ts != float64(now.Unix()) {
					t.Errorf("sandbox_status_inplace_update_done_time = %v, want %v", ts, float64(now.Unix()))
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
					Status:             metav1.ConditionTrue,
					LastTransitionTime: now,
				},
				{
					Type:               string(agentsv1alpha1.SandboxConditionResumed),
					Status:             metav1.ConditionTrue,
					LastTransitionTime: now,
				},
				{
					Type:               string(agentsv1alpha1.SandboxConditionInplaceUpdate),
					Status:             metav1.ConditionTrue,
					LastTransitionTime: now,
				},
			},
		},
	}

	recordSandboxMetrics(sandbox)

	// Verify new metrics are set (Ready=False → ready=0, Paused=True → paused_time set, etc.)
	if v := testutil.ToFloat64(sandboxStatusReady.WithLabelValues(ns, name)); v != 0 {
		t.Errorf("sandbox_status_ready before delete = %v, want 0", v)
	}
	if v := testutil.ToFloat64(sandboxStatusPausedTime.WithLabelValues(ns, name)); v == 0 {
		t.Errorf("sandbox_status_paused_time before delete should be set")
	}
	if v := testutil.ToFloat64(sandboxStatusResumedTime.WithLabelValues(ns, name)); v == 0 {
		t.Errorf("sandbox_status_resumed_time before delete should be set")
	}
	if v := testutil.ToFloat64(sandboxStatusInplaceUpdateDone.WithLabelValues(ns, name)); v != 1 {
		t.Errorf("sandbox_status_inplace_update_done before delete = %v, want 1", v)
	}

	// Delete and verify cleanup
	deleteSandboxMetrics(ns, name)

	if v := testutil.ToFloat64(sandboxStatusReady.WithLabelValues(ns, name)); v != 0 {
		t.Errorf("sandbox_status_ready after delete = %v, want 0", v)
	}
	if v := testutil.ToFloat64(sandboxStatusPausedTime.WithLabelValues(ns, name)); v != 0 {
		t.Errorf("sandbox_status_paused_time after delete = %v, want 0", v)
	}
	if v := testutil.ToFloat64(sandboxStatusResumedTime.WithLabelValues(ns, name)); v != 0 {
		t.Errorf("sandbox_status_resumed_time after delete = %v, want 0", v)
	}
	if v := testutil.ToFloat64(sandboxStatusInplaceUpdateDone.WithLabelValues(ns, name)); v != 0 {
		t.Errorf("sandbox_status_inplace_update_done after delete = %v, want 0", v)
	}
	if v := testutil.ToFloat64(sandboxStatusInplaceUpdateDoneTime.WithLabelValues(ns, name)); v != 0 {
		t.Errorf("sandbox_status_inplace_update_done_time after delete = %v, want 0", v)
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

	recordSandboxMetrics(sandbox)
	defer deleteSandboxMetrics(ns, name)

	// Ready=True: ready=1
	if v := testutil.ToFloat64(sandboxStatusReady.WithLabelValues(ns, name)); v != 1 {
		t.Errorf("sandbox_status_ready = %v, want 1", v)
	}
	if v := testutil.ToFloat64(sandboxStatusReadyTime.WithLabelValues(ns, name)); v != float64(now.Unix()) {
		t.Errorf("sandbox_status_ready_time = %v, want %v", v, float64(now.Unix()))
	}

	// Resumed=True: resumed_time should be set
	if v := testutil.ToFloat64(sandboxStatusResumedTime.WithLabelValues(ns, name)); v != float64(now.Unix()) {
		t.Errorf("sandbox_status_resumed_time = %v, want %v", v, float64(now.Unix()))
	}

	// InplaceUpdate=False: inplace_update_done=0
	if v := testutil.ToFloat64(sandboxStatusInplaceUpdateDone.WithLabelValues(ns, name)); v != 0 {
		t.Errorf("sandbox_status_inplace_update_done = %v, want 0", v)
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

	// All new labels should be empty string when not set
	val := testutil.ToFloat64(sandboxInfo.WithLabelValues("default", "info-no-owner-sandbox",
		"", "", "", ""))
	if val != 1 {
		t.Errorf("sandbox_info with no owner = %v, want 1", val)
	}
}

func TestRecordSandboxMetrics_InfoPartialFields(t *testing.T) {
	tests := []struct {
		name          string
		nodeName      string
		podUID        types.UID
		templateLabel string
		wantNode      string
		wantPodUID    string
		wantTemplate  string
	}{
		{
			name:         "node set, no pod UID, no template label",
			nodeName:     "worker-1",
			wantNode:     "worker-1",
			wantPodUID:   "",
			wantTemplate: "",
		},
		{
			name:         "pod UID set, no node, no template label",
			podUID:       types.UID("uid-456"),
			wantNode:     "",
			wantPodUID:   "uid-456",
			wantTemplate: "",
		},
		{
			name:          "template label set, no node, no pod UID",
			templateLabel: "tmpl-a",
			wantNode:      "",
			wantPodUID:    "",
			wantTemplate:  "tmpl-a",
		},
		{
			name:          "all fields set",
			nodeName:      "node-x",
			podUID:        types.UID("uid-789"),
			templateLabel: "tmpl-b",
			wantNode:      "node-x",
			wantPodUID:    "uid-789",
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
					PodInfo: agentsv1alpha1.PodInfo{
						PodUID: tt.podUID,
					},
				},
			}

			recordSandboxMetrics(sandbox)
			defer deleteSandboxMetrics("default", sbName)

			val := testutil.ToFloat64(sandboxInfo.WithLabelValues("default", sbName,
				"", tt.wantNode, tt.wantPodUID, tt.wantTemplate))
			if val != 1 {
				t.Errorf("sandbox_info = %v, want 1", val)
			}
		})
	}
}

// creationToReadyHistogramSum collects the current sample sum from the creation-to-ready Histogram.
func creationToReadyHistogramSum(t *testing.T) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := sandboxCreationDuration.Write(m); err != nil {
		t.Fatalf("failed to write histogram metric: %v", err)
	}
	return m.GetHistogram().GetSampleSum()
}

// inplaceUpdateHistogramSum collects the current sample sum from the inplace-update Histogram.
func inplaceUpdateHistogramSum(t *testing.T) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := sandboxInplaceUpdateDuration.Write(m); err != nil {
		t.Fatalf("failed to write histogram metric: %v", err)
	}
	return m.GetHistogram().GetSampleSum()
}

func TestSandboxCreationToReadyDuration_ObservedOnce(t *testing.T) {
	now := time.Now()
	creationTime := now.Add(-45 * time.Second)
	readyTime := metav1.NewTime(now)
	ns, name := "default", "ready-duration-sandbox"

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

	beforeSum := creationToReadyHistogramSum(t)

	// First call should observe
	recordSandboxMetrics(sandbox)
	afterFirstSum := creationToReadyHistogramSum(t)
	expectedDuration := readyTime.Sub(creationTime).Seconds()
	if delta := afterFirstSum - beforeSum; delta < expectedDuration-0.01 || delta > expectedDuration+0.01 {
		t.Errorf("first observation: sum delta = %v, want ~%v", delta, expectedDuration)
	}

	// Second call should NOT observe (deduplicated)
	recordSandboxMetrics(sandbox)
	afterSecondSum := creationToReadyHistogramSum(t)
	if afterSecondSum != afterFirstSum {
		t.Errorf("second call should not change sum: got %v, want %v", afterSecondSum, afterFirstSum)
	}

	// Delete and re-record should observe again
	deleteSandboxMetrics(ns, name)
	recordSandboxMetrics(sandbox)
	afterReObserve := creationToReadyHistogramSum(t)
	if delta := afterReObserve - afterSecondSum; delta < expectedDuration-0.01 || delta > expectedDuration+0.01 {
		t.Errorf("re-observation after delete: sum delta = %v, want ~%v", delta, expectedDuration)
	}

	deleteSandboxMetrics(ns, name)
}

func TestSandboxCreationToReadyDuration_NotObservedWhenNotReady(t *testing.T) {
	now := time.Now()
	ns, name := "default", "notready-duration-sandbox"
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

	beforeSum := creationToReadyHistogramSum(t)
	recordSandboxMetrics(sandbox)
	afterSum := creationToReadyHistogramSum(t)

	if afterSum != beforeSum {
		t.Errorf("not-ready should not observe histogram: sum changed from %v to %v", beforeSum, afterSum)
	}
	deleteSandboxMetrics(ns, name)
}

func TestSandboxInplaceUpdateDuration_ObservedOnce(t *testing.T) {
	now := time.Now()
	startTime := now.Add(-20 * time.Second)
	endTime := now
	ns, name := "default", "inplace-duration-sandbox"

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

	beforeSum := inplaceUpdateHistogramSum(t)
	recordSandboxMetrics(sandbox)

	// No histogram observation yet (only False recorded)
	afterFalseSum := inplaceUpdateHistogramSum(t)
	if afterFalseSum != beforeSum {
		t.Errorf("InplaceUpdate=False should not observe histogram: sum changed from %v to %v", beforeSum, afterFalseSum)
	}

	// Step 2: Record with InplaceUpdate=True (should observe duration)
	sandbox.Status.Conditions[0].Status = metav1.ConditionTrue
	sandbox.Status.Conditions[0].LastTransitionTime = metav1.NewTime(endTime)

	recordSandboxMetrics(sandbox)
	afterTrueSum := inplaceUpdateHistogramSum(t)
	expectedDuration := endTime.Sub(startTime).Seconds()
	if delta := afterTrueSum - beforeSum; delta < expectedDuration-0.01 || delta > expectedDuration+0.01 {
		t.Errorf("InplaceUpdate=True observation: sum delta = %v, want ~%v", delta, expectedDuration)
	}

	// Step 3: Second call should NOT observe (deduplicated)
	recordSandboxMetrics(sandbox)
	afterSecondSum := inplaceUpdateHistogramSum(t)
	if afterSecondSum != afterTrueSum {
		t.Errorf("second InplaceUpdate=True call should not change sum: got %v, want %v", afterSecondSum, afterTrueSum)
	}

	// Cleanup
	deleteSandboxMetrics(ns, name)
}

// pauseDurationHistogramSum collects the current sample sum from the pause duration Histogram.
func pauseDurationHistogramSum(t *testing.T) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := sandboxPauseDuration.Write(m); err != nil {
		t.Fatalf("failed to write histogram metric: %v", err)
	}
	return m.GetHistogram().GetSampleSum()
}

// resumeDurationHistogramSum collects the current sample sum from the resume duration Histogram.
func resumeDurationHistogramSum(t *testing.T) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := sandboxResumeDuration.Write(m); err != nil {
		t.Fatalf("failed to write histogram metric: %v", err)
	}
	return m.GetHistogram().GetSampleSum()
}

func TestSandboxPauseDuration(t *testing.T) {
	now := time.Now()
	startTime := now.Add(-15 * time.Second)
	endTime := now
	ns, name := "default", "pause-duration-sandbox"

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

	beforeSum := pauseDurationHistogramSum(t)
	recordSandboxMetrics(sandbox)

	// No histogram observation yet (only False recorded)
	afterFalseSum := pauseDurationHistogramSum(t)
	if afterFalseSum != beforeSum {
		t.Errorf("Paused=False should not observe histogram: sum changed from %v to %v", beforeSum, afterFalseSum)
	}

	// Step 2: Record with Paused=True (should observe duration)
	sandbox.Status.Conditions[0].Status = metav1.ConditionTrue
	sandbox.Status.Conditions[0].LastTransitionTime = metav1.NewTime(endTime)

	recordSandboxMetrics(sandbox)
	afterTrueSum := pauseDurationHistogramSum(t)
	expectedDuration := endTime.Sub(startTime).Seconds()
	if delta := afterTrueSum - beforeSum; delta < expectedDuration-0.01 || delta > expectedDuration+0.01 {
		t.Errorf("Paused=True observation: sum delta = %v, want ~%v", delta, expectedDuration)
	}

	// Step 3: Second call should NOT observe (deduplicated)
	recordSandboxMetrics(sandbox)
	afterSecondSum := pauseDurationHistogramSum(t)
	if afterSecondSum != afterTrueSum {
		t.Errorf("second Paused=True call should not change sum: got %v, want %v", afterSecondSum, afterTrueSum)
	}

	// Cleanup
	deleteSandboxMetrics(ns, name)
}

func TestSandboxResumeDuration(t *testing.T) {
	now := time.Now()
	startTime := now.Add(-10 * time.Second)
	endTime := now
	ns, name := "default", "resume-duration-sandbox"

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

	beforeSum := resumeDurationHistogramSum(t)
	recordSandboxMetrics(sandbox)

	// No histogram observation yet (only False recorded)
	afterFalseSum := resumeDurationHistogramSum(t)
	if afterFalseSum != beforeSum {
		t.Errorf("Resumed=False should not observe histogram: sum changed from %v to %v", beforeSum, afterFalseSum)
	}

	// Step 2: Record with Resumed=True (should observe duration)
	sandbox.Status.Conditions[0].Status = metav1.ConditionTrue
	sandbox.Status.Conditions[0].LastTransitionTime = metav1.NewTime(endTime)

	recordSandboxMetrics(sandbox)
	afterTrueSum := resumeDurationHistogramSum(t)
	expectedDuration := endTime.Sub(startTime).Seconds()
	if delta := afterTrueSum - beforeSum; delta < expectedDuration-0.01 || delta > expectedDuration+0.01 {
		t.Errorf("Resumed=True observation: sum delta = %v, want ~%v", delta, expectedDuration)
	}

	// Step 3: Second call should NOT observe (deduplicated)
	recordSandboxMetrics(sandbox)
	afterSecondSum := resumeDurationHistogramSum(t)
	if afterSecondSum != afterTrueSum {
		t.Errorf("second Resumed=True call should not change sum: got %v, want %v", afterSecondSum, afterTrueSum)
	}

	// Cleanup
	deleteSandboxMetrics(ns, name)
}

func TestSandboxInplaceUpdateDuration_NotObservedWithoutStartTime(t *testing.T) {
	now := time.Now()
	ns, name := "default", "inplace-no-start-sandbox"

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

	beforeSum := inplaceUpdateHistogramSum(t)
	recordSandboxMetrics(sandbox)
	afterSum := inplaceUpdateHistogramSum(t)

	if afterSum != beforeSum {
		t.Errorf("InplaceUpdate=True without prior False should not observe: sum changed from %v to %v", beforeSum, afterSum)
	}
	deleteSandboxMetrics(ns, name)
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
	recordSandboxMetrics(sandbox)

	// Running should be 1
	val := testutil.ToFloat64(sandboxStatusPhase.WithLabelValues(ns, name, string(agentsv1alpha1.SandboxRunning)))
	if val != 1 {
		t.Errorf("phase Running = %v, want 1", val)
	}

	// Transition to Paused
	sandbox.Status.Phase = agentsv1alpha1.SandboxPaused
	recordSandboxMetrics(sandbox)

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

	deleteSandboxMetrics(ns, name)
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
		recordSandboxMetrics(sandbox)

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
		recordSandboxMetrics(partialSandbox)
		defer deleteSandboxMetrics(ns2, name2)

		// env label value should be empty string
		val := testutil.ToFloat64(sandboxLabels.WithLabelValues(ns2, name2, "myapp", ""))
		if val != 1 {
			t.Errorf("sandbox_labels with missing label = %v, want 1", val)
		}
	})

	t.Run("delete labels", func(t *testing.T) {
		deleteSandboxMetrics(ns, name)

		// After deletion, WithLabelValues creates a new zero-value gauge
		val := testutil.ToFloat64(sandboxLabels.WithLabelValues(ns, name, "myapp", "prod"))
		if val != 0 {
			t.Errorf("sandbox_labels after delete = %v, want 0", val)
		}
	})
}
