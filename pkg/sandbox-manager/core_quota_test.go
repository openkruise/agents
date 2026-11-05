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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	redisserver "github.com/alicebob/miniredis/v2/server"
	"github.com/go-logr/logr/funcr"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
	"github.com/openkruise/agents/pkg/configreload"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/sandbox-manager/quota"
	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"
)

type quotaInitRegistration struct{}

func (quotaInitRegistration) HasSynced() bool { return true }
func (quotaInitRegistration) Remove() error   { return nil }

type quotaInitCache struct {
	addCalls    atomic.Int64
	lastHandler cache.ResourceEventHandler
	addErr      error
}

func (c *quotaInitCache) GetClaimedSandbox(context.Context, infracache.GetClaimedSandboxOptions) (*agentsv1alpha1.Sandbox, error) {
	return nil, nil
}
func (c *quotaInitCache) GetCheckpoint(context.Context, infracache.GetCheckpointOptions) (*agentsv1alpha1.Checkpoint, error) {
	return nil, nil
}
func (c *quotaInitCache) PickSandboxSet(context.Context, infracache.PickSandboxSetOptions) (*agentsv1alpha1.SandboxSet, error) {
	return nil, nil
}
func (c *quotaInitCache) ListSandboxSets(context.Context, infracache.ListSandboxSetsOptions) ([]*agentsv1alpha1.SandboxSet, error) {
	return nil, nil
}
func (c *quotaInitCache) ListSandboxes(context.Context, infracache.ListSandboxesOptions) ([]*agentsv1alpha1.Sandbox, error) {
	return nil, nil
}
func (c *quotaInitCache) CountActiveSandboxes(context.Context, infracache.ListSandboxesOptions) (int32, error) {
	return 0, nil
}
func (c *quotaInitCache) ListLiveSandboxesByOwner(context.Context, string) ([]*agentsv1alpha1.Sandbox, error) {
	return nil, nil
}
func (c *quotaInitCache) ListCheckpoints(context.Context, infracache.ListCheckpointsOptions) ([]*agentsv1alpha1.Checkpoint, error) {
	return nil, nil
}
func (c *quotaInitCache) ListSandboxesInPool(context.Context, infracache.ListSandboxesInPoolOptions) ([]*agentsv1alpha1.Sandbox, error) {
	return nil, nil
}
func (c *quotaInitCache) NewSandboxPauseTask(context.Context, *agentsv1alpha1.Sandbox) (*cacheutils.WaitTask[*agentsv1alpha1.Sandbox], error) {
	return nil, nil
}
func (c *quotaInitCache) NewSandboxResumeTask(context.Context, *agentsv1alpha1.Sandbox) (*cacheutils.WaitTask[*agentsv1alpha1.Sandbox], error) {
	return nil, nil
}
func (c *quotaInitCache) NewSandboxWaitReadyTask(context.Context, *agentsv1alpha1.Sandbox) *cacheutils.WaitTask[*agentsv1alpha1.Sandbox] {
	return nil
}
func (c *quotaInitCache) NewCheckpointTask(context.Context, *agentsv1alpha1.Checkpoint) *cacheutils.WaitTask[*agentsv1alpha1.Checkpoint] {
	return nil
}
func (c *quotaInitCache) NewPVCTask(context.Context, *corev1.PersistentVolumeClaim) *cacheutils.WaitTask[*corev1.PersistentVolumeClaim] {
	return nil
}
func (c *quotaInitCache) AddSandboxEventHandler(_ context.Context, handler cache.ResourceEventHandler) (infracache.SandboxEventHandlerRegistration, error) {
	c.addCalls.Add(1)
	if c.addErr != nil {
		return nil, c.addErr
	}
	c.lastHandler = handler
	return quotaInitRegistration{}, nil
}
func (c *quotaInitCache) SandboxInformerHealthy() bool    { return true }
func (c *quotaInitCache) Run(context.Context) error       { return nil }
func (c *quotaInitCache) Stop(context.Context)            {}
func (c *quotaInitCache) GetClient() ctrlclient.Client    { return nil }
func (c *quotaInitCache) GetAPIReader() ctrlclient.Reader { return nil }
func (c *quotaInitCache) GetCache() ctrlcache.Cache       { return nil }

type quotaInitSubjectLister struct{}

func (*quotaInitSubjectLister) ListLimited(context.Context) ([]quotaspec.Subject, error) {
	return nil, nil
}
func (*quotaInitSubjectLister) Load(context.Context, string) (quotaspec.Subject, bool) {
	return quotaspec.Subject{}, false
}

func buildQuotaTestManager(t *testing.T, spyCache *quotaInitCache) *SandboxManager {
	t.Helper()
	opts := config.InitOptions(config.SandboxManagerOptions{
		SystemNamespace:    "sandbox-system",
		MemberlistBindPort: config.DefaultMemberlistBindPort,
	})
	_, apiReader, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	proxyServer := proxy.NewServer(opts)
	mgr, err := NewSandboxManagerBuilder(opts).
		WithCustomInfra(func() (infra.Builder, error) {
			base := sandboxcr.NewInfraBuilder(opts).
				WithCache(spyCache).
				WithAPIReader(apiReader).
				WithRouteReader(proxyServer)
			return routeSourceOverrideBuilder{base: base, source: failingSandboxRouteSource{}}, nil
		}).
		Build()
	require.NoError(t, err)
	return mgr
}

func TestManagerConfigureQuotaWithoutRedisDoesNotRegisterAntiDrift(t *testing.T) {
	tests := []struct {
		name     string
		subjects quotaspec.SubjectLister
	}{
		{name: "no keys", subjects: nil},
		{name: "redis absent", subjects: &quotaInitSubjectLister{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spyCache := &quotaInitCache{}
			mgr := buildQuotaTestManager(t, spyCache)

			require.NoError(t, mgr.ConfigureQuota(context.Background(), config.QuotaOptions{}, tt.subjects))
			assert.Nil(t, mgr.quotaAntiDrift)
			assert.Equal(t, int64(0), spyCache.addCalls.Load())
			assert.Nil(t, spyCache.lastHandler)
		})
	}
}

func TestQuotaReloadPreservesRuntime(t *testing.T) {
	server := miniredis.RunT(t)
	server.RequireUserAuth("quota-user", "first")
	spyCache := &quotaInitCache{}
	mgr := buildQuotaTestManager(t, spyCache)
	require.NoError(t, mgr.ConfigureQuota(t.Context(), config.QuotaOptions{RedisAddr: server.Addr()}, &quotaInitSubjectLister{}))
	t.Cleanup(func() { require.NoError(t, mgr.quotaRedisBackend.Close(context.Background())) })
	result, err := mgr.ReloadQuota(t.Context(), map[string]string{configreload.RedisUsername: "quota-user", configreload.RedisPassword: "first"})
	require.NoError(t, err)
	require.Equal(t, configreload.Accepted, result)
	backend, manager, driver := mgr.quotaRedisBackend, mgr.quota, mgr.quotaAntiDrift
	attempt, stopAttempt := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stopAttempt()
	_, err = mgr.ReloadQuota(attempt, map[string]string{configreload.RedisPassword: "second"})
	require.Error(t, err)
	require.Equal(t, "first", mgr.quotaOptions.RedisPassword)
	server.RequireUserAuth("quota-user", "second")
	result, err = mgr.ReloadQuota(t.Context(), map[string]string{configreload.RedisPassword: "second"})
	require.NoError(t, err)
	require.Equal(t, configreload.Accepted, result)
	require.Same(t, backend, mgr.quotaRedisBackend)
	require.Same(t, manager, mgr.quota)
	require.Same(t, driver, mgr.quotaAntiDrift)
	require.Equal(t, int64(1), spyCache.addCalls.Load())
	// Only the published connection can serve the backend after the swap.
	require.NoError(t, backend.Cleanup(t.Context(), "user"))
}

func TestQuotaReloadRejectsInsufficientPermissions(t *testing.T) {
	for _, denied := range []string{"scripts", "quota keys", "list", "release", "cleanup"} {
		t.Run(denied, func(t *testing.T) {
			server := miniredis.RunT(t)
			server.RequireUserAuth("old-user", "old-password")
			server.RequireUserAuth("new-user", "new-password")
			old := redis.NewClient(&redis.Options{Addr: server.Addr(), Username: "old-user", Password: "old-password"})
			backend := quota.NewRedisBackend(old, time.Second)
			t.Cleanup(func() { require.NoError(t, backend.Close(context.Background())) })
			opts := config.QuotaOptions{RedisAddr: server.Addr(), RedisUsername: "old-user", RedisPassword: "old-password", OperationTimeout: time.Second}
			mgr := &SandboxManager{quotaRedisBackend: backend, quotaOptions: opts, quota: quota.NewManager(backend)}
			req := quota.AcquireRequest{User: "business-owner", LockString: "first", Quota: &quotaspec.QuotaSpec{
				Limits: []quotaspec.QuotaLimit{{Dimension: quotaspec.DimSandboxCount, Scope: quotaspec.ScopeAll, Limit: 1}},
			}}
			require.NoError(t, mgr.quota.Acquire(t.Context(), req))
			businessKeys := server.Keys()
			probe := redis.NewClient(&redis.Options{Addr: server.Addr(), Username: "new-user", Password: "new-password"})
			require.NoError(t, probe.Ping(t.Context()).Err(), "candidate authentication and PING succeed")
			require.NoError(t, probe.Close())

			var mu sync.Mutex
			candidates := map[*redisserver.Peer]bool{}
			var denials atomic.Int64
			server.Server().SetPreHook(func(peer *redisserver.Peer, cmd string, args ...string) bool {
				mu.Lock()
				defer mu.Unlock()
				if (cmd == "HELLO" || cmd == "AUTH") && slices.Contains(args, "new-user") {
					candidates[peer] = true
				}
				if !candidates[peer] {
					return false
				}
				script := cmd == "EVAL" || cmd == "EVALSHA"
				blocked := (denied == "scripts" && script) ||
					(denied == "quota keys" && slices.ContainsFunc(args, func(arg string) bool { return strings.HasPrefix(arg, "q:sum:") })) ||
					(denied == "list" && cmd == "HGETALL") ||
					(denied == "release" && script && len(args) == 7) ||
					(denied == "cleanup" && cmd == "DEL")
				if blocked {
					denials.Add(1)
					peer.WriteError("NOPERM sensitive permission detail")
				}
				return blocked
			})
			result, err := mgr.ReloadQuota(t.Context(), map[string]string{configreload.RedisUsername: "new-user", configreload.RedisPassword: "new-password"})
			require.Empty(t, result)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "sensitive")
			require.Positive(t, denials.Load())
			require.Equal(t, opts, mgr.quotaOptions)
			require.Eventually(t, func() bool { return server.CurrentConnectionCount() == 1 }, time.Second, time.Millisecond,
				"failed candidate must close, leaving only the old connection")
			req.LockString = "second"
			require.ErrorIs(t, mgr.quota.Acquire(t.Context(), req), quota.ErrQuotaExceeded)
			entries, err := backend.ListEntries(t.Context(), req.User)
			require.NoError(t, err)
			require.Len(t, entries, 1, "verification must not change business quota state")
			require.ElementsMatch(t, businessKeys, server.Keys(), "disposable verification keys must be cleaned")
			server.Server().SetPreHook(nil)
			result, err = mgr.ReloadQuota(t.Context(), map[string]string{configreload.RedisUsername: "new-user", configreload.RedisPassword: "new-password"})
			require.NoError(t, err)
			require.Equal(t, configreload.Accepted, result, "the same unconfirmed input can recover")
			require.ErrorIs(t, old.Ping(t.Context()).Err(), redis.ErrClosed)
			require.ErrorIs(t, mgr.quota.Acquire(t.Context(), req), quota.ErrQuotaExceeded)
		})
	}
}

type blockedQuotaCleanup struct {
	started, release chan struct{}
	once             sync.Once
}

func (h *blockedQuotaCleanup) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *blockedQuotaCleanup) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *blockedQuotaCleanup) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "del" {
			h.once.Do(func() { close(h.started); <-h.release })
		}
		return next(ctx, cmd)
	}
}

func TestQuotaReloadCanceledPublicationClosesCandidate(t *testing.T) {
	server := miniredis.RunT(t)
	server.RequireUserAuth("old-user", "old-password")
	server.RequireUserAuth("new-user", "new-password")
	old := redis.NewClient(&redis.Options{Addr: server.Addr(), Username: "old-user", Password: "old-password"})
	backend := quota.NewRedisBackend(old, time.Second)
	opts := config.QuotaOptions{RedisAddr: server.Addr(), RedisUsername: "old-user", RedisPassword: "old-password"}
	mgr := &SandboxManager{quotaRedisBackend: backend, quotaOptions: opts}
	hook := &blockedQuotaCleanup{started: make(chan struct{}), release: make(chan struct{})}
	old.AddHook(hook)
	maintenance := make(chan error, 1)
	go func() { maintenance <- backend.Cleanup(t.Context(), "user") }()
	<-hook.started
	defer func() {
		close(hook.release)
		require.NoError(t, <-maintenance)
		require.NoError(t, backend.Cleanup(t.Context(), "user"))
		require.NoError(t, backend.Close(context.Background()))
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		result, err := mgr.ReloadQuota(ctx, map[string]string{configreload.RedisUsername: "new-user", configreload.RedisPassword: "new-password"})
		assert.Empty(t, result)
		assert.ErrorIs(t, err, context.Canceled)
	}()
	// The old operation has not dialed; only the prepared candidate connects.
	require.Eventually(t, func() bool { return server.CurrentConnectionCount() > 0 }, time.Second, time.Millisecond)
	select {
	case <-done:
		t.Fatal("replacement returned before the old operation drained")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reload did not cancel its publication wait")
	}
	require.Equal(t, opts, mgr.quotaOptions)
	require.Eventually(t, func() bool { return server.CurrentConnectionCount() == 0 }, time.Second, time.Millisecond)
	require.NoError(t, old.Ping(t.Context()).Err())
}

func TestQuotaReloadAcceptsPublishedClientAfterCloseFailure(t *testing.T) {
	server := miniredis.RunT(t)
	server.RequireUserAuth("new-user", "new-password")
	old := redis.NewClient(&redis.Options{Addr: server.Addr()})
	require.NoError(t, old.Close())
	backend := quota.NewRedisBackend(old, time.Second)
	t.Cleanup(func() { require.NoError(t, backend.Close(context.Background())) })
	mgr := &SandboxManager{quotaRedisBackend: backend, quotaOptions: config.QuotaOptions{RedisAddr: server.Addr()}}
	result, err := mgr.ReloadQuota(t.Context(), map[string]string{configreload.RedisUsername: "new-user", configreload.RedisPassword: "new-password"})
	require.NoError(t, err)
	require.Equal(t, configreload.Accepted, result)
	require.Equal(t, "new-user", mgr.quotaOptions.RedisUsername)
	require.Equal(t, "new-password", mgr.quotaOptions.RedisPassword)
	require.NoError(t, backend.Cleanup(t.Context(), "user"))
}

func TestQuotaReloadSanitizesPreparationLogs(t *testing.T) {
	server := miniredis.RunT(t)
	const sensitive = "ERR sensitive backend password detail"
	server.SetError(sensitive)
	probe := redis.NewClient(&redis.Options{Addr: server.Addr()})
	require.ErrorContains(t, probe.Ping(t.Context()).Err(), sensitive)
	require.NoError(t, probe.Close())
	var logs []string
	logger := funcr.New(func(_, line string) { logs = append(logs, line) }, funcr.Options{})
	ctx := klog.NewContext(t.Context(), logger)
	backend := quota.NewRedisBackend(nil, time.Second)
	t.Cleanup(func() { require.NoError(t, backend.Close(context.Background())) })
	mgr := &SandboxManager{quotaRedisBackend: backend, quotaOptions: config.QuotaOptions{RedisAddr: server.Addr()}}
	result, err := mgr.ReloadQuota(ctx, map[string]string{configreload.RedisPassword: "new-password"})
	require.Empty(t, result)
	require.Error(t, err)
	require.NotContains(t, err.Error(), sensitive)
	output := strings.Join(logs, "\n")
	require.Contains(t, output, "quota redis connection verification failed")
	require.Contains(t, output, "connection_preparation")
	require.NotContains(t, output, sensitive)
	require.NotContains(t, output, "new-password")
}

func TestManagerConfigureQuotaRedisConfiguredRegistersHandlerAndFailsOpen(t *testing.T) {
	spyCache := &quotaInitCache{}
	mgr := buildQuotaTestManager(t, spyCache)

	require.NoError(t, mgr.ConfigureQuota(context.Background(), config.QuotaOptions{
		RedisAddr:         "127.0.0.1:1",
		OperationTimeout:  time.Millisecond,
		AntiDriftInterval: time.Minute,
		AntiDriftGrace:    time.Minute,
	}, &quotaInitSubjectLister{}))
	require.NotNil(t, mgr.quotaAntiDrift)
	assert.Equal(t, int64(1), spyCache.addCalls.Load())
	assert.NotNil(t, spyCache.lastHandler)

	err := mgr.quota.Acquire(context.Background(), quota.AcquireRequest{
		User:       "test-user",
		LockString: "test-lock",
		Quota:      &quotaspec.QuotaSpec{Limits: []quotaspec.QuotaLimit{{Dimension: quotaspec.DimSandboxCount, Scope: quotaspec.ScopeAll, Limit: 1}}},
		Footprint:  map[quotaspec.QuotaDimension]int64{quotaspec.DimSandboxCount: 1},
		Scopes:     []quotaspec.QuotaScope{quotaspec.ScopeAll},
	})
	require.NoError(t, err)
}

func TestManagerConfigureQuotaRedisConfiguredRequiresCache(t *testing.T) {
	mgr := &SandboxManager{}
	err := mgr.ConfigureQuota(context.Background(), config.QuotaOptions{
		RedisAddr:         "127.0.0.1:1",
		OperationTimeout:  time.Millisecond,
		AntiDriftInterval: time.Minute,
		AntiDriftGrace:    time.Minute,
	}, &quotaInitSubjectLister{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cache is not available")
}

func TestManagerConfigureQuotaRedisConfiguredRegistrationErrorDoesNotLeavePartialState(t *testing.T) {
	spyCache := &quotaInitCache{addErr: errors.New("informer unavailable")}
	mgr := buildQuotaTestManager(t, spyCache)

	err := mgr.ConfigureQuota(context.Background(), config.QuotaOptions{
		RedisAddr:         "127.0.0.1:1",
		OperationTimeout:  time.Millisecond,
		AntiDriftInterval: time.Minute,
		AntiDriftGrace:    time.Minute,
	}, &quotaInitSubjectLister{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "informer unavailable")
	assert.Equal(t, int64(1), spyCache.addCalls.Load())
	assert.Nil(t, mgr.quotaAntiDrift)
	assert.Nil(t, mgr.quotaRedisBackend)
}

func TestManagerStopClosesQuotaRedis(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	require.NoError(t, client.Ping(t.Context()).Err())

	opts := config.InitOptions(config.SandboxManagerOptions{
		SystemNamespace:    "sandbox-system",
		MemberlistBindPort: config.DefaultMemberlistBindPort,
	})
	mgr, err := NewSandboxManagerBuilder(opts).
		WithCustomInfra(withTestInfra(t, opts)).
		Build()
	require.NoError(t, err)

	require.NoError(t, mgr.GetInfra().Run(t.Context()))
	mgr.quotaRedisBackend = quota.NewRedisBackend(client, time.Second)

	mgr.Stop(t.Context())
	require.ErrorIs(t, client.Ping(t.Context()).Err(), redis.ErrClosed)
}
