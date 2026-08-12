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

func TestSandboxRouteSourceSubscribe(t *testing.T) {
	managerCache, _, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	provider := &routeEventProvider{Provider: managerCache, registration: &routeEventRegistration{}}
	source := &sandboxRouteSource{cache: provider}

	var events []infra.SandboxRouteEvent
	err = source.Subscribe(t.Context(), func(_ context.Context, event infra.SandboxRouteEvent) {
		events = append(events, event)
	})
	require.NoError(t, err)
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
	assert.Equal(t, "team-a", events[2].Delete.Namespace)
	assert.Equal(t, "sandbox-a", events[2].Delete.Name)
	assert.Equal(t, "11", events[2].Delete.ResourceVersion)
}

func TestSandboxRouteSourceEventValidation(t *testing.T) {
	tests := []struct {
		name string
		emit func(source *sandboxRouteSource, handler infra.SandboxRouteEventHandler)
		// expectDelete asserts one RV-less delete event; otherwise no event at all.
		expectDelete bool
	}{
		{
			name: "tombstone key without namespace is discarded",
			emit: func(source *sandboxRouteSource, handler infra.SandboxRouteEventHandler) {
				source.handleDeleteEvent(context.Background(), handler, toolscache.DeletedFinalStateUnknown{Key: "invalid"})
			},
		},
		{
			name: "tombstone key that fails parsing is discarded",
			emit: func(source *sandboxRouteSource, handler infra.SandboxRouteEventHandler) {
				source.handleDeleteEvent(context.Background(), handler, toolscache.DeletedFinalStateUnknown{Key: "a/b/c"})
			},
		},
		{
			name: "unknown delete object type is discarded",
			emit: func(source *sandboxRouteSource, handler infra.SandboxRouteEventHandler) {
				source.handleDeleteEvent(context.Background(), handler, "not-a-sandbox")
			},
		},
		{
			name: "non-sandbox object event is discarded",
			emit: func(source *sandboxRouteSource, handler infra.SandboxRouteEventHandler) {
				source.handleObjectEvent(context.Background(), handler, "not-a-sandbox")
			},
		},
		{
			name: "key-only tombstone emits an RV-less delete",
			emit: func(source *sandboxRouteSource, handler infra.SandboxRouteEventHandler) {
				source.handleDeleteEvent(context.Background(), handler, toolscache.DeletedFinalStateUnknown{
					Key: "team-a/sandbox-a",
				})
			},
			expectDelete: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			managerCache, _, err := cachetest.NewTestCache(t)
			require.NoError(t, err)
			source := &sandboxRouteSource{cache: managerCache}
			var events []infra.SandboxRouteEvent

			tt.emit(source, func(_ context.Context, event infra.SandboxRouteEvent) {
				events = append(events, event)
			})

			if !tt.expectDelete {
				assert.Empty(t, events)
				return
			}
			require.Len(t, events, 1)
			require.NotNil(t, events[0].Delete)
			assert.Equal(t, "team-a", events[0].Delete.Namespace)
			assert.Equal(t, "sandbox-a", events[0].Delete.Name)
			assert.Empty(t, events[0].Delete.ResourceVersion)
		})
	}
}

func TestSandboxRouteSourceSubscribeValidation(t *testing.T) {
	managerCache, _, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	handler := func(context.Context, infra.SandboxRouteEvent) {}

	err = (&sandboxRouteSource{cache: managerCache}).Subscribe(t.Context(), nil)
	require.Error(t, err)

	err = (&sandboxRouteSource{}).Subscribe(t.Context(), handler)
	require.Error(t, err)

	expected := errors.New("registration failed")
	provider := &routeEventProvider{Provider: managerCache, err: expected}
	err = (&sandboxRouteSource{cache: provider}).Subscribe(t.Context(), handler)
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

type routeEventRegistration struct{}

func (r *routeEventRegistration) HasSynced() bool {
	return true
}

func (r *routeEventRegistration) Remove() error {
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
