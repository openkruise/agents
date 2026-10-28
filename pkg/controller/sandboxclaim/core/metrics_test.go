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

package core

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

func TestRecordSandboxClaims(t *testing.T) {
	hot := agentsv1alpha1.ClaimMethodHotStart
	cold := agentsv1alpha1.ClaimMethodColdStart
	success, failure := infra.ClaimResultSuccess, infra.ClaimResultFailure
	result := func(uid, id, method, outcome string, duration time.Duration) infra.SandboxClaimResult {
		return infra.SandboxClaimResult{UID: uid, ID: id, Namespace: "claim-metrics", Method: method, Result: outcome, Duration: duration}
	}
	a := result("a", "cycle-a", hot, success, time.Second)
	b := result("b", "cycle-b", hot, failure, 2*time.Second)
	c := result("c", "cycle-c", cold, success, 3*time.Second)
	d := result("d", "cycle-d", cold, failure, 4*time.Second)
	for _, tt := range []struct {
		name    string
		results []infra.SandboxClaimResult
		counts  [4]float64
		sums    [4]float64
	}{
		{name: "no bound instance"},
		{name: "hot success", results: []infra.SandboxClaimResult{a}, counts: [4]float64{1}, sums: [4]float64{1}},
		{name: "hot failure", results: []infra.SandboxClaimResult{b}, counts: [4]float64{0, 1}, sums: [4]float64{0, 2}},
		{name: "cold success and failure", results: []infra.SandboxClaimResult{c, d}, counts: [4]float64{0, 0, 1, 1}, sums: [4]float64{0, 0, 3, 4}},
		{name: "mixed batch", results: []infra.SandboxClaimResult{a, b, c, d}, counts: [4]float64{1, 1, 1, 1}, sums: [4]float64{1, 2, 3, 4}},
		{name: "duplicate UID and cycle", results: []infra.SandboxClaimResult{a, a}, counts: [4]float64{1}, sums: [4]float64{1}},
		{name: "same UID new cycle", results: []infra.SandboxClaimResult{a, result("a", "next", hot, success, 2*time.Second)}, counts: [4]float64{2}, sums: [4]float64{3}},
		{name: "invalid facts", results: []infra.SandboxClaimResult{
			result("", "id", hot, success, time.Second), result("uid", "", hot, success, time.Second),
			result("uid", "id", "", success, time.Second), result("uid", "id", hot, "pending", time.Second),
			result("uid", "id", hot, success, -time.Second),
		}},
		{name: "zero duration is valid", results: []infra.SandboxClaimResult{result("uid", "id", hot, success, 0)}, counts: [4]float64{1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sandboxClaimTotal.Reset()
			sandboxClaimDuration.Reset()
			t.Cleanup(func() { sandboxClaimTotal.Reset(); sandboxClaimDuration.Reset() })
			recordSandboxClaims(tt.results)
			for i, labels := range [][]string{{"claim-metrics", success, hot}, {"claim-metrics", failure, hot}, {"claim-metrics", success, cold}, {"claim-metrics", failure, cold}} {
				assert.Equal(t, tt.counts[i], testutil.ToFloat64(sandboxClaimTotal.WithLabelValues(labels...)))
				m := &dto.Metric{}
				require.NoError(t, sandboxClaimDuration.WithLabelValues(labels...).(prometheus.Metric).Write(m))
				assert.Equal(t, uint64(tt.counts[i]), m.GetHistogram().GetSampleCount())
				assert.Equal(t, tt.sums[i], m.GetHistogram().GetSampleSum())
				bounds := append(prometheus.ExponentialBuckets(0.01, 2, 16), 600, 1200)
				require.Len(t, m.GetHistogram().Bucket, len(bounds))
				for j, bucket := range m.GetHistogram().Bucket {
					assert.Equal(t, bounds[j], bucket.GetUpperBound())
				}
			}
			registry := prometheus.NewRegistry()
			registry.MustRegister(sandboxClaimTotal, sandboxClaimDuration)
			families, err := registry.Gather()
			require.NoError(t, err)
			require.Len(t, families, 2)
			for _, family := range families {
				for _, metric := range family.Metric {
					labels := map[string]string{}
					for _, label := range metric.Label {
						labels[label.GetName()] = label.GetValue()
					}
					assert.Len(t, labels, 4)
					assert.Equal(t, "k8s", labels["source"])
					assert.Equal(t, "claim-metrics", labels["namespace"])
					assert.NotContains(t, labels, "lock_type")
				}
			}
		})
	}
}

func TestClaimSandboxesRecordsOutcomes(t *testing.T) {
	for i, tt := range []struct {
		name                string
		poolSize, batchSize int
		failAfterBinding    bool
	}{
		{name: "success", poolSize: 1, batchSize: 1},
		{name: "batch counts instances", poolSize: 3, batchSize: 3},
		{name: "post-binding failure still counted", poolSize: 1, batchSize: 1, failAfterBinding: true},
		{name: "pre-binding failure adds no instance", batchSize: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ns := fmt.Sprintf("claim-outcomes-%d", i)
			sandboxClaimTotal.Reset()
			sandboxClaimDuration.Reset()
			t.Cleanup(func() { sandboxClaimTotal.Reset(); sandboxClaimDuration.Reset() })
			sbs := &agentsv1alpha1.SandboxSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "pool"}}
			cache, fc, err := cachetest.NewTestCache(t, sbs)
			require.NoError(t, err)
			for j := 0; j < tt.poolSize; j++ {
				sb := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: ns, Name: fmt.Sprintf("sandbox-%d", j), UID: types.UID(fmt.Sprintf("%s-%d", ns, j)),
						CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
						OwnerReferences:   []metav1.OwnerReference{*metav1.NewControllerRef(sbs, agentsv1alpha1.SandboxSetControllerKind)},
						Labels:            map[string]string{agentsv1alpha1.LabelSandboxTemplate: sbs.Name, agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.False},
					},
					Spec: agentsv1alpha1.SandboxSpec{EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "test"}}}}}},
				}
				require.NoError(t, fc.Create(t.Context(), sb))
				sb.Status = agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning, PodInfo: agentsv1alpha1.PodInfo{PodIP: "10.0.0.1"}, Conditions: []metav1.Condition{{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}}
				require.NoError(t, fc.Status().Update(t.Context(), sb))
			}
			claim := &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "claim", UID: "claim-uid"},
				Spec:       agentsv1alpha1.SandboxClaimSpec{TemplateName: sbs.Name, SkipInitRuntime: true, ReserveFailedSandbox: true},
			}
			if tt.failAfterBinding {
				claim.Spec.Annotations = map[string]string{agentsv1alpha1.AnnotationRuntimeTLSPort: "49984"}
			}
			control := NewCommonControl(fc, record.NewFakeRecorder(10), cache, nil).(*commonControl)
			opts, err := control.buildClaimOptions(t.Context(), claim, sbs)
			require.NoError(t, err)
			assert.Equal(t, agentsv1alpha1.SandboxSourceK8s, opts.ControlPlaneSource)
			count, err := control.claimSandboxes(t.Context(), claim, sbs, tt.batchSize)
			wantSuccess, wantFailure := float64(tt.poolSize), 0.0
			if tt.failAfterBinding || tt.poolSize == 0 {
				require.Error(t, err)
				wantSuccess = 0
				if tt.failAfterBinding {
					wantFailure = 1
				}
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, int(wantSuccess), count)
			for outcome, want := range map[string]float64{infra.ClaimResultSuccess: wantSuccess, infra.ClaimResultFailure: wantFailure} {
				assert.Equal(t, want, testutil.ToFloat64(sandboxClaimTotal.WithLabelValues(ns, outcome, agentsv1alpha1.ClaimMethodHotStart)))
				m := &dto.Metric{}
				require.NoError(t, sandboxClaimDuration.WithLabelValues(ns, outcome, agentsv1alpha1.ClaimMethodHotStart).(prometheus.Metric).Write(m))
				assert.Equal(t, uint64(want), m.GetHistogram().GetSampleCount())
			}
		})
	}
}

func TestSandboxSetClaimsTotal_Success(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name           string
		namespace      string
		sandboxSetName string
		claimUID       string
		claimedStatus  int32
		desiredReplica int32
		expectError    string
	}{
		{
			name:           "claim completes successfully when replicas already met",
			namespace:      "default",
			sandboxSetName: "my-sandboxset",
			claimUID:       "uid-success-1",
			claimedStatus:  3,
			desiredReplica: 3,
			expectError:    "",
		},
		{
			name:           "claim completes successfully with single replica",
			namespace:      "production",
			sandboxSetName: "prod-pool",
			claimUID:       "uid-success-2",
			claimedStatus:  1,
			desiredReplica: 1,
			expectError:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset counter for test isolation
			sandboxSetClaimsTotal.Reset()

			claim := &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim",
					Namespace: tt.namespace,
					UID:       "test-uid",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: tt.sandboxSetName,
					Replicas:     int32Ptr(tt.desiredReplica),
				},
				Status: agentsv1alpha1.SandboxClaimStatus{
					ClaimedReplicas: tt.claimedStatus,
				},
			}

			sandboxSet := &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tt.sandboxSetName,
					Namespace: tt.namespace,
				},
			}

			// Setup sandboxes to match claimedStatus count
			sandboxes := make([]*agentsv1alpha1.Sandbox, 0, tt.claimedStatus)
			for i := int32(0); i < tt.claimedStatus; i++ {
				sbx := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("sandbox-%d", i),
						Namespace: tt.namespace,
						Annotations: map[string]string{
							agentsv1alpha1.AnnotationOwner: "test-uid",
						},
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxTemplate:  tt.sandboxSetName,
							agentsv1alpha1.LabelSandboxIsClaimed: "true",
						},
					},
				}
				sandboxes = append(sandboxes, sbx)
			}

			cache, fakeClient, err := cachetest.NewTestCache(t, claim, sandboxSet)
			require.NoError(t, err)

			ctx := context.Background()
			for _, sbx := range sandboxes {
				require.NoError(t, fakeClient.Create(ctx, sbx))
			}

			fakeRecorder := record.NewFakeRecorder(100)
			control := NewCommonControl(fakeClient, fakeRecorder, cache, nil)

			newStatus := &agentsv1alpha1.SandboxClaimStatus{
				Phase:           agentsv1alpha1.SandboxClaimPhaseClaiming,
				ClaimedReplicas: tt.claimedStatus,
			}

			args := ClaimArgs{
				Claim:      claim,
				SandboxSet: sandboxSet,
				NewStatus:  newStatus,
			}

			_, err = control.EnsureClaimClaiming(ctx, args)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}

			// Verify counter incremented with success label
			val := testutil.ToFloat64(sandboxSetClaimsTotal.WithLabelValues(tt.namespace, "success"))
			assert.Equal(t, float64(1), val, "sandboxset_claims_total{result=success} should be 1")

			// Verify failed counter was not incremented
			failedVal := testutil.ToFloat64(sandboxSetClaimsTotal.WithLabelValues(tt.namespace, "failed"))
			assert.Equal(t, float64(0), failedVal, "sandboxset_claims_total{result=failed} should be 0")
		})
	}
}

func TestSandboxSetClaimsTotal_Failed(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name           string
		namespace      string
		sandboxSetName string
		expectError    string
	}{
		{
			name:           "no available sandboxes triggers failed counter",
			namespace:      "default",
			sandboxSetName: "empty-pool",
			expectError:    "",
		},
		{
			name:           "different namespace no available sandboxes",
			namespace:      "staging",
			sandboxSetName: "staging-pool",
			expectError:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset counter for test isolation
			sandboxSetClaimsTotal.Reset()

			claim := &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim",
					Namespace: tt.namespace,
					UID:       "test-uid",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: tt.sandboxSetName,
					Replicas:     int32Ptr(2),
				},
				Status: agentsv1alpha1.SandboxClaimStatus{
					ClaimedReplicas: 0,
				},
			}

			sandboxSet := &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tt.sandboxSetName,
					Namespace: tt.namespace,
				},
			}

			cache, _, err := cachetest.NewTestCache(t, claim, sandboxSet)
			require.NoError(t, err)

			fakeRecorder := record.NewFakeRecorder(100)
			control := NewCommonControl(fake.NewClientBuilder().WithScheme(scheme).Build(), fakeRecorder, cache, nil)

			newStatus := &agentsv1alpha1.SandboxClaimStatus{
				Phase:           agentsv1alpha1.SandboxClaimPhaseClaiming,
				ClaimedReplicas: 0,
			}

			args := ClaimArgs{
				Claim:      claim,
				SandboxSet: sandboxSet,
				NewStatus:  newStatus,
			}

			ctx := context.Background()
			_, err = control.EnsureClaimClaiming(ctx, args)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}

			// Verify failed counter incremented
			val := testutil.ToFloat64(sandboxSetClaimsTotal.WithLabelValues(tt.namespace, "failed"))
			assert.Equal(t, float64(1), val, "sandboxset_claims_total{result=failed} should be 1")

			// Verify success counter was not incremented
			successVal := testutil.ToFloat64(sandboxSetClaimsTotal.WithLabelValues(tt.namespace, "success"))
			assert.Equal(t, float64(0), successVal, "sandboxset_claims_total{result=success} should be 0")
		})
	}
}

func TestSandboxClaimExpiredTotal(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)

	pastTime := metav1.NewTime(time.Now().Add(-10 * time.Second))

	tests := []struct {
		name        string
		namespace   string
		claimName   string
		ttl         time.Duration
		expectError string
	}{
		{
			name:        "TTL expired claim deletion increments counter",
			namespace:   "default",
			claimName:   "expired-claim-1",
			ttl:         5 * time.Second,
			expectError: "",
		},
		{
			name:        "TTL expired claim in different namespace",
			namespace:   "production",
			claimName:   "expired-claim-2",
			ttl:         3 * time.Second,
			expectError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset counter for test isolation
			sandboxClaimExpiredTotal.Reset()

			claim := &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tt.claimName,
					Namespace: tt.namespace,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:      "test-template",
					TTLAfterCompleted: &metav1.Duration{Duration: tt.ttl},
				},
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(claim).
				WithStatusSubresource(&agentsv1alpha1.SandboxClaim{}).
				Build()

			fakeRecorder := record.NewFakeRecorder(10)
			control := NewCommonControl(fakeClient, fakeRecorder, nil, nil)

			newStatus := &agentsv1alpha1.SandboxClaimStatus{
				Phase:          agentsv1alpha1.SandboxClaimPhaseCompleted,
				CompletionTime: &pastTime, // 10 seconds ago, TTL is shorter
			}

			args := ClaimArgs{
				Claim:     claim,
				NewStatus: newStatus,
			}

			ctx := context.Background()
			_, err := control.EnsureClaimCompleted(ctx, args)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}

			// Verify expired counter incremented
			val := testutil.ToFloat64(sandboxClaimExpiredTotal.WithLabelValues(tt.namespace))
			assert.Equal(t, float64(1), val, "sandboxclaim_expired_total should be 1 after TTL deletion")
		})
	}
}
