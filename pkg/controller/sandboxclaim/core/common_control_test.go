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
	"testing"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	clientsetfake "github.com/openkruise/agents/client/clientset/versioned/fake"
	informers "github.com/openkruise/agents/client/informers/externalversions"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

	if control == nil {
		t.Error("NewCommonControl() returned nil")
	}

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
			if got.Immediate != tt.expectedStrategy.Immediate {
				t.Errorf("Immediate = %v, want %v", got.Immediate, tt.expectedStrategy.Immediate)
			}
			if got.After != tt.expectedStrategy.After {
				t.Errorf("After = %v, want %v", got.After, tt.expectedStrategy.After)
			}
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
				if got.Immediate != false {
					t.Errorf("RequeueAfter(%v).Immediate = true, want false", d)
				}
				if got.After != d {
					t.Errorf("RequeueAfter(%v).After = %v, want %v", d, got.After, d)
				}
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

	if controls == nil {
		t.Fatal("NewClaimControl() returned nil")
	}

	// Verify the map contains expected control
	commonControl, exists := controls[CommonControlName]
	if !exists {
		t.Errorf("NewClaimControl() missing CommonControlName key")
	}
	if commonControl == nil {
		t.Errorf("NewClaimControl() CommonControl is nil")
	}

	// Verify it implements the interface
	var _ ClaimControl = commonControl
}

func TestCommonControl_EnsureClaimClaiming(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)

	// Initialize cache and sandboxClient
	sandboxClient := clientsetfake.NewSimpleClientset()
	sandboxInformerFactory := informers.NewSharedInformerFactory(sandboxClient, time.Minute*10)
	sandboxInformer := sandboxInformerFactory.Api().V1alpha1().Sandboxes().Informer()
	sandboxSetInformer := sandboxInformerFactory.Api().V1alpha1().SandboxSets().Informer()

	cache, err := sandboxcr.NewCache(sandboxInformerFactory, sandboxInformer, sandboxSetInformer, nil, nil, nil)
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

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
				if status.ClaimedReplicas != 0 {
					t.Errorf("Expected ClaimedReplicas=0, got %d", status.ClaimedReplicas)
				}
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
					if err != nil {
						t.Fatalf("Failed to create sandbox in sandboxClient: %v", err)
					}
				}
				time.Sleep(100 * time.Millisecond) // Wait for cache sync
				return sandboxes
			},
			expectedStrategy: RequeueImmediately(), // Should requeue to transition to Completed
			expectError:      false,
			checkStatus: func(t *testing.T, status *agentsv1alpha1.SandboxClaimStatus) {
				if status.ClaimedReplicas != 2 {
					t.Errorf("Expected ClaimedReplicas=2, got %d", status.ClaimedReplicas)
				}
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
					if err != nil {
						t.Fatalf("Failed to create sandbox in sandboxClient: %v", err)
					}
				}
				time.Sleep(100 * time.Millisecond) // Wait for cache sync
				return sandboxes
			},
			expectedStrategy: RequeueAfter(ClaimRetryInterval), // Should retry to claim remaining 1
			expectError:      false,
			checkStatus: func(t *testing.T, status *agentsv1alpha1.SandboxClaimStatus) {
				if status.ClaimedReplicas != 2 {
					t.Errorf("Expected ClaimedReplicas to be recovered to 2 (actualCount), got %d", status.ClaimedReplicas)
				}
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

			control := NewCommonControl(fakeClient, fakeRecorder, sandboxClient, cache)

			args := ClaimArgs{
				Claim:      tt.claim,
				SandboxSet: tt.sandboxSet,
				NewStatus:  tt.newStatus,
			}

			strategy, err := control.EnsureClaimClaiming(ctx, args)

			// Check error
			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
			}

			// Check requeue strategy
			if !tt.expectError {
				if strategy.Immediate != tt.expectedStrategy.Immediate {
					t.Errorf("Expected Immediate=%v, got %v", tt.expectedStrategy.Immediate, strategy.Immediate)
				}
				if tt.expectedStrategy.After > 0 {
					if strategy.After != tt.expectedStrategy.After {
						t.Errorf("Expected After=%v, got %v", tt.expectedStrategy.After, strategy.After)
					}
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
				if err == nil {
					t.Errorf("Expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
			}

			// Check if claim was deleted
			if tt.expectDeleted {
				deletedClaim := &agentsv1alpha1.SandboxClaim{}
				err := fakeClient.Get(ctx, client.ObjectKeyFromObject(tt.claim), deletedClaim)
				if err == nil {
					t.Errorf("Expected claim to be deleted, but it still exists")
				}
				// For TTL deletion, strategy should be NoRequeue
				if strategy.Immediate || strategy.After > 0 {
					t.Errorf("Expected NoRequeue after deletion, got %+v", strategy)
				}
			} else {
				// Claim should still exist
				existingClaim := &agentsv1alpha1.SandboxClaim{}
				err := fakeClient.Get(ctx, client.ObjectKeyFromObject(tt.claim), existingClaim)
				if err != nil {
					t.Errorf("Expected claim to still exist, got error: %v", err)
				}
			}

			// Check requeue strategy for "TTL not yet expired" case
			if tt.name == "TTL not yet expired - should requeue after remaining time" {
				if strategy.Immediate {
					t.Errorf("Expected delayed requeue, got immediate requeue")
				}
				if strategy.After < tt.expectedRequeueMin {
					t.Errorf("Expected requeue after at least %v, got %v", tt.expectedRequeueMin, strategy.After)
				}
				if strategy.After > 10*time.Second {
					t.Errorf("Expected requeue within 10s, got %v", strategy.After)
				}
			}
		})
	}
}

// Helper function for tests
func int32Ptr(i int32) *int32 {
	return &i
}
