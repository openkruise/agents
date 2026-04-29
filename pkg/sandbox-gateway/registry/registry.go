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
	"sync"

	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

type Registry struct {
	entries sync.Map
}

var registryInstance Registry

func GetRegistry() *Registry {
	return &registryInstance
}

// Get returns the full route.
func (r *Registry) Get(id string) (proxy.Route, bool) {
	raw, ok := r.entries.Load(id)
	if !ok {
		return proxy.Route{}, false
	}
	return raw.(proxy.Route), true
}

// Update sets the route with resourceVersion check using CAS pattern.
// Returns true if the update was applied, false if skipped due to older resourceVersion.
func (r *Registry) Update(id string, route proxy.Route) bool {
	for {
		old, loaded := r.entries.LoadOrStore(id, route)
		if !loaded {
			// First write, success directly
			return true
		}

		oldRoute := old.(proxy.Route)
		if !expectations.IsResourceVersionNewer(oldRoute.ResourceVersion, route.ResourceVersion) {
			// New version is not newer than old version, skip write
			return false
		}

		// Attempt CAS update
		if r.entries.CompareAndSwap(id, old, route) {
			// Successfully replaced
			return true
		}
		// CAS failed, modified by another goroutine, retry
	}
}

// Delete removes the entry for the given sandbox ID.
func (r *Registry) Delete(id string) {
	r.entries.Delete(id)
}

// List returns all routes in the registry.
func (r *Registry) List() map[string]proxy.Route {
	result := make(map[string]proxy.Route)
	r.entries.Range(func(key, value any) bool {
		result[key.(string)] = value.(proxy.Route)
		return true
	})
	return result
}

func (r *Registry) Clear() {
	r.entries = sync.Map{}
}
