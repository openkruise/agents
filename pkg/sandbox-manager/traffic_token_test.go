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
	"github.com/openkruise/agents/pkg/sandboxid"
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

type panickingTrafficTokenTestProvider struct {
	calls atomic.Int32
}

func (p *panickingTrafficTokenTestProvider) IssueToken(context.Context, *v1alpha1.Sandbox, identity.TokenOptions) (*identity.TokenResponse, error) {
	p.calls.Add(1)
	panic("issuer panic")
}

func (*panickingTrafficTokenTestProvider) PropagateSecurityToken(context.Context, *v1alpha1.Sandbox, *identity.TokenResponse, ...utilruntime.Option) error {
	return nil
}

func TestSandboxManagerRefreshTrafficAccessToken(t *testing.T) {
	providerErr := errors.New("issuer unavailable")
	tests := []struct {
		name          string
		mutate        func(*v1alpha1.Sandbox)
		user          string
		providerError error
		retryOnError  bool
		expectCode    managererrors.ErrorCode
		expectCalls   int32
	}{
		{name: "running sandbox", expectCalls: 1},
		{name: "paused sandbox", mutate: func(s *v1alpha1.Sandbox) { s.Status.Phase = v1alpha1.SandboxPaused }, expectCalls: 1},
		{name: "owner mismatch", user: "other", expectCode: managererrors.ErrorNotAllowed},
		{name: "JWT disabled", mutate: func(s *v1alpha1.Sandbox) { delete(s.Annotations, identity.AnnotationEnableJwtAuth) }, expectCode: managererrors.ErrorConflict},
		{name: "unsupported state", mutate: func(s *v1alpha1.Sandbox) { s.Status.Phase = v1alpha1.SandboxFailed }, expectCode: managererrors.ErrorConflict},
		{name: "provider unavailable", providerError: providerErr, retryOnError: true, expectCode: managererrors.ErrorUnavailable, expectCalls: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, client := setupTestManager(t, config.SandboxManagerOptions{})
			manager.trafficTokenOptions = identity.TokenOptions{RequestedValidity: time.Hour}
			manager.trafficTokenSingleflight = newTrafficTokenSingleflight()
			sandbox := getSandboxForApiTest(tt.name)
			sandbox.UID = types.UID("uid-" + tt.name)
			sandbox.Annotations[identity.AnnotationEnableJwtAuth] = v1alpha1.True
			sandbox.Status.Conditions = []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}
			if tt.mutate != nil {
				tt.mutate(sandbox)
			}
			CreateSandboxWithStatus(t, client, sandbox)
			sandboxID := sandboxid.Resolve(sandbox)
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
			opts := RefreshTrafficAccessTokenOptions{
				SandboxID: sandboxID,
				User:      user,
			}
			result, err := manager.RefreshTrafficAccessToken(t.Context(), opts)
			if tt.expectCode != "" {
				require.Error(t, err)
				assert.Equal(t, tt.expectCode, managererrors.GetErrCode(err))
				assert.Empty(t, result)
			} else {
				require.NoError(t, err)
				assert.Equal(t, "refreshed-token", result.Token)
				assert.WithinDuration(t, time.Now().Add(time.Hour), result.Expiration, 2*time.Second)
			}
			if tt.retryOnError {
				provider.err = nil
				result, err = manager.RefreshTrafficAccessToken(t.Context(), opts)
				require.NoError(t, err)
				assert.Equal(t, "refreshed-token", result.Token)
			}
			assert.Equal(t, tt.expectCalls, provider.calls.Load())
		})
	}
}

func TestSandboxManagerRefreshTrafficAccessTokenDoesNotReuseCompletedToken(t *testing.T) {
	manager, client := setupTestManager(t, config.SandboxManagerOptions{})
	manager.trafficTokenOptions = identity.TokenOptions{RequestedValidity: time.Hour}
	manager.trafficTokenSingleflight = newTrafficTokenSingleflight()
	sandbox := getSandboxForApiTest("no-result-reuse")
	sandbox.UID = "no-result-reuse-uid"
	sandbox.Annotations[identity.AnnotationEnableJwtAuth] = v1alpha1.True
	sandbox.Status.Conditions = []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}
	CreateSandboxWithStatus(t, client, sandbox)
	sandboxID := sandboxid.Resolve(sandbox)
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
	require.NoError(t, err)
	assert.Equal(t, int32(2), provider.calls.Load())
}

func TestSandboxManagerConcurrentRefreshTrafficAccessTokenCoalesces(t *testing.T) {
	manager, client := setupTestManager(t, config.SandboxManagerOptions{})
	manager.trafficTokenOptions = identity.TokenOptions{RequestedValidity: time.Hour}
	manager.trafficTokenSingleflight = newTrafficTokenSingleflight()
	sandbox := getSandboxForApiTest("concurrent-refresh")
	sandbox.UID = "concurrent-refresh-uid"
	sandbox.Annotations[identity.AnnotationEnableJwtAuth] = v1alpha1.True
	sandbox.Status.Conditions = []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}
	CreateSandboxWithStatus(t, client, sandbox)
	sandboxID := sandboxid.Resolve(sandbox)
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
	results := make(chan refreshResult, 2)
	refresh := func() {
		result, err := manager.RefreshTrafficAccessToken(t.Context(), opts)
		results <- refreshResult{result: result, err: err}
	}
	go refresh()
	<-provider.started
	go refresh()
	time.Sleep(50 * time.Millisecond)
	close(provider.unblock)

	first := <-results
	second := <-results
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	assert.Equal(t, first.result, second.result)
	assert.Equal(t, int32(1), provider.calls.Load())
}

func TestSandboxManagerTrafficAccessTokenLeaderCancellationDoesNotFailWaiter(t *testing.T) {
	manager, client := setupTestManager(t, config.SandboxManagerOptions{})
	manager.trafficTokenOptions = identity.TokenOptions{RequestedValidity: time.Hour}
	manager.trafficTokenSingleflight = newTrafficTokenSingleflight()
	manager.trafficTokenIssues.timeout = time.Second
	sandbox := getSandboxForApiTest("leader-cancellation")
	sandbox.UID = "leader-cancellation-uid"
	sandbox.Annotations[identity.AnnotationEnableJwtAuth] = v1alpha1.True
	sandbox.Status.Conditions = []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}
	CreateSandboxWithStatus(t, client, sandbox)
	sandboxID := sandboxid.Resolve(sandbox)
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
	leaderDone := make(chan refreshResult, 1)
	waiterDone := make(chan refreshResult, 1)
	leaderCtx, cancelLeader := context.WithCancel(t.Context())
	go func() {
		result, err := manager.RefreshTrafficAccessToken(leaderCtx, opts)
		leaderDone <- refreshResult{result: result, err: err}
	}()
	<-provider.started
	cancelLeader()
	go func() {
		result, err := manager.RefreshTrafficAccessToken(t.Context(), opts)
		waiterDone <- refreshResult{result: result, err: err}
	}()
	time.Sleep(50 * time.Millisecond)
	close(provider.unblock)

	leader := <-leaderDone
	waiter := <-waiterDone
	require.Error(t, leader.err)
	assert.Equal(t, managererrors.ErrorUnavailable, managererrors.GetErrCode(leader.err))
	assert.ErrorIs(t, leader.err, context.Canceled)
	assert.Empty(t, leader.result)
	require.NoError(t, waiter.err)
	assert.Equal(t, "coalesced-token", waiter.result.Token)
	assert.Equal(t, int32(1), provider.calls.Load())
}

func TestSandboxManagerTrafficAccessTokenIssueTimeoutFailsAllWaiters(t *testing.T) {
	manager, client := setupTestManager(t, config.SandboxManagerOptions{})
	manager.trafficTokenOptions = identity.TokenOptions{RequestedValidity: time.Hour}
	manager.trafficTokenSingleflight = newTrafficTokenSingleflight()
	manager.trafficTokenIssues.timeout = 500 * time.Millisecond
	sandbox := getSandboxForApiTest("issue-timeout")
	sandbox.UID = "issue-timeout-uid"
	sandbox.Annotations[identity.AnnotationEnableJwtAuth] = v1alpha1.True
	sandbox.Status.Conditions = []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}
	CreateSandboxWithStatus(t, client, sandbox)
	sandboxID := sandboxid.Resolve(sandbox)
	require.Eventually(t, func() bool {
		_, err := manager.infra.GetSandbox(t.Context(), infra.GetSandboxOptions{SandboxID: sandboxID})
		return err == nil
	}, time.Second, 10*time.Millisecond)

	provider := &blockingTrafficTokenTestProvider{started: make(chan struct{}), unblock: make(chan struct{})}
	identity.RegisterProvider(provider)
	t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })
	opts := RefreshTrafficAccessTokenOptions{SandboxID: sandboxID, User: testUser}
	errorsByCaller := make(chan error, 2)
	go func() {
		_, err := manager.RefreshTrafficAccessToken(t.Context(), opts)
		errorsByCaller <- err
	}()
	<-provider.started
	go func() {
		_, err := manager.RefreshTrafficAccessToken(t.Context(), opts)
		errorsByCaller <- err
	}()
	time.Sleep(50 * time.Millisecond)

	for range 2 {
		err := <-errorsByCaller
		require.Error(t, err)
		assert.Equal(t, managererrors.ErrorUnavailable, managererrors.GetErrCode(err))
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	}
	assert.Equal(t, int32(1), provider.calls.Load())
	assert.Empty(t, manager.trafficTokenSingleflight.flights)
}

func TestSandboxManagerTrafficAccessTokenProviderPanicCleansFlightAndAllowsRetry(t *testing.T) {
	manager, client := setupTestManager(t, config.SandboxManagerOptions{})
	manager.trafficTokenOptions = identity.TokenOptions{RequestedValidity: time.Hour}
	manager.trafficTokenSingleflight = newTrafficTokenSingleflight()
	sandbox := getSandboxForApiTest("provider-panic")
	sandbox.UID = "provider-panic-uid"
	sandbox.Annotations[identity.AnnotationEnableJwtAuth] = v1alpha1.True
	sandbox.Status.Conditions = []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}
	CreateSandboxWithStatus(t, client, sandbox)
	sandboxID := sandboxid.Resolve(sandbox)
	require.Eventually(t, func() bool {
		_, err := manager.infra.GetSandbox(t.Context(), infra.GetSandboxOptions{SandboxID: sandboxID})
		return err == nil
	}, time.Second, 10*time.Millisecond)

	provider := &panickingTrafficTokenTestProvider{}
	identity.RegisterProvider(provider)
	t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })
	opts := RefreshTrafficAccessTokenOptions{SandboxID: sandboxID, User: testUser}
	_, err := manager.RefreshTrafficAccessToken(t.Context(), opts)
	require.Error(t, err)
	assert.Equal(t, managererrors.ErrorInternal, managererrors.GetErrCode(err))
	assert.Equal(t, int32(1), provider.calls.Load())
	assert.Empty(t, manager.trafficTokenSingleflight.flights)

	healthyProvider := &trafficTokenTestProvider{}
	identity.RegisterProvider(healthyProvider)
	result, err := manager.RefreshTrafficAccessToken(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, "refreshed-token", result.Token)
	assert.Equal(t, int32(1), healthyProvider.calls.Load())
}

func TestTrafficTokenIssueLifecycleStopCancelsAndDrainsIssuance(t *testing.T) {
	manager, client := setupTestManager(t, config.SandboxManagerOptions{})
	manager.trafficTokenOptions = identity.TokenOptions{RequestedValidity: time.Hour}
	manager.trafficTokenSingleflight = newTrafficTokenSingleflight()
	manager.trafficTokenIssues.init(t.Context())
	sandbox := getSandboxForApiTest("lifecycle-stop")
	sandbox.UID = "lifecycle-stop-uid"
	sandbox.Annotations[identity.AnnotationEnableJwtAuth] = v1alpha1.True
	sandbox.Status.Conditions = []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}
	CreateSandboxWithStatus(t, client, sandbox)
	sandboxID := sandboxid.Resolve(sandbox)
	require.Eventually(t, func() bool {
		_, err := manager.infra.GetSandbox(t.Context(), infra.GetSandboxOptions{SandboxID: sandboxID})
		return err == nil
	}, time.Second, 10*time.Millisecond)

	provider := &blockingTrafficTokenTestProvider{started: make(chan struct{}), unblock: make(chan struct{})}
	identity.RegisterProvider(provider)
	t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })
	opts := RefreshTrafficAccessTokenOptions{SandboxID: sandboxID, User: testUser}
	refreshDone := make(chan error, 1)
	go func() {
		_, err := manager.RefreshTrafficAccessToken(t.Context(), opts)
		refreshDone <- err
	}()
	<-provider.started

	stopCtx, cancelStop := context.WithTimeout(t.Context(), time.Second)
	defer cancelStop()
	manager.Stop(stopCtx)
	err := <-refreshDone
	require.Error(t, err)
	assert.Equal(t, managererrors.ErrorUnavailable, managererrors.GetErrCode(err))
	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, manager.trafficTokenSingleflight.flights)
	assert.Equal(t, int32(1), provider.calls.Load())
}

func TestSandboxManagerTrafficAccessTokenSingleflightIsPerSandbox(t *testing.T) {
	manager, client := setupTestManager(t, config.SandboxManagerOptions{})
	manager.trafficTokenOptions = identity.TokenOptions{RequestedValidity: time.Hour}
	manager.trafficTokenSingleflight = newTrafficTokenSingleflight()
	sandboxNames := []string{"parallel-a", "parallel-b", "parallel-c"}
	sandboxIDs := make([]string, 0, len(sandboxNames))
	for _, name := range sandboxNames {
		sandbox := getSandboxForApiTest(name)
		sandbox.UID = types.UID("uid-" + name)
		sandbox.Annotations[identity.AnnotationEnableJwtAuth] = v1alpha1.True
		sandbox.Status.Conditions = []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}
		CreateSandboxWithStatus(t, client, sandbox)
		sandboxID := sandboxid.Resolve(sandbox)
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
			t.Fatal("traffic token issuance for different Sandboxes was serialized")
		}
	}
	close(provider.unblock)
	released = true
	for range sandboxIDs {
		require.NoError(t, <-results)
	}
	assert.Equal(t, int32(len(sandboxIDs)), provider.calls.Load())
}

func TestTrafficTokenSingleflight(t *testing.T) {
	singleflight := newTrafficTokenSingleflight()
	flight, leader := singleflight.acquire("sandbox-a")
	assert.True(t, leader)
	joined, leader := singleflight.acquire("sandbox-a")
	assert.False(t, leader)
	assert.Same(t, flight, joined)
	singleflight.complete("sandbox-a", flight, infra.TrafficAccessToken{Token: "token"}, nil)
	assert.Equal(t, "token", joined.result.Token)
	assert.Empty(t, singleflight.flights)

	next, leader := singleflight.acquire("sandbox-a")
	assert.True(t, leader)
	assert.NotSame(t, flight, next, "a completed result must not be reused")
	singleflight.complete("sandbox-a", next, infra.TrafficAccessToken{Token: "next-token"}, nil)

	failed, leader := singleflight.acquire("sandbox-failed")
	assert.True(t, leader)
	failedFollower, leader := singleflight.acquire("sandbox-failed")
	assert.False(t, leader)
	rootErr := errors.New("issuer unavailable")
	singleflight.complete("sandbox-failed", failed, infra.TrafficAccessToken{}, rootErr)
	<-failedFollower.done
	assert.ErrorIs(t, failedFollower.err, rootErr)
	assert.Empty(t, singleflight.flights)
	retry, leader := singleflight.acquire("sandbox-failed")
	assert.True(t, leader)
	assert.NotSame(t, failed, retry)
	singleflight.complete("sandbox-failed", retry, infra.TrafficAccessToken{Token: "retry-token"}, nil)
}
