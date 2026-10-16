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
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// histogramSum collects the current sample sum from a HistogramVec metric for a given namespace.
func histogramSum(t *testing.T, namespace string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := sandboxClaimClaimDuration.WithLabelValues(namespace).(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("failed to write histogram metric: %v", err)
	}
	return m.GetHistogram().GetSampleSum()
}

// claimDurationSampleCount collects the current sample count from the claim duration
// HistogramVec for a given namespace.
func claimDurationSampleCount(t *testing.T, namespace string) uint64 {
	t.Helper()
	m := &dto.Metric{}
	if err := sandboxClaimClaimDuration.WithLabelValues(namespace).(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("failed to write histogram metric: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

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
	now := time.Now()
	completionTime := metav1.NewTime(now)
	startTime := metav1.NewTime(now.Add(-1 * time.Minute))
	claim := &agentsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "completed-claim",
			Namespace:         "default",
			UID:               types.UID("claim-uid-002"),
			CreationTimestamp: metav1.NewTime(now.Add(-2 * time.Minute)),
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

	recordSandboxClaimMetrics(claim)
	defer deleteSandboxClaimMetrics("default", "completed-claim")

	// Verify phase: Claiming=0, Completed=1
	claimingVal := testutil.ToFloat64(sandboxClaimStatusPhase.WithLabelValues("default", "completed-claim", "Claiming"))
	if claimingVal != 0 {
		t.Errorf("sandbox_claim_status_phase{phase=Claiming} = %v, want 0", claimingVal)
	}
	completedVal := testutil.ToFloat64(sandboxClaimStatusPhase.WithLabelValues("default", "completed-claim", "Completed"))
	if completedVal != 1 {
		t.Errorf("sandbox_claim_status_phase{phase=Completed} = %v, want 1", completedVal)
	}

	// Verify completion time
	compVal := testutil.ToFloat64(sandboxClaimCompletionTime.WithLabelValues("default", "completed-claim"))
	if compVal != float64(completionTime.Unix()) {
		t.Errorf("sandbox_claim_completion_time = %v, want %v", compVal, float64(completionTime.Unix()))
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

	// Simulate the controller storing precise in-memory timestamps after a successful
	// status patch, so the delete cleanup of both caches can be verified.
	uid := string(claim.UID)
	storePreciseTime(&preciseClaimStartTimes, uid, startTime.Time)
	storePreciseTime(&preciseClaimCompletionTimes, uid, completionTime.Time)
	preciseClaimUIDs.Store(ns+"/"+name, uid)

	// Delete metrics
	deleteSandboxClaimMetrics(ns, name)

	// Verify precise timestamp caches are cleaned
	if _, ok := preciseClaimStartTimes.Load(uid); ok {
		t.Errorf("preciseClaimStartTimes should be cleaned after delete")
	}
	if _, ok := preciseClaimCompletionTimes.Load(uid); ok {
		t.Errorf("preciseClaimCompletionTimes should be cleaned after delete")
	}
	if _, ok := preciseClaimUIDs.Load(ns + "/" + name); ok {
		t.Errorf("preciseClaimUIDs should be cleaned after delete")
	}

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

func TestSandboxClaimClaimDuration_ObservedOnce(t *testing.T) {
	now := time.Now()
	startTime := metav1.NewTime(now.Add(-30 * time.Second))
	completionTime := metav1.NewTime(now)
	claim := &agentsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "duration-test-claim",
			Namespace:         "default",
			UID:               types.UID("claim-uid-005"),
			CreationTimestamp: metav1.NewTime(now.Add(-1 * time.Minute)),
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

	// Get initial sum value from histogram
	beforeSum := histogramSum(t, "default")

	// First call should observe
	recordSandboxClaimMetrics(claim)

	afterFirstSum := histogramSum(t, "default")
	expectedDuration := completionTime.Sub(startTime.Time).Seconds()
	if afterFirstSum-beforeSum != expectedDuration {
		t.Errorf("first observation: sum delta = %v, want %v", afterFirstSum-beforeSum, expectedDuration)
	}

	// Second call should NOT observe (deduplicated)
	recordSandboxClaimMetrics(claim)

	afterSecondSum := histogramSum(t, "default")
	if afterSecondSum != afterFirstSum {
		t.Errorf("second call should not change sum: got %v, want %v", afterSecondSum, afterFirstSum)
	}

	// Delete metrics and re-record should observe again
	deleteSandboxClaimMetrics("default", "duration-test-claim")
	// After DeleteLabelValues, the histogram series is reset; read the new baseline.
	baselineAfterDelete := histogramSum(t, "default")
	recordSandboxClaimMetrics(claim)

	afterReObserve := histogramSum(t, "default")
	if afterReObserve-baselineAfterDelete != expectedDuration {
		t.Errorf("re-observation after delete: sum delta = %v, want %v", afterReObserve-baselineAfterDelete, expectedDuration)
	}

	// Final cleanup
	deleteSandboxClaimMetrics("default", "duration-test-claim")
}

func TestSandboxClaimClaimDuration_NotObservedForClaimingPhase(t *testing.T) {
	now := time.Now()
	startTime := metav1.NewTime(now.Add(-10 * time.Second))
	claim := &agentsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "claiming-phase-claim",
			Namespace:         "default",
			UID:               types.UID("claim-uid-006"),
			CreationTimestamp: metav1.NewTime(now),
		},
		Spec: agentsv1alpha1.SandboxClaimSpec{
			TemplateName: "my-sandboxset",
			Replicas:     ptr.To[int32](1),
		},
		Status: agentsv1alpha1.SandboxClaimStatus{
			Phase:          agentsv1alpha1.SandboxClaimPhaseClaiming,
			ClaimStartTime: &startTime,
			// No CompletionTime
		},
	}

	beforeSum := histogramSum(t, "default")
	recordSandboxClaimMetrics(claim)
	afterSum := histogramSum(t, "default")

	if afterSum != beforeSum {
		t.Errorf("claiming phase should not observe histogram: sum changed from %v to %v", beforeSum, afterSum)
	}
	deleteSandboxClaimMetrics("default", "claiming-phase-claim")
}

func TestRecordSandboxClaimMetrics_PreciseDuration(t *testing.T) {
	base := time.Date(2026, time.October, 14, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		preciseUID   string
		preciseStart *time.Time
		preciseDone  *time.Time
		statusStart  time.Time
		statusDone   time.Time
		extraCalls   int
		wantCount    uint64
		wantSum      float64
	}{
		{
			// Both status timestamps round-tripped through the API server and are
			// second-truncated; without the in-memory precise values the duration
			// would collapse to 0.
			name:         "precise same-second timestamps win over truncated status",
			preciseStart: ptr.To(base.Add(100 * time.Millisecond)),
			preciseDone:  ptr.To(base.Add(750 * time.Millisecond)),
			statusStart:  base,
			statusDone:   base,
			wantCount:    1,
			wantSum:      0.65,
		},
		{
			name:        "missing precise timestamps fall back to cached status values",
			statusStart: base,
			statusDone:  base.Add(2 * time.Second),
			wantCount:   1,
			wantSum:     2,
		},
		{
			name:         "completion not after start is not observed",
			preciseStart: ptr.To(base.Add(750 * time.Millisecond)),
			preciseDone:  ptr.To(base.Add(100 * time.Millisecond)),
			statusStart:  base,
			statusDone:   base,
			wantCount:    0,
			wantSum:      0,
		},
		{
			name:        "repeated calls do not add samples",
			statusStart: base,
			statusDone:  base.Add(time.Second),
			extraCalls:  2,
			wantCount:   1,
			wantSum:     1,
		},
		{
			// Precise timestamps are keyed by claim UID because names are reused;
			// values stored under another claim's UID must not be inherited, and
			// the duration falls back to the status timestamps.
			name:         "precise times under a different uid are not inherited",
			preciseUID:   "other-claim-uid",
			preciseStart: ptr.To(base.Add(100 * time.Millisecond)),
			preciseDone:  ptr.To(base.Add(750 * time.Millisecond)),
			statusStart:  base,
			statusDone:   base.Add(2 * time.Second),
			wantCount:    1,
			wantSum:      2,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := fmt.Sprintf("claim-dur-precise-%d", i)
			name := "precise-claim"
			claim := &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:              name,
					Namespace:         ns,
					UID:               types.UID(fmt.Sprintf("claim-uid-precise-%d", i)),
					CreationTimestamp: metav1.NewTime(base),
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "my-sandboxset",
					Replicas:     ptr.To[int32](1),
				},
				Status: agentsv1alpha1.SandboxClaimStatus{
					Phase:           agentsv1alpha1.SandboxClaimPhaseCompleted,
					ClaimedReplicas: 1,
					ClaimStartTime:  ptr.To(metav1.NewTime(tt.statusStart)),
					CompletionTime:  ptr.To(metav1.NewTime(tt.statusDone)),
				},
			}

			storeKey := string(claim.UID)
			if tt.preciseUID != "" {
				storeKey = tt.preciseUID
			}
			if tt.preciseStart != nil {
				storePreciseTime(&preciseClaimStartTimes, storeKey, *tt.preciseStart)
			}
			if tt.preciseDone != nil {
				storePreciseTime(&preciseClaimCompletionTimes, storeKey, *tt.preciseDone)
			}
			defer func() {
				deleteSandboxClaimMetrics(ns, name)
				preciseClaimStartTimes.Delete(storeKey)
				preciseClaimCompletionTimes.Delete(storeKey)
			}()

			recordSandboxClaimMetrics(claim)
			for j := 0; j < tt.extraCalls; j++ {
				recordSandboxClaimMetrics(claim)
			}

			if got := claimDurationSampleCount(t, ns); got != tt.wantCount {
				t.Errorf("claim duration sample count = %d, want %d", got, tt.wantCount)
			}
			if got := histogramSum(t, ns); got < tt.wantSum-0.001 || got > tt.wantSum+0.001 {
				t.Errorf("claim duration sample sum = %v, want %v", got, tt.wantSum)
			}
		})
	}
}

func TestStorePreciseTime(t *testing.T) {
	base := time.Date(2026, time.October, 14, 12, 0, 0, 0, time.UTC)
	truncated := base
	precise := base.Add(250 * time.Millisecond)

	tests := []struct {
		name   string
		stored []time.Time
		want   time.Time
	}{
		{
			name:   "first value is stored",
			stored: []time.Time{precise},
			want:   precise,
		},
		{
			name:   "truncated value is upgraded to precise same-second value",
			stored: []time.Time{truncated, precise},
			want:   precise,
		},
		{
			name:   "truncated value does not overwrite precise value",
			stored: []time.Time{precise, truncated},
			want:   precise,
		},
		{
			name:   "second precise value does not overwrite the first",
			stored: []time.Time{precise, base.Add(500 * time.Millisecond)},
			want:   precise,
		},
		{
			name:   "precise value from another second does not upgrade truncated value",
			stored: []time.Time{truncated, base.Add(1250 * time.Millisecond)},
			want:   truncated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &sync.Map{}
			for _, v := range tt.stored {
				storePreciseTime(m, "test-key", v)
			}
			got, ok := m.Load("test-key")
			if !ok {
				t.Fatal("expected value to be stored")
			}
			if !got.(time.Time).Equal(tt.want) {
				t.Errorf("stored value = %v, want %v", got.(time.Time), tt.want)
			}
		})
	}
}
