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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-gateway/jwtauth"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/utils"
)

// InclusionFunc applies gateway-specific state and visibility policy.
type InclusionFunc func(sandbox *agentsv1alpha1.Sandbox, state string) bool

// SandboxPolicy applies gateway route visibility and inclusion rules.
type SandboxPolicy struct {
	Namespace string
	Selector  labels.Selector
	Include   InclusionFunc
}

// NewSandboxPolicy builds the gateway visibility and inclusion policy.
func NewSandboxPolicy(namespace, labelSelector string, include InclusionFunc) (SandboxPolicy, error) {
	selector, err := labels.Parse(labelSelector)
	if err != nil {
		return SandboxPolicy{}, fmt.Errorf("parse sandbox label selector: %w", err)
	}
	if include == nil {
		include = func(*agentsv1alpha1.Sandbox, string) bool { return true }
	}
	return SandboxPolicy{Namespace: namespace, Selector: selector, Include: include}, nil
}

// ManagerOptions supplies gateway composition dependencies.
type ManagerOptions struct {
	Registry       *registry.Registry
	Namespace      string
	LabelSelector  string
	Include        InclusionFunc
	JWTAuthManager *jwtauth.Manager
}

type routeEventHandler struct {
	registry *registry.Registry
	policy   SandboxPolicy
}

func (h *routeEventHandler) onObject(ctx context.Context, obj any) {
	sandbox, ok := obj.(*agentsv1alpha1.Sandbox)
	if !ok {
		log.FromContext(ctx).Error(
			nil,
			"discarding unexpected gateway route informer object",
			"type", fmt.Sprintf("%T", obj),
		)
		return
	}

	key := types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Name}
	deletion := sandboxroute.Delete{
		ObjectKey:       key,
		ResourceVersion: sandbox.ResourceVersion,
	}
	if sandbox.DeletionTimestamp != nil {
		h.logMutation(ctx, "delete", key, h.registry.Delete(deletion))
		return
	}
	if !h.visible(sandbox) {
		h.logMutation(ctx, "delete_if_tracked", key, h.registry.DeleteIfTracked(deletion))
		return
	}

	route, err := sandboxroute.ProjectRoute(newGatewayProjectionSource(sandbox))
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to project gateway route", "namespace", key.Namespace, "name", key.Name)
		return
	}
	if !h.policy.Include(sandbox, route.State) {
		h.logMutation(ctx, "delete_if_tracked", key, h.registry.DeleteIfTracked(deletion))
		return
	}
	h.logMutation(ctx, "upsert", key, h.registry.Upsert(route))
}

func (h *routeEventHandler) onDelete(ctx context.Context, obj any) {
	var deletion sandboxroute.Delete
	switch value := obj.(type) {
	case *agentsv1alpha1.Sandbox:
		deletion = sandboxroute.Delete{
			ObjectKey: types.NamespacedName{
				Namespace: value.Namespace,
				Name:      value.Name,
			},
			ResourceVersion: value.ResourceVersion,
		}
	case toolscache.DeletedFinalStateUnknown:
		namespace, name, err := toolscache.SplitMetaNamespaceKey(value.Key)
		if err != nil || namespace == "" || name == "" {
			log.FromContext(ctx).Error(err, "discarding invalid gateway route tombstone", "key", value.Key)
			return
		}
		deletion.ObjectKey = types.NamespacedName{Namespace: namespace, Name: name}
	default:
		log.FromContext(ctx).Error(
			nil,
			"discarding unexpected gateway route delete object",
			"type", fmt.Sprintf("%T", obj),
		)
		return
	}
	h.logMutation(ctx, "delete", deletion.ObjectKey, h.registry.Delete(deletion))
}

func (h *routeEventHandler) visible(sandbox *agentsv1alpha1.Sandbox) bool {
	if h.policy.Namespace != "" && sandbox.Namespace != h.policy.Namespace {
		return false
	}
	return h.policy.Selector.Matches(labels.Set(sandbox.Labels))
}

func (h *routeEventHandler) logMutation(
	ctx context.Context,
	operation string,
	key types.NamespacedName,
	result sandboxroute.MutationResult,
) {
	logger := log.FromContext(ctx)
	if result.Result == sandboxroute.EventResultInvalid {
		logger.Error(
			errors.New(string(result.Reason)),
			"gateway route mutation rejected",
			"operation", operation,
			"namespace", key.Namespace,
			"name", key.Name,
		)
		return
	}
	logger.V(utils.DebugLogLevel).Info(
		"gateway route mutation completed",
		"operation", operation,
		"namespace", key.Namespace,
		"name", key.Name,
		"result", result.Result,
		"reason", result.Reason,
	)
}

// StartManager starts the gateway Sandbox informer route feed.
func StartManager(ctx context.Context, options ManagerOptions) error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	if options.Registry == nil {
		return errors.New("gateway manager route dependencies must not be nil")
	}
	options.Registry.SetReady(false)
	defer options.Registry.SetReady(false)

	policy, err := NewSandboxPolicy(options.Namespace, options.LabelSelector, options.Include)
	if err != nil {
		return err
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
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

	informer, err := mgr.GetCache().GetInformer(ctx, &agentsv1alpha1.Sandbox{})
	if err != nil {
		return fmt.Errorf("get gateway Sandbox informer: %w", err)
	}
	handler := &routeEventHandler{registry: options.Registry, policy: policy}
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
