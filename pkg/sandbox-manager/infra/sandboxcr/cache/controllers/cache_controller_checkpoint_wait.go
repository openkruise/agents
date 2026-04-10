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

package controllers

import (
	"sync"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
)

// CacheCheckpointWaitReconciler reconciles Checkpoint objects to check wait hooks.
// It replicates the logic of addWaiterHandler[*agentsv1alpha1.Checkpoint].
type CacheCheckpointWaitReconciler struct {
	WaitReconciler[*agentsv1alpha1.Checkpoint]
}

func NewCheckpoint() *agentsv1alpha1.Checkpoint {
	return &agentsv1alpha1.Checkpoint{}
}

// SetupWithManager sets up the controller with the Manager.
func (r *CacheCheckpointWaitReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.Checkpoint{}).
		Named("cache-checkpoint-wait-controller").
		Complete(r)
}

// AddCacheCheckpointWaitReconciler creates and registers the CacheCheckpointWaitReconciler with the manager.
func AddCacheCheckpointWaitReconciler(mgr ctrl.Manager, waitHooks *sync.Map) error {
	err := (&CacheCheckpointWaitReconciler{
		WaitReconciler: WaitReconciler[*agentsv1alpha1.Checkpoint]{
			Client:    mgr.GetClient(),
			Scheme:    mgr.GetScheme(),
			waitHooks: waitHooks,
			NewObject: NewCheckpoint,
			Name:      "CheckpointWait",
		},
	}).SetupWithManager(mgr)
	if err != nil {
		return err
	}
	klog.InfoS("CacheCheckpointWaitReconciler started successfully")
	return nil
}
