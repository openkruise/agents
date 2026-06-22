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
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// CacheSandboxSetCustomReconciler reconciles SandboxSet objects and dispatches events
// to registered ResourceEventHandlerFuncs. It corresponds to the original
// AddSandboxSetEventHandler functionality.
type CacheSandboxSetCustomReconciler struct {
	CustomReconciler[*agentsv1alpha1.SandboxSet]
}

func NewSandboxSet() *agentsv1alpha1.SandboxSet {
	return &agentsv1alpha1.SandboxSet{}
}

// SetupWithManager sets up the controller with the Manager.
func (r *CacheSandboxSetCustomReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.SandboxSet{}).
		Named("cache-sandboxset-custom-controller").
		Complete(r)
}

// AddCacheSandboxSetCustomReconciler creates and registers the CacheSandboxSetCustomReconciler with the manager.
// It returns the reconciler instance so callers can register event handlers via AddEventHandler.
func AddCacheSandboxSetCustomReconciler(mgr ctrl.Manager) (*CacheSandboxSetCustomReconciler, error) {
	r := &CacheSandboxSetCustomReconciler{
		CustomReconciler: CustomReconciler[*agentsv1alpha1.SandboxSet]{
			Client:    mgr.GetClient(),
			NewObject: NewSandboxSet,
			Scheme:    mgr.GetScheme(),
			Name:      "SandboxSetCustom",
		},
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return nil, err
	}
	klog.InfoS("CacheSandboxSetCustomReconciler started successfully")
	return r, nil
}
