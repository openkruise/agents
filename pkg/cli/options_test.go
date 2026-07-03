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

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/rest"
)

func TestNewGlobalOptions(t *testing.T) {
	opts := NewGlobalOptions()
	assert.NotNil(t, opts)
	assert.Equal(t, "default", opts.Namespace)
	assert.Empty(t, opts.KubeConfig)
	assert.Empty(t, opts.Context)
}

func TestAddFlags(t *testing.T) {
	opts := NewGlobalOptions()
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	opts.AddFlags(flags)

	// Verify flags are registered
	kubeConfigFlag := flags.Lookup("kubeconfig")
	assert.NotNil(t, kubeConfigFlag)
	assert.Equal(t, "", kubeConfigFlag.DefValue)

	nsFlag := flags.Lookup("namespace")
	assert.NotNil(t, nsFlag)
	assert.Equal(t, "n", nsFlag.Shorthand)
	assert.Equal(t, "default", nsFlag.DefValue)

	ctxFlag := flags.Lookup("context")
	assert.NotNil(t, ctxFlag)
	assert.Equal(t, "", ctxFlag.DefValue)

	// Parse flags and verify they set the options
	err := flags.Parse([]string{"--kubeconfig=/tmp/config", "-n", "my-ns", "--context=my-ctx"})
	assert.NoError(t, err)
	assert.Equal(t, "/tmp/config", opts.KubeConfig)
	assert.Equal(t, "my-ns", opts.Namespace)
	assert.Equal(t, "my-ctx", opts.Context)
}

func TestRESTConfig(t *testing.T) {
	tests := []struct {
		name            string
		kubeConfig      string
		context         string
		inClusterErr    error
		inClusterConfig *rest.Config
		expectError     string
	}{
		{
			name:            "in-cluster config succeeds",
			kubeConfig:      "",
			context:         "",
			inClusterConfig: &rest.Config{Host: "https://kubernetes.default.svc"},
			inClusterErr:    nil,
		},
		{
			name:         "in-cluster fails, kubeconfig not found",
			kubeConfig:   "/nonexistent/kubeconfig",
			context:      "",
			inClusterErr: fmt.Errorf("not in cluster"),
			expectError:  "failed to build kubeconfig",
		},
		{
			name:         "explicit context skips in-cluster, kubeconfig not found",
			kubeConfig:   "/nonexistent/kubeconfig",
			context:      "my-context",
			inClusterErr: nil,
			expectError:  "failed to build kubeconfig",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Override in-cluster config function
			origFn := inClusterConfigFn
			defer func() { inClusterConfigFn = origFn }()
			inClusterConfigFn = func() (*rest.Config, error) {
				if tt.inClusterErr != nil {
					return nil, tt.inClusterErr
				}
				return tt.inClusterConfig, nil
			}

			opts := &GlobalOptions{
				KubeConfig: tt.kubeConfig,
				Context:    tt.context,
			}

			cfg, err := opts.RESTConfig()

			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, cfg)
				if tt.inClusterConfig != nil {
					assert.Equal(t, tt.inClusterConfig.Host, cfg.Host)
				}
			}
		})
	}
}

func TestRESTConfigWithValidKubeconfig(t *testing.T) {
	// Create a minimal kubeconfig file for testing
	tmpDir := t.TempDir()
	kubeconfigPath := filepath.Join(tmpDir, "config")

	kubeconfigContent := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
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
	err := os.WriteFile(kubeconfigPath, []byte(kubeconfigContent), 0600)
	assert.NoError(t, err)

	// Override inClusterConfigFn to fail so it falls through to kubeconfig
	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	opts := &GlobalOptions{
		KubeConfig: kubeconfigPath,
	}

	cfg, err := opts.RESTConfig()
	assert.NoError(t, err)
	assert.NotNil(t, cfg)
	assert.Equal(t, "https://127.0.0.1:6443", cfg.Host)
}

func TestRESTConfigWithContext(t *testing.T) {
	// Create a kubeconfig with multiple contexts
	tmpDir := t.TempDir()
	kubeconfigPath := filepath.Join(tmpDir, "config")

	kubeconfigContent := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://cluster-a:6443
  name: cluster-a
- cluster:
    server: https://cluster-b:6443
  name: cluster-b
contexts:
- context:
    cluster: cluster-a
    user: user-a
  name: context-a
- context:
    cluster: cluster-b
    user: user-b
  name: context-b
current-context: context-a
users:
- name: user-a
  user:
    token: token-a
- name: user-b
  user:
    token: token-b
`
	err := os.WriteFile(kubeconfigPath, []byte(kubeconfigContent), 0600)
	assert.NoError(t, err)

	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	opts := &GlobalOptions{
		KubeConfig: kubeconfigPath,
		Context:    "context-b",
	}

	cfg, err := opts.RESTConfig()
	assert.NoError(t, err)
	assert.NotNil(t, cfg)
	assert.Equal(t, "https://cluster-b:6443", cfg.Host)
}

func TestAgentsClient(t *testing.T) {
	// Create a minimal kubeconfig
	tmpDir := t.TempDir()
	kubeconfigPath := filepath.Join(tmpDir, "config")
	kubeconfigContent := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
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
	err := os.WriteFile(kubeconfigPath, []byte(kubeconfigContent), 0600)
	assert.NoError(t, err)

	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	tests := []struct {
		name        string
		opts        *GlobalOptions
		expectError string
	}{
		{
			name:        "valid kubeconfig",
			opts:        &GlobalOptions{KubeConfig: kubeconfigPath},
			expectError: "",
		},
		{
			name:        "invalid kubeconfig path",
			opts:        &GlobalOptions{KubeConfig: "/nonexistent/path"},
			expectError: "failed to build kubeconfig",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := tt.opts.AgentsClient()
			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, client)
			}
		})
	}
}

func TestKubeClient(t *testing.T) {
	tmpDir := t.TempDir()
	kubeconfigPath := filepath.Join(tmpDir, "config")
	kubeconfigContent := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
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
	err := os.WriteFile(kubeconfigPath, []byte(kubeconfigContent), 0600)
	assert.NoError(t, err)

	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	tests := []struct {
		name        string
		opts        *GlobalOptions
		expectError string
	}{
		{
			name:        "valid kubeconfig",
			opts:        &GlobalOptions{KubeConfig: kubeconfigPath},
			expectError: "",
		},
		{
			name:        "invalid kubeconfig path",
			opts:        &GlobalOptions{KubeConfig: "/nonexistent/path"},
			expectError: "failed to build kubeconfig",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := tt.opts.KubeClient()
			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, client)
			}
		})
	}
}
