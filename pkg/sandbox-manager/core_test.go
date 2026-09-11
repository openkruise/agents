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
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/cache/controllers"
	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/sandboxroute/refresh"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
	"github.com/openkruise/agents/pkg/utils/network"
)

type routeSourceOverrideBuilder struct {
	base   infra.Builder
	source infra.SandboxRouteSource
}

func (b routeSourceOverrideBuilder) Build() infra.Infrastructure {
	return routeSourceOverrideInfra{Infrastructure: b.base.Build(), source: b.source}
}

type routeSourceOverrideInfra struct {
	infra.Infrastructure
	source infra.SandboxRouteSource
}

func (i routeSourceOverrideInfra) GetSandboxRouteSource() infra.SandboxRouteSource {
	return i.source
}

func (i routeSourceOverrideInfra) GetQuotaSandboxSource() infra.QuotaSandboxSource {
	provider, ok := i.Infrastructure.(infra.QuotaSandboxSourceProvider)
	if !ok {
		return nil
	}
	return provider.GetQuotaSandboxSource()
}

type failingSandboxRouteSource struct {
	err error
}

// recordingInfra is an Infrastructure whose Run succeeds immediately, records
// "infra" into events when set, and serves cache as its cache provider.
type recordingInfra struct {
	infra.Infrastructure
	cache  infracache.Provider
	events *[]string
}

func (i recordingInfra) Run(context.Context) error {
	if i.events != nil {
		*i.events = append(*i.events, "infra")
	}
	return nil
}

func (i recordingInfra) GetCache() infracache.Provider {
	return i.cache
}

type workerAllocationFailureCache struct {
	infracache.Provider
	reader ctrlclient.Reader
}

func (c workerAllocationFailureCache) GetAPIReader() ctrlclient.Reader {
	return c.reader
}

func (s failingSandboxRouteSource) Subscribe(
	context.Context,
	infra.SandboxRouteEventHandler,
) error {
	return s.err
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

// withTestInfra returns the default WithCustomInfra builder func: test cache,
// fake client as API reader, and a proxy route reader. Tests needing special
// caches or route sources keep constructing their infra explicitly.
func withTestInfra(t *testing.T, opts config.SandboxManagerOptions) func() (infra.Builder, error) {
	t.Helper()
	return func() (infra.Builder, error) {
		cache, fc, err := cachetest.NewTestCache(t)
		if err != nil {
			return nil, err
		}
		proxyServer := proxy.NewServer(opts)
		return sandboxcr.NewInfraBuilder(opts).
			WithCache(cache).
			WithAPIReader(fc).
			WithRouteReader(proxyServer), nil
	}
}

func TestSandboxManagerRunFailsWhenWorkerAllocationFails(t *testing.T) {
	cache, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	watchClient, ok := fc.(ctrlclient.WithWatch)
	require.True(t, ok)
	failingReader := interceptor.NewClient(watchClient, interceptor.Funcs{
		Get: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectKey, ctrlclient.Object, ...ctrlclient.GetOption) error {
			return assert.AnError
		},
	})
	manager := &SandboxManager{
		infra: recordingInfra{cache: workerAllocationFailureCache{
			Provider: cache,
			reader:   failingReader,
		}},
		systemNamespace: "sandbox-system",
		enableShortID:   true,
		shortIDPrefix:   "prod-",
		primary:         &primaryState{},
	}

	err = manager.Run(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, assert.AnError)
	assert.Nil(t, manager.generateSandboxID)
}

// TestSandboxManagerRunStartupOrder pins infra -> proxy -> peers: the peer
// route listener must be serving before memberlist can advertise this
// replica, and a proxy bind failure must stop startup before peers start.
// The proxy binds the fixed route-refresh port; `make test` runs packages
// serially so this cannot collide with the proxy and e2b package tests.
func TestSandboxManagerRunStartupOrder(t *testing.T) {
	routeAddr := network.ListenAddress("", refresh.DefaultPort)
	tests := []struct {
		name            string
		occupyRoutePort bool
		wantErr         string
		wantEvents      []string
		wantBindAddress string
	}{
		{
			name:            "proxy bind failure stops startup before peers",
			occupyRoutePort: true,
			wantErr:         "failed to start proxy",
			wantEvents:      []string{"infra"},
		},
		{
			name:            "peers start after infra and proxy with the bind address",
			wantErr:         assert.AnError.Error(),
			wantEvents:      []string{"infra", "peers"},
			wantBindAddress: "10.0.0.8",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.occupyRoutePort {
				occupied, err := net.Listen("tcp", routeAddr)
				require.NoError(t, err)
				t.Cleanup(func() { _ = occupied.Close() })
			}
			events := []string{}
			recorded := &staticPeers{events: &events, startErr: assert.AnError}
			proxyServer := proxy.NewServer(config.SandboxManagerOptions{DisableEnvoyExtProc: true})
			t.Cleanup(func() { proxyServer.Stop(context.Background()) })
			manager := &SandboxManager{
				infra:              recordingInfra{events: &events},
				proxy:              proxyServer,
				peersManager:       recorded,
				primary:            &primaryState{},
				bindAddress:        "10.0.0.8",
				memberlistBindPort: 9000,
			}

			err := manager.Run(t.Context())
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.wantErr)
			assert.Equal(t, tt.wantEvents, events)
			assert.Equal(t, tt.wantBindAddress, recorded.bindAddress)
			if tt.wantBindAddress != "" {
				assert.Equal(t, 9000, recorded.bindPort)
				// Run returned from peers.Start, so a serving route listener
				// proves the proxy came up before peers.
				conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(refresh.DefaultPort)))
				require.NoError(t, err, "peer route listener must serve before peers start")
				require.NoError(t, conn.Close())
			}
		})
	}
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
		expectBindAddress        string
		expectShortIDPrefix      string
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
				BindAddress:           "10.0.0.8",
				MaxClaimWorkers:       20,
				ExtProcMaxConcurrency: 200,
				MaxCreateQPS:          50,
				MemberlistBindPort:    8000,
				ShortSandboxIDPrefix:  "prod-",
			},
			expectSystemNamespace:    "custom-namespace",
			expectBindAddress:        "10.0.0.8",
			expectMaxClaimWorkers:    20,
			expectExtProcConcurrency: 200,
			expectMaxCreateQPS:       50,
			expectMemberlistBindPort: 8000,
			expectShortIDPrefix:      "prod-",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := NewSandboxManagerBuilder(tt.opts)

			assert.NotNil(t, builder, "builder should not be nil")
			assert.NotNil(t, builder.instance, "instance should not be nil")
			assert.NotNil(t, builder.instance.proxy, "proxy should not be nil")
			assert.Equal(t, tt.expectSystemNamespace, builder.opts.SystemNamespace)
			assert.Equal(t, tt.expectSystemNamespace, builder.instance.systemNamespace)
			assert.Equal(t, tt.expectMaxClaimWorkers, builder.opts.MaxClaimWorkers)
			assert.Equal(t, tt.expectExtProcConcurrency, builder.opts.ExtProcMaxConcurrency)
			assert.Equal(t, tt.expectMaxCreateQPS, builder.opts.MaxCreateQPS)
			assert.Equal(t, tt.expectMemberlistBindPort, builder.opts.MemberlistBindPort)
			assert.Equal(t, tt.expectMemberlistBindPort, builder.instance.memberlistBindPort)
			assert.Equal(t, tt.expectBindAddress, builder.instance.bindAddress)
			assert.Equal(t, tt.expectShortIDPrefix, builder.instance.shortIDPrefix)
		})
	}
}

func TestSandboxManagerBuilderValidatesShortIDPrefixBeforeInfra(t *testing.T) {
	tests := []struct {
		name        string
		prefix      string
		expectError string
	}{
		{name: "invalid characters are rejected", prefix: "INVALID_", expectError: "short sandbox id prefix"},
		{name: "legacy separator is rejected", prefix: "prod--x", expectError: "legacy ID separator"},
		{name: "50-character prefix passes length validation", prefix: strings.Repeat("a", 50), expectError: "infra builder is not configured"},
		{name: "51-character prefix is rejected", prefix: strings.Repeat("a", 51), expectError: "too long"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSandboxManagerBuilder(config.SandboxManagerOptions{
				ShortSandboxIDPrefix: tt.prefix,
			}).Build()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
		})
	}
}

func TestSandboxManagerBuilder_WithSandboxInfra(t *testing.T) {
	builder := NewSandboxManagerBuilder(config.SandboxManagerOptions{}).
		WithSandboxInfra()

	assert.NotNil(t, builder.buildInfraFunc, "buildInfraFunc should be set")

	// Build should fail with nil RestConfig
	_, err := builder.Build()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get infra builder")
}

func TestSandboxManagerBuilder_WithCustomInfra(t *testing.T) {
	opts := config.SandboxManagerOptions{}
	customBuilderCalled := false
	buildInfra := withTestInfra(t, opts)

	builder := NewSandboxManagerBuilder(opts).
		WithCustomInfra(func() (infra.Builder, error) {
			customBuilderCalled = true
			return buildInfra()
		})

	manager, err := builder.Build()
	require.NoError(t, err)
	assert.True(t, customBuilderCalled, "custom builder function should be called")
	assert.NotNil(t, manager)
	assert.NotNil(t, manager.infra)
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
		{
			name:         "missing rest config should return error",
			hostname:     "test-host",
			peerSelector: "app=sandbox-manager",
			expectError:  "rest config is required for peer discovery",
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
			if tt.expectError == "" {
				opts.RestConfig = &rest.Config{Host: "https://127.0.0.1:1"}
			}

			builder := NewSandboxManagerBuilder(opts).
				WithCustomInfra(withTestInfra(t, opts)).
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
	adapter := adapters.NewE2BAdapter(0, "")
	builder := NewSandboxManagerBuilder(config.SandboxManagerOptions{}).
		WithRequestAdapter(adapter)
	assert.Same(t, adapter, builder.requestAdapter, "requestAdapter should be set")
}

func TestSandboxManagerBuilder_Build(t *testing.T) {
	t.Run("should build complete SandboxManager with all components", func(t *testing.T) {
		opts := config.SandboxManagerOptions{
			SystemNamespace:  "test-namespace",
			SandboxNamespace: "default",
		}

		builder := NewSandboxManagerBuilder(opts).
			WithCustomInfra(withTestInfra(t, opts)).
			WithRequestAdapter(adapters.NewE2BAdapter(0, ""))

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
			RestConfig:       &rest.Config{Host: "https://127.0.0.1:1"},
		}

		builder := NewSandboxManagerBuilder(opts).
			WithCustomInfra(withTestInfra(t, opts)).
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

		builder := NewSandboxManagerBuilder(opts).
			WithCustomInfra(withTestInfra(t, opts))

		_, err := builder.Build()
		require.NoError(t, err)
		// Infra may still use this reader for non-route operations; route setup does not.
	})

	t.Run("should require sandbox route source", func(t *testing.T) {
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
		assert.Contains(t, err.Error(), "sandbox route source is not configured")
	})

	t.Run("should defer route feeder registration until run", func(t *testing.T) {
		opts := config.InitOptions(config.SandboxManagerOptions{})
		managerCache, apiReader, err := cachetest.NewTestCache(t)
		require.NoError(t, err)
		registerErr := stderrors.New("register failed")

		manager, err := NewSandboxManagerBuilder(opts).
			WithCustomInfra(func() (infra.Builder, error) {
				base := sandboxcr.NewInfraBuilder(opts).WithCache(managerCache).WithAPIReader(apiReader)
				return routeSourceOverrideBuilder{base: base, source: failingSandboxRouteSource{err: registerErr}}, nil
			}).
			Build()
		require.NoError(t, err)
		err = manager.routeSource.Subscribe(t.Context(), manager.handleSandboxRouteEvent)
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

		builder := NewSandboxManagerBuilder(opts).
			WithCustomInfra(withTestInfra(t, opts)).
			WithCustomPeers(func() (peers.Peers, error) {
				return nil, assert.AnError
			})

		_, err := builder.Build()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to get peers")
		assert.Equal(t, errors.ErrorInternal, errors.GetErrCode(err))
	})
}

func TestSandboxManagerBuilder_Chaining(t *testing.T) {
	opts := config.SandboxManagerOptions{
		SystemNamespace: "test-namespace",
		PeerSelector:    "app=test",
		RestConfig:      &rest.Config{Host: "https://127.0.0.1:1"},
	}

	t.Setenv("HOSTNAME", "test-host-chain")
	t.Setenv("POD_NAME", "")

	builder := NewSandboxManagerBuilder(opts)

	assert.Same(t, builder, builder.WithSandboxInfra())
	assert.Same(t, builder, builder.WithCustomInfra(withTestInfra(t, opts)))
	assert.Same(t, builder, builder.WithMemberlistPeers())
	assert.Same(t, builder, builder.WithRequestAdapter(adapters.NewE2BAdapter(0, "")))

	// The fully chained builder must still build.
	manager, err := builder.Build()
	require.NoError(t, err)
	assert.NotNil(t, manager)
}

// WithCustomPeers is a helper method for testing custom peers function
func (b *SandboxManagerBuilder) WithCustomPeers(getPeersFunc GetPeersFunc) *SandboxManagerBuilder {
	b.getPeersFunc = getPeersFunc
	return b
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
