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

package sandboxcr

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/controllers"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

type sandboxControllerProvider interface {
	GetSandboxController() *controllers.CacheSandboxCustomReconciler
}

type routeSandboxSource struct {
	cache     cache.Provider
	apiReader client.Reader
}

var _ infra.RouteSandboxSource = (*routeSandboxSource)(nil)

func (i *Infra) GetRouteSandboxSource() infra.RouteSandboxSource {
	return &routeSandboxSource{cache: i.Cache, apiReader: i.APIReader}
}

func (s *routeSandboxSource) RegisterEventHandler(handler infra.RouteSandboxEventHandler) error {
	if handler == nil {
		return fmt.Errorf("route sandbox event handler must not be nil")
	}
	if s == nil || s.cache == nil {
		return fmt.Errorf("route sandbox cache is not configured")
	}
	controllerProvider, ok := s.cache.(sandboxControllerProvider)
	if !ok {
		return fmt.Errorf("route sandbox cache does not expose its Sandbox controller")
	}
	controller := controllerProvider.GetSandboxController()
	if controller == nil {
		return fmt.Errorf("route sandbox controller is not configured")
	}
	controller.AddReconcileHandlers(func(ctx context.Context, sandbox *agentsv1alpha1.Sandbox, notFound bool) (ctrl.Result, error) {
		if sandbox == nil {
			return ctrl.Result{}, fmt.Errorf("route sandbox reconcile object must not be nil")
		}
		key := types.NamespacedName{Namespace: sandbox.GetNamespace(), Name: sandbox.GetName()}
		if notFound {
			return ctrl.Result{}, handler(ctx, key, nil)
		}
		return ctrl.Result{}, handler(ctx, key, AsSandbox(sandbox, s.cache))
	})
	return nil
}

func (s *routeSandboxSource) Observe(ctx context.Context, key types.NamespacedName) (infra.Sandbox, error) {
	if s == nil || s.apiReader == nil {
		return nil, fmt.Errorf("route sandbox API reader is not configured")
	}

	sandbox := &agentsv1alpha1.Sandbox{}
	if err := s.apiReader.Get(ctx, key, sandbox); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get route sandbox %s/%s: %w", key.Namespace, key.Name, err)
	}
	return AsSandbox(sandbox, s.cache), nil
}
