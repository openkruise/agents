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

package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned"
	sandboxfake "github.com/openkruise/agents/client/clientset/versioned/fake"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
)

func TestNewCommonControl(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	fakeRecorder := record.NewFakeRecorder(10)

	// NewCommonControl should handle nil cache/client gracefully
	control := NewCommonControl(fakeClient, fakeRecorder, nil, nil)

	require.NotNil(t, control, "NewCommonControl() returned nil")

	// Check it implements the interface
	var _ ClaimControl = control
}

func TestRequeueStrategy_Factories(t *testing.T) {
	tests := []struct {
		name             string
		factory          func() RequeueStrategy
		expectedStrategy RequeueStrategy
	}{
		{
			name:    "RequeueImmediately",
			factory: RequeueImmediately,
			expectedStrategy: RequeueStrategy{
				Immediate: true,
				After:     0,
			},
		},
		{
			name:    "NoRequeue",
			factory: NoRequeue,
			expectedStrategy: RequeueStrategy{
				Immediate: false,
				After:     0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.factory()
			assert.Equal(t, tt.expectedStrategy.Immediate, got.Immediate, "Immediate mismatch")
			assert.Equal(t, tt.expectedStrategy.After, got.After, "After mismatch")
		})
	}

	// Test RequeueAfter separately with different durations
	t.Run("RequeueAfter", func(t *testing.T) {
		durations := []time.Duration{
			1 * time.Second,
			5 * time.Minute,
			1 * time.Hour,
		}
		for _, d := range durations {
			t.Run(d.String(), func(t *testing.T) {
				got := RequeueAfter(d)
				assert.False(t, got.Immediate, "RequeueAfter(%v).Immediate should be false", d)
				assert.Equal(t, d, got.After, "RequeueAfter(%v).After mismatch", d)
			})
		}
	})
}

func TestNewClaimControl(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	fakeRecorder := record.NewFakeRecorder(10)

	controls := NewClaimControl(fakeClient, fakeRecorder, nil, nil)

	require.NotNil(t, controls, "NewClaimControl() returned nil")

	// Verify the map contains expected control
	commonControl, exists := controls[CommonControlName]
	require.True(t, exists, "NewClaimControl() missing CommonControlName key")
	require.NotNil(t, commonControl, "NewClaimControl() CommonControl is nil")

	// Verify it implements the interface
	var _ ClaimControl = commonControl
}

func TestCommonControl_EnsureClaimClaiming(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)

	cache, clientSet, err := sandboxcr.NewTestCache(t)
	require.NoError(t, err, "Failed to create cache")
	sandboxClient := clientSet.SandboxClient

	// Start cache
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = cache.Run(ctx)
	}()
	time.Sleep(200 * time.Millisecond) // Wait for cache to start

	tests := []struct {
		name             string
		claim            *agentsv1alpha1.SandboxClaim
		sandboxSet       *agentsv1alpha1.SandboxSet
		newStatus        *agentsv1alpha1.SandboxClaimStatus
		setupSandboxes   func(*testing.T) []*agentsv1alpha1.Sandbox
		expectedStrategy RequeueStrategy
		expectError      bool
		checkStatus      func(*testing.T, *agentsv1alpha1.SandboxClaimStatus)
	}{
		{
			name: "no available sandboxes - should retry after interval",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim",
					Namespace: "default",
					UID:       "test-uid",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					Replicas:     int32Ptr(2),
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			newStatus: &agentsv1alpha1.SandboxClaimStatus{
				Phase:           agentsv1alpha1.SandboxClaimPhaseClaiming,
				ClaimedReplicas: 0,
			},
			setupSandboxes: func(t *testing.T) []*agentsv1alpha1.Sandbox {
				// No available sandboxes
				return nil
			},
			expectedStrategy: RequeueAfter(ClaimRetryInterval),
			expectError:      false,
			checkStatus: func(t *testing.T, status *agentsv1alpha1.SandboxClaimStatus) {
				assert.Equal(t, int32(0), status.ClaimedReplicas, "ClaimedReplicas mismatch")
			},
		},
		{
			name: "replicas already met - should transition to completed",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-2",
					Namespace: "default",
					UID:       "test-uid-2",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					Replicas:     int32Ptr(2),
				},
				Status: agentsv1alpha1.SandboxClaimStatus{
					ClaimedReplicas: 2, // Already claimed 2
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			newStatus: &agentsv1alpha1.SandboxClaimStatus{
				Phase:           agentsv1alpha1.SandboxClaimPhaseClaiming,
				ClaimedReplicas: 2,
			},
			setupSandboxes: func(t *testing.T) []*agentsv1alpha1.Sandbox {
				// Create 2 sandboxes already claimed
				sandboxes := []*agentsv1alpha1.Sandbox{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "sandbox-1",
							Namespace: "default",
							Annotations: map[string]string{
								agentsv1alpha1.AnnotationOwner: "test-uid-2",
							},
							Labels: map[string]string{
								agentsv1alpha1.LabelSandboxTemplate:  "test-template",
								agentsv1alpha1.LabelSandboxIsClaimed: "true",
								agentsv1alpha1.LabelSandboxClaimName: "test-claim-2",
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "sandbox-2",
							Namespace: "default",
							Annotations: map[string]string{
								agentsv1alpha1.AnnotationOwner: "test-uid-2",
							},
							Labels: map[string]string{
								agentsv1alpha1.LabelSandboxTemplate:  "test-template",
								agentsv1alpha1.LabelSandboxIsClaimed: "true",
								agentsv1alpha1.LabelSandboxClaimName: "test-claim-2",
							},
						},
					},
				}
				// Add sandboxes to cache
				for _, sbx := range sandboxes {
					_, err := sandboxClient.ApiV1alpha1().Sandboxes(sbx.Namespace).Create(ctx, sbx, metav1.CreateOptions{})
					require.NoError(t, err, "Failed to create sandbox in sandboxClient")
				}
				time.Sleep(100 * time.Millisecond) // Wait for cache sync
				return sandboxes
			},
			expectedStrategy: RequeueImmediately(), // Should requeue to transition to Completed
			expectError:      false,
			checkStatus: func(t *testing.T, status *agentsv1alpha1.SandboxClaimStatus) {
				assert.Equal(t, int32(2), status.ClaimedReplicas, "ClaimedReplicas mismatch")
			},
		},
		{
			name: "actualCount > statusCount - recovery from status update failure",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-3",
					Namespace: "default",
					UID:       "test-uid-3",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					Replicas:     int32Ptr(3),
				},
				Status: agentsv1alpha1.SandboxClaimStatus{
					ClaimedReplicas: 1, // Status shows only 1 claimed
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			newStatus: &agentsv1alpha1.SandboxClaimStatus{
				Phase:           agentsv1alpha1.SandboxClaimPhaseClaiming,
				ClaimedReplicas: 1, // Initial status count
			},
			setupSandboxes: func(t *testing.T) []*agentsv1alpha1.Sandbox {
				// Create 2 sandboxes that are already claimed (simulating crash after claim but before status update)
				// The status says 1, but actually 2 are claimed
				sandboxes := []*agentsv1alpha1.Sandbox{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "sandbox-3a",
							Namespace: "default",
							Annotations: map[string]string{
								agentsv1alpha1.AnnotationOwner: "test-uid-3",
							},
							Labels: map[string]string{
								agentsv1alpha1.LabelSandboxTemplate:  "test-template",
								agentsv1alpha1.LabelSandboxIsClaimed: "true",
								agentsv1alpha1.LabelSandboxClaimName: "test-claim-3",
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "sandbox-3b",
							Namespace: "default",
							Annotations: map[string]string{
								agentsv1alpha1.AnnotationOwner: "test-uid-3",
							},
							Labels: map[string]string{
								agentsv1alpha1.LabelSandboxTemplate:  "test-template",
								agentsv1alpha1.LabelSandboxIsClaimed: "true",
								agentsv1alpha1.LabelSandboxClaimName: "test-claim-3",
							},
						},
					},
				}
				// Add sandboxes to cache
				for _, sbx := range sandboxes {
					_, err := sandboxClient.ApiV1alpha1().Sandboxes(sbx.Namespace).Create(ctx, sbx, metav1.CreateOptions{})
					require.NoError(t, err, "Failed to create sandbox in sandboxClient")
				}
				time.Sleep(100 * time.Millisecond) // Wait for cache sync
				return sandboxes
			},
			expectedStrategy: RequeueAfter(ClaimRetryInterval), // Should retry to claim remaining 1
			expectError:      false,
			checkStatus: func(t *testing.T, status *agentsv1alpha1.SandboxClaimStatus) {
				assert.Equal(t, int32(2), status.ClaimedReplicas, "Expected ClaimedReplicas to be recovered to 2 (actualCount)")
			},
		},
		{
			name: "skip dead sandboxes",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-4",
					Namespace: "default",
					UID:       "test-uid-4",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					Replicas:     int32Ptr(3),
				},
				Status: agentsv1alpha1.SandboxClaimStatus{
					ClaimedReplicas: 1, // Status shows only 1 claimed
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			newStatus: &agentsv1alpha1.SandboxClaimStatus{
				Phase:           agentsv1alpha1.SandboxClaimPhaseClaiming,
				ClaimedReplicas: 1, // Initial status count
			},
			setupSandboxes: func(t *testing.T) []*agentsv1alpha1.Sandbox {
				// Create 2 sandboxes that are already claimed (simulating crash after claim but before status update)
				// The status says 1, but actually 2 are claimed, including 1 dead sandbox
				sandboxes := []*agentsv1alpha1.Sandbox{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "sandbox-dead",
							Namespace: "default",
							Annotations: map[string]string{
								agentsv1alpha1.AnnotationOwner: "test-uid-4",
							},
							Labels: map[string]string{
								agentsv1alpha1.LabelSandboxTemplate:  "test-template",
								agentsv1alpha1.LabelSandboxIsClaimed: "true",
								agentsv1alpha1.LabelSandboxClaimName: "test-claim-3",
							},
						},
						Status: agentsv1alpha1.SandboxStatus{
							Phase: agentsv1alpha1.SandboxRunning,
							Conditions: []metav1.Condition{
								{
									Type:   string(agentsv1alpha1.SandboxConditionReady),
									Status: metav1.ConditionFalse,
								},
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "sandbox-alive",
							Namespace: "default",
							Annotations: map[string]string{
								agentsv1alpha1.AnnotationOwner: "test-uid-4",
							},
							Labels: map[string]string{
								agentsv1alpha1.LabelSandboxTemplate:  "test-template",
								agentsv1alpha1.LabelSandboxIsClaimed: "true",
								agentsv1alpha1.LabelSandboxClaimName: "test-claim-3",
							},
						},
						Status: agentsv1alpha1.SandboxStatus{
							Phase: agentsv1alpha1.SandboxRunning,
							Conditions: []metav1.Condition{
								{
									Type:   string(agentsv1alpha1.SandboxConditionReady),
									Status: metav1.ConditionTrue,
								},
							},
						},
					},
				}
				// Add sandboxes to cache
				for _, sbx := range sandboxes {
					CreateSandboxWithStatus(t, sandboxClient, sbx)
				}
				time.Sleep(100 * time.Millisecond) // Wait for cache sync
				return sandboxes
			},
			expectedStrategy: RequeueAfter(ClaimRetryInterval), // Should retry to claim remaining 1
			expectError:      false,
			checkStatus: func(t *testing.T, status *agentsv1alpha1.SandboxClaimStatus) {
				assert.Equal(t, int32(1), status.ClaimedReplicas, "Expected ClaimedReplicas to be still 1 (dead sandbox skipped)")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup sandboxes
			if tt.setupSandboxes != nil {
				tt.setupSandboxes(t)
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.claim, tt.sandboxSet).
				Build()

			fakeRecorder := record.NewFakeRecorder(100)

			control := NewCommonControl(fakeClient, fakeRecorder, clientSet, cache)

			args := ClaimArgs{
				Claim:      tt.claim,
				SandboxSet: tt.sandboxSet,
				NewStatus:  tt.newStatus,
			}

			strategy, err := control.EnsureClaimClaiming(ctx, args)

			// Check error
			if tt.expectError {
				assert.Error(t, err, "Expected error but got nil")
			} else {
				assert.NoError(t, err, "Unexpected error")
			}

			// Check requeue strategy
			if !tt.expectError {
				assert.Equal(t, tt.expectedStrategy.Immediate, strategy.Immediate, "Immediate mismatch")
				if tt.expectedStrategy.After > 0 {
					assert.Equal(t, tt.expectedStrategy.After, strategy.After, "After mismatch")
				}
			}

			// Check status updates
			if tt.checkStatus != nil && !tt.expectError {
				tt.checkStatus(t, tt.newStatus)
			}
		})
	}
}

func TestCommonControl_EnsureClaimCompleted(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)

	now := metav1.Now()
	pastTime := metav1.NewTime(now.Add(-10 * time.Second))
	futureTime := metav1.NewTime(now.Add(-1 * time.Second))

	tests := []struct {
		name               string
		claim              *agentsv1alpha1.SandboxClaim
		newStatus          *agentsv1alpha1.SandboxClaimStatus
		expectedStrategy   RequeueStrategy
		expectError        bool
		expectDeleted      bool
		expectedRequeueMin time.Duration // minimum expected requeue time (for TTL not yet expired)
	}{
		{
			name: "no TTL configured - should not requeue",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					// No TTLAfterCompleted
				},
			},
			newStatus: &agentsv1alpha1.SandboxClaimStatus{
				Phase:          agentsv1alpha1.SandboxClaimPhaseCompleted,
				CompletionTime: &now,
			},
			expectedStrategy: NoRequeue(),
			expectError:      false,
			expectDeleted:    false,
		},
		{
			name: "TTL configured but no completion time - should not requeue",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:      "test-template",
					TTLAfterCompleted: &metav1.Duration{Duration: 5 * time.Second},
				},
			},
			newStatus: &agentsv1alpha1.SandboxClaimStatus{
				Phase: agentsv1alpha1.SandboxClaimPhaseCompleted,
				// No CompletionTime
			},
			expectedStrategy: NoRequeue(),
			expectError:      false,
			expectDeleted:    false,
		},
		{
			name: "TTL expired - should delete claim",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:      "test-template",
					TTLAfterCompleted: &metav1.Duration{Duration: 5 * time.Second},
				},
			},
			newStatus: &agentsv1alpha1.SandboxClaimStatus{
				Phase:          agentsv1alpha1.SandboxClaimPhaseCompleted,
				CompletionTime: &pastTime, // 10 seconds ago, TTL is 5 seconds
			},
			expectedStrategy: NoRequeue(),
			expectError:      false,
			expectDeleted:    true,
		},
		{
			name: "TTL not yet expired - should requeue after remaining time",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:      "test-template",
					TTLAfterCompleted: &metav1.Duration{Duration: 10 * time.Second},
				},
			},
			newStatus: &agentsv1alpha1.SandboxClaimStatus{
				Phase:          agentsv1alpha1.SandboxClaimPhaseCompleted,
				CompletionTime: &futureTime, // 1 second ago, TTL is 10 seconds, should requeue after ~9s
			},
			expectedStrategy:   RequeueAfter(9 * time.Second), // placeholder, will check range
			expectError:        false,
			expectDeleted:      false,
			expectedRequeueMin: 8 * time.Second, // allow some tolerance
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake client with the claim
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.claim).
				WithStatusSubresource(&agentsv1alpha1.SandboxClaim{}).
				Build()

			fakeRecorder := record.NewFakeRecorder(10)

			control := NewCommonControl(fakeClient, fakeRecorder, nil, nil)

			args := ClaimArgs{
				Claim:     tt.claim,
				NewStatus: tt.newStatus,
			}

			ctx := context.Background()
			strategy, err := control.EnsureClaimCompleted(ctx, args)

			// Check error
			if tt.expectError {
				assert.Error(t, err, "Expected error but got nil")
			} else {
				assert.NoError(t, err, "Unexpected error")
			}

			// Check if claim was deleted
			if tt.expectDeleted {
				deletedClaim := &agentsv1alpha1.SandboxClaim{}
				err := fakeClient.Get(ctx, client.ObjectKeyFromObject(tt.claim), deletedClaim)
				assert.Error(t, err, "Expected claim to be deleted, but it still exists")
				// For TTL deletion, strategy should be NoRequeue
				assert.False(t, strategy.Immediate, "Expected NoRequeue after deletion")
				assert.Equal(t, time.Duration(0), strategy.After, "Expected NoRequeue after deletion")
			} else {
				// Claim should still exist
				existingClaim := &agentsv1alpha1.SandboxClaim{}
				err := fakeClient.Get(ctx, client.ObjectKeyFromObject(tt.claim), existingClaim)
				assert.NoError(t, err, "Expected claim to still exist")
			}

			// Check requeue strategy for "TTL not yet expired" case
			if tt.name == "TTL not yet expired - should requeue after remaining time" {
				assert.False(t, strategy.Immediate, "Expected delayed requeue, got immediate requeue")
				assert.GreaterOrEqual(t, strategy.After, tt.expectedRequeueMin, "Requeue After too short")
				assert.LessOrEqual(t, strategy.After, 10*time.Second, "Requeue After too long")
			}
		})
	}
}

func TestCommonControl_buildClaimOptions(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	fakeRecorder := record.NewFakeRecorder(10)
	control := NewCommonControl(fakeClient, fakeRecorder, nil, nil).(*commonControl)

	ctx := context.Background()
	shutdownTime := metav1.Now()
	timeoutDuration := metav1.Duration{Duration: 3 * time.Minute}

	tests := []struct {
		name        string
		claim       *agentsv1alpha1.SandboxClaim
		sandboxSet  *agentsv1alpha1.SandboxSet
		expectError bool
		validate    func(t *testing.T, opts infra.ClaimSandboxOptions)
	}{
		{
			name: "basic claim without optional fields",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim",
					Namespace: "default",
					UID:       "test-uid-123",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError: false,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				assert.Equal(t, "test-uid-123", opts.User, "User mismatch")
				assert.Equal(t, "test-template", opts.Template, "Template mismatch")
				require.NotNil(t, opts.Modifier, "Modifier should not be nil")
				assert.Nil(t, opts.InplaceUpdate, "InplaceUpdate should be nil when not specified")

				// Test modifier by applying it to a mock sandbox
				mockSandbox := &sandboxcr.Sandbox{
					Sandbox: &agentsv1alpha1.Sandbox{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "test-sandbox",
							Namespace: "default",
							Labels: map[string]string{
								"existing-label": "existing-value",
							},
						},
					},
				}
				opts.Modifier(mockSandbox)

				// Verify modifier set the claim name label correctly
				assert.Equal(t, "test-claim", mockSandbox.Labels[agentsv1alpha1.LabelSandboxClaimName], "LabelSandboxClaimName mismatch")
				assert.Equal(t, "existing-value", mockSandbox.Labels["existing-label"], "existing-label should be preserved")
			},
		},
		{
			name: "claim with labels and annotations",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim",
					Namespace: "default",
					UID:       "test-uid-456",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					Labels: map[string]string{
						"env":  "test",
						"team": "platform",
					},
					Annotations: map[string]string{
						"description": "test annotation",
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError: false,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				assert.Equal(t, "test-uid-456", opts.User, "User mismatch")
				require.NotNil(t, opts.Modifier, "Modifier should not be nil")

				// Test modifier by applying it to a mock sandbox
				mockSandbox := &sandboxcr.Sandbox{
					Sandbox: &agentsv1alpha1.Sandbox{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "test-sandbox",
							Namespace: "default",
							Labels: map[string]string{
								"existing-label": "existing-value",
							},
						},
					},
				}
				opts.Modifier(mockSandbox)

				// Verify modifier set labels and annotations correctly
				assert.Equal(t, "test-claim", mockSandbox.Labels[agentsv1alpha1.LabelSandboxClaimName], "LabelSandboxClaimName mismatch")
				assert.Equal(t, "test", mockSandbox.Labels["env"], "env label mismatch")
				assert.Equal(t, "platform", mockSandbox.Labels["team"], "team label mismatch")
				assert.Equal(t, "existing-value", mockSandbox.Labels["existing-label"], "existing-label should be preserved")
				assert.Equal(t, "test annotation", mockSandbox.Annotations["description"], "description annotation mismatch")
			},
		},
		{
			name: "claim with shutdownTime",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim",
					Namespace: "default",
					UID:       "test-uid-789",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					ShutdownTime: &shutdownTime,
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError: false,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				assert.Equal(t, "test-uid-789", opts.User, "User mismatch")
				require.NotNil(t, opts.Modifier, "Modifier should not be nil")

				// Test modifier by applying it to a mock sandbox
				mockSandbox := &sandboxcr.Sandbox{
					Sandbox: &agentsv1alpha1.Sandbox{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "test-sandbox",
							Namespace: "default",
						},
					},
				}
				opts.Modifier(mockSandbox)

				// Verify modifier set the claim name label and shutdown annotation
				assert.Equal(t, "test-claim", mockSandbox.Labels[agentsv1alpha1.LabelSandboxClaimName], "LabelSandboxClaimName mismatch")
				assert.Equal(t, shutdownTime.Time.Format(time.RFC3339), mockSandbox.Spec.ShutdownTime.Time.Format(time.RFC3339), "ShutdownTime annotation mismatch")
			},
		},
		{
			name: "claim with inplaceUpdate - image only",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim",
					Namespace: "default",
					UID:       "test-uid-update",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					InplaceUpdate: &agentsv1alpha1.SandboxClaimInplaceUpdateOptions{
						Image: "nginx:latest",
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError: false,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				require.NotNil(t, opts.InplaceUpdate, "InplaceUpdate should not be nil")
				assert.Equal(t, "nginx:latest", opts.InplaceUpdate.Image, "InplaceUpdate.Image mismatch")
				assert.NotZero(t, opts.WaitReadyTimeout, "WaitReadyTimeout should be set to default, got 0")
			},
		},
		{
			name: "claim with inplaceUpdate - image and timeout",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim",
					Namespace: "default",
					UID:       "test-uid-update-timeout",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					InplaceUpdate: &agentsv1alpha1.SandboxClaimInplaceUpdateOptions{
						Image: "redis:7.0",
					},
					WaitReadyTimeout: &timeoutDuration,
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError: false,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				require.NotNil(t, opts.InplaceUpdate, "InplaceUpdate should not be nil")
				assert.Equal(t, "redis:7.0", opts.InplaceUpdate.Image, "InplaceUpdate.Image mismatch")
				assert.Equal(t, 3*time.Minute, opts.WaitReadyTimeout, "WaitReadyTimeout mismatch")
			},
		},
		{
			name: "claim with runtimes",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-runtimes",
					Namespace: "default",
					UID:       "test-uid-runtimes",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount},
						{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime},
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError: false,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				assert.Equal(t, "test-uid-runtimes", opts.User, "User mismatch")
				assert.Len(t, opts.RuntimeConfig, 2, "RuntimeConfig length mismatch")
				assert.Equal(t, agentsv1alpha1.RuntimeConfigForInjectCsiMount, opts.RuntimeConfig[0].Name, "RuntimeConfig[0].Name mismatch")
				assert.Equal(t, agentsv1alpha1.RuntimeConfigForInjectAgentRuntime, opts.RuntimeConfig[1].Name, "RuntimeConfig[1].Name mismatch")
			},
		},
		{
			name: "claim with single runtime",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-single-runtime",
					Namespace: "default",
					UID:       "test-uid-single-runtime",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount},
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError: false,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				assert.Len(t, opts.RuntimeConfig, 1, "RuntimeConfig length mismatch")
				assert.Equal(t, agentsv1alpha1.RuntimeConfigForInjectCsiMount, opts.RuntimeConfig[0].Name, "RuntimeConfig[0].Name mismatch")
			},
		},
		{
			name: "claim without runtimes should have nil RuntimeConfig",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-no-runtime",
					Namespace: "default",
					UID:       "test-uid-no-runtime",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError: false,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				assert.Nil(t, opts.RuntimeConfig, "RuntimeConfig should be nil when Runtimes is not specified")
			},
		},
		{
			name: "SkipInitRuntime=false (default) should set InitRuntime with EnvVars and AccessToken",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-skip-false",
					Namespace: "default",
					UID:       "test-uid-skip-false",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					EnvVars: map[string]string{
						"KEY1": "value1",
						"KEY2": "value2",
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError: false,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				require.NotNil(t, opts.InitRuntime, "InitRuntime should not be nil when SkipInitRuntime is false")
				assert.Equal(t, "value1", opts.InitRuntime.EnvVars["KEY1"], "InitRuntime.EnvVars[KEY1] mismatch")
				assert.Equal(t, "value2", opts.InitRuntime.EnvVars["KEY2"], "InitRuntime.EnvVars[KEY2] mismatch")
				assert.NotEmpty(t, opts.InitRuntime.AccessToken, "InitRuntime.AccessToken should not be empty")
			},
		},
		{
			name: "SkipInitRuntime=true should not set InitRuntime",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-skip-true",
					Namespace: "default",
					UID:       "test-uid-skip-true",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    "test-template",
					SkipInitRuntime: true,
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError: false,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				assert.Nil(t, opts.InitRuntime, "InitRuntime should be nil when SkipInitRuntime is true")
			},
		},
		{
			name: "SkipInitRuntime=true with EnvVars should still not set InitRuntime",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-skip-true-env",
					Namespace: "default",
					UID:       "test-uid-skip-true-env",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    "test-template",
					SkipInitRuntime: true,
					EnvVars: map[string]string{
						"KEY1": "value1",
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError: false,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				assert.Nil(t, opts.InitRuntime, "InitRuntime should be nil when SkipInitRuntime is true, even with EnvVars")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := control.buildClaimOptions(ctx, tt.claim, tt.sandboxSet)
			if tt.expectError {
				assert.Error(t, err, "Expected error but got nil")
			} else {
				assert.NoError(t, err, "Unexpected error")
				if tt.validate != nil {
					tt.validate(t, opts)
				}
			}
		})
	}
}

// Helper function for tests
func int32Ptr(i int32) *int32 {
	return &i
}

func CreateSandboxWithStatus(t *testing.T, client versioned.Interface, sbx *agentsv1alpha1.Sandbox) {
	_, err := client.ApiV1alpha1().Sandboxes(sbx.Namespace).Create(t.Context(), sbx, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = client.ApiV1alpha1().Sandboxes(sbx.Namespace).UpdateStatus(t.Context(), sbx, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func TestBuildClaimOptions_CSIMount_ConfigValidation(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	// Create test PersistentVolumes for integration tests
	testPVs := []client.Object{
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-pv-nas",
			},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						Driver:       "nasplugin.csi.alibabacloud.com",
						VolumeHandle: "test-pv-nas",
						VolumeAttributes: map[string]string{
							"path": "/shares/data",
						},
					},
				},
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteMany,
				},
			},
		},
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-pv-oss",
			},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						Driver:       "ossplugin.csi.alibabacloud.com",
						VolumeHandle: "test-pv-oss",
						VolumeAttributes: map[string]string{
							"bucket": "my-test-bucket",
						},
					},
				},
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadOnlyMany,
				},
			},
		},
	}

	// Use controller-runtime fake client for controller operations
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(testPVs...).
		Build()

	fakeRecorder := record.NewFakeRecorder(10)

	// Convert client.Object to runtime.Object for kubernetes clientset
	runtimeObjects := make([]runtime.Object, len(testPVs))
	for i, obj := range testPVs {
		runtimeObjects[i] = obj.(runtime.Object)
	}

	// Use kubernetes fake clientset for K8sClient
	k8sClientset := k8sfake.NewSimpleClientset(runtimeObjects...)

	// Create sandbox client with both K8s and Sandbox clients
	sandboxClient := &clients.ClientSet{
		K8sClient:     k8sClientset,
		SandboxClient: sandboxfake.NewSimpleClientset(),
	}

	// Create a minimal cache with PV support
	cache, err := sandboxcr.NewCache(sandboxClient, config.SandboxManagerOptions{})
	require.NoError(t, err, "Failed to create cache")

	// Start cache
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = cache.Run(ctx)
	}()
	time.Sleep(300 * time.Millisecond) // Wait for cache to sync

	// Create storage registry and manually register supported drivers
	storageRegistry := storages.NewStorageProvider()
	storageRegistry.RegisterProvider("nasplugin.csi.alibabacloud.com", &storages.MountProvider{})
	storageRegistry.RegisterProvider("ossplugin.csi.alibabacloud.com", &storages.MountProvider{})

	control := NewCommonControl(fakeClient, fakeRecorder, sandboxClient, cache)
	// Inject the storage registry into the control
	commonControl := control.(*commonControl)
	commonControl.storageRegistry = storageRegistry

	tests := []struct {
		name               string
		claim              *agentsv1alpha1.SandboxClaim
		sandboxSet         *agentsv1alpha1.SandboxSet
		expectError        bool
		errorContains      string
		expectedMountCount int
		expectedDriver     string
		validate           func(t *testing.T, opts infra.ClaimSandboxOptions)
	}{
		{
			name: "claim without CSI mount configs",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-no-csi",
					Namespace: "default",
					UID:       "test-uid-no-csi",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					EnvVars: map[string]string{
						"ENV1": "value1",
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError: false,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				// CSIMount should be nil when no configs specified
				assert.Nil(t, opts.CSIMount, "CSIMount should be nil when no configs specified")
				// InitRuntime should be set by default (SkipInitRuntime defaults to false)
				assert.NotNil(t, opts.InitRuntime, "InitRuntime should be set by default when SkipInitRuntime is false")
			},
		},
		{
			name: "claim with empty CSI mount configs slice",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-empty-csi",
					Namespace: "default",
					UID:       "test-uid-empty-csi",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:        "test-template",
					DynamicVolumesMount: []agentsv1alpha1.CSIMountConfig{},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError: false,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				// CSIMount should be nil when configs slice is empty
				assert.Nil(t, opts.CSIMount, "CSIMount should be nil when configs slice is empty")
			},
		},
		{
			name: "claim with nil DynamicVolumesMount",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-nil-csi",
					Namespace: "default",
					UID:       "test-uid-nil-csi",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:        "test-template",
					DynamicVolumesMount: nil,
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError: false,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				// CSIMount should be nil when DynamicVolumesMount is nil
				assert.Nil(t, opts.CSIMount, "CSIMount should be nil when DynamicVolumesMount is nil")
			},
		},
		{
			name: "claim with InplaceUpdate but no CSI mount",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-inplace-no-csi",
					Namespace: "default",
					UID:       "test-uid-inplace-no-csi",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					InplaceUpdate: &agentsv1alpha1.SandboxClaimInplaceUpdateOptions{
						Image: "nginx:latest",
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError: false,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				// InplaceUpdate should be set
				assert.NotNil(t, opts.InplaceUpdate, "InplaceUpdate should not be nil")
				if opts.InplaceUpdate != nil {
					assert.Equal(t, "nginx:latest", opts.InplaceUpdate.Image, "InplaceUpdate.Image mismatch")
				}
				// CSIMount should still be nil
				assert.Nil(t, opts.CSIMount, "CSIMount should be nil when no CSI configs specified")
			},
		},
		{
			name: "claim with ShutdownTime but no CSI mount",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-shutdown-no-csi",
					Namespace: "default",
					UID:       "test-uid-shutdown-no-csi",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					ShutdownTime: &metav1.Time{Time: time.Now()},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError: false,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				// CSIMount should be nil
				assert.Nil(t, opts.CSIMount, "CSIMount should be nil when no CSI configs specified")
			},
		},
		{
			name: "claim with all optional fields except CSI mount",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-all-no-csi",
					Namespace: "default",
					UID:       "test-uid-all-no-csi",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					Labels: map[string]string{
						"app": "test",
					},
					Annotations: map[string]string{
						"description": "test",
					},
					EnvVars: map[string]string{
						"KEY1": "val1",
					},
					InplaceUpdate: &agentsv1alpha1.SandboxClaimInplaceUpdateOptions{
						Image: "redis:7.0",
					},
					WaitReadyTimeout: &metav1.Duration{Duration: 5 * time.Minute},
					ShutdownTime:     &metav1.Time{Time: time.Now()},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError: false,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				// All other fields should be processed normally
				assert.NotNil(t, opts.InplaceUpdate, "InplaceUpdate should not be nil")
				assert.Equal(t, 5*time.Minute, opts.WaitReadyTimeout, "WaitReadyTimeout mismatch")
				// CSIMount should still be nil
				assert.Nil(t, opts.CSIMount, "CSIMount should be nil when no CSI configs specified")
			},
		},
		{
			name: "single CSI mount config structure validation",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-csi-single",
					Namespace: "default",
					UID:       "test-uid-csi-single",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					DynamicVolumesMount: []agentsv1alpha1.CSIMountConfig{
						{
							PvName:    "test-pv-nas",
							MountPath: "/data",
							SubPath:   "subdir",
							ReadOnly:  true,
						},
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError:        false,
			expectedMountCount: 1,
			expectedDriver:     "nasplugin.csi.alibabacloud.com",
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				require.NotNil(t, opts.CSIMount, "CSIMount should not be nil")
				assert.Len(t, opts.CSIMount.MountOptionList, 1, "Expected 1 mount config")
			},
		},
		{
			name: "multiple CSI mount configs structure validation",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-csi-multi",
					Namespace: "default",
					UID:       "test-uid-csi-multi",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					DynamicVolumesMount: []agentsv1alpha1.CSIMountConfig{
						{
							PvName:    "test-pv-nas",
							MountPath: "/workspace",
						},
						{
							PvName:    "test-pv-oss",
							MountPath: "/models",
							ReadOnly:  true,
						},
						{
							PvName:    "test-pv-nas",
							MountPath: "/storage",
							SubPath:   "data",
						},
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError:        false,
			expectedMountCount: 3,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				require.NotNil(t, opts.CSIMount, "CSIMount should not be nil")
				assert.Len(t, opts.CSIMount.MountOptionList, 3, "Expected 3 mount configs")
			},
		},
		{
			name: "CSI mount with env vars structure validation",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-csi-env",
					Namespace: "default",
					UID:       "test-uid-csi-env",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					EnvVars: map[string]string{
						"ENV1": "value1",
						"ENV2": "value2",
						"ENV3": "value3",
					},
					DynamicVolumesMount: []agentsv1alpha1.CSIMountConfig{
						{
							PvName:    "test-pv-nas",
							MountPath: "/data",
						},
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError:        false,
			expectedMountCount: 1,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				assert.NotNil(t, opts.InitRuntime, "InitRuntime should not be nil")
				if opts.InitRuntime != nil {
					assert.Len(t, opts.InitRuntime.EnvVars, 3, "Expected 3 env vars")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := commonControl.buildClaimOptions(ctx, tt.claim, tt.sandboxSet)

			// Check error expectations
			if tt.expectError {
				require.Error(t, err, "Expected error but got nil")
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains, "Error message mismatch")
				}
				return
			}

			// For non-error cases, verify no error occurred
			assert.NoError(t, err, "Unexpected error")

			// Verify mount count
			if tt.expectedMountCount > 0 {
				require.NotNil(t, opts.CSIMount, "CSIMount should not be nil")
				assert.Len(t, opts.CSIMount.MountOptionList, tt.expectedMountCount, "Mount config count mismatch")

				// Verify driver if specified
				if tt.expectedDriver != "" && len(opts.CSIMount.MountOptionList) > 0 {
					assert.Equal(t, tt.expectedDriver, opts.CSIMount.MountOptionList[0].Driver, "Driver mismatch")
				}
			}

			// Run custom validation if provided
			if tt.validate != nil {
				tt.validate(t, opts)
			}
		})
	}
}

func TestBuildClaimOptions_CSIMount_Test(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	// Create test PersistentVolumes
	testPVs := []client.Object{
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-pv-nas",
			},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						Driver:       "nasplugin.csi.alibabacloud.com",
						VolumeHandle: "test-pv-nas",
						VolumeAttributes: map[string]string{
							"path": "/shares/data",
						},
					},
				},
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteMany,
				},
			},
		},
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-pv-oss",
			},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						Driver:       "ossplugin.csi.alibabacloud.com",
						VolumeHandle: "test-pv-oss",
						VolumeAttributes: map[string]string{
							"bucket": "my-test-bucket",
						},
					},
				},
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadOnlyMany,
				},
			},
		},
	}

	// Use controller-runtime fake client for controller operations
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(testPVs...).
		Build()

	fakeRecorder := record.NewFakeRecorder(10)

	// Convert client.Object to runtime.Object for kubernetes clientset
	runtimeObjects := make([]runtime.Object, len(testPVs))
	for i, obj := range testPVs {
		runtimeObjects[i] = obj.(runtime.Object)
	}

	// Use kubernetes fake clientset for K8sClient
	k8sClientset := k8sfake.NewSimpleClientset(runtimeObjects...)

	// Create sandbox client with both K8s and Sandbox clients
	sandboxClient := &clients.ClientSet{
		K8sClient:     k8sClientset,
		SandboxClient: sandboxfake.NewSimpleClientset(),
	}

	// Create a minimal cache with PV support
	cache, err := sandboxcr.NewCache(sandboxClient, config.SandboxManagerOptions{})
	require.NoError(t, err, "Failed to create cache")

	// Start cache
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = cache.Run(ctx)
	}()
	time.Sleep(300 * time.Millisecond) // Wait for cache to sync

	// Create storage registry and manually register supported drivers
	storageRegistry := storages.NewStorageProvider()
	storageRegistry.RegisterProvider("nasplugin.csi.alibabacloud.com", &storages.MountProvider{})
	storageRegistry.RegisterProvider("ossplugin.csi.alibabacloud.com", &storages.MountProvider{})

	control := NewCommonControl(fakeClient, fakeRecorder, sandboxClient, cache)
	// Inject the storage registry into the control
	commonControl := control.(*commonControl)
	commonControl.storageRegistry = storageRegistry

	tests := []struct {
		name               string
		claim              *agentsv1alpha1.SandboxClaim
		sandboxSet         *agentsv1alpha1.SandboxSet
		expectError        bool
		errorContains      string
		expectedMountCount int
		expectedDriver     string
		validate           func(t *testing.T, opts infra.ClaimSandboxOptions)
	}{
		{
			name: "single CSI mount config structure validation",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-csi-single",
					Namespace: "default",
					UID:       "test-uid-csi-single",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					DynamicVolumesMount: []agentsv1alpha1.CSIMountConfig{
						{
							PvName:    "test-pv-nas",
							MountPath: "/data",
							SubPath:   "subdir",
							ReadOnly:  true,
						},
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError:        false,
			expectedMountCount: 1,
			expectedDriver:     "nasplugin.csi.alibabacloud.com",
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				require.NotNil(t, opts.CSIMount, "CSIMount should not be nil")
				assert.Len(t, opts.CSIMount.MountOptionList, 1, "Expected 1 mount config")
			},
		},
		{
			name: "multiple CSI mount configs structure validation",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-csi-multi",
					Namespace: "default",
					UID:       "test-uid-csi-multi",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					DynamicVolumesMount: []agentsv1alpha1.CSIMountConfig{
						{
							PvName:    "test-pv-nas",
							MountPath: "/workspace",
						},
						{
							PvName:    "test-pv-oss",
							MountPath: "/models",
							ReadOnly:  true,
						},
						{
							PvName:    "test-pv-nas",
							MountPath: "/storage",
							SubPath:   "data",
						},
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError:        false,
			expectedMountCount: 3,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				require.NotNil(t, opts.CSIMount, "CSIMount should not be nil")
				assert.Len(t, opts.CSIMount.MountOptionList, 3, "Expected 3 mount configs")
			},
		},
		{
			name: "CSI mount with env vars structure validation",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-csi-env",
					Namespace: "default",
					UID:       "test-uid-csi-env",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					EnvVars: map[string]string{
						"ENV1": "value1",
						"ENV2": "value2",
						"ENV3": "value3",
					},
					DynamicVolumesMount: []agentsv1alpha1.CSIMountConfig{
						{
							PvName:    "test-pv-nas",
							MountPath: "/data",
						},
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError:        false,
			expectedMountCount: 1,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				assert.NotNil(t, opts.InitRuntime, "InitRuntime should not be nil")
				if opts.InitRuntime != nil {
					assert.Len(t, opts.InitRuntime.EnvVars, 3, "Expected 3 env vars")
				}
			},
		},
		{
			name: "CSI mount with ReadOnly flag",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-csi-readonly",
					Namespace: "default",
					UID:       "test-uid-readonly",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					EnvVars: map[string]string{
						"TEST_ENV": "test_value",
					},
					DynamicVolumesMount: []agentsv1alpha1.CSIMountConfig{
						{
							PvName:    "test-pv-nas",
							MountPath: "/data",
							ReadOnly:  true,
						},
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError:        false,
			expectedMountCount: 1,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				require.NotNil(t, opts.CSIMount, "CSIMount should not be nil")
				assert.Len(t, opts.CSIMount.MountOptionList, 1, "Expected 1 mount config")
				assert.NotNil(t, opts.InitRuntime, "InitRuntime should be auto-created when CSI mount is specified")
				if opts.InitRuntime != nil {
					assert.NotEmpty(t, opts.InitRuntime.AccessToken, "AccessToken should be generated")
				}
			},
		},
		{
			name: "CSI mount with special characters in paths",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-csi-special-chars",
					Namespace: "default",
					UID:       "test-uid-special-chars",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					EnvVars: map[string]string{
						"TEST_ENV": "test_value",
					},
					DynamicVolumesMount: []agentsv1alpha1.CSIMountConfig{
						{
							PvName:    "test-pv-nas",
							MountPath: "/data/my-folder_2024",
							SubPath:   "sub.dir-test",
						},
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError:        false,
			expectedMountCount: 1,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				require.NotNil(t, opts.CSIMount, "CSIMount should not be nil")
				assert.NotNil(t, opts.InitRuntime, "InitRuntime should be auto-created")
			},
		},
		{
			name: "CSI mount preserves InitRuntime when already set",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-csi-preserve-init",
					Namespace: "default",
					UID:       "test-uid-preserve-init",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					EnvVars: map[string]string{
						"ORIGINAL_VAR": "original_value",
					},
					DynamicVolumesMount: []agentsv1alpha1.CSIMountConfig{
						{
							PvName:    "test-pv-nas",
							MountPath: "/data",
						},
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError:        false,
			expectedMountCount: 1,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				assert.NotNil(t, opts.InitRuntime, "InitRuntime should not be nil")
				if opts.InitRuntime != nil {
					// Verify original env vars are preserved
					assert.Equal(t, "original_value", opts.InitRuntime.EnvVars["ORIGINAL_VAR"], "ORIGINAL_VAR should be preserved")
					// AccessToken should be generated
					assert.NotEmpty(t, opts.InitRuntime.AccessToken, "AccessToken should be generated")
				}
			},
		},
		{
			name: "CSI mount JSON marshal verification",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-csi-json",
					Namespace: "default",
					UID:       "test-uid-json",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					EnvVars: map[string]string{
						"TEST_ENV": "test_value",
					},
					DynamicVolumesMount: []agentsv1alpha1.CSIMountConfig{
						{
							PvName:    "test-pv-nas",
							MountPath: "/data1",
							ReadOnly:  false,
						},
						{
							PvName:    "test-pv-nas",
							MountPath: "/data2",
							ReadOnly:  true,
							SubPath:   "subdir",
						},
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError:        false,
			expectedMountCount: 2,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				require.NotNil(t, opts.CSIMount, "CSIMount should not be nil")
				assert.NotEmpty(t, opts.CSIMount.MountOptionListRaw, "MountOptionListRaw should not be empty")
				// Verify it's valid JSON
				var decoded []agentsv1alpha1.CSIMountConfig
				assert.NoError(t, json.Unmarshal([]byte(opts.CSIMount.MountOptionListRaw), &decoded), "MountOptionListRaw should be valid JSON")
				assert.Len(t, decoded, 2, "Expected 2 decoded configs")
				// Verify first config
				assert.Equal(t, "test-pv-nas", decoded[0].PvName, "First PvName mismatch")
				assert.False(t, decoded[0].ReadOnly, "First ReadOnly should be false")
				// Verify second config
				assert.Equal(t, "test-pv-nas", decoded[1].PvName, "Second PvName mismatch")
				assert.True(t, decoded[1].ReadOnly, "Second ReadOnly should be true")
				assert.Equal(t, "subdir", decoded[1].SubPath, "Second SubPath mismatch")
			},
		},
		{
			name: "CSI mount with SubPath traversal attempt should fail",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-csi-traversal",
					Namespace: "default",
					UID:       "test-uid-traversal",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					DynamicVolumesMount: []agentsv1alpha1.CSIMountConfig{
						{
							PvName:    "test-pv-nas",
							MountPath: "/workspace",
							SubPath:   "../../../etc/passwd",
						},
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError:   true,
			errorContains: "sub path must not traverse to parent directory",
		},
		{
			name: "CSI mount with single NAS volume - integration",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-csi-nas",
					Namespace: "default",
					UID:       "test-uid-nas",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					EnvVars: map[string]string{
						"TEST_ENV": "test_value",
					},
					DynamicVolumesMount: []agentsv1alpha1.CSIMountConfig{
						{
							PvName:    "test-pv-nas",
							MountPath: "/workspace",
						},
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError:        false,
			expectedMountCount: 1,
			expectedDriver:     "nasplugin.csi.alibabacloud.com",
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				assert.NotNil(t, opts.InitRuntime, "InitRuntime should be auto-created when CSI mount is specified")
				if opts.InitRuntime != nil {
					assert.NotEmpty(t, opts.InitRuntime.AccessToken, "AccessToken should be generated")
					assert.NotNil(t, opts.InitRuntime.EnvVars, "EnvVars should not be nil")
					assert.NotEmpty(t, opts.InitRuntime.EnvVars, "EnvVars should not be empty")
				}
				require.NotNil(t, opts.CSIMount, "CSIMount should not be nil")
				assert.Len(t, opts.CSIMount.MountOptionList, 1, "Expected 1 mount config")
				assert.NotEmpty(t, opts.CSIMount.MountOptionListRaw, "MountOptionListRaw should not be empty")
			},
		},
		{
			name: "CSI mount with multiple volumes - integration",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-csi-multi",
					Namespace: "default",
					UID:       "test-uid-multi",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					DynamicVolumesMount: []agentsv1alpha1.CSIMountConfig{
						{
							PvName:    "test-pv-nas",
							MountPath: "/workspace",
						},
						{
							PvName:    "test-pv-oss",
							MountPath: "/data",
							ReadOnly:  true,
						},
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError:        false,
			expectedMountCount: 2,
			validate: func(t *testing.T, opts infra.ClaimSandboxOptions) {
				require.NotNil(t, opts.CSIMount, "CSIMount should not be nil")
				assert.Len(t, opts.CSIMount.MountOptionList, 2, "Expected 2 mount configs")
				// Verify all drivers are present
				drivers := make(map[string]bool)
				for _, c := range opts.CSIMount.MountOptionList {
					drivers[c.Driver] = true
				}
				assert.True(t, drivers["nasplugin.csi.alibabacloud.com"], "Expected nasplugin.csi.alibabacloud.com driver to be present")
				assert.True(t, drivers["ossplugin.csi.alibabacloud.com"], "Expected ossplugin.csi.alibabacloud.com driver to be present")
			},
		},
		{
			name: "CSI mount with non-existent PV should fail - integration",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-csi-notfound",
					Namespace: "default",
					UID:       "test-uid-notfound",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-template",
					DynamicVolumesMount: []agentsv1alpha1.CSIMountConfig{
						{
							PvName:    "non-existent-pv",
							MountPath: "/data",
						},
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError:   true,
			errorContains: "failed to generate csi mount options config",
		},
		{
			name: "SkipInitRuntime=true with CSI mount should fail validation",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-claim-skip-csi",
					Namespace: "default",
					UID:       "test-uid-skip-csi",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:    "test-template",
					SkipInitRuntime: true,
					DynamicVolumesMount: []agentsv1alpha1.CSIMountConfig{
						{
							PvName:    "test-pv-nas",
							MountPath: "/data",
						},
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
			},
			expectError:   true,
			errorContains: "init runtime is required when csi mount is specified",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := commonControl.buildClaimOptions(ctx, tt.claim, tt.sandboxSet)

			// Check error expectations
			if tt.expectError {
				require.Error(t, err, "Expected error but got nil")
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains, "Error message mismatch")
				}
				return
			}

			// For non-error cases, verify no error occurred
			assert.NoError(t, err, "Unexpected error")

			// Verify mount count
			if tt.expectedMountCount > 0 {
				require.NotNil(t, opts.CSIMount, "CSIMount should not be nil")
				assert.Len(t, opts.CSIMount.MountOptionList, tt.expectedMountCount, "Mount config count mismatch")

				// Verify driver if specified
				if tt.expectedDriver != "" && len(opts.CSIMount.MountOptionList) > 0 {
					assert.Equal(t, tt.expectedDriver, opts.CSIMount.MountOptionList[0].Driver, "Driver mismatch")
				}
			}

			// Run custom validation if provided
			if tt.validate != nil {
				tt.validate(t, opts)
			}
		})
	}
}
