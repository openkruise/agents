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

package storage

import (
	"fmt"
	"sort"
	"sync"
)

var (
	registryMu sync.RWMutex
	registry   = map[string]Provider{}
)

// Register installs p into the global registry, keyed by p.Driver().
//
// Register panics if p is nil, p.Driver() is empty, or another Provider has
// already been registered for the same driver name. It is intended to be
// called from package init() functions.
func Register(p Provider) {
	if p == nil {
		panic("storage: Register called with nil Provider")
	}
	driver := p.Driver()
	if driver == "" {
		panic("storage: Register called with Provider that has empty Driver()")
	}

	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[driver]; exists {
		panic(fmt.Sprintf("storage: Provider already registered for driver %q", driver))
	}
	registry[driver] = p
}

// Lookup returns the Provider registered for the given driver name, if any.
func Lookup(driver string) (Provider, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	p, ok := registry[driver]
	return p, ok
}

// Drivers returns a sorted snapshot of all registered driver names. The
// result is safe to use as user-facing diagnostics (e.g. "supported
// drivers: ...").
func Drivers() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// resetRegistryForTesting clears the registry. It is only used by unit tests
// in the same package.
func resetRegistryForTesting() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]Provider{}
}
