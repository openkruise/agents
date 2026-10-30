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

package sandboxclaim

import (
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestRecordSandboxClaimMetrics_ClaimingPhase(t *testing.T) {
	now := time.Now()
	startTime := metav1.NewTime(now.Add(-30 * time.Second))
	claim := &agentsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-claim",
			Namespace:         "default",
			UID:               types.UID("claim-uid-001"),
			CreationTimestamp: metav1.NewTime(now),
		},
		Spec: agentsv1alpha1.SandboxClaimSpec{
			TemplateName: "my-sandboxset",
			Replicas:     ptr.To[int32](3),
		},
		Status: agentsv1alpha1.SandboxClaimStatus{
			Phase:           agentsv1alpha1.SandboxClaimPhaseClaiming,
			ClaimedReplicas: 1,
			ClaimStartTime:  &startTime,
		},
	}

	recordSandboxClaimMetrics(claim)
	defer deleteSandboxClaimMetrics("default", "test-claim")

	// Verify info metric
	infoVal := testutil.ToFloat64(sandboxClaimInfo.WithLabelValues("default", "test-claim", "my-sandboxset", "claim-uid-001"))
	if infoVal != 1 {
		t.Errorf("sandbox_claim_info = %v, want 1", infoVal)
	}

	// Verify created timestamp
	createdVal := testutil.ToFloat64(sandboxClaimCreated.WithLabelValues("default", "test-claim"))
	if createdVal != float64(now.Unix()) {
		t.Errorf("sandbox_claim_created = %v, want %v", createdVal, float64(now.Unix()))
	}

	// Verify phase: Claiming=1, Completed=0
	claimingVal := testutil.ToFloat64(sandboxClaimStatusPhase.WithLabelValues("default", "test-claim", "Claiming"))
	if claimingVal != 1 {
		t.Errorf("sandbox_claim_status_phase{phase=Claiming} = %v, want 1", claimingVal)
	}
	completedVal := testutil.ToFloat64(sandboxClaimStatusPhase.WithLabelValues("default", "test-claim", "Completed"))
	if completedVal != 0 {
		t.Errorf("sandbox_claim_status_phase{phase=Completed} = %v, want 0", completedVal)
	}

	// Verify claim start time
	startVal := testutil.ToFloat64(sandboxClaimClaimStartTime.WithLabelValues("default", "test-claim"))
	if startVal != float64(startTime.Unix()) {
		t.Errorf("sandbox_claim_start_time = %v, want %v", startVal, float64(startTime.Unix()))
	}

	// Verify claimed replicas
	claimedVal := testutil.ToFloat64(sandboxClaimClaimedReplicas.WithLabelValues("default", "test-claim"))
	if claimedVal != 1 {
		t.Errorf("sandbox_claim_claimed_replicas = %v, want 1", claimedVal)
	}

	// Verify desired replicas
	desiredVal := testutil.ToFloat64(sandboxClaimDesiredReplicas.WithLabelValues("default", "test-claim"))
	if desiredVal != 3 {
		t.Errorf("sandbox_claim_desired_replicas = %v, want 3", desiredVal)
	}
}

func TestRecordSandboxClaimMetrics_CompletedPhase(t *testing.T) {
	tests := []struct {
		name                                  string
		replicas                              *int32
		claimed                               int32
		reason                                string
		status                                metav1.ConditionStatus
		noStart, reuseName, deleteBeforeReuse bool
	}{
		{name: "one replica", replicas: ptr.To[int32](1), claimed: 1, reason: "AllReplicasClaimed"},
		{name: "multiple replicas keep the request gauge and record no instance result", replicas: ptr.To[int32](3), claimed: 3, reason: "AllReplicasClaimed"},
		{name: "partial delivery times out", replicas: ptr.To[int32](3), claimed: 1, reason: "TimeoutReached"},
		{name: "missing template without a start time", replicas: ptr.To[int32](1), reason: "TemplateNotFound", noStart: true},
		{name: "enough replicas without a successful terminal condition", replicas: ptr.To[int32](1), claimed: 1, reason: "TimeoutReached"},
		{name: "success reason with insufficient delivery", replicas: ptr.To[int32](3), claimed: 1, reason: "AllReplicasClaimed"},
		{name: "Completed condition is not True", claimed: 1, reason: "AllReplicasClaimed", status: metav1.ConditionFalse},
		{name: "legacy status without a condition defaults to one replica", claimed: 1},
		{name: "legacy status without a condition has insufficient replicas", replicas: ptr.To[int32](2), claimed: 1},
		{name: "same name with a different UID", claimed: 1, reuseName: true},
		{name: "same name with a different UID after deletion", claimed: 1, reuseName: true, deleteBeforeReuse: true},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns, name := fmt.Sprintf("claim-gauge-%d", i), "completed-claim"
			now := time.Now()
			claim := &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(ns + "-uid"), CreationTimestamp: metav1.NewTime(now.Add(-2 * time.Minute))},
				Spec:       agentsv1alpha1.SandboxClaimSpec{TemplateName: "my-sandboxset", Replicas: tt.replicas},
				Status:     agentsv1alpha1.SandboxClaimStatus{Phase: agentsv1alpha1.SandboxClaimPhaseClaiming, ClaimedReplicas: tt.claimed, CompletionTime: ptr.To(metav1.NewTime(now))},
			}
			if !tt.noStart {
				claim.Status.ClaimStartTime = ptr.To(metav1.NewTime(now.Add(-time.Minute)))
			}
			if tt.reason != "" {
				status := tt.status
				if status == "" {
					status = metav1.ConditionTrue
				}
				claim.Status.Conditions = []metav1.Condition{{Type: string(agentsv1alpha1.SandboxClaimConditionCompleted), Status: status, Reason: tt.reason}}
			}
			defer deleteSandboxClaimMetrics(ns, name)
			recordSandboxClaimMetrics(claim)
			claim.Status.Phase = agentsv1alpha1.SandboxClaimPhaseCompleted
			for range 3 {
				recordSandboxClaimMetrics(claim)
			}
			if tt.reuseName {
				if tt.deleteBeforeReuse {
					deleteSandboxClaimMetrics(ns, name)
				}
				claim.UID = types.UID(ns + "-successor")
				for range 2 {
					recordSandboxClaimMetrics(claim)
				}
			}
			if got := testutil.ToFloat64(sandboxClaimClaimedReplicas.WithLabelValues(ns, name)); got != float64(tt.claimed) {
				t.Errorf("claimed replicas gauge = %v, want %v", got, tt.claimed)
			}
			if got := testutil.ToFloat64(sandboxClaimStatusPhase.WithLabelValues(ns, name, "Completed")); got != 1 {
				t.Errorf("Completed phase = %v", got)
			}
			if got := testutil.ToFloat64(sandboxClaimCompletionTime.WithLabelValues(ns, name)); got != float64(now.Unix()) {
				t.Errorf("completion time = %v", got)
			}
			reg := prometheus.NewRegistry()
			reg.MustRegister(sandboxClaimInfo, sandboxClaimCreated, sandboxClaimStatusPhase,
				sandboxClaimClaimStartTime, sandboxClaimCompletionTime, sandboxClaimClaimedReplicas, sandboxClaimDesiredReplicas)
			families, err := reg.Gather()
			if err != nil {
				t.Fatal(err)
			}
			for _, family := range families {
				if family.GetName() == "sandbox_claim_total" || family.GetName() == "sandbox_claim_duration_seconds" {
					t.Errorf("the request gauge layer must not register an instance collector: %s", family.GetName())
				}
				for _, metric := range family.Metric {
					found := false
					for _, label := range metric.Label {
						if label.GetName() == "source" && label.GetValue() == "k8s" {
							found = true
						}
					}
					if !found {
						t.Error("missing source=k8s")
					}
				}
			}
		})
	}
}

func TestRecordSandboxClaimMetrics_EmptyPhase(t *testing.T) {
	claim := &agentsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "empty-phase-claim",
			Namespace:         "default",
			UID:               types.UID("claim-uid-003"),
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Spec: agentsv1alpha1.SandboxClaimSpec{
			TemplateName: "my-sandboxset",
		},
		Status: agentsv1alpha1.SandboxClaimStatus{
			Phase: "", // empty phase
		},
	}

	// Should not panic and should skip phase metric recording
	recordSandboxClaimMetrics(claim)
	defer deleteSandboxClaimMetrics("default", "empty-phase-claim")
}

func TestDeleteSandboxClaimMetrics(t *testing.T) {
	ns, name := "default", "delete-test-claim"
	now := time.Now()
	startTime := metav1.NewTime(now)
	completionTime := metav1.NewTime(now)
	claim := &agentsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			UID:               types.UID("claim-uid-004"),
			CreationTimestamp: metav1.NewTime(now),
		},
		Spec: agentsv1alpha1.SandboxClaimSpec{
			TemplateName: "my-sandboxset",
			Replicas:     ptr.To[int32](2),
		},
		Status: agentsv1alpha1.SandboxClaimStatus{
			Phase:           agentsv1alpha1.SandboxClaimPhaseCompleted,
			ClaimedReplicas: 2,
			ClaimStartTime:  &startTime,
			CompletionTime:  &completionTime,
		},
	}

	// First record metrics
	recordSandboxClaimMetrics(claim)

	// Verify metrics are set
	val := testutil.ToFloat64(sandboxClaimCreated.WithLabelValues(ns, name))
	if val == 0 {
		t.Fatal("sandbox_claim_created should be set before delete")
	}

	infoVal := testutil.ToFloat64(sandboxClaimInfo.WithLabelValues(ns, name, "my-sandboxset", "claim-uid-004"))
	if infoVal != 1 {
		t.Errorf("sandbox_claim_info before delete = %v, want 1", infoVal)
	}

	deleteSandboxClaimMetrics(ns, name)

	// After deletion, WithLabelValues creates a new zero-value gauge.
	val = testutil.ToFloat64(sandboxClaimCreated.WithLabelValues(ns, name))
	if val != 0 {
		t.Errorf("sandbox_claim_created after delete = %v, want 0", val)
	}

	// Verify phase metrics are cleaned
	for _, phase := range allClaimPhases {
		v := testutil.ToFloat64(sandboxClaimStatusPhase.WithLabelValues(ns, name, string(phase)))
		if v != 0 {
			t.Errorf("sandbox_claim_status_phase{phase=%s} after delete = %v, want 0", phase, v)
		}
	}

	// Verify claim start time is cleaned
	startVal := testutil.ToFloat64(sandboxClaimClaimStartTime.WithLabelValues(ns, name))
	if startVal != 0 {
		t.Errorf("sandbox_claim_start_time after delete = %v, want 0", startVal)
	}

	// Verify completion time is cleaned
	compVal := testutil.ToFloat64(sandboxClaimCompletionTime.WithLabelValues(ns, name))
	if compVal != 0 {
		t.Errorf("sandbox_claim_completion_time after delete = %v, want 0", compVal)
	}

	// Verify claimed replicas is cleaned
	claimedVal := testutil.ToFloat64(sandboxClaimClaimedReplicas.WithLabelValues(ns, name))
	if claimedVal != 0 {
		t.Errorf("sandbox_claim_claimed_replicas after delete = %v, want 0", claimedVal)
	}

	// Verify desired replicas is cleaned
	desiredVal := testutil.ToFloat64(sandboxClaimDesiredReplicas.WithLabelValues(ns, name))
	if desiredVal != 0 {
		t.Errorf("sandbox_claim_desired_replicas after delete = %v, want 0", desiredVal)
	}
}
