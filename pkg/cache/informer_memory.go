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

package cache

import (
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// memoryEfficientInformer composes controller-runtime's transform with reuse
// against this informer's own store, including namespace-specific informers.
type memoryEfficientInformer struct {
	toolscache.SharedIndexInformer
}

func (i *memoryEfficientInformer) SetTransform(transform toolscache.TransformFunc) error {
	store := i.GetIndexer()
	return i.SharedIndexInformer.SetTransform(func(obj any) (any, error) {
		// Stored objects already passed through this transform. Reuse must
		// precede any mutation: WatchList staging and Replace both call it.
		if cached, unchanged := reuseUnchangedObject(store, obj); unchanged {
			return cached, nil
		}
		if transform != nil {
			return transform(obj)
		}
		return obj, nil
	})
}

func newMemoryEfficientInformer(lw toolscache.ListerWatcher, obj runtime.Object, resync time.Duration, indexers toolscache.Indexers) toolscache.SharedIndexInformer {
	return &memoryEfficientInformer{
		SharedIndexInformer: toolscache.NewSharedIndexInformer(lw, obj, resync, indexers),
	}
}

func reuseUnchangedObject(store toolscache.Store, obj any) (any, bool) {
	incoming, err := meta.Accessor(obj)
	if err != nil || incoming.GetUID() == "" || incoming.GetResourceVersion() == "" {
		return obj, false
	}

	// A failed identity lookup is an optimization miss. Preserve the original
	// transform and its error handling for the incoming object.
	cached, exists, err := store.Get(obj)
	if err != nil || !exists {
		return obj, false
	}
	previous, err := meta.Accessor(cached)
	if err != nil || previous.GetUID() != incoming.GetUID() || previous.GetResourceVersion() != incoming.GetResourceVersion() {
		return obj, false
	}
	return cached, true
}

// controllerManagerCacheOptions assembles the manager informer cache options:
// unsafe-disable-deep-copy reads, managedFields stripping, reuse-informers for
// unchanged objects, and the optional watch-error health hook.
//
// known-limit: WatchList still decodes every object and builds snapshot keys
// and queues. Ordinary LIST fallback decodes the full list before transforming,
// so it can still overlap two complete payloads. Validate real-CR reloads with
// Linux heap/RSS measurements when sizing recovery headroom.
func controllerManagerCacheOptions(byObject map[ctrlclient.Object]ctrlcache.ByObject, health *InformerHealth) ctrlcache.Options {
	opts := ctrlcache.Options{
		ByObject:                     byObject,
		DefaultUnsafeDisableDeepCopy: ptr.To(true),
		DefaultTransform:             ctrlcache.TransformStripManagedFields(),
		NewInformer:                  newMemoryEfficientInformer,
	}
	if health != nil {
		opts.DefaultWatchErrorHandler = health.RecordWatchError
	}
	return opts
}
