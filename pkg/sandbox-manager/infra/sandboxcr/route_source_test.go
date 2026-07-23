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

package sandboxcr

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

func TestRouteSandboxSourceSubscribe(t *testing.T) {
	managerCache, _, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	provider := &routeEventProvider{Provider: managerCache, registration: &routeEventRegistration{}}
	source := &routeSandboxSource{cache: provider}

	var events []infra.RouteSandboxEvent
	subscription, err := source.Subscribe(t.Context(), func(_ context.Context, event infra.RouteSandboxEvent) {
		events = append(events, event)
	})
	require.NoError(t, err)
	require.Same(t, provider.registration, subscription)
	require.NotNil(t, provider.handler)

	sandbox := routeSourceSandbox()
	provider.handler.OnAdd(sandbox, false)
	require.Len(t, events, 1)
	require.NotNil(t, events[0].Sandbox)
	assert.Equal(t, "10.0.0.1", events[0].Sandbox.GetIP())

	updated := sandbox.DeepCopy()
	updated.ResourceVersion = "11"
	provider.handler.OnUpdate(sandbox, updated)
	require.Len(t, events, 2)
	assert.Equal(t, "11", events[1].Sandbox.GetResourceVersion())

	provider.handler.OnDelete(updated)
	require.Len(t, events, 3)
	require.NotNil(t, events[2].Delete)
	assert.Equal(t, "team-a", events[2].Delete.ObjectKey.Namespace)
	assert.Equal(t, "sandbox-a", events[2].Delete.ObjectKey.Name)
	assert.Equal(t, "11", events[2].Delete.ResourceVersion)

	provider.handler.OnDelete(toolscache.DeletedFinalStateUnknown{
		Key: "team-a/sandbox-a",
		Obj: updated,
	})
	require.Len(t, events, 4)
	require.NotNil(t, events[3].Delete)
	assert.Equal(t, "team-a", events[3].Delete.ObjectKey.Namespace)
	assert.Equal(t, "sandbox-a", events[3].Delete.ObjectKey.Name)
	assert.Empty(t, events[3].Delete.ResourceVersion)

	require.NoError(t, subscription.Remove())
	assert.True(t, provider.registration.removed)
}

func TestRouteSandboxSourceTombstoneValidation(t *testing.T) {
	managerCache, _, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	source := &routeSandboxSource{cache: managerCache}
	var events []infra.RouteSandboxEvent

	source.handleDeleteEvent(t.Context(), func(_ context.Context, event infra.RouteSandboxEvent) {
		events = append(events, event)
	}, toolscache.DeletedFinalStateUnknown{Key: "invalid"})
	assert.Empty(t, events)

	source.handleDeleteEvent(t.Context(), func(_ context.Context, event infra.RouteSandboxEvent) {
		events = append(events, event)
	}, toolscache.DeletedFinalStateUnknown{Key: "team-a/sandbox-a", Obj: nil})
	require.Len(t, events, 1)
	assert.Empty(t, events[0].Delete.ResourceVersion)
}

func TestRouteSandboxSourceSubscribeValidation(t *testing.T) {
	managerCache, _, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	handler := func(context.Context, infra.RouteSandboxEvent) {}

	_, err = (&routeSandboxSource{cache: managerCache}).Subscribe(t.Context(), nil)
	require.Error(t, err)

	_, err = (&routeSandboxSource{}).Subscribe(t.Context(), handler)
	require.Error(t, err)

	expected := errors.New("registration failed")
	provider := &routeEventProvider{Provider: managerCache, err: expected}
	_, err = (&routeSandboxSource{cache: provider}).Subscribe(t.Context(), handler)
	require.ErrorIs(t, err, expected)
}

type routeEventProvider struct {
	cache.Provider
	handler      toolscache.ResourceEventHandler
	registration *routeEventRegistration
	err          error
}

func (p *routeEventProvider) AddSandboxEventHandler(
	_ context.Context,
	handler toolscache.ResourceEventHandler,
) (cache.SandboxEventHandlerRegistration, error) {
	if p.err != nil {
		return nil, p.err
	}
	p.handler = handler
	return p.registration, nil
}

type routeEventRegistration struct {
	removed bool
}

func (r *routeEventRegistration) HasSynced() bool {
	return true
}

func (r *routeEventRegistration) Remove() error {
	r.removed = true
	return nil
}

func routeSourceSandbox() *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "team-a",
			Name:            "sandbox-a",
			UID:             "uid-a",
			ResourceVersion: "10",
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase:   agentsv1alpha1.SandboxRunning,
			PodInfo: agentsv1alpha1.PodInfo{PodIP: "10.0.0.1"},
		},
	}
}
