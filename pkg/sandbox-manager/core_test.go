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

package sandbox_manager

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
)

func TestNewSandboxManagerBuilder(t *testing.T) {
	tests := []struct {
		name                     string
		opts                     config.SandboxManagerOptions
		expectSystemNamespace    string
		expectMaxClaimWorkers    int
		expectExtProcConcurrency uint32
		expectMaxCreateQPS       int
		expectMemberlistBindPort int
	}{
		{
			name:                     "default options should be initialized",
			opts:                     config.SandboxManagerOptions{},
			expectSystemNamespace:    "sandbox-system",
			expectMaxClaimWorkers:    500,
			expectExtProcConcurrency: 1000,
			expectMaxCreateQPS:       49,
			expectMemberlistBindPort: config.DefaultMemberlistBindPort,
		},
		{
			name: "custom options should be preserved",
			opts: config.SandboxManagerOptions{
				SystemNamespace:       "custom-namespace",
				MaxClaimWorkers:       20,
				ExtProcMaxConcurrency: 200,
				MaxCreateQPS:          50,
				MemberlistBindPort:    8000,
			},
			expectSystemNamespace:    "custom-namespace",
			expectMaxClaimWorkers:    20,
			expectExtProcConcurrency: 200,
			expectMaxCreateQPS:       50,
			expectMemberlistBindPort: 8000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := NewSandboxManagerBuilder(tt.opts)

			assert.NotNil(t, builder, "builder should not be nil")
			assert.NotNil(t, builder.instance, "instance should not be nil")
			assert.NotNil(t, builder.instance.proxy, "proxy should not be nil")
			assert.Equal(t, tt.expectSystemNamespace, builder.opts.SystemNamespace)
			assert.Equal(t, tt.expectMaxClaimWorkers, builder.opts.MaxClaimWorkers)
			assert.Equal(t, tt.expectExtProcConcurrency, builder.opts.ExtProcMaxConcurrency)
			assert.Equal(t, tt.expectMaxCreateQPS, builder.opts.MaxCreateQPS)
			assert.Equal(t, tt.expectMemberlistBindPort, builder.opts.MemberlistBindPort)
		})
	}
}

func TestSandboxManagerBuilder_WithSandboxInfra(t *testing.T) {
	t.Run("should set buildInfraFunc when nil RestConfig", func(t *testing.T) {
		opts := config.SandboxManagerOptions{}

		builder := NewSandboxManagerBuilder(opts).
			WithSandboxInfra()

		assert.NotNil(t, builder.buildInfraFunc, "buildInfraFunc should be set")

		// Build should fail with nil RestConfig
		_, err := builder.Build()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to get infra builder")
	})

	t.Run("should support chaining", func(t *testing.T) {
		opts := config.SandboxManagerOptions{}

		builder := NewSandboxManagerBuilder(opts)
		result := builder.WithSandboxInfra()

		assert.Same(t, builder, result, "should return same builder instance for chaining")
	})
}

func TestSandboxManagerBuilder_WithCustomInfra(t *testing.T) {
	t.Run("should use custom infra builder", func(t *testing.T) {
		opts := config.SandboxManagerOptions{}
		customBuilderCalled := false

		builder := NewSandboxManagerBuilder(opts).
			WithCustomInfra(func() (infra.Builder, error) {
				customBuilderCalled = true
				cache, fc, err := cachetest.NewTestCache(t)
				if err != nil {
					return nil, err
				}
				proxyServer := proxy.NewServer(opts)
				return sandboxcr.NewInfraBuilder(opts).
					WithCache(cache).
					WithAPIReader(fc).
					WithProxy(proxyServer), nil
			})

		manager, err := builder.Build()
		require.NoError(t, err)
		assert.True(t, customBuilderCalled, "custom builder function should be called")
		assert.NotNil(t, manager)
		assert.NotNil(t, manager.infra)
	})

	t.Run("should support chaining", func(t *testing.T) {
		opts := config.SandboxManagerOptions{}

		builder := NewSandboxManagerBuilder(opts)
		result := builder.WithCustomInfra(func() (infra.Builder, error) {
			cache, fc, err := cachetest.NewTestCache(t)
			if err != nil {
				return nil, err
			}
			proxyServer := proxy.NewServer(opts)
			return sandboxcr.NewInfraBuilder(opts).
				WithCache(cache).
				WithAPIReader(fc).
				WithProxy(proxyServer), nil
		})

		assert.Same(t, builder, result, "should return same builder instance for chaining")
	})
}

func TestSandboxManagerBuilder_WithMemberlistPeers(t *testing.T) {
	tests := []struct {
		name             string
		hostname         string
		podName          string
		peerSelector     string
		expectError      string
		expectNodePrefix string
	}{
		{
			name:             "empty peer selector should return error",
			hostname:         "test-host",
			podName:          "",
			peerSelector:     "",
			expectError:      "peer selector is empty",
			expectNodePrefix: "",
		},
		{
			name:             "use HOSTNAME env var",
			hostname:         "test-host-123",
			podName:          "test-pod",
			peerSelector:     "app=sandbox-manager",
			expectError:      "",
			expectNodePrefix: peers.NodePrefixSandboxManager + "test-host-123",
		},
		{
			name:             "fallback to POD_NAME when HOSTNAME empty",
			hostname:         "",
			podName:          "test-pod-456",
			peerSelector:     "app=sandbox-manager",
			expectError:      "",
			expectNodePrefix: peers.NodePrefixSandboxManager + "test-pod-456",
		},
		{
			name:             "fallback to uuid when both HOSTNAME and POD_NAME empty",
			hostname:         "",
			podName:          "",
			peerSelector:     "app=sandbox-manager",
			expectError:      "",
			expectNodePrefix: peers.NodePrefixSandboxManager,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Setenv isolates env vars per test and restores them on cleanup.
			t.Setenv("HOSTNAME", tt.hostname)
			t.Setenv("POD_NAME", tt.podName)

			opts := config.SandboxManagerOptions{
				PeerSelector:    tt.peerSelector,
				SystemNamespace: "test-namespace",
			}

			cache, fc, err := cachetest.NewTestCache(t)
			require.NoError(t, err)

			builder := NewSandboxManagerBuilder(opts).
				WithCustomInfra(func() (infra.Builder, error) {
					proxyServer := proxy.NewServer(opts)
					return sandboxcr.NewInfraBuilder(opts).
						WithCache(cache).
						WithAPIReader(fc).
						WithProxy(proxyServer), nil
				}).
				WithMemberlistPeers()

			if tt.expectError != "" {
				_, err := builder.Build()
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Equal(t, errors.ErrorInternal, errors.GetErrCode(err))
			} else {
				// Build should succeed
				manager, err := builder.Build()
				require.NoError(t, err)
				assert.NotNil(t, manager)
				require.NotNil(t, manager.peersManager)

				// verify generated memberlist node name uses the expected prefix
				memberlistPeers, ok := manager.peersManager.(*peers.MemberlistPeers)
				require.True(t, ok, "peersManager should be a *MemberlistPeers")
				actualName := memberlistPeers.LocalName()
				assert.True(t, strings.HasPrefix(actualName, tt.expectNodePrefix),
					"memberlist node name %q should start with %q", actualName, tt.expectNodePrefix)
			}
		})
	}
}

func TestSandboxManagerBuilder_WithRequestAdapter(t *testing.T) {
	t.Run("should set request adapter", func(t *testing.T) {
		opts := config.SandboxManagerOptions{}

		// Create a mock request adapter
		mockAdapter := &mockRequestAdapter{entry: "test-entry"}

		builder := NewSandboxManagerBuilder(opts).
			WithRequestAdapter(mockAdapter)

		assert.Same(t, mockAdapter, builder.requestAdapter, "requestAdapter should be set")
	})

	t.Run("should support chaining", func(t *testing.T) {
		opts := config.SandboxManagerOptions{}
		mockAdapter := &mockRequestAdapter{entry: "test-entry"}

		builder := NewSandboxManagerBuilder(opts)
		result := builder.WithRequestAdapter(mockAdapter)

		assert.Same(t, builder, result, "should return same builder instance for chaining")
	})
}

func TestSandboxManagerBuilder_Build(t *testing.T) {
	t.Run("should build complete SandboxManager with all components", func(t *testing.T) {
		opts := config.SandboxManagerOptions{
			SystemNamespace:  "test-namespace",
			SandboxNamespace: "default",
		}

		cache, fc, err := cachetest.NewTestCache(t)
		require.NoError(t, err)

		mockAdapter := &mockRequestAdapter{entry: "test-entry"}

		builder := NewSandboxManagerBuilder(opts).
			WithCustomInfra(func() (infra.Builder, error) {
				proxyServer := proxy.NewServer(opts)
				return sandboxcr.NewInfraBuilder(opts).
					WithCache(cache).
					WithAPIReader(fc).
					WithProxy(proxyServer), nil
			}).
			WithRequestAdapter(mockAdapter)

		manager, err := builder.Build()
		require.NoError(t, err)
		assert.NotNil(t, manager)
		assert.NotNil(t, manager.infra)
		assert.NotNil(t, manager.proxy)
		assert.Nil(t, manager.peersManager, "peersManager should be nil when not configured")
	})

	t.Run("should build with peers manager", func(t *testing.T) {
		t.Setenv("HOSTNAME", "test-host")
		t.Setenv("POD_NAME", "")

		opts := config.SandboxManagerOptions{
			SystemNamespace:  "test-namespace",
			SandboxNamespace: "default",
			PeerSelector:     "app=test",
		}

		cache, fc, err := cachetest.NewTestCache(t)
		require.NoError(t, err)

		builder := NewSandboxManagerBuilder(opts).
			WithCustomInfra(func() (infra.Builder, error) {
				proxyServer := proxy.NewServer(opts)
				return sandboxcr.NewInfraBuilder(opts).
					WithCache(cache).
					WithAPIReader(fc).
					WithProxy(proxyServer), nil
			}).
			WithMemberlistPeers()

		manager, err := builder.Build()
		require.NoError(t, err)
		assert.NotNil(t, manager)
		assert.NotNil(t, manager.infra)
		assert.NotNil(t, manager.proxy)
		assert.NotNil(t, manager.peersManager, "peersManager should be set")
	})

	t.Run("should get APIReader from infra cache", func(t *testing.T) {
		opts := config.SandboxManagerOptions{
			SystemNamespace: "test-namespace",
		}

		cache, fc, err := cachetest.NewTestCache(t)
		require.NoError(t, err)

		builder := NewSandboxManagerBuilder(opts).
			WithCustomInfra(func() (infra.Builder, error) {
				proxyServer := proxy.NewServer(opts)
				return sandboxcr.NewInfraBuilder(opts).
					WithCache(cache).
					WithAPIReader(fc).
					WithProxy(proxyServer), nil
			})

		_, err = builder.Build()
		require.NoError(t, err)
		// The build process should successfully get APIReader from cache
	})

	t.Run("should return error when peers func fails", func(t *testing.T) {
		opts := config.SandboxManagerOptions{}

		cache, fc, err := cachetest.NewTestCache(t)
		require.NoError(t, err)

		builder := NewSandboxManagerBuilder(opts).
			WithCustomInfra(func() (infra.Builder, error) {
				proxyServer := proxy.NewServer(opts)
				return sandboxcr.NewInfraBuilder(opts).
					WithCache(cache).
					WithAPIReader(fc).
					WithProxy(proxyServer), nil
			}).
			WithCustomPeers(func(args NewPeerArgs) (peers.Peers, error) {
				return nil, assert.AnError
			})

		_, err = builder.Build()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to get peers")
		assert.Equal(t, errors.ErrorInternal, errors.GetErrCode(err))
	})

	t.Run("should support full builder chain", func(t *testing.T) {
		t.Setenv("HOSTNAME", "test-host-full")
		t.Setenv("POD_NAME", "")

		opts := config.SandboxManagerOptions{
			SystemNamespace:  "test-namespace",
			SandboxNamespace: "default",
			PeerSelector:     "app=test",
		}

		cache, fc, err := cachetest.NewTestCache(t)
		require.NoError(t, err)

		mockAdapter := &mockRequestAdapter{entry: "test-entry"}

		builder := NewSandboxManagerBuilder(opts).
			WithCustomInfra(func() (infra.Builder, error) {
				proxyServer := proxy.NewServer(opts)
				return sandboxcr.NewInfraBuilder(opts).
					WithCache(cache).
					WithAPIReader(fc).
					WithProxy(proxyServer), nil
			}).
			WithMemberlistPeers().
			WithRequestAdapter(mockAdapter)

		manager, err := builder.Build()
		require.NoError(t, err)
		assert.NotNil(t, manager)
		assert.NotNil(t, manager.infra)
		assert.NotNil(t, manager.proxy)
		assert.NotNil(t, manager.peersManager)
	})
}

func TestSandboxManagerBuilder_Chaining(t *testing.T) {
	t.Run("all builder methods should support chaining", func(t *testing.T) {
		opts := config.SandboxManagerOptions{
			SystemNamespace: "test-namespace",
			PeerSelector:    "app=test",
		}

		t.Setenv("HOSTNAME", "test-host-chain")
		t.Setenv("POD_NAME", "")

		cache, fc, err := cachetest.NewTestCache(t)
		require.NoError(t, err)

		mockAdapter := &mockRequestAdapter{entry: "test-entry"}

		// Chain all methods
		builder := NewSandboxManagerBuilder(opts)

		result1 := builder.WithCustomInfra(func() (infra.Builder, error) {
			proxyServer := proxy.NewServer(opts)
			return sandboxcr.NewInfraBuilder(opts).
				WithCache(cache).
				WithAPIReader(fc).
				WithProxy(proxyServer), nil
		})
		assert.Same(t, builder, result1)

		result2 := builder.WithMemberlistPeers()
		assert.Same(t, builder, result2)

		result3 := builder.WithRequestAdapter(mockAdapter)
		assert.Same(t, builder, result3)

		// Should be able to build
		manager, err := builder.Build()
		require.NoError(t, err)
		assert.NotNil(t, manager)
	})
}

// mockRequestAdapter is a mock implementation of proxy.RequestAdapter for testing
type mockRequestAdapter struct {
	entry string
}

func (m *mockRequestAdapter) Entry() string {
	return m.entry
}

func (m *mockRequestAdapter) Map(string, string, string, int, map[string]string) (sandboxID string, sandboxPort int, extraHeaders map[string]string, err error) {
	return "", 0, nil, nil
}

func (m *mockRequestAdapter) IsSandboxRequest(string, string, int) bool {
	return false
}

// WithCustomPeers is a helper method for testing custom peers function
func (b *SandboxManagerBuilder) WithCustomPeers(getPeersFunc GetPeersFunc) *SandboxManagerBuilder {
	b.getPeersFunc = getPeersFunc
	return b
}
