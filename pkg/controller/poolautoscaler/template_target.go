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

package poolautoscaler

import (
	"context"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/templateindex"
)

var errInactiveManagedTarget = errors.New("managed scaling target is inactive")

// Managed checks use snapshots only; unseen deletes and submitted writes are not drained.
func checkManagedTarget(pa *api.PoolAutoscaler, pool *api.SandboxSet) (bool, error) {
	owner := templateindex.VersionOwnerReference(pa)
	if owner == nil {
		return false, nil
	}
	if pa.DeletionTimestamp != nil || pool.DeletionTimestamp != nil {
		return true, fmt.Errorf("%w: autoscaler or pool is deleting", errInactiveManagedTarget)
	}
	poolOwner := templateindex.VersionOwnerReference(pool)
	if pool.Namespace != pa.Namespace || poolOwner == nil || poolOwner.Name != owner.Name || poolOwner.UID != owner.UID {
		return true, fmt.Errorf("%w: pool does not share the autoscaler's SandboxTemplate controller owner", errInactiveManagedTarget)
	}
	return true, nil
}

// The observed resourceVersion rejects managed writes to same-name replacements.
func (r *Reconciler) scaleTarget(ctx context.Context, pool *api.SandboxSet, desired int32, managed bool) error {
	if !managed {
		return r.doScale(ctx, pool, desired)
	}
	before := pool.DeepCopy()
	pool.Spec.Replicas = desired
	return r.Patch(ctx, pool, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
