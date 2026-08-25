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

package cache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/pkg/cache/controllers"
)

func TestCache_SandboxInformerHealthyHealth(t *testing.T) {
	c, health := newHealthCacheForTest(t)
	assert.False(t, c.SandboxInformerHealthy())

	health.MarkSynced()
	assert.True(t, c.SandboxInformerHealthy())

	health.RecordWatchError(nil, errors.New("watch failed"))
	assert.False(t, c.SandboxInformerHealthy(), "a fresh watch error disables health during the settle window")

	health.lastWatchError.Store(time.Now().Add(-time.Hour).UnixNano())
	assert.True(t, c.SandboxInformerHealthy(), "health re-enables after the settle window; release stays conservative because leaked cleanup still needs a second pass plus grace")
}

func TestCache_SandboxInformerHealthyAggregatesQuotaAndRouteSubscriptions(t *testing.T) {
	c, health := newHealthCacheForTest(t)
	health.MarkSynced()

	reg1 := &fakeSandboxEventRegistration{owner: c}
	reg2 := &fakeSandboxEventRegistration{owner: c}
	c.sandboxEventRegistrationMu.Lock()
	c.sandboxEventRegistrations = map[SandboxEventHandlerRegistration]struct{}{
		reg1: {},
		reg2: {},
	}
	c.sandboxEventRegistrationMu.Unlock()
	assert.False(t, c.SandboxInformerHealthy())

	reg1.synced = true
	assert.False(t, c.SandboxInformerHealthy())

	reg2.synced = true
	assert.True(t, c.SandboxInformerHealthy())

	require.NoError(t, reg1.Remove())
	reg2.synced = false
	assert.False(t, c.SandboxInformerHealthy())

	require.NoError(t, reg2.Remove())
	assert.True(t, c.SandboxInformerHealthy())
}

func TestSandboxEventRegistrationRemoveIsIdempotent(t *testing.T) {
	c := &Cache{}
	handle := &fakeSandboxEventRegistration{synced: true}
	informer := &testSandboxInformer{}
	reg := &sandboxEventRegistration{
		informer: informer,
		handle:   handle,
		owner:    c,
	}
	c.sandboxEventRegistrations = map[SandboxEventHandlerRegistration]struct{}{reg: {}}

	require.NoError(t, reg.Remove())
	require.NoError(t, reg.Remove())
	assert.Equal(t, 2, informer.removeCalls)
	assert.Empty(t, c.sandboxEventRegistrations)
}

func TestCache_AddSandboxEventHandlerLifecycle(t *testing.T) {
	tests := []struct {
		name        string
		getErr      error
		addErr      error
		expectError string
	}{
		{name: "get informer error", getErr: errors.New("no informer"), expectError: "no informer"},
		{name: "add handler error", addErr: errors.New("add rejected"), expectError: "add rejected"},
		{name: "registration is tracked and removable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			informer := &testSandboxInformer{addErr: tt.addErr}
			c := newLifecycleTestCache(informer, tt.getErr)

			reg, err := c.AddSandboxEventHandler(t.Context(), toolscache.ResourceEventHandlerFuncs{})
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Nil(t, reg)
				assert.Empty(t, c.sandboxEventRegistrations)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, reg)
			assert.True(t, reg.HasSynced())
			assert.Len(t, c.sandboxEventRegistrations, 1)

			require.NoError(t, reg.Remove())
			assert.Empty(t, c.sandboxEventRegistrations)
			assert.Equal(t, 1, informer.removeCalls)
		})
	}
}

func TestCache_SandboxEventRegistrationRemoveFailureStaysTracked(t *testing.T) {
	informer := &testSandboxInformer{removeErr: errors.New("remove rejected")}
	c := newLifecycleTestCache(informer, nil)
	reg, err := c.AddSandboxEventHandler(t.Context(), toolscache.ResourceEventHandlerFuncs{})
	require.NoError(t, err)

	require.Error(t, reg.Remove())
	assert.Len(t, c.sandboxEventRegistrations, 1, "a failed removal must keep the registration tracked")

	informer.removeErr = nil
	require.NoError(t, reg.Remove())
	assert.Empty(t, c.sandboxEventRegistrations)
	assert.Equal(t, 2, informer.removeCalls)
}

func newLifecycleTestCache(informer *testSandboxInformer, getErr error) *Cache {
	return &Cache{mgr: &lifecycleTestManager{
		cache: &lifecycleTestCtrlCache{informer: informer, getErr: getErr},
	}}
}

type lifecycleTestManager struct {
	ctrl.Manager
	cache ctrlcache.Cache
}

func (m *lifecycleTestManager) GetCache() ctrlcache.Cache {
	return m.cache
}

type lifecycleTestCtrlCache struct {
	ctrlcache.Cache
	informer ctrlcache.Informer
	getErr   error
}

func (c *lifecycleTestCtrlCache) GetInformer(
	context.Context, ctrlclient.Object, ...ctrlcache.InformerGetOption,
) (ctrlcache.Informer, error) {
	if c.getErr != nil {
		return nil, c.getErr
	}
	return c.informer, nil
}

type testSandboxInformer struct {
	ctrlcache.Informer
	addErr      error
	removeErr   error
	removeCalls int
}

func (i *testSandboxInformer) AddEventHandler(
	toolscache.ResourceEventHandler,
) (toolscache.ResourceEventHandlerRegistration, error) {
	if i.addErr != nil {
		return nil, i.addErr
	}
	return syncedTestHandle{}, nil
}

func (i *testSandboxInformer) RemoveEventHandler(toolscache.ResourceEventHandlerRegistration) error {
	i.removeCalls++
	return i.removeErr
}

type syncedTestHandle struct{}

func (syncedTestHandle) HasSynced() bool {
	return true
}

type fakeSandboxEventRegistration struct {
	synced bool
	owner  *Cache
}

func (r *fakeSandboxEventRegistration) HasSynced() bool {
	return r.synced
}

func (r *fakeSandboxEventRegistration) Remove() error {
	if r.owner != nil {
		r.owner.removeSandboxEventRegistration(r)
		r.owner = nil
	}
	return nil
}

func newHealthCacheForTest(t *testing.T) (*Cache, *InformerHealth) {
	t.Helper()

	mgrBuilder, err := controllers.NewMockManagerBuilder(t)
	require.NoError(t, err)

	health := NewInformerHealth()
	c, err := NewCacheWithHealth(mgrBuilder.Build(), health)
	require.NoError(t, err)
	return c, health
}
