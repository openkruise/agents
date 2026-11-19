/*
Copyright 2025.

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

package cachetest

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// ResourceVersionInterceptorFuncs returns interceptor functions that reconcile
// the fake client's resourceVersion handling with a real apiserver for tests.
//
// The fake client only allows unconditional updates (empty resourceVersion) for
// built-in kinds, so this interceptor syncs the latest resourceVersion into
// hand-constructed CRD objects that carry none. Objects that already carry a
// resourceVersion keep it: a stale write then fails with a conflict exactly like
// a real apiserver, which production retry paths (e.g. Sandbox retryUpdate)
// depend on to keep concurrent writes from silently clobbering a winner.
//
// Usage:
//
//	fakeClient := fake.NewClientBuilder().
//	    WithScheme(scheme).
//	    WithInterceptorFuncs(cache.ResourceVersionInterceptorFuncs()).
//	    Build()
func ResourceVersionInterceptorFuncs() interceptor.Funcs {
	return interceptor.Funcs{
		Update: func(ctx context.Context, client ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.UpdateOption) error {
			if obj.GetResourceVersion() == "" {
				// Get the latest resourceVersion from the fake client.
				// The Deepcopy is not necessary, but it's the simplest way to create a new object.
				// For it is only used by unit tests, so the performance impact is negligible.
				latest := obj.DeepCopyObject().(ctrlclient.Object)
				if err := client.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, latest); err == nil {
					obj.SetResourceVersion(latest.GetResourceVersion())
				}
			}
			return client.Update(ctx, obj, opts...)
		},
	}
}
