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

package sandboxtemplate

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/openkruise/agents/api/v1alpha1"
	template "github.com/openkruise/agents/pkg/sandboxtemplate"
)

// reconcileBuildCleanup releases the temporary resources a finished build
// leaves behind: the worker, the build Sandbox, and — for failed builds —
// the captured Checkpoints. The SandboxTemplate itself is retained as the
// build record; deleting it relies on ownerReference garbage collection.
func (r *Reconciler) reconcileBuildCleanup(ctx context.Context, version *api.SandboxTemplate) (ctrl.Result, error) {
	if r.stopVersionWorker(client.ObjectKeyFromObject(version)) {
		return ctrl.Result{RequeueAfter: retryInterval}, nil
	}
	box := &api.Sandbox{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(version), box); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	} else if ownedByVersion(box, version) {
		pending, err := r.requestDeletion(ctx, box, metav1.DeletePropagationForeground)
		if err != nil {
			return ctrl.Result{}, err
		}
		if pending {
			return ctrl.Result{RequeueAfter: retryInterval}, nil
		}
	} else {
		// A foreign Sandbox occupying the build name must not wedge the
		// cleanup; the name collision surfaces when a new build binds.
		logf.FromContext(ctx).Info("build cleanup skipped a Sandbox with changed ownership", "sandbox", client.ObjectKeyFromObject(box))
	}
	if version.Status.Phase == api.SandboxTemplatePhaseFailed {
		checkpoint := &api.Checkpoint{}
		if err := r.Get(ctx, buildCheckpointKey(version), checkpoint); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		} else if ownedByVersion(checkpoint, version) {
			pending, err := r.requestDeletion(ctx, checkpoint, metav1.DeletePropagationForeground)
			if err != nil {
				return ctrl.Result{}, err
			}
			if pending {
				return ctrl.Result{RequeueAfter: retryInterval}, nil
			}
		}
	}
	return ctrl.Result{}, nil
}

func ownedByVersion(obj client.Object, v *api.SandboxTemplate) bool {
	owner := template.VersionOwnerReference(obj)
	return obj.GetNamespace() == v.Namespace && owner != nil && owner.Name == v.Name && owner.UID == v.UID
}

// requestDeletion issues a Kubernetes DELETE with UID and resourceVersion
// preconditions. It returns pending=true when the deletion was requested
// but not yet confirmed complete; callers requeue and re-check.
func (r *Reconciler) requestDeletion(ctx context.Context, obj client.Object, propagation metav1.DeletionPropagation) (bool, error) {
	if obj.GetDeletionTimestamp() != nil {
		return true, nil
	}
	uid, rv := obj.GetUID(), obj.GetResourceVersion()
	preconditions := client.Preconditions{UID: &uid}
	if rv != "" {
		preconditions.ResourceVersion = &rv
	}
	err := r.Delete(ctx, obj, preconditions, client.PropagationPolicy(propagation))
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("delete %T %s/%s: %w", obj, obj.GetNamespace(), obj.GetName(), err)
	}
	return true, nil
}
