// Copyright 2026.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	proxyutils "github.com/openkruise/agents/pkg/utils/sandbox-manager/proxyutils"
)

// SandboxReconciler reconciles Sandbox objects and updates the local registry
type SandboxReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	key := req.Namespace + "--" + req.Name

	var sandbox agentsv1alpha1.Sandbox
	if err := r.Get(ctx, req.NamespacedName, &sandbox); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("sandbox deleted, removing from registry", "key", key)
			registry.GetRegistry().Delete(key)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if sandbox.DeletionTimestamp != nil {
		logger.Info("sandbox being deleted, removing from registry", "key", key)
		registry.GetRegistry().Delete(key)
		return ctrl.Result{}, nil
	}

	route := proxyutils.DefaultGetRouteFunc(&sandbox)
	logger.Info("updating registry", "key", key, "podIP", route.IP, "state", route.State, "resourceVersion", route.ResourceVersion)
	registry.GetRegistry().Update(key, route)
	return ctrl.Result{}, nil
}

func StartManager(ctx context.Context) error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	scheme := runtime.NewScheme()
	utilruntime.Must(agentsv1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		// Disable metrics and health probe servers to avoid port conflicts with Envoy.
		HealthProbeBindAddress: "0",
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
	})
	if err != nil {
		return fmt.Errorf("unable to create manager: %w", err)
	}

	if err := ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.Sandbox{}).
		Complete(&SandboxReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
		}); err != nil {
		return fmt.Errorf("unable to create controller: %w", err)
	}

	return mgr.Start(ctx)
}
