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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	toolscache "k8s.io/client-go/tools/cache"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"

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
	informer := &idempotentTestInformer{}
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

type idempotentTestInformer struct {
	ctrlcache.Informer
	removeCalls int
}

func (i *idempotentTestInformer) RemoveEventHandler(toolscache.ResourceEventHandlerRegistration) error {
	i.removeCalls++
	return nil
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
	c, err := NewCacheWithOptions(mgrBuilder.Build(), Options{Health: health})
	require.NoError(t, err)
	return c, health
}
