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
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/proxy/routestore"
)

// Registry is the sandbox-gateway's route table. It is written by two unordered
// sources — peer /refresh pushes and the Sandbox CR-watch controller — so it
// delegates to routestore, which keeps the resourceVersion ordering invariant
// across both writes and deletes and therefore prevents either writer from
// resurrecting a route a newer event already removed.
type Registry struct {
	store *routestore.Store
}

var registryInstance = Registry{store: routestore.New()}

func GetRegistry() *Registry {
	return &registryInstance
}

// Get returns the full route.
func (r *Registry) Get(id string) (proxy.Route, bool) {
	return r.store.Get(id)
}

// Update sets the route with a resourceVersion check.
// Returns true if the update was applied, false if skipped due to an older resourceVersion.
func (r *Registry) Update(id string, route proxy.Route) bool {
	applied, _ := r.store.Set(id, route)
	return applied
}

// Delete removes the entry for the given sandbox ID, leaving a versioned
// tombstone so a stale write cannot resurrect it. resourceVersion is the version
// of the deleting event; an empty value falls back to the recorded one.
func (r *Registry) Delete(id string, resourceVersion string) {
	r.store.Delete(id, resourceVersion)
}

// List returns all routes in the registry.
func (r *Registry) List() map[string]proxy.Route {
	return r.store.List()
}

// GC reclaims expired tombstones.
func (r *Registry) GC() {
	r.store.GC()
}

func (r *Registry) Clear() {
	r.store.Clear()
}
