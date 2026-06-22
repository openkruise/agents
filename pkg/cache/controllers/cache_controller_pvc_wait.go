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

package controllers

import (
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
)

// CachePVCWaitReconciler reconciles PersistentVolumeClaim objects to check wait hooks.
// It monitors PVC state changes and triggers wait hook checks for PVC binding operations.
type CachePVCWaitReconciler struct {
	WaitReconciler[*corev1.PersistentVolumeClaim]
}

func NewPVC() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{}
}

// SetupWithManager sets up the controller with the Manager.
func (r *CachePVCWaitReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.PersistentVolumeClaim{}).
		Named("cache-pvc-wait-controller").
		Complete(r)
}

// AddCachePVCWaitReconciler creates and registers the CachePVCWaitReconciler with the manager.
func AddCachePVCWaitReconciler(mgr ctrl.Manager, waitHooks *sync.Map) error {
	err := (&CachePVCWaitReconciler{
		WaitReconciler: WaitReconciler[*corev1.PersistentVolumeClaim]{
			Client:    mgr.GetClient(),
			Scheme:    mgr.GetScheme(),
			waitHooks: waitHooks,
			NewObject: NewPVC,
			Name:      "PVCWait",
		},
	}).SetupWithManager(mgr)
	if err != nil {
		return err
	}
	klog.InfoS("CachePVCWaitReconciler started successfully")
	return nil
}
