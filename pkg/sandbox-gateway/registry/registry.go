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
	"errors"
	"sync"

	"github.com/openkruise/agents/pkg/sandboxroute"
)

// ErrNotReady indicates that the gateway route registry has not processed its
// initial informer snapshot.
var ErrNotReady = errors.New("gateway route registry is not ready")

// Registry is the sandbox-gateway facade over its process-local route Store.
// Readiness gates production reads only; informer and peer mutations are
// accepted while the initial informer snapshot is loading.
type Registry struct {
	mu    sync.RWMutex
	store *sandboxroute.Store
	ready bool
}

var registryInstance = NewRegistry()

// NewRegistry creates an empty gateway Registry.
func NewRegistry() *Registry {
	return &Registry{store: sandboxroute.NewStore()}
}

// GetRegistry returns the process-local gateway Registry.
func GetRegistry() *Registry {
	return registryInstance
}

// SetReady controls whether production route reads may use the Registry.
func (r *Registry) SetReady(ready bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ready = ready
}

// Ready reports whether the initial informer snapshot has been processed.
func (r *Registry) Ready() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ready
}

// Get returns the unique active route for an opaque Sandbox ID.
func (r *Registry) Get(id string) (sandboxroute.Route, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.store.Get(id)
}

// GetIfReady atomically checks lifecycle readiness and reads one active route.
func (r *Registry) GetIfReady(id string) (sandboxroute.Route, bool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.ready {
		return sandboxroute.Route{}, false, false
	}
	route, found := r.store.Get(id)
	return route, found, true
}

// Upsert applies a route update regardless of readiness.
func (r *Registry) Upsert(route sandboxroute.Route) sandboxroute.MutationResult {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.store.Upsert(route)
}

// Delete applies an authoritative route deletion regardless of readiness.
func (r *Registry) Delete(deletion sandboxroute.Delete) sandboxroute.MutationResult {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.store.Delete(deletion)
}

// DeleteIfTracked applies a policy-exclusion deletion without creating Store
// state for an ObjectKey that has never been tracked.
func (r *Registry) DeleteIfTracked(deletion sandboxroute.Delete) sandboxroute.MutationResult {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.store.DeleteIfTracked(deletion)
}

// List returns a snapshot of all active routes keyed by opaque Sandbox ID.
func (r *Registry) List() map[string]sandboxroute.Route {
	r.mu.RLock()
	defer r.mu.RUnlock()
	routes := r.store.List()
	result := make(map[string]sandboxroute.Route, len(routes))
	for _, route := range routes {
		result[route.ID] = route
	}
	return result
}

// Clear resets the process-local Store and readiness. It is intended for
// isolated tests only.
func (r *Registry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.store = sandboxroute.NewStore()
	r.ready = false
}
