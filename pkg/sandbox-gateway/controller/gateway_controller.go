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
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-gateway/jwtauth"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	"github.com/openkruise/agents/pkg/sandboxroute"
)

// ManagerOptions supplies gateway composition dependencies.
type ManagerOptions struct {
	Registry       *registry.Registry
	Namespace      string
	LabelSelector  string
	JWTAuthManager *jwtauth.Manager
}

type routeEventHandler struct {
	registry *registry.Registry
}

func (h *routeEventHandler) onObject(ctx context.Context, obj any) {
	logger := log.FromContext(ctx)
	sandbox, ok := obj.(*agentsv1alpha1.Sandbox)
	if !ok {
		logger.Error(
			nil,
			"discarding unexpected gateway route informer object",
			"type", fmt.Sprintf("%T", obj),
		)
		return
	}

	key := types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Name}
	deletion := sandboxroute.Route{
		Namespace:       key.Namespace,
		Name:            key.Name,
		ResourceVersion: sandbox.ResourceVersion,
	}
	if sandbox.DeletionTimestamp != nil {
		sandboxroute.LogMutation(logger, "delete", deletion, h.registry.Delete(deletion))
		return
	}
	route, err := sandboxroute.RouteFromSandbox(sandbox)
	if err != nil {
		logger.Error(err, "failed to project gateway route", "namespace", key.Namespace, "name", key.Name)
		return
	}
	sandboxroute.LogMutation(logger, "upsert", route, h.registry.Upsert(route))
}

func (h *routeEventHandler) onDelete(ctx context.Context, obj any) {
	logger := log.FromContext(ctx)
	var deletion sandboxroute.Route
	switch value := obj.(type) {
	case *agentsv1alpha1.Sandbox:
		// Normal deletes retain the Kubernetes resource version.
		deletion = sandboxroute.Route{
			Namespace:       value.Namespace,
			Name:            value.Name,
			ResourceVersion: value.ResourceVersion,
		}
	case toolscache.DeletedFinalStateUnknown:
		// Empty resource version is reserved for an untrusted tombstone.
		namespace, name, err := toolscache.SplitMetaNamespaceKey(value.Key)
		if err != nil || namespace == "" || name == "" {
			logger.Error(err, "discarding invalid gateway route tombstone", "key", value.Key)
			return
		}
		deletion.Namespace = namespace
		deletion.Name = name
	default:
		logger.Error(
			nil,
			"discarding unexpected gateway route delete object",
			"type", fmt.Sprintf("%T", obj),
		)
		return
	}
	sandboxroute.LogMutation(logger, "delete", deletion, h.registry.Delete(deletion))
}

// StartManager starts the gateway Sandbox informer route feed.
func StartManager(ctx context.Context, options ManagerOptions) error {
	if options.Registry == nil {
		return errors.New("gateway manager route dependencies must not be nil")
	}
	options.Registry.SetReady(false)

	cacheOptions, err := buildGatewayCacheOptions(options.Namespace, options.LabelSelector)
	if err != nil {
		return err
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agentsv1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache:  cacheOptions,
		// Disable metrics and health probe servers to avoid port conflicts with Envoy.
		HealthProbeBindAddress: "0",
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
	})
	if err != nil {
		return fmt.Errorf("unable to create manager: %w", err)
	}

	informer, err := mgr.GetCache().GetInformer(ctx, &agentsv1alpha1.Sandbox{})
	if err != nil {
		return fmt.Errorf("get gateway Sandbox informer: %w", err)
	}
	handler := &routeEventHandler{registry: options.Registry}
	registration, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			handler.onObject(ctx, obj)
		},
		UpdateFunc: func(_, newObj any) {
			handler.onObject(ctx, newObj)
		},
		DeleteFunc: func(obj any) {
			handler.onDelete(ctx, obj)
		},
	})
	if err != nil {
		return fmt.Errorf("register gateway Sandbox informer handler: %w", err)
	}
	defer func() {
		if removeErr := informer.RemoveEventHandler(registration); removeErr != nil {
			log.FromContext(ctx).Error(removeErr, "failed to remove gateway Sandbox informer handler")
		}
	}()

	if err := addJWTAuthManager(mgr.GetAPIReader(), mgr.Add, options.JWTAuthManager); err != nil {
		return err
	}

	if err := mgr.Add(manager.RunnableFunc(func(runCtx context.Context) error {
		if !toolscache.WaitForCacheSync(runCtx.Done(), registration.HasSynced) {
			return nil
		}
		options.Registry.SetReady(true)
		defer options.Registry.SetReady(false)
		<-runCtx.Done()
		return nil
	})); err != nil {
		return fmt.Errorf("register gateway route readiness: %w", err)
	}

	return mgr.Start(ctx)
}

func buildGatewayCacheOptions(namespace, labelSelector string) (ctrlcache.Options, error) {
	sandboxConfig := ctrlcache.ByObject{Transform: stripSandboxCacheFields}
	if namespace != "" {
		sandboxConfig.Namespaces = map[string]ctrlcache.Config{
			namespace: {},
		}
	}
	if labelSelector != "" {
		selector, err := labels.Parse(labelSelector)
		if err != nil {
			return ctrlcache.Options{}, fmt.Errorf("parse sandbox label selector: %w", err)
		}
		sandboxConfig.Label = selector
	}
	return ctrlcache.Options{
		ByObject: map[client.Object]ctrlcache.ByObject{
			&agentsv1alpha1.Sandbox{}: sandboxConfig,
		},
	}, nil
}

func addJWTAuthManager(reader client.Reader, add func(manager.Runnable) error, jwtAuthManager *jwtauth.Manager) error {
	if jwtAuthManager == nil {
		return nil
	}
	if err := jwtAuthManager.SetReader(reader); err != nil {
		return fmt.Errorf("unable to configure JWT authentication reader: %w", err)
	}
	if err := add(jwtAuthManager); err != nil {
		return fmt.Errorf("unable to add JWT authentication manager: %w", err)
	}
	return nil
}
