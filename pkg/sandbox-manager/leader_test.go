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

package sandbox_manager

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
)

func TestPrimaryState(t *testing.T) {
	t.Run("state transitions", func(t *testing.T) {
		state := &primaryState{}
		assert.False(t, state.IsPrimary())

		state.set(true)
		assert.True(t, state.IsPrimary())

		state.set(false)
		assert.False(t, state.IsPrimary())
	})

	t.Run("nil state defaults to primary", func(t *testing.T) {
		var state *primaryState
		assert.True(t, state.IsPrimary())
	})
}

func TestSandboxManagerIsPrimary(t *testing.T) {
	tests := []struct {
		name    string
		manager *SandboxManager
		expect  bool
	}{
		{
			name:    "nil manager defaults to primary",
			manager: nil,
			expect:  true,
		},
		{
			name:    "nil primary state defaults to primary",
			manager: &SandboxManager{},
			expect:  true,
		},
		{
			name:    "unset state is not primary",
			manager: &SandboxManager{primary: &primaryState{}},
			expect:  false,
		},
		{
			name:    "true state is primary",
			manager: func() *SandboxManager { s := &primaryState{}; s.set(true); return &SandboxManager{primary: s} }(),
			expect:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expect, tt.manager.IsPrimary())
		})
	}
}

func TestResolvePrimaryIdentity(t *testing.T) {
	tests := []struct {
		name         string
		hostname     string
		podName      string
		expectPrefix string
	}{
		{
			name:         "prefers hostname",
			hostname:     "manager-0",
			podName:      "pod-0",
			expectPrefix: "manager-0",
		},
		{
			name:         "falls back to pod name",
			hostname:     "",
			podName:      "pod-1",
			expectPrefix: "pod-1",
		},
		{
			name:         "falls back to generated suffix",
			hostname:     "",
			podName:      "",
			expectPrefix: "sandbox-manager-",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOSTNAME", tt.hostname)
			t.Setenv("POD_NAME", tt.podName)

			identity := resolvePrimaryIdentity()
			assert.True(t, strings.HasPrefix(identity, tt.expectPrefix), "identity %q should start with %q", identity, tt.expectPrefix)
		})
	}
}

func TestNewSandboxManagerBuilderPrimaryDefaults(t *testing.T) {
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

	require.NotNil(t, builder.instance.primary)

	manager, err := builder.Build()
	require.NoError(t, err)
	require.NotNil(t, manager.primary)
	assert.Nil(t, manager.elector)
	assert.True(t, manager.IsPrimary())
}
