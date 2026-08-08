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
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
	utilruntime "github.com/openkruise/agents/pkg/utils/runtime"
)

type trafficTokenTestProvider struct {
	calls atomic.Int32
	err   error
}

func (p *trafficTokenTestProvider) IssueToken(_ context.Context, _ *v1alpha1.Sandbox, opts identity.TokenOptions) (*identity.TokenResponse, error) {
	p.calls.Add(1)
	if p.err != nil {
		return nil, p.err
	}
	return &identity.TokenResponse{
		AccessToken:           "refreshed-token",
		AccessTokenExpiration: time.Now().Add(opts.RequestedValidity).UTC().Format(time.RFC3339),
	}, nil
}

func (*trafficTokenTestProvider) PropagateSecurityToken(context.Context, *v1alpha1.Sandbox, *identity.TokenResponse, ...utilruntime.Option) error {
	return nil
}

type blockingTrafficTokenTestProvider struct {
	calls   atomic.Int32
	started chan struct{}
	unblock chan struct{}
}

func (p *blockingTrafficTokenTestProvider) IssueToken(ctx context.Context, _ *v1alpha1.Sandbox, opts identity.TokenOptions) (*identity.TokenResponse, error) {
	if p.calls.Add(1) == 1 {
		close(p.started)
		select {
		case <-p.unblock:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &identity.TokenResponse{
		AccessToken:           "coalesced-token",
		AccessTokenExpiration: time.Now().Add(opts.RequestedValidity).UTC().Format(time.RFC3339),
	}, nil
}

func (*blockingTrafficTokenTestProvider) PropagateSecurityToken(context.Context, *v1alpha1.Sandbox, *identity.TokenResponse, ...utilruntime.Option) error {
	return nil
}

type parallelTrafficTokenTestProvider struct {
	calls   atomic.Int32
	started chan struct{}
	unblock chan struct{}
}

func (p *parallelTrafficTokenTestProvider) IssueToken(ctx context.Context, _ *v1alpha1.Sandbox, opts identity.TokenOptions) (*identity.TokenResponse, error) {
	p.calls.Add(1)
	p.started <- struct{}{}
	select {
	case <-p.unblock:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &identity.TokenResponse{
		AccessToken:           "parallel-token",
		AccessTokenExpiration: time.Now().Add(opts.RequestedValidity).UTC().Format(time.RFC3339),
	}, nil
}

func (*parallelTrafficTokenTestProvider) PropagateSecurityToken(context.Context, *v1alpha1.Sandbox, *identity.TokenResponse, ...utilruntime.Option) error {
	return nil
}

func TestSandboxManagerRefreshTrafficAccessToken(t *testing.T) {
	providerErr := errors.New("issuer unavailable")
	tests := []struct {
		name          string
		mutate        func(*v1alpha1.Sandbox)
		user          string
		providerError error
		expectCode    managererrors.ErrorCode
		expectCalls   int32
	}{
		{name: "running sandbox", expectCalls: 1},
		{name: "paused sandbox", mutate: func(s *v1alpha1.Sandbox) { s.Status.Phase = v1alpha1.SandboxPaused }, expectCalls: 1},
		{name: "owner mismatch", user: "other", expectCode: managererrors.ErrorNotAllowed},
		{name: "JWT disabled", mutate: func(s *v1alpha1.Sandbox) { delete(s.Annotations, identity.AnnotationEnableJwtAuth) }, expectCode: managererrors.ErrorConflict},
		{name: "unsupported state", mutate: func(s *v1alpha1.Sandbox) { s.Status.Phase = v1alpha1.SandboxFailed }, expectCode: managererrors.ErrorConflict},
		{name: "provider unavailable", providerError: providerErr, expectCode: managererrors.ErrorUnavailable, expectCalls: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, client := setupTestManager(t, config.SandboxManagerOptions{DisableRouteReconciliation: true})
			manager.trafficTokenOptions = identity.TokenOptions{RequestedValidity: time.Hour}
			manager.trafficTokenLimiter = newTrafficTokenLimiter(time.Minute, time.Now)
			sandbox := getSandboxForApiTest(tt.name)
			sandbox.UID = types.UID("uid-" + tt.name)
			sandbox.Annotations[identity.AnnotationEnableJwtAuth] = v1alpha1.True
			sandbox.Status.Conditions = []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}
			if tt.mutate != nil {
				tt.mutate(sandbox)
			}
			CreateSandboxWithStatus(t, client, sandbox)
			sandboxID := utils.GetSandboxID(sandbox)
			require.Eventually(t, func() bool {
				_, err := manager.infra.GetSandbox(t.Context(), infra.GetSandboxOptions{SandboxID: sandboxID})
				return err == nil
			}, time.Second, 10*time.Millisecond)

			provider := &trafficTokenTestProvider{err: tt.providerError}
			identity.RegisterProvider(provider)
			t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })
			user := tt.user
			if user == "" {
				user = testUser
			}
			result, err := manager.RefreshTrafficAccessToken(t.Context(), RefreshTrafficAccessTokenOptions{
				SandboxID: sandboxID,
				User:      user,
			})
			if tt.expectCode != "" {
				require.Error(t, err)
				assert.Equal(t, tt.expectCode, managererrors.GetErrCode(err))
				assert.Empty(t, result)
			} else {
				require.NoError(t, err)
				assert.Equal(t, "refreshed-token", result.Token)
				assert.WithinDuration(t, time.Now().Add(time.Hour), result.Expiration, 2*time.Second)
			}
			assert.Equal(t, tt.expectCalls, provider.calls.Load())
		})
	}
}

func TestSandboxManagerRefreshTrafficAccessTokenRateLimit(t *testing.T) {
	manager, client := setupTestManager(t, config.SandboxManagerOptions{DisableRouteReconciliation: true})
	manager.trafficTokenOptions = identity.TokenOptions{RequestedValidity: time.Hour}
	manager.trafficTokenLimiter = newTrafficTokenLimiter(time.Hour, time.Now)
	sandbox := getSandboxForApiTest("rate-limit")
	sandbox.UID = "rate-limit-uid"
	sandbox.Annotations[identity.AnnotationEnableJwtAuth] = v1alpha1.True
	sandbox.Status.Conditions = []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}
	CreateSandboxWithStatus(t, client, sandbox)
	sandboxID := utils.GetSandboxID(sandbox)
	require.Eventually(t, func() bool {
		_, err := manager.infra.GetSandbox(t.Context(), infra.GetSandboxOptions{SandboxID: sandboxID})
		return err == nil
	}, time.Second, 10*time.Millisecond)
	provider := &trafficTokenTestProvider{}
	identity.RegisterProvider(provider)
	t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })
	opts := RefreshTrafficAccessTokenOptions{SandboxID: sandboxID, User: testUser}
	_, err := manager.RefreshTrafficAccessToken(t.Context(), opts)
	require.NoError(t, err)
	_, err = manager.RefreshTrafficAccessToken(t.Context(), opts)
	require.Error(t, err)
	assert.Equal(t, managererrors.ErrorRateLimited, managererrors.GetErrCode(err))
	assert.Equal(t, int32(1), provider.calls.Load())
	result, issued, err := manager.BootstrapTrafficAccessToken(t.Context(), opts)
	require.NoError(t, err)
	assert.True(t, issued)
	assert.Equal(t, "refreshed-token", result.Token)
	assert.Equal(t, int32(2), provider.calls.Load())
}

func TestSandboxManagerBootstrapTrafficAccessTokenJoinsRefresh(t *testing.T) {
	manager, client := setupTestManager(t, config.SandboxManagerOptions{DisableRouteReconciliation: true})
	manager.trafficTokenOptions = identity.TokenOptions{RequestedValidity: time.Hour}
	manager.trafficTokenLimiter = newTrafficTokenLimiter(time.Hour, time.Now)
	sandbox := getSandboxForApiTest("bootstrap-during-refresh")
	sandbox.UID = "bootstrap-during-refresh-uid"
	sandbox.Annotations[identity.AnnotationEnableJwtAuth] = v1alpha1.True
	sandbox.Status.Conditions = []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}
	CreateSandboxWithStatus(t, client, sandbox)
	sandboxID := utils.GetSandboxID(sandbox)
	require.Eventually(t, func() bool {
		_, err := manager.infra.GetSandbox(t.Context(), infra.GetSandboxOptions{SandboxID: sandboxID})
		return err == nil
	}, time.Second, 10*time.Millisecond)

	provider := &blockingTrafficTokenTestProvider{started: make(chan struct{}), unblock: make(chan struct{})}
	identity.RegisterProvider(provider)
	t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })
	opts := RefreshTrafficAccessTokenOptions{SandboxID: sandboxID, User: testUser}
	type refreshResult struct {
		result infra.TrafficAccessToken
		err    error
	}
	refreshDone := make(chan refreshResult, 1)
	go func() {
		result, err := manager.RefreshTrafficAccessToken(t.Context(), opts)
		refreshDone <- refreshResult{result: result, err: err}
	}()
	<-provider.started

	type bootstrapResult struct {
		result infra.TrafficAccessToken
		issued bool
		err    error
	}
	bootstrapDone := make(chan bootstrapResult, 1)
	go func() {
		result, issued, err := manager.BootstrapTrafficAccessToken(t.Context(), opts)
		bootstrapDone <- bootstrapResult{result: result, issued: issued, err: err}
	}()
	var bootstrap bootstrapResult
	bootstrapReturned := false
	select {
	case bootstrap = <-bootstrapDone:
		bootstrapReturned = true
	case <-time.After(50 * time.Millisecond):
	}
	close(provider.unblock)

	refresh := <-refreshDone
	if !bootstrapReturned {
		bootstrap = <-bootstrapDone
	}
	require.NoError(t, refresh.err)
	require.NoError(t, bootstrap.err)
	assert.True(t, bootstrap.issued)
	assert.Equal(t, refresh.result, bootstrap.result)
	assert.Equal(t, int32(1), provider.calls.Load())
}

func TestSandboxManagerTrafficAccessTokenLimitIsPerSandbox(t *testing.T) {
	manager, client := setupTestManager(t, config.SandboxManagerOptions{DisableRouteReconciliation: true})
	manager.trafficTokenOptions = identity.TokenOptions{RequestedValidity: time.Hour}
	manager.trafficTokenLimiter = newTrafficTokenLimiter(time.Hour, time.Now)
	sandboxNames := []string{"parallel-a", "parallel-b", "parallel-c"}
	sandboxIDs := make([]string, 0, len(sandboxNames))
	for _, name := range sandboxNames {
		sandbox := getSandboxForApiTest(name)
		sandbox.UID = types.UID("uid-" + name)
		sandbox.Annotations[identity.AnnotationEnableJwtAuth] = v1alpha1.True
		sandbox.Status.Conditions = []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}
		CreateSandboxWithStatus(t, client, sandbox)
		sandboxID := utils.GetSandboxID(sandbox)
		require.Eventually(t, func() bool {
			_, err := manager.infra.GetSandbox(t.Context(), infra.GetSandboxOptions{SandboxID: sandboxID})
			return err == nil
		}, time.Second, 10*time.Millisecond)
		sandboxIDs = append(sandboxIDs, sandboxID)
	}

	provider := &parallelTrafficTokenTestProvider{
		started: make(chan struct{}, len(sandboxIDs)),
		unblock: make(chan struct{}),
	}
	identity.RegisterProvider(provider)
	t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })
	released := false
	defer func() {
		if !released {
			close(provider.unblock)
		}
	}()
	results := make(chan error, len(sandboxIDs))
	for _, sandboxID := range sandboxIDs {
		go func() {
			_, err := manager.RefreshTrafficAccessToken(t.Context(), RefreshTrafficAccessTokenOptions{
				SandboxID: sandboxID,
				User:      testUser,
			})
			results <- err
		}()
	}
	for range sandboxIDs {
		select {
		case <-provider.started:
		case <-time.After(time.Second):
			t.Fatal("traffic token issuance for different Sandboxes was serialized or rate limited")
		}
	}
	close(provider.unblock)
	released = true
	for range sandboxIDs {
		require.NoError(t, <-results)
	}
	assert.Equal(t, int32(len(sandboxIDs)), provider.calls.Load())
}

func TestTrafficTokenLimiter(t *testing.T) {
	now := time.Now()
	limiter := newTrafficTokenLimiter(time.Minute, func() time.Time { return now })
	flight, leader, ok := limiter.acquire("sandbox-a", true)
	require.True(t, ok)
	assert.True(t, leader)
	joined, leader, ok := limiter.acquire("sandbox-a", true)
	require.True(t, ok)
	assert.False(t, leader)
	assert.Same(t, flight, joined)
	limiter.complete("sandbox-a", flight, infra.TrafficAccessToken{Token: "token"}, nil)
	now = now.Add(30 * time.Second)
	_, _, ok = limiter.acquire("sandbox-a", true)
	assert.False(t, ok)
	now = now.Add(30 * time.Second)
	flight, leader, ok = limiter.acquire("sandbox-a", true)
	require.True(t, ok)
	assert.True(t, leader)
	limiter.complete("sandbox-a", flight, infra.TrafficAccessToken{Token: "token"}, nil)
	flight, leader, ok = limiter.acquire("sandbox-a", false)
	require.True(t, ok, "bootstrap issuance bypasses the completed-attempt interval")
	assert.True(t, leader)
	limiter.complete("sandbox-a", flight, infra.TrafficAccessToken{Token: "token"}, nil)
}
