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

	"github.com/openkruise/agents/pkg/cache/controllers"
)

func TestCache_RemoveSafeHealth(t *testing.T) {
	c, health := newHealthCacheForTest(t)
	assert.False(t, c.RemoveSafe())

	health.MarkSynced()
	assert.True(t, c.RemoveSafe())

	health.RecordWatchError(nil, errors.New("watch failed"))
	assert.False(t, c.RemoveSafe(), "a fresh watch error disables remove during the settle window")

	health.lastWatchError.Store(time.Now().Add(-time.Hour).UnixNano())
	assert.True(t, c.RemoveSafe(), "remove re-enables after the settle window; any spurious remove during recovery self-heals via the add direction within grace (spec section 6.4.2)")
}

func TestCache_RemoveSafeWaitsForSandboxEventHandlerSync(t *testing.T) {
	c, health := newHealthCacheForTest(t)
	health.MarkSynced()

	reg := &fakeSandboxEventRegistration{}
	c.sandboxEventRegistrationMu.Lock()
	c.sandboxEventRegistration = reg
	c.sandboxEventRegistrationMu.Unlock()
	assert.False(t, c.RemoveSafe())

	reg.synced = true
	assert.True(t, c.RemoveSafe())
}

type fakeSandboxEventRegistration struct {
	synced bool
}

func (r *fakeSandboxEventRegistration) HasSynced() bool {
	return r.synced
}

func (r *fakeSandboxEventRegistration) Remove() error {
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
