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

package utils

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// ResourceVersionInterceptorFuncs returns interceptor functions that handle resourceVersion
// conflicts in tests. The controller-runtime fake client strictly checks resourceVersion,
// but tests often use separate object instances that don't have the updated resourceVersion.
// This interceptor automatically syncs resourceVersion before Update operations.
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
			// Get the latest resourceVersion from the fake client
			latest := obj.DeepCopyObject().(ctrlclient.Object)
			if err := client.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, latest); err == nil {
				obj.SetResourceVersion(latest.GetResourceVersion())
			}
			return client.Update(ctx, obj, opts...)
		},
	}
}
