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
package utils

import (
	"crypto/md5"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestGetControllerKey(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		objName   string
		expected  string
	}{
		{
			name:      "normal namespace and name",
			namespace: "default",
			objName:   "test-sandbox",
			expected:  "default/test-sandbox",
		},
		{
			name:      "empty namespace",
			namespace: "",
			objName:   "test-sandbox",
			expected:  "/test-sandbox",
		},
		{
			name:      "namespace with hyphens",
			namespace: "sandbox-system",
			objName:   "my-sandbox-123",
			expected:  "sandbox-system/my-sandbox-123",
		},
		{
			name:      "complex names",
			namespace: "prod-env-v2",
			objName:   "agent-sandbox-abc-xyz-789",
			expected:  "prod-env-v2/agent-sandbox-abc-xyz-789",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: tt.namespace,
					Name:      tt.objName,
				},
			}
			result := GetControllerKey(obj)
			assert.Equal(t, tt.expected, result)
		})
	}

	// Test with Pod object
	t.Run("pod object", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "kube-system",
				Name:      "controller-pod",
			},
		}
		result := GetControllerKey(pod)
		assert.Equal(t, "kube-system/controller-pod", result)
	})
}

func TestGetSandboxControllerUsername(t *testing.T) {
	// Save original env value
	originalEnv := os.Getenv("SANDBOX_CONTROLLER_USERNAME")
	defer func() {
		// Restore original env value after test
		if originalEnv == "" {
			os.Unsetenv("SANDBOX_CONTROLLER_USERNAME")
		} else {
			os.Setenv("SANDBOX_CONTROLLER_USERNAME", originalEnv)
		}
	}()

	tests := []struct {
		name     string
		envValue string
		expected string
	}{
		{
			name:     "default username when env not set",
			envValue: "",
			expected: "system:serviceaccount:sandbox-system:sandbox-controller-manager",
		},
		{
			name:     "custom username from env",
			envValue: "system:serviceaccount:custom-ns:custom-controller",
			expected: "system:serviceaccount:custom-ns:custom-controller",
		},
		{
			name:     "simple username from env",
			envValue: "custom-user",
			expected: "custom-user",
		},
		{
			name:     "username with special characters",
			envValue: "user@domain.com",
			expected: "user@domain.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envValue == "" {
				os.Unsetenv("SANDBOX_CONTROLLER_USERNAME")
			} else {
				os.Setenv("SANDBOX_CONTROLLER_USERNAME", tt.envValue)
			}
			result := GetSandboxControllerUsername()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetClusterIDHash(t *testing.T) {
	// Save original env value
	originalEnv := os.Getenv("CLUSTER_ID")
	defer func() {
		// Restore original env value after test
		if originalEnv == "" {
			os.Unsetenv("CLUSTER_ID")
		} else {
			os.Setenv("CLUSTER_ID", originalEnv)
		}
	}()

	tests := []struct {
		name          string
		envValue      string
		expectEmpty   bool
		expectedLen   int
		validateValue bool
	}{
		{
			name:        "empty cluster ID returns empty string",
			envValue:    "",
			expectEmpty: true,
		},
		{
			name:          "simple cluster ID produces 4-char hash",
			envValue:      "test-cluster",
			expectEmpty:   false,
			expectedLen:   4,
			validateValue: true,
		},
		{
			name:          "complex cluster ID produces 4-char hash",
			envValue:      "production-cluster-us-east-1-v2",
			expectEmpty:   false,
			expectedLen:   4,
			validateValue: true,
		},
		{
			name:          "numeric cluster ID produces 4-char hash",
			envValue:      "12345",
			expectEmpty:   false,
			expectedLen:   4,
			validateValue: true,
		},
		{
			name:          "UUID-like cluster ID produces 4-char hash",
			envValue:      "550e8400-e29b-41d4-a716-446655440000",
			expectEmpty:   false,
			expectedLen:   4,
			validateValue: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envValue == "" {
				os.Unsetenv("CLUSTER_ID")
			} else {
				os.Setenv("CLUSTER_ID", tt.envValue)
			}
			result := GetClusterIDHash()

			if tt.expectEmpty {
				assert.Empty(t, result)
			} else {
				assert.NotEmpty(t, result)
				assert.Equal(t, tt.expectedLen, len(result))
				assert.True(t, len(result) == 4, "hash should be exactly 4 characters")

				if tt.validateValue {
					// Verify the hash is correct by computing it manually
					expectedHash := fmt.Sprintf("%x", md5.Sum([]byte(tt.envValue)))
					expectedPrefix := expectedHash[:4]
					assert.Equal(t, expectedPrefix, result, "hash should match MD5 computation")
				}

				// Verify hash contains only hex characters
				for _, c := range result {
					assert.True(t, (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'),
						"hash should contain only lowercase hex characters, got: %c", c)
				}
			}
		})
	}

	// Test determinism - same input should always produce same output
	t.Run("deterministic hash", func(t *testing.T) {
		os.Setenv("CLUSTER_ID", "deterministic-test")
		hash1 := GetClusterIDHash()
		hash2 := GetClusterIDHash()
		assert.Equal(t, hash1, hash2, "same input should produce same hash")
	})

	// Test different inputs produce different hashes
	t.Run("different inputs produce different hashes", func(t *testing.T) {
		os.Setenv("CLUSTER_ID", "cluster-a")
		hashA := GetClusterIDHash()

		os.Setenv("CLUSTER_ID", "cluster-b")
		hashB := GetClusterIDHash()

		assert.NotEqual(t, hashA, hashB, "different inputs should produce different hashes")
	})
}

func TestDoItSlowly(t *testing.T) {
	tests := []struct {
		name             string
		count            int
		initialBatchSize int
		failAtCall       int // -1 means no failure
		wantSuccesses    int
		expectError      string
	}{
		{
			name:             "all succeed with count 1",
			count:            1,
			initialBatchSize: 1,
			failAtCall:       -1,
			wantSuccesses:    1,
		},
		{
			name:             "all succeed with count 5",
			count:            5,
			initialBatchSize: 1,
			failAtCall:       -1,
			wantSuccesses:    5,
		},
		{
			name:             "all succeed with larger batch size",
			count:            10,
			initialBatchSize: 5,
			failAtCall:       -1,
			wantSuccesses:    10,
		},
		{
			name:             "zero count",
			count:            0,
			initialBatchSize: 1,
			failAtCall:       -1,
			wantSuccesses:    0,
		},
		{
			name:             "failure in first batch",
			count:            5,
			initialBatchSize: 5,
			failAtCall:       3, // calls 1-2 succeed, calls 3-5 fail
			wantSuccesses:    2,
			expectError:      "intentional failure",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			callCount := 0
			var mu sync.Mutex

			fn := func() error {
				mu.Lock()
				callCount++
				currentCall := callCount
				mu.Unlock()

				if tt.failAtCall > 0 && currentCall >= tt.failAtCall {
					return fmt.Errorf("intentional failure at call %d", currentCall)
				}
				return nil
			}

			successes, err := DoItSlowly(tt.count, tt.initialBatchSize, fn)
			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.wantSuccesses, successes)
		})
	}
}

func TestDoItSlowlyWithInputs(t *testing.T) {
	tests := []struct {
		name             string
		inputs           []int
		initialBatchSize int
		failOnInput      int // -1 means no failure
		wantSuccesses    int
		expectError      string
	}{
		{
			name:             "all succeed empty inputs",
			inputs:           []int{},
			initialBatchSize: 1,
			failOnInput:      -1,
			wantSuccesses:    0,
		},
		{
			name:             "all succeed single input",
			inputs:           []int{1},
			initialBatchSize: 1,
			failOnInput:      -1,
			wantSuccesses:    1,
		},
		{
			name:             "all succeed multiple inputs",
			inputs:           []int{1, 2, 3, 4, 5},
			initialBatchSize: 2,
			failOnInput:      -1,
			wantSuccesses:    5,
		},
		{
			name:             "process string inputs",
			inputs:           []int{10, 20, 30},
			initialBatchSize: 1,
			failOnInput:      -1,
			wantSuccesses:    3,
		},
		{
			name:             "failure on specific input",
			inputs:           []int{1, 2, 3},
			initialBatchSize: 1,
			failOnInput:      1, // first input fails, batch stops immediately
			wantSuccesses:    0,
			expectError:      "intentional failure on input",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			processedInputs := make([]int, 0)
			var mu sync.Mutex

			fn := func(input int) error {
				mu.Lock()
				processedInputs = append(processedInputs, input)
				mu.Unlock()

				if tt.failOnInput > 0 && input == tt.failOnInput {
					return fmt.Errorf("intentional failure on input %d", input)
				}
				return nil
			}

			successes, err := DoItSlowlyWithInputs(tt.inputs, tt.initialBatchSize, fn)
			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
				// Verify all inputs were processed when no error
				assert.Equal(t, len(tt.inputs), len(processedInputs))
			}
			assert.Equal(t, tt.wantSuccesses, successes)
		})
	}
}
