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
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
)

// CacheSandboxCustomReconciler reconciles Sandbox objects and dispatches events
// to registered CustomReconcileHandler. It corresponds to the original
// AddSandboxEventHandler functionality.
type CacheSandboxCustomReconciler struct {
	CustomReconciler[*agentsv1alpha1.Sandbox]
}

func NewSandbox() *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{}
}

// SetupWithManager sets up the controller with the Manager.
func (r *CacheSandboxCustomReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.Sandbox{}).
		Named("cache-sandbox-custom-controller").
		Complete(r)
}

// AddCacheSandboxCustomReconciler creates and registers the CacheSandboxCustomReconciler with the manager.
// It returns the reconciler instance so callers can register event handlers via AddReconcileHandlers.
func AddCacheSandboxCustomReconciler(mgr ctrl.Manager) (*CacheSandboxCustomReconciler, error) {
	r := &CacheSandboxCustomReconciler{
		CustomReconciler: CustomReconciler[*agentsv1alpha1.Sandbox]{
			Client:    mgr.GetClient(),
			NewObject: NewSandbox,
			Scheme:    mgr.GetScheme(),
			Name:      "SandboxCustom",
		},
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return nil, err
	}
	klog.InfoS("CacheSandboxCustomReconciler started successfully")
	return r, nil
}
