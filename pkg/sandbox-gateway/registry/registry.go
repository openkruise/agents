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
	"sync/atomic"

	"github.com/openkruise/agents/pkg/sandboxroute"
)

// ErrNotReady indicates that the gateway route registry has not processed its
// initial informer snapshot.
var ErrNotReady = errors.New("gateway route registry is not ready")

// Registry is the sandbox-gateway facade over its process-local route Store.
// Readiness gates production reads only; informer and peer mutations are
// accepted while the initial informer snapshot is loading. The Store serializes
// its own route data, so the Registry only tracks the readiness flag.
type Registry struct {
	store *sandboxroute.Store
	ready atomic.Bool
}

var registryInstance = NewRegistry()

// NewRegistry creates an empty gateway Registry.
func NewRegistry() *Registry {
	return &Registry{store: sandboxroute.NewStore()}
}

// GetRegistry returns the process-local gateway Registry. It is a variable so
// tests can point production reads at an isolated Registry and restore it via
// t.Cleanup.
var GetRegistry = func() *Registry {
	return registryInstance
}

// SetReady controls whether production route reads may use the Registry.
func (r *Registry) SetReady(ready bool) {
	r.ready.Store(ready)
}

// Ready reports whether the initial informer snapshot has been processed.
func (r *Registry) Ready() bool {
	return r.ready.Load()
}

// Get returns the unique active route for an opaque Sandbox ID.
func (r *Registry) Get(id string) (sandboxroute.Route, bool) {
	return r.store.Get(id)
}

// Upsert applies a route update regardless of readiness.
func (r *Registry) Upsert(route sandboxroute.Route) sandboxroute.MutationResult {
	return r.store.Upsert(route)
}

// Delete applies an authoritative route deletion regardless of readiness.
func (r *Registry) Delete(route sandboxroute.Route) sandboxroute.MutationResult {
	return r.store.Delete(route)
}
