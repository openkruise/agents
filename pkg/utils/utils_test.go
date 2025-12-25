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

package utils

import (
	"context"
	"testing"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSetSandboxCondition(t *testing.T) {
	tests := []struct {
		name     string
		status   *agentsv1alpha1.SandboxStatus
		cond     metav1.Condition
		expected *agentsv1alpha1.SandboxStatus
	}{
		{
			name:   "add new condition",
			status: &agentsv1alpha1.SandboxStatus{},
			cond: metav1.Condition{
				Type:    "Ready",
				Status:  metav1.ConditionTrue,
				Reason:  "TestReason",
				Message: "Test message",
			},
			expected: &agentsv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:    "Ready",
						Status:  metav1.ConditionTrue,
						Reason:  "TestReason",
						Message: "Test message",
					},
				},
			},
		},
		{
			name: "update existing condition with different status",
			status: &agentsv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:    "Ready",
						Status:  metav1.ConditionFalse,
						Reason:  "OldReason",
						Message: "Old message",
					},
				},
			},
			cond: metav1.Condition{
				Type:    "Ready",
				Status:  metav1.ConditionTrue,
				Reason:  "NewReason",
				Message: "New message",
			},
			expected: &agentsv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:    "Ready",
						Status:  metav1.ConditionTrue,
						Reason:  "NewReason",
						Message: "New message",
					},
				},
			},
		},
		{
			name: "condition with same status, reason, message and timestamp - no update",
			status: &agentsv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:    "Ready",
						Status:  metav1.ConditionTrue,
						Reason:  "TestReason",
						Message: "Test message",
					},
				},
			},
			cond: metav1.Condition{
				Type:    "Ready",
				Status:  metav1.ConditionTrue,
				Reason:  "TestReason",
				Message: "Test message",
			},
			expected: &agentsv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:    "Ready",
						Status:  metav1.ConditionTrue,
						Reason:  "TestReason",
						Message: "Test message",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			SetSandboxCondition(tt.status, tt.cond)

			if len(tt.status.Conditions) != len(tt.expected.Conditions) {
				t.Errorf("Expected %d conditions, got %d", len(tt.expected.Conditions), len(tt.status.Conditions))
				return
			}

			for i, expectedCond := range tt.expected.Conditions {
				actualCond := tt.status.Conditions[i]
				if actualCond.Type != expectedCond.Type {
					t.Errorf("Condition %d: expected type %s, got %s", i, expectedCond.Type, actualCond.Type)
				}
				if actualCond.Status != expectedCond.Status {
					t.Errorf("Condition %d: expected status %s, got %s", i, expectedCond.Status, actualCond.Status)
				}
				if actualCond.Reason != expectedCond.Reason {
					t.Errorf("Condition %d: expected reason %s, got %s", i, expectedCond.Reason, actualCond.Reason)
				}
				if actualCond.Message != expectedCond.Message {
					t.Errorf("Condition %d: expected message %s, got %s", i, expectedCond.Message, actualCond.Message)
				}
			}
		})
	}
}

func TestGetSandboxCondition(t *testing.T) {
	status := &agentsv1alpha1.SandboxStatus{
		Conditions: []metav1.Condition{
			{
				Type:    "Ready",
				Status:  metav1.ConditionTrue,
				Reason:  "TestReason",
				Message: "Test message",
			},
			{
				Type:    "Progressing",
				Status:  metav1.ConditionFalse,
				Reason:  "TestReason2",
				Message: "Test message2",
			},
		},
	}

	tests := []struct {
		name     string
		condType string
		expected *metav1.Condition
	}{
		{
			name:     "find existing condition",
			condType: "Ready",
			expected: &metav1.Condition{
				Type:    "Ready",
				Status:  metav1.ConditionTrue,
				Reason:  "TestReason",
				Message: "Test message",
			},
		},
		{
			name:     "find non-existing condition",
			condType: "NonExisting",
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetSandboxCondition(status, tt.condType)
			if tt.expected == nil {
				if result != nil {
					t.Errorf("Expected nil, got %v", result)
				}
				return
			}

			if result == nil {
				t.Errorf("Expected condition, got nil")
				return
			}

			if result.Type != tt.expected.Type {
				t.Errorf("Expected type %s, got %s", tt.expected.Type, result.Type)
			}
			if result.Status != tt.expected.Status {
				t.Errorf("Expected status %s, got %s", tt.expected.Status, result.Status)
			}
			if result.Reason != tt.expected.Reason {
				t.Errorf("Expected reason %s, got %s", tt.expected.Reason, result.Reason)
			}
			if result.Message != tt.expected.Message {
				t.Errorf("Expected message %s, got %s", tt.expected.Message, result.Message)
			}
		})
	}
}

func TestGetPodCondition(t *testing.T) {
	status := &corev1.PodStatus{
		Conditions: []corev1.PodCondition{
			{
				Type:    corev1.PodReady,
				Status:  corev1.ConditionTrue,
				Reason:  "TestReason",
				Message: "Test message",
			},
			{
				Type:    corev1.PodScheduled,
				Status:  corev1.ConditionFalse,
				Reason:  "TestReason2",
				Message: "Test message2",
			},
		},
	}

	tests := []struct {
		name     string
		condType corev1.PodConditionType
		expected *corev1.PodCondition
	}{
		{
			name:     "find existing condition",
			condType: corev1.PodReady,
			expected: &corev1.PodCondition{
				Type:    corev1.PodReady,
				Status:  corev1.ConditionTrue,
				Reason:  "TestReason",
				Message: "Test message",
			},
		},
		{
			name:     "find non-existing condition",
			condType: corev1.PodConditionType("NonExisting"),
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetPodCondition(status, tt.condType)
			if tt.expected == nil {
				if result != nil {
					t.Errorf("Expected nil, got %v", result)
				}
				return
			}

			if result == nil {
				t.Errorf("Expected condition, got nil")
				return
			}

			if result.Type != tt.expected.Type {
				t.Errorf("Expected type %s, got %s", tt.expected.Type, result.Type)
			}
			if result.Status != tt.expected.Status {
				t.Errorf("Expected status %s, got %s", tt.expected.Status, result.Status)
			}
			if result.Reason != tt.expected.Reason {
				t.Errorf("Expected reason %s, got %s", tt.expected.Reason, result.Reason)
			}
			if result.Message != tt.expected.Message {
				t.Errorf("Expected message %s, got %s", tt.expected.Message, result.Message)
			}
		})
	}
}

func TestRemoveSandboxCondition(t *testing.T) {
	status := &agentsv1alpha1.SandboxStatus{
		Conditions: []metav1.Condition{
			{
				Type:    "Ready",
				Status:  metav1.ConditionTrue,
				Reason:  "TestReason",
				Message: "Test message",
			},
			{
				Type:    "Progressing",
				Status:  metav1.ConditionFalse,
				Reason:  "TestReason2",
				Message: "Test message2",
			},
			{
				Type:    "Available",
				Status:  metav1.ConditionTrue,
				Reason:  "TestReason3",
				Message: "Test message3",
			},
		},
	}

	tests := []struct {
		name          string
		condType      string
		expectedCount int
	}{
		{
			name:          "remove existing condition",
			condType:      "Progressing",
			expectedCount: 2,
		},
		{
			name:          "remove non-existing condition",
			condType:      "NonExisting",
			expectedCount: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newStatus := status.DeepCopy()
			RemoveSandboxCondition(newStatus, tt.condType)
			if len(newStatus.Conditions) != tt.expectedCount {
				t.Errorf("Expected %d conditions after removal, got %s", tt.expectedCount, DumpJson(newStatus.Conditions))
			}

			// Verify the condition was actually removed
			if tt.condType != "NonExisting" {
				for _, cond := range newStatus.Conditions {
					if cond.Type == tt.condType {
						t.Errorf("Condition %s was not removed", tt.condType)
					}
				}
			}

			// Verify other conditions are still there
			if tt.condType == "Progressing" {
				foundReady := false
				foundAvailable := false
				for _, cond := range newStatus.Conditions {
					if cond.Type == "Ready" {
						foundReady = true
					}
					if cond.Type == "Available" {
						foundAvailable = true
					}
				}

				if !foundReady || !foundAvailable {
					t.Errorf("Other conditions were removed when they shouldn't be")
				}
			}
		})
	}
}

func TestUpdateFinalizer(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name        string
		op          FinalizerOpType
		finalizer   string
		initialObj  *agentsv1alpha1.Sandbox
		expectError bool
	}{
		{
			name:      "add finalizer to object without it",
			op:        AddFinalizerOpType,
			finalizer: "test.finalizer",
			initialObj: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Finalizers: []string{},
				},
			},
			expectError: false,
		},
		{
			name:      "add finalizer to object that already has it",
			op:        AddFinalizerOpType,
			finalizer: "test.finalizer",
			initialObj: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Finalizers: []string{"test.finalizer"},
				},
			},
			expectError: false,
		},
		{
			name:      "remove finalizer from object that has it",
			op:        RemoveFinalizerOpType,
			finalizer: "test.finalizer",
			initialObj: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Finalizers: []string{"test.finalizer", "another.finalizer"},
				},
			},
			expectError: false,
		},
		{
			name:      "remove finalizer from object that doesn't have it",
			op:        RemoveFinalizerOpType,
			finalizer: "test.finalizer",
			initialObj: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Finalizers: []string{"another.finalizer"},
				},
			},
			expectError: false,
		},
		{
			name:      "invalid operation type",
			op:        "InvalidOp",
			finalizer: "test.finalizer",
			initialObj: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Finalizers: []string{},
				},
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.op == "InvalidOp" {
				// Test panic for invalid operation
				defer func() {
					if r := recover(); r == nil && tt.expectError {
						t.Errorf("Expected panic for invalid operation type")
					}
				}()
				_ = UpdateFinalizer(nil, tt.initialObj, tt.op, tt.finalizer)
				return
			}

			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.initialObj).Build()
			err := UpdateFinalizer(client, tt.initialObj, tt.op, tt.finalizer)

			if (err != nil) != tt.expectError {
				t.Errorf("Expected error: %v, got: %v", tt.expectError, err)
			}

			if !tt.expectError {
				// Verify the finalizer was updated correctly
				updatedObj := tt.initialObj
				key := types.NamespacedName{
					Namespace: tt.initialObj.GetNamespace(),
					Name:      tt.initialObj.GetName(),
				}
				_ = client.Get(context.TODO(), key, updatedObj)

				finalizers := updatedObj.GetFinalizers()
				hasFinalizer := false
				for _, f := range finalizers {
					if f == tt.finalizer {
						hasFinalizer = true
						break
					}
				}

				if tt.op == AddFinalizerOpType {
					if !hasFinalizer {
						t.Errorf("Finalizer %s was not added", tt.finalizer)
					}
				} else if tt.op == RemoveFinalizerOpType {
					if hasFinalizer {
						t.Errorf("Finalizer %s was not removed", tt.finalizer)
					}
				}
			}
		})
	}
}

func TestPatchFinalizer(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name        string
		op          FinalizerOpType
		finalizer   string
		initialObj  *agentsv1alpha1.Sandbox
		expectError bool
	}{
		{
			name:      "add finalizer using patch",
			op:        AddFinalizerOpType,
			finalizer: "test.finalizer",
			initialObj: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Finalizers: []string{},
				},
			},
			expectError: false,
		},
		{
			name:      "add finalizer to object that already has it using patch",
			op:        AddFinalizerOpType,
			finalizer: "test.finalizer",
			initialObj: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Finalizers: []string{"test.finalizer"},
				},
			},
			expectError: false,
		},
		{
			name:      "remove finalizer using patch",
			op:        RemoveFinalizerOpType,
			finalizer: "test.finalizer",
			initialObj: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Finalizers: []string{"test.finalizer", "another.finalizer"},
				},
			},
			expectError: false,
		},
		{
			name:      "remove finalizer from object that doesn't have it using patch",
			op:        RemoveFinalizerOpType,
			finalizer: "test.finalizer",
			initialObj: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Finalizers: []string{"another.finalizer"},
				},
			},
			expectError: false,
		},
		{
			name:      "invalid operation type with patch",
			op:        "InvalidOp",
			finalizer: "test.finalizer",
			initialObj: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Finalizers: []string{},
				},
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.op == "InvalidOp" {
				// Test panic for invalid operation
				defer func() {
					if r := recover(); r == nil && tt.expectError {
						t.Errorf("Expected panic for invalid operation type")
					}
				}()
				_, _ = PatchFinalizer(context.TODO(), nil, tt.initialObj, tt.op, tt.finalizer)
				return
			}

			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.initialObj).Build()
			_, err := PatchFinalizer(context.TODO(), client, tt.initialObj, tt.op, tt.finalizer)

			if (err != nil) != tt.expectError {
				t.Errorf("Expected error: %v, got: %v", tt.expectError, err)
			}

			if !tt.expectError {
				// Verify the finalizer was updated correctly
				updatedObj := tt.initialObj
				key := types.NamespacedName{
					Namespace: tt.initialObj.GetNamespace(),
					Name:      tt.initialObj.GetName(),
				}
				_ = client.Get(context.TODO(), key, updatedObj)

				finalizers := updatedObj.GetFinalizers()
				hasFinalizer := false
				for _, f := range finalizers {
					if f == tt.finalizer {
						hasFinalizer = true
						break
					}
				}

				if tt.op == AddFinalizerOpType {
					if !hasFinalizer {
						t.Errorf("Finalizer %s was not added", tt.finalizer)
					}
				} else if tt.op == RemoveFinalizerOpType {
					if hasFinalizer {
						t.Errorf("Finalizer %s was not removed", tt.finalizer)
					}
				}
			}
		})
	}
}
