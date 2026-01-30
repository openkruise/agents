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
	"testing"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGetDesiredReplicas(t *testing.T) {
	tests := []struct {
		name     string
		claim    *agentsv1alpha1.SandboxClaim
		expected int32
	}{
		{
			name: "replicas not set (nil)",
			claim: &agentsv1alpha1.SandboxClaim{
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test",
				},
			},
			expected: DefaultReplicasCount,
		},
		{
			name: "replicas set to 1",
			claim: &agentsv1alpha1.SandboxClaim{
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test",
					Replicas:     int32Ptr(1),
				},
			},
			expected: 1,
		},
		{
			name: "replicas set to 10",
			claim: &agentsv1alpha1.SandboxClaim{
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test",
					Replicas:     int32Ptr(10),
				},
			},
			expected: 10,
		},
		{
			name: "replicas set to 100",
			claim: &agentsv1alpha1.SandboxClaim{
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test",
					Replicas:     int32Ptr(100),
				},
			},
			expected: 100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getDesiredReplicas(tt.claim)
			if got != tt.expected {
				t.Errorf("getDesiredReplicas() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestIsClaimTimeout(t *testing.T) {
	now := metav1.Now()
	pastTime := metav1.NewTime(now.Add(-10 * time.Second))
	futureTime := metav1.NewTime(now.Add(10 * time.Second)) // For clock skew test

	tests := []struct {
		name     string
		claim    *agentsv1alpha1.SandboxClaim
		status   *agentsv1alpha1.SandboxClaimStatus
		expected bool
	}{
		{
			name: "no timeout set",
			claim: &agentsv1alpha1.SandboxClaim{
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test",
				},
			},
			status: &agentsv1alpha1.SandboxClaimStatus{
				ClaimStartTime: &pastTime,
			},
			expected: false,
		},
		{
			name: "no start time",
			claim: &agentsv1alpha1.SandboxClaim{
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test",
					ClaimTimeout: &metav1.Duration{Duration: 5 * time.Second},
				},
			},
			status:   &agentsv1alpha1.SandboxClaimStatus{},
			expected: false,
		},
		{
			name: "not timed out yet",
			claim: &agentsv1alpha1.SandboxClaim{
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test",
					ClaimTimeout: &metav1.Duration{Duration: 20 * time.Second},
				},
			},
			status: &agentsv1alpha1.SandboxClaimStatus{
				ClaimStartTime: &pastTime,
			},
			expected: false,
		},
		{
			name: "timed out",
			claim: &agentsv1alpha1.SandboxClaim{
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test",
					ClaimTimeout: &metav1.Duration{Duration: 5 * time.Second},
				},
			},
			status: &agentsv1alpha1.SandboxClaimStatus{
				ClaimStartTime: &pastTime,
			},
			expected: true,
		},
		{
			name: "clock skew - start time in future",
			claim: &agentsv1alpha1.SandboxClaim{
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test",
					ClaimTimeout: &metav1.Duration{Duration: 5 * time.Second},
				},
			},
			status: &agentsv1alpha1.SandboxClaimStatus{
				ClaimStartTime: &futureTime,
			},
			expected: false, // Should handle clock skew gracefully
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isClaimTimeout(tt.claim, tt.status)
			if got != tt.expected {
				t.Errorf("isClaimTimeout() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestIsReplicasMet(t *testing.T) {
	tests := []struct {
		name     string
		claim    *agentsv1alpha1.SandboxClaim
		status   *agentsv1alpha1.SandboxClaimStatus
		expected bool
	}{
		{
			name: "replicas met - default 1",
			claim: &agentsv1alpha1.SandboxClaim{
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test",
				},
			},
			status: &agentsv1alpha1.SandboxClaimStatus{
				ClaimedReplicas: 1,
			},
			expected: true,
		},
		{
			name: "replicas not met yet",
			claim: &agentsv1alpha1.SandboxClaim{
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test",
					Replicas:     int32Ptr(10),
				},
			},
			status: &agentsv1alpha1.SandboxClaimStatus{
				ClaimedReplicas: 5,
			},
			expected: false,
		},
		{
			name: "replicas exactly met",
			claim: &agentsv1alpha1.SandboxClaim{
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test",
					Replicas:     int32Ptr(10),
				},
			},
			status: &agentsv1alpha1.SandboxClaimStatus{
				ClaimedReplicas: 10,
			},
			expected: true,
		},
		{
			name: "replicas exceeded (should still be true)",
			claim: &agentsv1alpha1.SandboxClaim{
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test",
					Replicas:     int32Ptr(10),
				},
			},
			status: &agentsv1alpha1.SandboxClaimStatus{
				ClaimedReplicas: 15,
			},
			expected: true,
		},
		{
			name: "zero claimed, zero desired (edge case)",
			claim: &agentsv1alpha1.SandboxClaim{
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test",
				},
			},
			status: &agentsv1alpha1.SandboxClaimStatus{
				ClaimedReplicas: 0,
			},
			expected: false, // Default is 1, so 0 < 1
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isReplicasMet(tt.claim, tt.status)
			if got != tt.expected {
				t.Errorf("isReplicasMet() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestCalculateClaimStatus(t *testing.T) {
	now := metav1.Now()
	pastTime := metav1.NewTime(now.Add(-10 * time.Second))

	tests := []struct {
		name               string
		args               ClaimArgs
		expectedPhase      agentsv1alpha1.SandboxClaimPhase
		shouldRequeue      bool
		checkCompletedSet  bool // Whether CompletionTime should be set
		checkStartTimeSet  bool // Whether ClaimStartTime should be set
	}{
		{
			name: "initialize new claim",
			args: ClaimArgs{
				Claim: &agentsv1alpha1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{
						Generation: 1,
					},
					Spec: agentsv1alpha1.SandboxClaimSpec{
						TemplateName: "test",
					},
				},
				SandboxSet: &agentsv1alpha1.SandboxSet{},
				NewStatus:  &agentsv1alpha1.SandboxClaimStatus{},
			},
			expectedPhase:     agentsv1alpha1.SandboxClaimPhaseClaiming,
			shouldRequeue:     false,
			checkStartTimeSet: true, // ClaimStartTime should be set when initializing
		},
		{
			name: "already completed",
			args: ClaimArgs{
				Claim: &agentsv1alpha1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{
						Generation: 1,
					},
					Spec: agentsv1alpha1.SandboxClaimSpec{
						TemplateName: "test",
					},
				},
				SandboxSet: &agentsv1alpha1.SandboxSet{},
				NewStatus: &agentsv1alpha1.SandboxClaimStatus{
					Phase: agentsv1alpha1.SandboxClaimPhaseCompleted,
				},
			},
			expectedPhase: agentsv1alpha1.SandboxClaimPhaseCompleted,
			shouldRequeue: false, // allow EnsureClaimCompleted to run for TTL cleanup
		},
		{
			name: "sandboxset not found",
			args: ClaimArgs{
				Claim: &agentsv1alpha1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{
						Generation: 1,
					},
					Spec: agentsv1alpha1.SandboxClaimSpec{
						TemplateName: "test",
					},
				},
				SandboxSet: nil, // SandboxSet not found
				NewStatus: &agentsv1alpha1.SandboxClaimStatus{
					Phase: agentsv1alpha1.SandboxClaimPhaseClaiming,
				},
			},
			expectedPhase:     agentsv1alpha1.SandboxClaimPhaseCompleted,
			shouldRequeue:     true,
			checkCompletedSet: true,
		},
		{
			name: "claim timeout",
			args: ClaimArgs{
				Claim: &agentsv1alpha1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{
						Generation: 1,
					},
					Spec: agentsv1alpha1.SandboxClaimSpec{
						TemplateName: "test",
						ClaimTimeout: &metav1.Duration{Duration: 5 * time.Second},
					},
				},
				SandboxSet: &agentsv1alpha1.SandboxSet{},
				NewStatus: &agentsv1alpha1.SandboxClaimStatus{
					Phase:          agentsv1alpha1.SandboxClaimPhaseClaiming,
					ClaimStartTime: &pastTime,
				},
			},
			expectedPhase:     agentsv1alpha1.SandboxClaimPhaseCompleted,
			shouldRequeue:     true,
			checkCompletedSet: true,
		},
		{
			name: "replicas met",
			args: ClaimArgs{
				Claim: &agentsv1alpha1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{
						Generation: 1,
					},
					Spec: agentsv1alpha1.SandboxClaimSpec{
						TemplateName: "test",
						Replicas:     int32Ptr(5),
					},
				},
				SandboxSet: &agentsv1alpha1.SandboxSet{},
				NewStatus: &agentsv1alpha1.SandboxClaimStatus{
					Phase:           agentsv1alpha1.SandboxClaimPhaseClaiming,
					ClaimedReplicas: 5,
				},
			},
			expectedPhase:     agentsv1alpha1.SandboxClaimPhaseCompleted,
			shouldRequeue:     true,
			checkCompletedSet: true,
		},
		{
			name: "still claiming",
			args: ClaimArgs{
				Claim: &agentsv1alpha1.SandboxClaim{
					ObjectMeta: metav1.ObjectMeta{
						Generation: 1,
					},
					Spec: agentsv1alpha1.SandboxClaimSpec{
						TemplateName: "test",
						Replicas:     int32Ptr(10),
					},
				},
				SandboxSet: &agentsv1alpha1.SandboxSet{},
				NewStatus: &agentsv1alpha1.SandboxClaimStatus{
					Phase:           agentsv1alpha1.SandboxClaimPhaseClaiming,
					ClaimedReplicas: 5,
				},
			},
			expectedPhase: agentsv1alpha1.SandboxClaimPhaseClaiming,
			shouldRequeue: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotStatus, gotShouldRequeue := CalculateClaimStatus(tt.args)

			if gotStatus.Phase != tt.expectedPhase {
				t.Errorf("CalculateClaimStatus() phase = %v, want %v", gotStatus.Phase, tt.expectedPhase)
			}

			if gotShouldRequeue != tt.shouldRequeue {
				t.Errorf("CalculateClaimStatus() shouldRequeue = %v, want %v", gotShouldRequeue, tt.shouldRequeue)
			}

			if tt.checkCompletedSet && gotStatus.CompletionTime == nil {
				t.Errorf("CalculateClaimStatus() CompletionTime should be set but is nil")
			}

			if tt.checkStartTimeSet && gotStatus.ClaimStartTime == nil {
				t.Errorf("CalculateClaimStatus() ClaimStartTime should be set but is nil")
			}

			// Check ObservedGeneration is updated
			if gotStatus.ObservedGeneration != tt.args.Claim.Generation {
				t.Errorf("CalculateClaimStatus() ObservedGeneration = %v, want %v",
					gotStatus.ObservedGeneration, tt.args.Claim.Generation)
			}
		})
	}
}

func TestSetClaimCondition(t *testing.T) {
	now := metav1.Now()
	pastTime := metav1.NewTime(now.Add(-10 * time.Second))

	t.Run("add first condition", func(t *testing.T) {
		status := &agentsv1alpha1.SandboxClaimStatus{}
		
		condition := metav1.Condition{
			Type:               "TestCondition",
			Status:             metav1.ConditionTrue,
			Reason:             "TestReason",
			Message:            "Test message",
			LastTransitionTime: now,
		}
		
		SetClaimCondition(status, condition)
		
		if len(status.Conditions) != 1 {
			t.Errorf("Expected 1 condition, got %d", len(status.Conditions))
		}
		
		if status.Conditions[0].Type != "TestCondition" {
			t.Errorf("Expected Type TestCondition, got %v", status.Conditions[0].Type)
		}
	})

	t.Run("add second condition", func(t *testing.T) {
		status := &agentsv1alpha1.SandboxClaimStatus{
			Conditions: []metav1.Condition{
				{
					Type:               "FirstCondition",
					Status:             metav1.ConditionTrue,
					Reason:             "FirstReason",
					Message:            "First message",
					LastTransitionTime: now,
				},
			},
		}
		
		condition := metav1.Condition{
			Type:               "SecondCondition",
			Status:             metav1.ConditionFalse,
			Reason:             "SecondReason",
			Message:            "Second message",
			LastTransitionTime: now,
		}
		
		SetClaimCondition(status, condition)
		
		if len(status.Conditions) != 2 {
			t.Errorf("Expected 2 conditions, got %d", len(status.Conditions))
		}
	})

	t.Run("update existing condition", func(t *testing.T) {
		status := &agentsv1alpha1.SandboxClaimStatus{
			Conditions: []metav1.Condition{
				{
					Type:               "TestCondition",
					Status:             metav1.ConditionTrue,
					Reason:             "OldReason",
					Message:            "Old message",
					LastTransitionTime: pastTime,
				},
			},
		}
		
		condition := metav1.Condition{
			Type:               "TestCondition",
			Status:             metav1.ConditionFalse,
			Reason:             "NewReason",
			Message:            "New message",
			LastTransitionTime: now,
		}
		
		SetClaimCondition(status, condition)
		
		if len(status.Conditions) != 1 {
			t.Errorf("Expected 1 condition, got %d", len(status.Conditions))
		}
		
		if status.Conditions[0].Status != metav1.ConditionFalse {
			t.Errorf("Expected Status False, got %v", status.Conditions[0].Status)
		}
		
		if status.Conditions[0].Reason != "NewReason" {
			t.Errorf("Expected Reason NewReason, got %v", status.Conditions[0].Reason)
		}
	})

	t.Run("no update when nothing changes", func(t *testing.T) {
		originalTime := metav1.NewTime(now.Add(-5 * time.Minute))
		status := &agentsv1alpha1.SandboxClaimStatus{
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					LastTransitionTime: originalTime,
					Reason:             "AllReady",
					Message:            "Everything is ready",
				},
			},
		}
		
		// Try to update with same values but different LastTransitionTime
		condition := metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now, // Different time!
			Reason:             "AllReady",
			Message:            "Everything is ready",
		}
		
		SetClaimCondition(status, condition)
		
		// LastTransitionTime should NOT change because Status/Reason/Message are the same
		if !status.Conditions[0].LastTransitionTime.Equal(&originalTime) {
			t.Errorf("Expected LastTransitionTime to remain %v, got %v (should not update when nothing changes)",
				originalTime, status.Conditions[0].LastTransitionTime)
		}
	})

	t.Run("preserve LastTransitionTime when only Reason changes", func(t *testing.T) {
		originalTime := metav1.NewTime(now.Add(-10 * time.Minute))
		status := &agentsv1alpha1.SandboxClaimStatus{
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					LastTransitionTime: originalTime,
					Reason:             "OldReason",
					Message:            "Old message",
				},
			},
		}
		
		// Update only Reason and Message, Status stays the same
		condition := metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue, // Same Status
			LastTransitionTime: now,
			Reason:             "NewReason", // Different Reason
			Message:            "New message", // Different Message
		}
		
		SetClaimCondition(status, condition)
		
		// LastTransitionTime should NOT change because Status didn't change
		if !status.Conditions[0].LastTransitionTime.Equal(&originalTime) {
			t.Errorf("Expected LastTransitionTime to remain %v when Status doesn't change, got %v",
				originalTime, status.Conditions[0].LastTransitionTime)
		}
		
		// But Reason and Message should be updated
		if status.Conditions[0].Reason != "NewReason" {
			t.Errorf("Expected Reason to be updated to NewReason, got %v", status.Conditions[0].Reason)
		}
		if status.Conditions[0].Message != "New message" {
			t.Errorf("Expected Message to be updated, got %v", status.Conditions[0].Message)
		}
	})

	t.Run("update LastTransitionTime when Status changes", func(t *testing.T) {
		originalTime := metav1.NewTime(now.Add(-10 * time.Minute))
		status := &agentsv1alpha1.SandboxClaimStatus{
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					LastTransitionTime: originalTime,
					Reason:             "AllReady",
					Message:            "Everything is ready",
				},
			},
		}
		
		// Change Status
		condition := metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse, // Status changed!
			LastTransitionTime: now,
			Reason:             "NotReady",
			Message:            "Something went wrong",
		}
		
		SetClaimCondition(status, condition)
		
		// LastTransitionTime SHOULD change because Status changed
		if !status.Conditions[0].LastTransitionTime.Equal(&now) {
			t.Errorf("Expected LastTransitionTime to be updated to %v when Status changes, got %v",
				now, status.Conditions[0].LastTransitionTime)
		}
		
		// All fields should be updated
		if status.Conditions[0].Status != metav1.ConditionFalse {
			t.Errorf("Expected Status to be False, got %v", status.Conditions[0].Status)
		}
		if status.Conditions[0].Reason != "NotReady" {
			t.Errorf("Expected Reason to be NotReady, got %v", status.Conditions[0].Reason)
		}
		if status.Conditions[0].Message != "Something went wrong" {
			t.Errorf("Expected Message to be updated, got %v", status.Conditions[0].Message)
		}
	})
}

func TestGetClaimCondition(t *testing.T) {
	now := metav1.Now()
	
	t.Run("find existing condition", func(t *testing.T) {
		status := &agentsv1alpha1.SandboxClaimStatus{
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "AllReady",
					Message:            "Everything is ready",
					LastTransitionTime: now,
				},
				{
					Type:               "Completed",
					Status:             metav1.ConditionTrue,
					Reason:             "AllDone",
					Message:            "All done",
					LastTransitionTime: now,
				},
			},
		}
		
		cond := GetClaimCondition(status, "Ready")
		if cond == nil {
			t.Error("Expected to find Ready condition, got nil")
		}
		if cond != nil && cond.Reason != "AllReady" {
			t.Errorf("Expected Reason AllReady, got %v", cond.Reason)
		}
	})
	
	t.Run("condition not found", func(t *testing.T) {
		status := &agentsv1alpha1.SandboxClaimStatus{
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "AllReady",
					Message:            "Everything is ready",
					LastTransitionTime: now,
				},
			},
		}
		
		cond := GetClaimCondition(status, "NotExist")
		if cond != nil {
			t.Errorf("Expected nil for non-existent condition, got %v", cond)
		}
	})
	
	t.Run("empty conditions", func(t *testing.T) {
		status := &agentsv1alpha1.SandboxClaimStatus{}
		
		cond := GetClaimCondition(status, "Ready")
		if cond != nil {
			t.Errorf("Expected nil for empty conditions, got %v", cond)
		}
	})
}

func TestTransitionFunctions(t *testing.T) {
	now := metav1.Now()
	pastTime := metav1.NewTime(now.Add(-10 * time.Second))

	t.Run("TransitionToCompleted", func(t *testing.T) {
		status := &agentsv1alpha1.SandboxClaimStatus{
			Phase: agentsv1alpha1.SandboxClaimPhaseClaiming,
		}

		result := TransitionToCompleted(status, "TestReason", "Test message")

		if result.Phase != agentsv1alpha1.SandboxClaimPhaseCompleted {
			t.Errorf("TransitionToCompleted() phase = %v, want Completed", result.Phase)
		}

		if result.CompletionTime == nil {
			t.Error("TransitionToCompleted() CompletionTime should be set")
		}

		if result.Message != "Test message" {
			t.Errorf("TransitionToCompleted() message = %v, want 'Test message'", result.Message)
		}
	})

	t.Run("transitionToCompletedWithTimeout", func(t *testing.T) {
		claim := &agentsv1alpha1.SandboxClaim{
			Spec: agentsv1alpha1.SandboxClaimSpec{
				Replicas: int32Ptr(10),
			},
		}
		status := &agentsv1alpha1.SandboxClaimStatus{
			ClaimedReplicas: 5,
		}

		elapsed := 30 * time.Second
		result := transitionToCompletedWithTimeout(status, elapsed, claim)

		if result.Phase != agentsv1alpha1.SandboxClaimPhaseCompleted {
			t.Errorf("transitionToCompletedWithTimeout() phase = %v, want Completed", result.Phase)
		}

		// Check timeout condition is set
		foundTimeout := false
		for _, c := range result.Conditions {
			if c.Type == string(agentsv1alpha1.SandboxClaimConditionTimedOut) {
				foundTimeout = true
				if c.Status != metav1.ConditionTrue {
					t.Error("TimedOut condition should be True")
				}
			}
		}
		if !foundTimeout {
			t.Error("TimedOut condition not found")
		}
	})

	t.Run("transitionToCompletedWithSuccess", func(t *testing.T) {
		claim := &agentsv1alpha1.SandboxClaim{
			Spec: agentsv1alpha1.SandboxClaimSpec{
				Replicas: int32Ptr(10),
			},
		}
		status := &agentsv1alpha1.SandboxClaimStatus{
			ClaimedReplicas: 10,
			ClaimStartTime:  &pastTime,
		}

		result := transitionToCompletedWithSuccess(status, claim)

		if result.Phase != agentsv1alpha1.SandboxClaimPhaseCompleted {
			t.Errorf("transitionToCompletedWithSuccess() phase = %v, want Completed", result.Phase)
		}

		// Check completed condition is set
		foundCompleted := false
		for _, c := range result.Conditions {
			if c.Type == string(agentsv1alpha1.SandboxClaimConditionCompleted) {
				foundCompleted = true
				if c.Status != metav1.ConditionTrue {
					t.Error("Completed condition should be True")
				}
				if c.Reason != "AllReplicasClaimed" {
					t.Errorf("Completed condition reason = %v, want 'AllReplicasClaimed'", c.Reason)
				}
			}
		}
		if !foundCompleted {
			t.Error("Completed condition not found")
		}
	})
}
