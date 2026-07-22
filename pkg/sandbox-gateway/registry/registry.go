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

	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/pkg/metrics"
	"github.com/openkruise/agents/pkg/sandboxroute"
)

// ErrNotReady indicates that the gateway route lifecycle is not active.
var ErrNotReady = errors.New("gateway route registry is not ready")

// Registry is the sandbox-gateway facade over its process-local route Store.
type Registry struct {
	mu      sync.RWMutex
	store   *sandboxroute.Store
	enqueue func(sandboxroute.MutationResult)
}

var registryInstance = mustNewRegistry()

func mustNewRegistry() *Registry {
	registry, err := NewRegistry(newGatewayStore())
	if err != nil {
		panic(err)
	}
	return registry
}

func newGatewayStore() *sandboxroute.Store {
	return sandboxroute.NewStore(
		sandboxroute.StoreOptions{CollisionRecorder: metrics.RecordSandboxIDCollisionGatewayRoute},
	)
}

// NewRegistry creates a gateway Registry around the supplied shared Store.
func NewRegistry(store *sandboxroute.Store) (*Registry, error) {
	if store == nil {
		return nil, errors.New("gateway route Store must not be nil")
	}
	return &Registry{store: store}, nil
}

// GetRegistry returns the process-local gateway Registry.
func GetRegistry() *Registry {
	return registryInstance
}

// Store returns the shared Store wrapped by the Registry.
func (r *Registry) Store() *sandboxroute.Store {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.store
}

// SetRepairEnqueuer installs the non-blocking targeted-repair handoff.
func (r *Registry) SetRepairEnqueuer(enqueue func(sandboxroute.MutationResult)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enqueue = enqueue
}

// Ready reports whether route reads and mutations have an active repair handoff.
func (r *Registry) Ready() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.enqueue != nil
}

// Get returns the unique active route for an opaque Sandbox ID.
func (r *Registry) Get(id string) (sandboxroute.Route, bool) {
	return r.Store().Get(id)
}

// GetIfReady atomically checks lifecycle readiness and reads one active route.
func (r *Registry) GetIfReady(id string) (sandboxroute.Route, bool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.enqueue == nil {
		return sandboxroute.Route{}, false, false
	}
	route, found := r.store.Get(id)
	return route, found, true
}

// Upsert applies a route update.
func (r *Registry) Upsert(route sandboxroute.Route) (sandboxroute.MutationResult, error) {
	return r.mutate(func(store *sandboxroute.Store) sandboxroute.MutationResult {
		return store.Upsert(route)
	})
}

// DeleteAuthoritativeByObjectKey applies a local authoritative deletion.
func (r *Registry) DeleteAuthoritativeByObjectKey(
	key types.NamespacedName,
) (sandboxroute.MutationResult, error) {
	return r.mutate(func(store *sandboxroute.Store) sandboxroute.MutationResult {
		return store.DeleteAuthoritativeByObjectKey(key)
	})
}

// DeleteConditionally applies a peer deletion when identity fences match.
func (r *Registry) DeleteConditionally(route sandboxroute.Route) (sandboxroute.MutationResult, error) {
	return r.mutate(func(store *sandboxroute.Store) sandboxroute.MutationResult {
		return store.DeleteConditionally(route)
	})
}

// List returns a snapshot of all active routes keyed by opaque Sandbox ID.
func (r *Registry) List() map[string]sandboxroute.Route {
	routes := r.Store().List()
	result := make(map[string]sandboxroute.Route, len(routes))
	for _, route := range routes {
		result[route.ID] = route
	}
	return result
}

func (r *Registry) mutate(
	mutateStore func(*sandboxroute.Store) sandboxroute.MutationResult,
) (sandboxroute.MutationResult, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.enqueue == nil {
		return sandboxroute.MutationResult{}, ErrNotReady
	}
	result := mutateStore(r.store)
	r.enqueue(result)
	return result, nil
}

// Clear resets the process-local Store. It is intended for isolated tests only.
func (r *Registry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.store = newGatewayStore()
	r.enqueue = nil
}
