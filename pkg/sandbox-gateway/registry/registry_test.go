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
	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/pkg/sandboxroute"
)

func TestRegistryMutationsDoNotRequireReadiness(t *testing.T) {
	registry := NewRegistry()

	route := fullRoute("short-a", "ns", "a", "uid-a", "1")
	assert.Equal(t, sandboxroute.EventResultApplied, registry.Upsert(route).Result)

	assert.False(t, registry.Ready())
	_, stored := registry.Get(route.ID)
	assert.True(t, stored)

	registry.SetReady(true)
	assert.True(t, registry.Ready())
	got, present := registry.Get(route.ID)
	assert.True(t, present)
	assert.Equal(t, route, got)

	deletion := sandboxroute.Route{
		Namespace:       route.Namespace,
		Name:            route.Name,
		ResourceVersion: "2",
	}
	assert.Equal(t, sandboxroute.EventResultApplied, registry.Delete(deletion).Result)
	_, present = registry.Get(route.ID)
	assert.False(t, present)
}

func TestGetRegistryOverride(t *testing.T) {
	assert.Same(t, registryInstance, GetRegistry())

	orig := GetRegistry
	t.Cleanup(func() { GetRegistry = orig })

	isolated := NewRegistry()
	GetRegistry = func() *Registry { return isolated }
	assert.Same(t, isolated, GetRegistry())
	assert.NotSame(t, registryInstance, GetRegistry())
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
