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

package identity

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	agentsruntime "github.com/openkruise/agents/pkg/utils/runtime"
)

func TestRegisterSecurityTokenPropagator(t *testing.T) {
	// Save and restore the global slice.
	saved := securityTokenPropagators
	defer func() { securityTokenPropagators = saved }()
	securityTokenPropagators = nil

	assert.Equal(t, 0, SecurityTokenPropagatorCount())

	// Register first propagator.
	RegisterSecurityTokenPropagator(func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse,
		_ ...agentsruntime.Option) error {
		return nil
	})
	assert.Equal(t, 1, SecurityTokenPropagatorCount())

	// Register second propagator.
	RegisterSecurityTokenPropagator(func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse,
		_ ...agentsruntime.Option) error {
		return fmt.Errorf("err")
	})
	assert.Equal(t, 2, SecurityTokenPropagatorCount())
}

func TestSecurityTokenPropagatorCount_Empty(t *testing.T) {
	saved := securityTokenPropagators
	defer func() { securityTokenPropagators = saved }()
	securityTokenPropagators = nil

	assert.Equal(t, 0, SecurityTokenPropagatorCount())
}

func TestRegisterSecurityTokenCleaner(t *testing.T) {
	// Save and restore the global slice.
	saved := securityTokenCleaners
	defer func() { securityTokenCleaners = saved }()
	securityTokenCleaners = nil

	assert.Equal(t, 0, SecurityTokenCleanerCount())

	RegisterSecurityTokenCleaner(func(_ context.Context, _ *agentsv1alpha1.Sandbox,
		_ ...agentsruntime.Option) error {
		return nil
	})
	assert.Equal(t, 1, SecurityTokenCleanerCount())

	RegisterSecurityTokenCleaner(func(_ context.Context, _ *agentsv1alpha1.Sandbox,
		_ ...agentsruntime.Option) error {
		return fmt.Errorf("err")
	})
	assert.Equal(t, 2, SecurityTokenCleanerCount())
}

func TestCleanupSecurityToken(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{}

	tests := []struct {
		name string
		// cleaners is installed as the whole registry for the case.
		cleaners []SecurityTokenCleaner
		// wantCalls is how many of them must have run.
		wantCalls   int
		wantErrs    []string
		wantNoError bool
	}{
		{
			name:        "no cleaners registered is a no-op",
			cleaners:    nil,
			wantCalls:   0,
			wantNoError: true,
		},
		{
			name: "the single registered cleaner runs",
			cleaners: []SecurityTokenCleaner{
				func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ ...agentsruntime.Option) error {
					return nil
				},
			},
			wantCalls:   1,
			wantNoError: true,
		},
		{
			// One cleaner owning a broken credential must not deny the others
			// their turn, so every failure has to surface together.
			name: "a failing cleaner does not stop the rest, and both errors surface",
			cleaners: []SecurityTokenCleaner{
				func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ ...agentsruntime.Option) error {
					return fmt.Errorf("first failed")
				},
				func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ ...agentsruntime.Option) error {
					return nil
				},
				func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ ...agentsruntime.Option) error {
					return fmt.Errorf("third failed")
				},
			},
			wantCalls: 3,
			wantErrs:  []string{"first failed", "third failed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := securityTokenCleaners
			defer func() { securityTokenCleaners = saved }()

			calls := 0
			securityTokenCleaners = nil
			for _, cleaner := range tt.cleaners {
				securityTokenCleaners = append(securityTokenCleaners,
					func(ctx context.Context, s *agentsv1alpha1.Sandbox, opts ...agentsruntime.Option) error {
						calls++
						return cleaner(ctx, s, opts...)
					})
			}

			err := CleanupSecurityToken(context.Background(), sbx)

			assert.Equal(t, tt.wantCalls, calls)
			if tt.wantNoError {
				assert.NoError(t, err)
				return
			}
			assert.Error(t, err)
			for _, want := range tt.wantErrs {
				assert.ErrorContains(t, err, want)
			}
		})
	}
}

func TestCleanupSecurityTokenForwardsTransport(t *testing.T) {
	saved := securityTokenCleaners
	defer func() { securityTokenCleaners = saved }()

	var got []agentsruntime.Option
	securityTokenCleaners = []SecurityTokenCleaner{
		func(_ context.Context, _ *agentsv1alpha1.Sandbox, opts ...agentsruntime.Option) error {
			got = opts
			return nil
		},
	}

	// The transport the caller resolved has to reach the cleaner unchanged, so a
	// removal rides the same connection its propagator's write did.
	want := []agentsruntime.Option{
		agentsruntime.WithAuthority("sandbox.example"),
		agentsruntime.WithTLSPort(8443),
	}
	assert.NoError(t, CleanupSecurityToken(context.Background(), &agentsv1alpha1.Sandbox{}, want...))
	assert.Len(t, got, len(want))
}
