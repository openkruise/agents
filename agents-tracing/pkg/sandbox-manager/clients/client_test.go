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

package clients

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minimalKubeconfig returns the content of a minimal valid kubeconfig that points to a local server.
const minimalKubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://localhost:6443
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    user: test-user
  name: test-context
current-context: test-context
users:
- name: test-user
  user:
    token: test-token
`

func TestNewRestConfig(t *testing.T) {
	// Create a temporary kubeconfig so test cases that don't specify a kubeconfig
	// are not affected by whether ~/.kube/config exists in the current environment
	// (e.g. GitHub Actions runners).
	tmpDir := t.TempDir()
	tmpKubeconfig := filepath.Join(tmpDir, "kubeconfig")
	require.NoError(t, os.WriteFile(tmpKubeconfig, []byte(minimalKubeconfig), 0600))

	tests := []struct {
		name               string
		qps                float32
		burst              int
		kubeconfigEnv      string
		kubeClientQpsEnv   string
		kubeClientBurstEnv string
		expectError        string
		expectedQPS        float32
		expectedBurst      int
	}{
		{
			name:               "use parameter values for QPS and Burst",
			qps:                100,
			burst:              200,
			kubeconfigEnv:      "",
			kubeClientQpsEnv:   "",
			kubeClientBurstEnv: "",
			expectError:        "",
			expectedQPS:        100,
			expectedBurst:      200,
		},
		{
			name:               "override QPS with KUBE_CLIENT_QPS env var",
			qps:                100,
			burst:              200,
			kubeconfigEnv:      "",
			kubeClientQpsEnv:   "150",
			kubeClientBurstEnv: "",
			expectError:        "",
			expectedQPS:        150,
			expectedBurst:      200,
		},
		{
			name:               "override Burst with KUBE_CLIENT_BURST env var",
			qps:                100,
			burst:              200,
			kubeconfigEnv:      "",
			kubeClientQpsEnv:   "",
			kubeClientBurstEnv: "250",
			expectError:        "",
			expectedQPS:        100,
			expectedBurst:      250,
		},
		{
			name:               "override both QPS and Burst with env vars",
			qps:                100,
			burst:              200,
			kubeconfigEnv:      "",
			kubeClientQpsEnv:   "150.5",
			kubeClientBurstEnv: "300",
			expectError:        "",
			expectedQPS:        150.5,
			expectedBurst:      300,
		},
		{
			name:               "ignore invalid KUBE_CLIENT_QPS env var",
			qps:                100,
			burst:              200,
			kubeconfigEnv:      "",
			kubeClientQpsEnv:   "invalid",
			kubeClientBurstEnv: "",
			expectError:        "",
			expectedQPS:        100,
			expectedBurst:      200,
		},
		{
			name:               "ignore invalid KUBE_CLIENT_BURST env var",
			qps:                100,
			burst:              200,
			kubeconfigEnv:      "",
			kubeClientQpsEnv:   "",
			kubeClientBurstEnv: "invalid",
			expectError:        "",
			expectedQPS:        100,
			expectedBurst:      200,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Setenv isolates env vars per test and restores them on cleanup,
			// which avoids data races with parallel tests that read the same vars.
			// When kubeconfigEnv is empty, use the pre-created temp kubeconfig to
			// keep tests hermetic across environments (e.g. GitHub Actions).
			kubeconfigVal := tt.kubeconfigEnv
			if kubeconfigVal == "" {
				kubeconfigVal = tmpKubeconfig
			}
			t.Setenv("KUBECONFIG", kubeconfigVal)
			t.Setenv("KUBE_CLIENT_QPS", tt.kubeClientQpsEnv)
			t.Setenv("KUBE_CLIENT_BURST", tt.kubeClientBurstEnv)

			config, err := NewRestConfig(tt.qps, tt.burst)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
				require.NotNil(t, config)
				assert.Equal(t, tt.expectedQPS, config.QPS)
				assert.Equal(t, tt.expectedBurst, config.Burst)
			}
		})
	}
}

func TestNewRestConfig_WithKubeconfigFile(t *testing.T) {
	t.Run("use KUBECONFIG env var when set", func(t *testing.T) {
		// Create a temporary kubeconfig file
		tmpDir := t.TempDir()
		kubeconfigPath := filepath.Join(tmpDir, "test-kubeconfig")
		kubeconfigContent := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://localhost:6443
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    user: test-user
  name: test-context
current-context: test-context
users:
- name: test-user
  user:
    token: test-token
`
		err := os.WriteFile(kubeconfigPath, []byte(kubeconfigContent), 0644)
		require.NoError(t, err)

		t.Setenv("KUBECONFIG", kubeconfigPath)
		t.Setenv("KUBE_CLIENT_QPS", "")
		t.Setenv("KUBE_CLIENT_BURST", "")

		config, err := NewRestConfig(50, 100)
		require.NoError(t, err)
		require.NotNil(t, config)
		assert.Equal(t, float32(50), config.QPS)
		assert.Equal(t, 100, config.Burst)
		assert.Contains(t, config.Host, "localhost:6443")
	})

	t.Run("fallback to default kubeconfig path when KUBECONFIG not set", func(t *testing.T) {
		t.Setenv("KUBECONFIG", "")
		t.Setenv("KUBE_CLIENT_QPS", "")
		t.Setenv("KUBE_CLIENT_BURST", "")

		// This test will try to use the default ~/.kube/config path.
		// If the file doesn't exist, it will return an error.
		// Both outcomes are acceptable as we only verify it doesn't panic.
		config, err := NewRestConfig(75, 150)

		if err != nil {
			assert.Contains(t, err.Error(), "failed to build config from kubeconfig")
		} else {
			require.NotNil(t, config)
			assert.Equal(t, float32(75), config.QPS)
			assert.Equal(t, 150, config.Burst)
		}
	})
}

func TestNewRestConfig_ZeroValues(t *testing.T) {
	t.Run("accept zero QPS and Burst", func(t *testing.T) {
		t.Setenv("KUBECONFIG", "")
		t.Setenv("KUBE_CLIENT_QPS", "")
		t.Setenv("KUBE_CLIENT_BURST", "")

		// Zero values are valid - they will use Kubernetes client-go defaults.
		// The call may succeed or fail depending on environment, both are acceptable.
		config, err := NewRestConfig(0, 0)

		if err != nil {
			assert.Contains(t, err.Error(), "failed to build config from kubeconfig")
		} else {
			require.NotNil(t, config)
			assert.Equal(t, float32(0), config.QPS)
			assert.Equal(t, 0, config.Burst)
		}
	})
}
