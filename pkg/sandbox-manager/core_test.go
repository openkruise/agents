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
	"context"
	stderrors "errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/cache/controllers"
	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
)

type routeSourceOverrideBuilder struct {
	base   infra.Builder
	source infra.RouteSandboxSource
}

func (b routeSourceOverrideBuilder) Build() infra.Infrastructure {
	return routeSourceOverrideInfra{Infrastructure: b.base.Build(), source: b.source}
}

type routeSourceOverrideInfra struct {
	infra.Infrastructure
	source infra.RouteSandboxSource
}

func (i routeSourceOverrideInfra) GetRouteSandboxSource() infra.RouteSandboxSource {
	return i.source
}

func (i routeSourceOverrideInfra) GetQuotaSandboxSource() infra.QuotaSandboxSource {
	provider, ok := i.Infrastructure.(infra.QuotaSandboxSourceProvider)
	if !ok {
		return nil
	}
	return provider.GetQuotaSandboxSource()
}

type failingRouteSandboxSource struct {
	err error
}

func (s failingRouteSandboxSource) Subscribe(
	context.Context,
	infra.RouteSandboxEventHandler,
) (infra.RouteSandboxSubscription, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &managerRouteSubscription{}, nil
}

type panicAPIReaderCache struct {
	infracache.Provider
}

func (panicAPIReaderCache) GetAPIReader() ctrlclient.Reader {
	panic("manager route setup must not read the cache API reader")
}

func (c panicAPIReaderCache) GetSandboxController() *controllers.CacheSandboxCustomReconciler {
	return c.Provider.(*infracache.Cache).GetSandboxController()
}

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
					WithRouteVersionReader(proxyServer), nil
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
				WithRouteVersionReader(proxyServer), nil
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
						WithRouteVersionReader(proxyServer), nil
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

		adapter := adapters.NewE2BAdapter(0)

		builder := NewSandboxManagerBuilder(opts).
			WithRequestAdapter(adapter)

		assert.Same(t, adapter, builder.requestAdapter, "requestAdapter should be set")
	})

	t.Run("should support chaining", func(t *testing.T) {
		opts := config.SandboxManagerOptions{}
		adapter := adapters.NewE2BAdapter(0)

		builder := NewSandboxManagerBuilder(opts)
		result := builder.WithRequestAdapter(adapter)

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

		adapter := adapters.NewE2BAdapter(0)

		builder := NewSandboxManagerBuilder(opts).
			WithCustomInfra(func() (infra.Builder, error) {
				proxyServer := proxy.NewServer(opts)
				return sandboxcr.NewInfraBuilder(opts).
					WithCache(cache).
					WithAPIReader(fc).
					WithRouteVersionReader(proxyServer), nil
			}).
			WithRequestAdapter(adapter)

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
					WithRouteVersionReader(proxyServer), nil
			}).
			WithMemberlistPeers()

		manager, err := builder.Build()
		require.NoError(t, err)
		assert.NotNil(t, manager)
		assert.NotNil(t, manager.infra)
		assert.NotNil(t, manager.proxy)
		assert.NotNil(t, manager.peersManager, "peersManager should be set")
	})

	t.Run("should build with configured infra APIReader", func(t *testing.T) {
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
					WithRouteVersionReader(proxyServer), nil
			})

		_, err = builder.Build()
		require.NoError(t, err)
		// Infra may still use this reader for non-route operations; route setup does not.
	})

	t.Run("should require route sandbox source", func(t *testing.T) {
		opts := config.InitOptions(config.SandboxManagerOptions{})
		managerCache, apiReader, err := cachetest.NewTestCache(t)
		require.NoError(t, err)

		_, err = NewSandboxManagerBuilder(opts).
			WithCustomInfra(func() (infra.Builder, error) {
				base := sandboxcr.NewInfraBuilder(opts).WithCache(managerCache).WithAPIReader(apiReader)
				return routeSourceOverrideBuilder{base: base}, nil
			}).
			Build()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "route sandbox source is not configured")
	})

	t.Run("should defer route feeder registration until run", func(t *testing.T) {
		opts := config.InitOptions(config.SandboxManagerOptions{})
		managerCache, apiReader, err := cachetest.NewTestCache(t)
		require.NoError(t, err)
		registerErr := stderrors.New("register failed")

		manager, err := NewSandboxManagerBuilder(opts).
			WithCustomInfra(func() (infra.Builder, error) {
				base := sandboxcr.NewInfraBuilder(opts).WithCache(managerCache).WithAPIReader(apiReader)
				return routeSourceOverrideBuilder{base: base, source: failingRouteSandboxSource{err: registerErr}}, nil
			}).
			Build()
		require.NoError(t, err)
		_, err = manager.routeSource.Subscribe(t.Context(), manager.handleRouteSandboxEvent)
		require.ErrorIs(t, err, registerErr)
	})

	t.Run("route setup should not access cache API reader", func(t *testing.T) {
		opts := config.InitOptions(config.SandboxManagerOptions{})
		managerCache, apiReader, err := cachetest.NewTestCache(t)
		require.NoError(t, err)

		manager, err := NewSandboxManagerBuilder(opts).
			WithCustomInfra(func() (infra.Builder, error) {
				return sandboxcr.NewInfraBuilder(opts).
					WithCache(panicAPIReaderCache{Provider: managerCache}).
					WithAPIReader(apiReader), nil
			}).
			Build()
		require.NoError(t, err)
		assert.NotNil(t, manager.routeSource)
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
					WithRouteVersionReader(proxyServer), nil
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

		adapter := adapters.NewE2BAdapter(0)

		builder := NewSandboxManagerBuilder(opts).
			WithCustomInfra(func() (infra.Builder, error) {
				proxyServer := proxy.NewServer(opts)
				return sandboxcr.NewInfraBuilder(opts).
					WithCache(cache).
					WithAPIReader(fc).
					WithRouteVersionReader(proxyServer), nil
			}).
			WithMemberlistPeers().
			WithRequestAdapter(adapter)

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

		adapter := adapters.NewE2BAdapter(0)

		// Chain all methods
		builder := NewSandboxManagerBuilder(opts)

		result1 := builder.WithCustomInfra(func() (infra.Builder, error) {
			proxyServer := proxy.NewServer(opts)
			return sandboxcr.NewInfraBuilder(opts).
				WithCache(cache).
				WithAPIReader(fc).
				WithRouteVersionReader(proxyServer), nil
		})
		assert.Same(t, builder, result1)

		result2 := builder.WithMemberlistPeers()
		assert.Same(t, builder, result2)

		result3 := builder.WithRequestAdapter(adapter)
		assert.Same(t, builder, result3)

		// Should be able to build
		manager, err := builder.Build()
		require.NoError(t, err)
		assert.NotNil(t, manager)
	})
}

// WithCustomPeers is a helper method for testing custom peers function
func (b *SandboxManagerBuilder) WithCustomPeers(getPeersFunc GetPeersFunc) *SandboxManagerBuilder {
	b.getPeersFunc = getPeersFunc
	return b
}

func TestInitOptionsQuotaDefaults(t *testing.T) {
	opts := config.InitOptions(config.SandboxManagerOptions{})
	assert.Equal(t, consts.DefaultQuotaRedisOperationTimeout, opts.Quota.OperationTimeout)
	assert.Equal(t, consts.DefaultQuotaRedisBreakerN, opts.Quota.BreakerN)
	assert.Equal(t, consts.DefaultQuotaRedisBreakerD, opts.Quota.BreakerD)
	assert.Equal(t, consts.DefaultQuotaAntiDriftInterval, opts.Quota.AntiDriftInterval)
	assert.Equal(t, consts.DefaultQuotaAntiDriftGrace, opts.Quota.AntiDriftGrace)
}

func TestCleanupQuotaNilSafe(t *testing.T) {
	// nil manager
	var m *SandboxManager
	require.NoError(t, m.CleanupQuota(t.Context(), "user-1"))

	// nil quota
	m2 := &SandboxManager{}
	require.NoError(t, m2.CleanupQuota(t.Context(), "user-1"))

	// empty user
	m3 := &SandboxManager{}
	require.NoError(t, m3.CleanupQuota(t.Context(), ""))
}
