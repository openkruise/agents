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

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
)

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
			control := NewCommonControl(fakeClient, fakeRecorder, cache)

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
			val := testutil.ToFloat64(sandboxSetClaimsTotal.WithLabelValues(tt.namespace, tt.sandboxSetName, "success"))
			assert.Equal(t, float64(1), val, "sandboxset_claims_total{result=success} should be 1")

			// Verify failed counter was not incremented
			failedVal := testutil.ToFloat64(sandboxSetClaimsTotal.WithLabelValues(tt.namespace, tt.sandboxSetName, "failed"))
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
			control := NewCommonControl(fake.NewClientBuilder().WithScheme(scheme).Build(), fakeRecorder, cache)

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
			val := testutil.ToFloat64(sandboxSetClaimsTotal.WithLabelValues(tt.namespace, tt.sandboxSetName, "failed"))
			assert.Equal(t, float64(1), val, "sandboxset_claims_total{result=failed} should be 1")

			// Verify success counter was not incremented
			successVal := testutil.ToFloat64(sandboxSetClaimsTotal.WithLabelValues(tt.namespace, tt.sandboxSetName, "success"))
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
			control := NewCommonControl(fakeClient, fakeRecorder, nil)

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
			val := testutil.ToFloat64(sandboxClaimExpiredTotal.WithLabelValues(tt.namespace, tt.claimName))
			assert.Equal(t, float64(1), val, "sandboxclaim_expired_total should be 1 after TTL deletion")
		})
	}
}

func TestDeleteSandboxClaimCounterMetrics(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		claimName string
	}{
		{
			name:      "cleanup removes expired counter label set",
			namespace: "default",
			claimName: "cleanup-claim",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandboxClaimExpiredTotal.Reset()

			// Set a value first
			sandboxClaimExpiredTotal.WithLabelValues(tt.namespace, tt.claimName).Inc()
			val := testutil.ToFloat64(sandboxClaimExpiredTotal.WithLabelValues(tt.namespace, tt.claimName))
			assert.Equal(t, float64(1), val, "counter should be 1 before cleanup")

			// Call cleanup
			DeleteSandboxClaimCounterMetrics(tt.namespace, tt.claimName)

			// After deletion, WithLabelValues creates a new zero-value metric
			val = testutil.ToFloat64(sandboxClaimExpiredTotal.WithLabelValues(tt.namespace, tt.claimName))
			assert.Equal(t, float64(0), val, "counter should be 0 after cleanup")
		})
	}
}
