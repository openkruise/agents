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

package keys

import (
	"context"
	"sync"

	toolscache "k8s.io/client-go/tools/cache"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// stubCache is a minimal ctrlcache.Cache for tests. Only GetInformer is
// implemented; all other methods are unused by the storage code under test
// and the embedded nil interface will panic if any test reaches them, which
// is the desired loud-failure behavior.
type stubCache struct {
	ctrlcache.Cache // nil-embedded; methods we don't override panic
	informer        *stubInformer
}

func newStubCache() *stubCache {
	return &stubCache{informer: &stubInformer{}}
}

func (s *stubCache) GetInformer(_ context.Context, _ client.Object, _ ...ctrlcache.InformerGetOption) (ctrlcache.Informer, error) {
	return s.informer, nil
}

// stubInformer records the registered handler so tests can drive events
// directly. AddEventHandlerWithOptions/AddIndexers are inherited from the
// embedded nil interface and will panic if called.
type stubInformer struct {
	ctrlcache.Informer

	mu      sync.Mutex
	handler toolscache.ResourceEventHandler
	removed bool
}

func (s *stubInformer) AddEventHandler(handler toolscache.ResourceEventHandler) (toolscache.ResourceEventHandlerRegistration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handler = handler
	return stubReg{}, nil
}

func (s *stubInformer) RemoveEventHandler(_ toolscache.ResourceEventHandlerRegistration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removed = true
	s.handler = nil
	return nil
}

func (s *stubInformer) currentHandler() toolscache.ResourceEventHandler {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handler
}

func (s *stubInformer) wasRemoved() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removed
}

type stubReg struct{}

// HasSynced satisfies toolscache.ResourceEventHandlerRegistration. Tests that
// drive events directly never consult this; returning true is harmless.
func (stubReg) HasSynced() bool { return true }
