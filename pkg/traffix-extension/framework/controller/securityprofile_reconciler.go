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

package controller

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/traffix-extension/framework/configstore"
	"github.com/openkruise/agents/pkg/traffix-extension/util/logging"
)

// SecurityProfileReconciler reconciles SecurityProfile objects
// and keeps the in-memory config store in sync.
type SecurityProfileReconciler struct {
	client.Reader
	Store configstore.Store
}

func (c *SecurityProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	profile := &v1alpha1.SecurityProfile{}
	if err := c.Get(ctx, req.NamespacedName, profile); err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(logging.VERBOSE).Info("SecurityProfile deleted", "name", req.NamespacedName.String())
			c.Store.ProfileDelete(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Unable to get SecurityProfile")
		return ctrl.Result{}, err
	}

	logger.V(logging.VERBOSE).Info("SecurityProfile updated", "name", req.NamespacedName.String())
	c.Store.ProfileSet(profile)

	return ctrl.Result{}, nil
}

func (c *SecurityProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.SecurityProfile{}).
		Complete(c)
}
