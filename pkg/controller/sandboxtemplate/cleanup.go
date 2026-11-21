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
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	api "github.com/openkruise/agents/api/v1alpha1"
	template "github.com/openkruise/agents/pkg/sandboxtemplate"
)

func (r *Reconciler) ensureVersionFinalizer(ctx context.Context, v *api.SandboxTemplate) error {
	before := v.DeepCopy()
	if !controllerutil.AddFinalizer(v, api.SandboxTemplateFinalizer) {
		return nil
	}
	return r.Patch(ctx, v, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (r *Reconciler) reconcileBuildCleanup(ctx context.Context, version *api.SandboxTemplate) (ctrl.Result, error) {
	if r.stopVersionWorker(client.ObjectKeyFromObject(version)) {
		return ctrl.Result{RequeueAfter: retryInterval}, nil
	}
	if _, err := r.deleteBuildLease(ctx, version); err != nil {
		return ctrl.Result{RequeueAfter: retryInterval}, err
	}
	box := &api.Sandbox{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(version), box); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	} else {
		if !ownedByVersion(box, version) {
			return ctrl.Result{}, fmt.Errorf("build cleanup Sandbox ownership changed")
		}
		if pending, err := r.requestDeletion(ctx, box, metav1.DeletePropagationForeground); pending || err != nil {
			return ctrl.Result{RequeueAfter: retryInterval}, err
		}
	}
	if version.Status.Phase == api.SandboxTemplatePhaseFailed {
		checkpoints := &api.CheckpointList{}
		if err := r.List(ctx, checkpoints, client.InNamespace(version.Namespace)); err != nil {
			return ctrl.Result{}, err
		}
		for i := range checkpoints.Items {
			if checkpoint := &checkpoints.Items[i]; ownedByVersion(checkpoint, version) {
				if pending, err := r.requestDeletion(ctx, checkpoint, metav1.DeletePropagationForeground); pending || err != nil {
					return ctrl.Result{RequeueAfter: retryInterval}, err
				}
			}
		}
	}
	return ctrl.Result{}, nil
}

func ownedByVersion(obj client.Object, v *api.SandboxTemplate) bool {
	owner := template.VersionOwnerReference(obj)
	return obj.GetNamespace() == v.Namespace && owner != nil && owner.Name == v.Name && owner.UID == v.UID
}

func (r *Reconciler) cleanupVersion(ctx context.Context, v *api.SandboxTemplate) (bool, error) {
	if r.stopVersionWorker(client.ObjectKeyFromObject(v)) {
		return true, nil
	}
	if v.DeletionTimestamp == nil {
		if v.ResourceVersion != "" {
			if err := r.ensureVersionFinalizer(ctx, v); err != nil {
				return false, err
			}
		}
		if pending, err := r.requestDeletion(ctx, v, metav1.DeletePropagationForeground); pending || err != nil {
			return pending, err
		}
	}
	if pending, err := r.deleteBuildLease(ctx, v); pending || err != nil {
		return pending, err
	}
	boxes := &api.SandboxList{}
	if err := r.List(ctx, boxes, client.InNamespace(v.Namespace)); err != nil {
		return false, fmt.Errorf("list build Sandboxes: %w", err)
	}
	for i := range boxes.Items {
		if box := &boxes.Items[i]; ownedByVersion(box, v) {
			if pending, err := r.requestDeletion(ctx, box, metav1.DeletePropagationForeground); pending || err != nil {
				return pending, err
			}
		}
	}
	checkpoints := &api.CheckpointList{}
	if err := r.List(ctx, checkpoints, client.InNamespace(v.Namespace)); err != nil {
		return false, fmt.Errorf("list build Checkpoints: %w", err)
	}
	for i := range checkpoints.Items {
		if checkpoint := &checkpoints.Items[i]; ownedByVersion(checkpoint, v) {
			if pending, err := r.requestDeletion(ctx, checkpoint, metav1.DeletePropagationForeground); pending || err != nil {
				return pending, err
			}
		}
	}
	if v.DeletionTimestamp != nil {
		return true, r.finishCleanup(ctx, v, api.SandboxTemplateFinalizer)
	}
	return false, nil
}

func (r *Reconciler) finishCleanup(ctx context.Context, obj client.Object, finalizer string) error {
	uid, rv := obj.GetUID(), obj.GetResourceVersion()
	if err := r.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &rv}, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
		return client.IgnoreNotFound(fmt.Errorf("request foreground deletion of %T %s/%s: %w", obj, obj.GetNamespace(), obj.GetName(), err))
	}
	// Foreground deletion changes resourceVersion, so finalizer removal reloads the object.
	return wait.ExponentialBackoffWithContext(ctx, retry.DefaultBackoff, func(ctx context.Context) (bool, error) {
		current := obj.DeepCopyObject().(client.Object)
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
			return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
		}
		if current.GetUID() != uid {
			return false, fmt.Errorf("cleanup %T identity changed", obj)
		}
		before := current.DeepCopyObject().(client.Object)
		if !controllerutil.RemoveFinalizer(current, finalizer) {
			return true, nil
		}
		if err := r.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
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
