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
