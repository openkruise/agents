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
	"crypto/md5"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	_ "github.com/openkruise/agents/pkg/features"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
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

func TestTruncateConditionMessage(t *testing.T) {
	// Ensure default MaxConditionMessageLen is 1024 (no env override in this test).
	if MaxConditionMessageLen != 1024 {
		t.Fatalf("expected default MaxConditionMessageLen=1024, got %d", MaxConditionMessageLen)
	}

	tests := []struct {
		name     string
		msg      string
		expected string
	}{
		{
			name:     "shorter than max length",
			msg:      "short message",
			expected: "short message",
		},
		{
			name:     "exactly max length",
			msg:      strings.Repeat("a", MaxConditionMessageLen),
			expected: strings.Repeat("a", MaxConditionMessageLen),
		},
		{
			name:     "longer than max length",
			msg:      strings.Repeat("b", MaxConditionMessageLen+10),
			expected: strings.Repeat("b", MaxConditionMessageLen) + "...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateConditionMessage(tt.msg)
			if got != tt.expected {
				t.Fatalf("TruncateConditionMessage() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestTruncateConditionMessage_EnvOverride(t *testing.T) {
	original := MaxConditionMessageLen
	defer func() { MaxConditionMessageLen = original }()

	tests := []struct {
		name    string
		envVal  string
		wantLen int
	}{
		{
			name:    "valid env value",
			envVal:  "512",
			wantLen: 512,
		},
		{
			name:    "invalid env value falls back to default",
			envVal:  "not_a_number",
			wantLen: 1024,
		},
		{
			name:    "zero env value falls back to default",
			envVal:  "0",
			wantLen: 1024,
		},
		{
			name:    "negative env value falls back to default",
			envVal:  "-1",
			wantLen: 1024,
		},
		{
			name:    "empty env value falls back to default",
			envVal:  "",
			wantLen: 1024,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envVal != "" {
				t.Setenv("MAX_CONDITION_MESSAGE_LEN", tt.envVal)
			}
			MaxConditionMessageLen = getEnvIntOrDefault("MAX_CONDITION_MESSAGE_LEN", 1024)

			if MaxConditionMessageLen != tt.wantLen {
				t.Fatalf("MaxConditionMessageLen = %d, want %d", MaxConditionMessageLen, tt.wantLen)
			}

			// Verify truncation works with overridden length
			longMsg := strings.Repeat("x", tt.wantLen+5)
			got := TruncateConditionMessage(longMsg)
			expected := strings.Repeat("x", tt.wantLen) + "..."
			if got != expected {
				t.Fatalf("TruncateConditionMessage() len = %d, want %d", len(got), len(expected))
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

func TestDumpJson(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		expected string
	}{
		{
			name:     "simple string",
			input:    "hello",
			expected: `"hello"`,
		},
		{
			name:     "integer",
			input:    42,
			expected: "42",
		},
		{
			name:     "float",
			input:    3.14,
			expected: "3.14",
		},
		{
			name:     "boolean true",
			input:    true,
			expected: "true",
		},
		{
			name:     "boolean false",
			input:    false,
			expected: "false",
		},
		{
			name:     "nil",
			input:    nil,
			expected: "null",
		},
		{
			name:     "simple map",
			input:    map[string]string{"key": "value"},
			expected: `{"key":"value"}`,
		},
		{
			name:     "simple slice",
			input:    []int{1, 2, 3},
			expected: "[1,2,3]",
		},
		{
			name: "struct",
			input: struct {
				Name  string `json:"name"`
				Value int    `json:"value"`
			}{Name: "test", Value: 123},
			expected: `{"name":"test","value":123}`,
		},
		{
			name:     "empty map",
			input:    map[string]string{},
			expected: "{}",
		},
		{
			name:     "empty slice",
			input:    []int{},
			expected: "[]",
		},
		{
			name:     "nested structure",
			input:    map[string]interface{}{"outer": map[string]int{"inner": 1}},
			expected: `{"outer":{"inner":1}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DumpJson(tt.input)
			if result != tt.expected {
				t.Errorf("DumpJson() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestHashData(t *testing.T) {
	tests := []struct {
		name                string
		input               []byte
		expectNonEmpty      bool
		expectDeterministic bool
	}{
		{
			name:                "empty bytes",
			input:               []byte{},
			expectNonEmpty:      true,
			expectDeterministic: true,
		},
		{
			name:                "simple string bytes",
			input:               []byte("hello world"),
			expectNonEmpty:      true,
			expectDeterministic: true,
		},
		{
			name:                "binary data",
			input:               []byte{0x00, 0x01, 0x02, 0x03, 0xff},
			expectNonEmpty:      true,
			expectDeterministic: true,
		},
		{
			name:                "large data",
			input:               make([]byte, 10000),
			expectNonEmpty:      true,
			expectDeterministic: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := HashData(tt.input)

			if tt.expectNonEmpty && result == "" {
				t.Errorf("HashData() returned empty string")
			}

			// Check max length constraint (9 chars after truncation + encoding)
			if len(result) > 12 { // SafeEncodeString may add some chars
				t.Errorf("HashData() result too long: %d chars", len(result))
			}

			// Verify determinism
			if tt.expectDeterministic {
				result2 := HashData(tt.input)
				if result != result2 {
					t.Errorf("HashData() not deterministic: %s != %s", result, result2)
				}
			}
		})
	}

	// Test that different inputs produce different hashes
	t.Run("different inputs produce different hashes", func(t *testing.T) {
		hash1 := HashData([]byte("input1"))
		hash2 := HashData([]byte("input2"))
		if hash1 == hash2 {
			t.Errorf("HashData() produced same hash for different inputs")
		}
	})
}

func TestRandStringN(t *testing.T) {
	tests := []struct {
		name       string
		n          int
		expectLen  int
		expectRand bool
	}{
		{
			name:       "zero length returns empty",
			n:          0,
			expectLen:  0,
			expectRand: false,
		},
		{
			name:       "negative length returns empty",
			n:          -1,
			expectLen:  0,
			expectRand: false,
		},
		{
			name:       "length 1",
			n:          1,
			expectLen:  1,
			expectRand: true,
		},
		{
			name:       "length 8",
			n:          8,
			expectLen:  8,
			expectRand: true,
		},
		{
			name:       "length 32",
			n:          32,
			expectLen:  32,
			expectRand: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := RandStringN(tt.n)
			if len(result) != tt.expectLen {
				t.Errorf("RandStringN(%d) returned len %d, want %d", tt.n, len(result), tt.expectLen)
			}
			// Returned characters must be lower-case alphanumeric.
			for _, r := range result {
				isLower := r >= 'a' && r <= 'z'
				isDigit := r >= '0' && r <= '9'
				if !isLower && !isDigit {
					t.Errorf("RandStringN(%d) contains invalid character %q", tt.n, r)
				}
			}
			if tt.expectRand {
				// With reasonable n, two consecutive calls should almost certainly differ.
				// Allow a tiny retry window to keep the test deterministic for n==1.
				differ := false
				for i := 0; i < 5; i++ {
					if RandStringN(tt.n) != result {
						differ = true
						break
					}
				}
				if !differ {
					t.Errorf("RandStringN(%d) appears non-random: repeated identical output", tt.n)
				}
			}
		})
	}
}

func TestFilterOutCondition(t *testing.T) {
	tests := []struct {
		name           string
		conditions     []metav1.Condition
		condType       string
		expectedLength int
	}{
		{
			name:           "empty conditions",
			conditions:     []metav1.Condition{},
			condType:       "Ready",
			expectedLength: 0,
		},
		{
			name: "remove existing condition",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
				{Type: "Progressing", Status: metav1.ConditionFalse},
			},
			condType:       "Ready",
			expectedLength: 1,
		},
		{
			name: "remove non-existing condition",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
				{Type: "Progressing", Status: metav1.ConditionFalse},
			},
			condType:       "NonExisting",
			expectedLength: 2,
		},
		{
			name: "remove all conditions of same type",
			conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue},
				{Type: "Ready", Status: metav1.ConditionFalse},
			},
			condType:       "Ready",
			expectedLength: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterOutCondition(tt.conditions, tt.condType)
			if len(result) != tt.expectedLength {
				t.Errorf("filterOutCondition() returned %d conditions, want %d", len(result), tt.expectedLength)
			}

			// Verify the specific condition type was removed
			for _, cond := range result {
				if cond.Type == tt.condType {
					t.Errorf("filterOutCondition() did not remove condition type %s", tt.condType)
				}
			}
		})
	}
}

func TestIsLoopbackIP(t *testing.T) {
	tests := []struct {
		name     string
		ip       string
		expected bool
	}{
		// IPv4 loopback addresses
		{
			name:     "IPv4 loopback 127.0.0.1",
			ip:       "127.0.0.1",
			expected: true,
		},
		{
			name:     "IPv4 loopback 127.0.0.2",
			ip:       "127.0.0.2",
			expected: true,
		},
		{
			name:     "IPv4 loopback 127.255.255.255",
			ip:       "127.255.255.255",
			expected: true,
		},
		// IPv6 loopback address
		{
			name:     "IPv6 loopback ::1",
			ip:       "::1",
			expected: true,
		},
		{
			name:     "IPv6 loopback expanded",
			ip:       "0:0:0:0:0:0:0:1",
			expected: true,
		},
		// Non-loopback IPv4 addresses
		{
			name:     "IPv4 non-loopback 192.168.1.1",
			ip:       "192.168.1.1",
			expected: false,
		},
		{
			name:     "IPv4 non-loopback 10.0.0.1",
			ip:       "10.0.0.1",
			expected: false,
		},
		{
			name:     "IPv4 non-loopback 172.16.0.1",
			ip:       "172.16.0.1",
			expected: false,
		},
		{
			name:     "IPv4 non-loopback 8.8.8.8",
			ip:       "8.8.8.8",
			expected: false,
		},
		{
			name:     "IPv4 non-loopback 0.0.0.0",
			ip:       "0.0.0.0",
			expected: false,
		},
		// Non-loopback IPv6 addresses
		{
			name:     "IPv6 non-loopback ::",
			ip:       "::",
			expected: false,
		},
		{
			name:     "IPv6 non-loopback 2001:db8::1",
			ip:       "2001:db8::1",
			expected: false,
		},
		{
			name:     "IPv6 non-loopback fe80::1",
			ip:       "fe80::1",
			expected: false,
		},
		// Invalid IP addresses
		{
			name:     "empty string",
			ip:       "",
			expected: false,
		},
		{
			name:     "invalid IP format",
			ip:       "invalid",
			expected: false,
		},
		{
			name:     "invalid IP with port",
			ip:       "127.0.0.1:8080",
			expected: false,
		},
		{
			name:     "invalid IP out of range",
			ip:       "256.0.0.1",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsLoopbackIP(tt.ip)
			if result != tt.expected {
				t.Errorf("IsLoopbackIP(%q) = %v, want %v", tt.ip, result, tt.expected)
			}
		})
	}
}

func TestGenerateSandboxName(t *testing.T) {
	// Import features package to register feature gates
	_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxMultiClusterNaming=false")

	tests := []struct {
		name                 string
		featureGateEnabled   bool
		clusterID            string
		baseName             string
		expectedGenerateName string
	}{
		{
			name:                 "feature gate disabled - generateName unchanged",
			featureGateEnabled:   false,
			clusterID:            "cluster-east-1",
			baseName:             "test-sbs",
			expectedGenerateName: "test-sbs-",
		},
		{
			name:               "feature gate enabled with CLUSTER_ID set",
			featureGateEnabled: true,
			clusterID:          "cluster-east-1",
			baseName:           "test-sbs",
			expectedGenerateName: fmt.Sprintf("test-sbs-%s-",
				fmt.Sprintf("%x", md5.Sum([]byte("cluster-east-1")))[:4]),
		},
		{
			name:                 "feature gate enabled but CLUSTER_ID empty - fallback to original",
			featureGateEnabled:   true,
			clusterID:            "",
			baseName:             "test-sbs",
			expectedGenerateName: "test-sbs-",
		},
		{
			name:               "generateName exceeds 58 chars - truncated",
			featureGateEnabled: true,
			clusterID:          "my-cluster",
			baseName:           strings.Repeat("a", 60),
			expectedGenerateName: func() string {
				name := fmt.Sprintf("%s-%s-", strings.Repeat("a", 60),
					fmt.Sprintf("%x", md5.Sum([]byte("my-cluster")))[:4])
				if len(name) > 58 {
					name = name[:58]
				}
				return name
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set feature gate
			if tt.featureGateEnabled {
				_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxMultiClusterNaming=true")
				defer func() { _ = utilfeature.DefaultMutableFeatureGate.Set("SandboxMultiClusterNaming=false") }()
			} else {
				_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxMultiClusterNaming=false")
			}

			// Set environment variable
			if tt.clusterID != "" {
				t.Setenv("CLUSTER_ID", tt.clusterID)
			} else {
				os.Unsetenv("CLUSTER_ID")
			}

			result := GenerateSandboxName(tt.baseName)
			if result != tt.expectedGenerateName {
				t.Errorf("GenerateSandboxName(%q) = %q, want %q", tt.baseName, result, tt.expectedGenerateName)
			}
		})
	}
}

func TestGetSandboxState(t *testing.T) {
	now := metav1.Now()
	pastTime := metav1.NewTime(now.Add(-time.Hour))
	futureTime := metav1.NewTime(now.Add(time.Hour))

	tests := []struct {
		name           string
		sandbox        *agentsv1alpha1.Sandbox
		expectedState  string
		expectedReason string
	}{
		{
			name: "Sandbox with DeletionTimestamp",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &now,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "1.2.3.4",
					},
				},
			},
			expectedState:  agentsv1alpha1.SandboxStateDead,
			expectedReason: "ResourceDeleted",
		},
		{
			name: "Sandbox with expired ShutdownTime",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					ShutdownTime: &pastTime,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "1.2.3.4",
					},
				},
			},
			expectedState:  agentsv1alpha1.SandboxStateDead,
			expectedReason: "ShutdownTimeReached",
		},
		{
			name: "Sandbox with future ShutdownTime",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{},
				Spec: agentsv1alpha1.SandboxSpec{
					ShutdownTime: &futureTime,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(agentsv1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "1.2.3.4",
					},
				},
			},
			expectedState:  agentsv1alpha1.SandboxStateRunning,
			expectedReason: "RunningResourceClaimedAndReady",
		},
		{
			name: "Sandbox in Pending phase",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPending,
				},
			},
			expectedState:  agentsv1alpha1.SandboxStateCreating,
			expectedReason: "ResourcePending",
		},
		{
			name: "Sandbox in Succeeded phase",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxSucceeded,
				},
			},
			expectedState:  agentsv1alpha1.SandboxStateDead,
			expectedReason: "ResourceSucceeded",
		},
		{
			name: "Sandbox in Failed phase",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxFailed,
				},
			},
			expectedState:  agentsv1alpha1.SandboxStateDead,
			expectedReason: "ResourceFailed",
		},
		{
			name: "Sandbox in Terminating phase",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxTerminating,
				},
			},
			expectedState:  agentsv1alpha1.SandboxStateDead,
			expectedReason: "ResourceTerminating",
		},
		{
			name: "Sandbox controlled by SandboxSet and Ready",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "agents.kruise.io/v1alpha1",
							Kind:       "SandboxSet",
							Controller: &[]bool{true}[0],
						},
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
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "1.2.3.4",
					},
				},
			},
			expectedState:  agentsv1alpha1.SandboxStateAvailable,
			expectedReason: "ResourceControlledBySbsAndReady",
		},
		{
			name: "Sandbox controlled by SandboxSet but not Ready",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "agents.kruise.io/v1alpha1",
							Kind:       "SandboxSet",
							Controller: &[]bool{true}[0],
						},
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
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "1.2.3.4",
					},
				},
			},
			expectedState:  agentsv1alpha1.SandboxStateCreating,
			expectedReason: "ResourceControlledBySbsButNotReady",
		},
		{
			name: "Running Sandbox claimed and Ready",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(agentsv1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "1.2.3.4",
					}},
			},
			expectedState:  agentsv1alpha1.SandboxStateRunning,
			expectedReason: "RunningResourceClaimedAndReady",
		},
		{
			name: "Running Sandbox claimed but not Ready and Paused",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: true,
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
			expectedState:  agentsv1alpha1.SandboxStatePaused,
			expectedReason: "RunningResourceClaimedAndPaused",
		},
		{
			name: "Running Sandbox claimed but not Ready and not Paused",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: false,
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
			expectedState:  agentsv1alpha1.SandboxStateDead,
			expectedReason: "RunningResourceClaimedButNotReady",
		},
		{
			name: "Not Running Sandbox claimed",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
				},
			},
			expectedState:  agentsv1alpha1.SandboxStatePaused,
			expectedReason: "NotRunningResourceClaimed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, reason := GetSandboxState(tt.sandbox)
			assert.Equal(t, tt.expectedState, state)
			assert.Equal(t, tt.expectedReason, reason)
		})
	}
}

func TestIsControlledBySandboxCR(t *testing.T) {
	tests := []struct {
		name     string
		sandbox  *agentsv1alpha1.Sandbox
		expected bool
	}{
		{
			name: "Sandbox controlled by SandboxSet",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "agents.kruise.io/v1alpha1",
							Kind:       "SandboxSet",
							Controller: &[]bool{true}[0],
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "Sandbox not controlled by anything",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{},
				},
			},
			expected: false,
		},
		{
			name: "Sandbox controlled by non-SandboxSet resource",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "apps/v1",
							Kind:       "Deployment",
							Controller: &[]bool{true}[0],
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "Sandbox with nil controller reference",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "agents.kruise.io/v1alpha1",
							Kind:       "SandboxSet",
							Controller: nil,
						},
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsControlledBySandboxSet(tt.sandbox)
			if result != tt.expected {
				t.Errorf("IsControlledBySandboxSet() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestGetSandboxID(t *testing.T) {
	tests := []struct {
		name     string
		sandbox  *agentsv1alpha1.Sandbox
		expected string
	}{
		{
			name: "Standard namespace and name",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-namespace",
					Name:      "test-name",
				},
			},
			expected: "test-namespace--test-name",
		},
		{
			name: "Empty namespace",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "",
					Name:      "test-name",
				},
			},
			expected: "--test-name",
		},
		{
			name: "Empty name",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-namespace",
					Name:      "",
				},
			},
			expected: "test-namespace--",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetSandboxID(tt.sandbox)
			if result != tt.expected {
				t.Errorf("GetSandboxID() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestValidateNamespaceForSandboxID(t *testing.T) {
	tests := []struct {
		name        string
		namespace   string
		expectError string
	}{
		{name: "standard name", namespace: "team-a", expectError: ""},
		{name: "single dash", namespace: "team-a-blue", expectError: ""},
		{name: "empty namespace rejected", namespace: "", expectError: "must not be empty"},
		{name: "double dash rejected", namespace: "team--blue", expectError: "must not contain"},
		{name: "double dash at start", namespace: "--team", expectError: "must not contain"},
		{name: "double dash at end", namespace: "team--", expectError: "must not contain"},
		{name: "triple dash contains double dash", namespace: "a---b", expectError: "must not contain"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateNamespaceForSandboxID(tt.namespace)
			if tt.expectError == "" {
				assert.NoError(t, err, "unexpected error for %q", tt.namespace)
				return
			}
			assert.Error(t, err, "expected error for %q", tt.namespace)
			if err != nil {
				assert.Contains(t, err.Error(), tt.expectError)
			}
		})
	}
}

func TestIsSandboxPausable(t *testing.T) {
	tests := []struct {
		name           string
		sandbox        *agentsv1alpha1.Sandbox
		expectedResult bool
		expectedReason string
	}{
		{
			name: "Running sandbox is pausable",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			expectedResult: true,
			expectedReason: "SandboxIsRunningOrPaused",
		},
		{
			name: "Paused sandbox is pausable",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
				},
			},
			expectedResult: true,
			expectedReason: "SandboxIsRunningOrPaused",
		},
		{
			name: "Pending sandbox is not pausable",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPending,
				},
			},
			expectedResult: false,
			expectedReason: "SandboxPhaseNotAllowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, reason := IsSandboxPausable(tt.sandbox)
			assert.Equal(t, tt.expectedResult, result)
			assert.Equal(t, tt.expectedReason, reason)
		})
	}
}

func TestIsSandboxResumable(t *testing.T) {
	tests := []struct {
		name           string
		sandbox        *agentsv1alpha1.Sandbox
		expectedResult bool
		expectedReason string
	}{
		{
			name: "Running sandbox is resumable",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			expectedResult: true,
			expectedReason: "SandboxIsRunning",
		},
		{
			name: "Running sandbox with spec.paused=true is not resumable (pausing in progress)",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: true,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			expectedResult: false,
			expectedReason: "SandboxIsPausing",
		},
		{
			name: "Resuming sandbox is resumable",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxResuming,
				},
			},
			expectedResult: true,
			expectedReason: "SandboxIsResuming",
		},
		{
			name: "Paused sandbox with paused condition is resumable",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{
							Type:   string(agentsv1alpha1.SandboxConditionPaused),
							Status: metav1.ConditionTrue,
						},
					},
				},
			},
			expectedResult: true,
			expectedReason: "SandboxIsPaused",
		},
		{
			name: "Paused sandbox without paused condition is not resumable",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
				},
			},
			expectedResult: false,
			expectedReason: "SandboxIsPausing",
		},
		{
			name: "Succeeded sandbox is not resumable",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxSucceeded,
				},
			},
			expectedResult: false,
			expectedReason: "SandboxPhaseNotAllowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, reason := IsSandboxResumable(tt.sandbox)
			assert.Equal(t, tt.expectedResult, result)
			assert.Equal(t, tt.expectedReason, reason)
		})
	}
}

func TestGetAccessToken(t *testing.T) {
	tests := []struct {
		name     string
		obj      metav1.Object
		expected string
	}{
		{
			name:     "nil object returns empty string",
			obj:      nil,
			expected: "",
		},
		{
			name: "sandbox with runtime access token annotation returns runtime token",
			obj: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationRuntimeAccessToken: "runtime-token",
					},
				},
			},
			expected: "runtime-token",
		},
		{
			name: "sandbox-claim with only legacy envd token falls back to legacy",
			obj: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationEnvdAccessToken: "envd-token",
					},
				},
			},
			expected: "envd-token",
		},
		{
			name: "runtime token takes precedence over legacy envd token",
			obj: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationRuntimeAccessToken: "runtime-token",
						agentsv1alpha1.AnnotationEnvdAccessToken:    "envd-token",
					},
				},
			},
			expected: "runtime-token",
		},
		{
			name:     "object without annotations returns empty string",
			obj:      &agentsv1alpha1.Sandbox{},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, GetAccessToken(tt.obj))
		})
	}
}

func TestGetTemplateSpec(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	inlineTemplate := &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"foo": "bar"},
		},
	}

	referencedTemplate := &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"ref": "val"},
		},
	}

	existingTmpl := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "my-tmpl",
		},
		Spec: agentsv1alpha1.SandboxTemplateSpec{
			Template: referencedTemplate,
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingTmpl).Build()

	tests := []struct {
		name           string
		embedded       *agentsv1alpha1.EmbeddedSandboxTemplate
		expectedLabels map[string]string
		expectError    string
	}{
		{
			name:        "Nil embedded template",
			embedded:    nil,
			expectError: "",
		},
		{
			name: "Inline Template only",
			embedded: &agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: inlineTemplate,
			},
			expectedLabels: map[string]string{"foo": "bar"},
			expectError:    "",
		},
		{
			name: "TemplateRef referencing existing SandboxTemplate",
			embedded: &agentsv1alpha1.EmbeddedSandboxTemplate{
				TemplateRef: &agentsv1alpha1.SandboxTemplateRef{
					Name: "my-tmpl",
				},
			},
			expectedLabels: map[string]string{"ref": "val"},
			expectError:    "",
		},
		{
			name: "TemplateRef referencing missing SandboxTemplate",
			embedded: &agentsv1alpha1.EmbeddedSandboxTemplate{
				TemplateRef: &agentsv1alpha1.SandboxTemplateRef{
					Name: "non-existent-tmpl",
				},
			},
			expectError: "sandboxtemplates.agents.kruise.io \"non-existent-tmpl\" not found",
		},
		{
			name:        "Both Template and TemplateRef are nil",
			embedded:    &agentsv1alpha1.EmbeddedSandboxTemplate{},
			expectError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetTemplateSpec(context.TODO(), k8sClient, "default", tt.embedded)
			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
				if tt.expectedLabels != nil {
					assert.NotNil(t, got)
					assert.Equal(t, tt.expectedLabels, got.Labels)
				} else {
					assert.Nil(t, got)
				}
			}
		})
	}
}
