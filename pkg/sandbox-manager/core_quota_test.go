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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
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
				WithRouteVersionReader(proxyServer)
			return routeSourceOverrideBuilder{base: base, source: failingRouteSandboxSource{}}, nil
		}).
		Build()
	require.NoError(t, err)
	return mgr
}

func TestManagerInitQuotaWithoutRedisDoesNotRegisterAntiDrift(t *testing.T) {
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

			require.NoError(t, mgr.InitQuota(context.Background(), config.QuotaOptions{}, tt.subjects))
			assert.Nil(t, mgr.quotaAntiDrift)
			assert.Equal(t, int64(0), spyCache.addCalls.Load())
			assert.Nil(t, spyCache.lastHandler)
		})
	}
}

func TestManagerInitQuotaRedisConfiguredRegistersHandlerAndFailsOpen(t *testing.T) {
	spyCache := &quotaInitCache{}
	mgr := buildQuotaTestManager(t, spyCache)

	require.NoError(t, mgr.InitQuota(context.Background(), config.QuotaOptions{
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

func TestManagerInitQuotaRedisConfiguredRequiresCache(t *testing.T) {
	mgr := &SandboxManager{}
	err := mgr.InitQuota(context.Background(), config.QuotaOptions{
		RedisAddr:         "127.0.0.1:1",
		OperationTimeout:  time.Millisecond,
		AntiDriftInterval: time.Minute,
		AntiDriftGrace:    time.Minute,
	}, &quotaInitSubjectLister{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cache is not available")
}

func TestManagerInitQuotaRedisConfiguredRegistrationErrorDoesNotLeavePartialState(t *testing.T) {
	spyCache := &quotaInitCache{addErr: errors.New("informer unavailable")}
	mgr := buildQuotaTestManager(t, spyCache)

	err := mgr.InitQuota(context.Background(), config.QuotaOptions{
		RedisAddr:         "127.0.0.1:1",
		OperationTimeout:  time.Millisecond,
		AntiDriftInterval: time.Minute,
		AntiDriftGrace:    time.Minute,
	}, &quotaInitSubjectLister{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "informer unavailable")
	assert.Equal(t, int64(1), spyCache.addCalls.Load())
	assert.Nil(t, mgr.quotaAntiDrift)
	assert.Nil(t, mgr.quotaRedisClient)
}

type recordingRedisCloser struct {
	closed atomic.Bool
}

func (c *recordingRedisCloser) Close() error {
	c.closed.Store(true)
	return nil
}

func TestManagerStopClosesQuotaRedis(t *testing.T) {
	closer := &recordingRedisCloser{}
	opts := config.InitOptions(config.SandboxManagerOptions{
		SystemNamespace:    "sandbox-system",
		MemberlistBindPort: config.DefaultMemberlistBindPort,
	})
	cache, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)

	proxyServer := proxy.NewServer(opts)
	mgr, err := NewSandboxManagerBuilder(opts).
		WithCustomInfra(func() (infra.Builder, error) {
			return sandboxcr.NewInfraBuilder(opts).
				WithCache(cache).
				WithAPIReader(fc).
				WithRouteVersionReader(proxyServer), nil
		}).
		Build()
	require.NoError(t, err)

	require.NoError(t, mgr.GetInfra().Run(t.Context()))
	mgr.quotaRedisClient = closer

	mgr.Stop(t.Context())
	assert.True(t, closer.closed.Load())
}
