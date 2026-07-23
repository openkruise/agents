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

package registry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/pkg/sandboxroute"
)

func TestRegistryMutationsDoNotRequireReadiness(t *testing.T) {
	registry := NewRegistry()

	route := fullRoute("short-a", "ns", "a", "uid-a", "1")
	assert.Equal(t, sandboxroute.EventResultApplied, registry.Upsert(route).Result)

	_, present, ready := registry.GetIfReady(route.ID)
	assert.False(t, ready)
	assert.False(t, present)
	_, stored := registry.Get(route.ID)
	assert.True(t, stored)

	registry.SetReady(true)
	got, present, ready := registry.GetIfReady(route.ID)
	assert.True(t, ready)
	assert.True(t, present)
	assert.Equal(t, route, got)

	deletion := sandboxroute.Delete{
		ObjectKey:       types.NamespacedName{Namespace: route.Namespace, Name: route.Name},
		ResourceVersion: "2",
	}
	assert.Equal(t, sandboxroute.EventResultApplied, registry.Delete(deletion).Result)
	_, present, ready = registry.GetIfReady(route.ID)
	assert.True(t, ready)
	assert.False(t, present)
}

func TestRegistryDeleteIfTracked(t *testing.T) {
	registry := NewRegistry()
	key := types.NamespacedName{Namespace: "ns", Name: "a"}

	assert.Equal(t, sandboxroute.EventResultApplied, registry.DeleteIfTracked(sandboxroute.Delete{
		ObjectKey:       key,
		ResourceVersion: "1",
	}).Result)
	assert.Empty(t, registry.List())

	route := fullRoute("short-a", key.Namespace, key.Name, "uid-a", "2")
	require.Equal(t, sandboxroute.EventResultApplied, registry.Upsert(route).Result)
	assert.Equal(t, sandboxroute.EventResultApplied, registry.DeleteIfTracked(sandboxroute.Delete{
		ObjectKey:       key,
		ResourceVersion: "3",
	}).Result)
	_, found := registry.Get(route.ID)
	assert.False(t, found)

	assert.Equal(t, sandboxroute.EventResultIgnored, registry.Upsert(fullRoute(
		route.ID,
		key.Namespace,
		key.Name,
		"uid-a",
		"3",
	)).Result)
}

func TestRegistryListAndClear(t *testing.T) {
	registry := NewRegistry()
	registry.SetReady(true)
	registry.Upsert(fullRoute("a", "ns", "a", "uid-a", "1"))
	registry.Upsert(fullRoute("b", "ns", "b", "uid-b", "1"))
	assert.Len(t, registry.List(), 2)

	registry.Clear()
	assert.Empty(t, registry.List())
	assert.False(t, registry.Ready())
	_, _, ready := registry.GetIfReady("a")
	assert.False(t, ready)
}

func fullRoute(id, namespace, name, uid, resourceVersion string) sandboxroute.Route {
	return sandboxroute.Route{
		ID:              id,
		Namespace:       namespace,
		Name:            name,
		UID:             types.UID(uid),
		ResourceVersion: resourceVersion,
	}
}
