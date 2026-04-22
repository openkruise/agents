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

	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// CacheSandboxWaitReconciler reconciles Sandbox objects to check wait hooks.
// It replicates the logic of addWaiterHandler[*agentsv1alpha1.Sandbox].
type CacheSandboxWaitReconciler struct {
	WaitReconciler[*agentsv1alpha1.Sandbox]
}

// SetupWithManager sets up the controller with the Manager.
func (r *CacheSandboxWaitReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.Sandbox{}).
		Named("cache-sandbox-wait-controller").
		Complete(r)
}

// AddCacheSandboxWaitReconciler creates and registers the CacheSandboxWaitReconciler with the manager.
func AddCacheSandboxWaitReconciler(mgr ctrl.Manager, waitHooks *sync.Map) error {
	err := (&CacheSandboxWaitReconciler{
		WaitReconciler: WaitReconciler[*agentsv1alpha1.Sandbox]{
			Client:    mgr.GetClient(),
			Scheme:    mgr.GetScheme(),
			waitHooks: waitHooks,
			NewObject: NewSandbox,
			Name:      "SandboxWait",
		},
	}).SetupWithManager(mgr)
	if err != nil {
		return err
	}
	klog.InfoS("Started CacheSandboxWaitReconciler successfully")
	return nil
}
