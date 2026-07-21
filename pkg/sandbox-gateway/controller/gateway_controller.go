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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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

// LegacyFallback resolves the mixed-version compatibility ID for an ObjectKey.
type LegacyFallback func(namespace, name string) string

// InclusionFunc applies gateway-specific state and visibility policy.
type InclusionFunc func(sandbox *agentsv1alpha1.Sandbox, state string) bool

// SandboxPolicy is shared by the watch feeder and direct-reader observer.
type SandboxPolicy struct {
	Namespace string
	Selector  labels.Selector
	Include   InclusionFunc
}

// NewSandboxPolicy builds the gateway visibility and inclusion policy.
func NewSandboxPolicy(namespace, labelSelector string, include InclusionFunc) (SandboxPolicy, error) {
	selector := labels.Everything()
	if labelSelector != "" {
		parsed, err := labels.Parse(labelSelector)
		if err != nil {
			return SandboxPolicy{}, fmt.Errorf("parse sandbox label selector: %w", err)
		}
		selector = parsed
	}
	if include == nil {
		include = func(*agentsv1alpha1.Sandbox, string) bool { return true }
	}
	return SandboxPolicy{Namespace: namespace, Selector: selector, Include: include}, nil
}

// ManagerOptions supplies gateway composition dependencies.
type ManagerOptions struct {
	Registry        *registry.Registry
	LegacyFallback  LegacyFallback
	Namespace       string
	LabelSelector   string
	Include         InclusionFunc
	RepairerOptions sandboxroute.RepairerOptions
	JWTAuthManager  *jwtauth.Manager
}

// SandboxReconciler reconciles Sandbox objects into the gateway route Store.
type SandboxReconciler struct {
	client.Client
	Registry       *registry.Registry
	LegacyFallback LegacyFallback
	Policy         SandboxPolicy
}

// Reconcile applies one informer observation without blocking on direct repair reads.
func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	observation, err := r.observe(ctx, r.Client, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, err
	}

	var result sandboxroute.MutationResult
	if observation.Present {
		result, err = r.Registry.Upsert(observation.Route)
		if err != nil {
			return ctrl.Result{}, err
		}
		logger.V(utils.DebugLogLevel).Info(
			"offered sandbox route to gateway registry",
			"sandboxID", observation.Route.ID,
			"namespace", observation.Route.Namespace,
			"name", observation.Route.Name,
			"resourceVersion", observation.Route.ResourceVersion,
			"result", result.Result,
			"reason", result.Reason,
		)
	} else {
		result, err = r.Registry.DeleteAuthoritativeByObjectKey(
			req.NamespacedName,
			r.LegacyFallback(req.Namespace, req.Name),
		)
		if err != nil {
			return ctrl.Result{}, err
		}
		logger.V(utils.DebugLogLevel).Info(
			"removed absent sandbox route from gateway registry",
			"namespace", req.Namespace,
			"name", req.Name,
			"result", result.Result,
			"reason", result.Reason,
		)
	}
	if result.Result == sandboxroute.EventResultInvalid {
		return ctrl.Result{}, fmt.Errorf("gateway route mutation rejected: %s", result.Reason)
	}
	return ctrl.Result{}, nil
}

func (r *SandboxReconciler) observe(
	ctx context.Context,
	reader client.Reader,
	key types.NamespacedName,
) (sandboxroute.AuthoritativeObservation, error) {
	var sandbox agentsv1alpha1.Sandbox
	if err := reader.Get(ctx, key, &sandbox); err != nil {
		if apierrors.IsNotFound(err) {
			return sandboxroute.AuthoritativeObservation{}, nil
		}
		return sandboxroute.AuthoritativeObservation{}, sandboxroute.NewGetObservationError(err)
	}
	return r.observeSandbox(&sandbox)
}

func (r *SandboxReconciler) observeSandbox(
	sandbox *agentsv1alpha1.Sandbox,
) (sandboxroute.AuthoritativeObservation, error) {
	if sandbox.DeletionTimestamp != nil || !r.visible(sandbox) {
		return sandboxroute.AuthoritativeObservation{}, nil
	}

	source := newGatewayProjectionSource(sandbox)
	route, err := sandboxroute.ProjectRoute(source)
	if err != nil {
		return sandboxroute.AuthoritativeObservation{}, sandboxroute.NewProjectionObservationError(err)
	}
	if !r.Policy.Include(sandbox, route.State) {
		return sandboxroute.AuthoritativeObservation{}, nil
	}
	if err := route.Validate(); err != nil {
		return sandboxroute.AuthoritativeObservation{}, sandboxroute.NewProjectionObservationError(err)
	}
	return sandboxroute.AuthoritativeObservation{Present: true, Route: route}, nil
}

func (r *SandboxReconciler) visible(sandbox *agentsv1alpha1.Sandbox) bool {
	if r.Policy.Namespace != "" && sandbox.Namespace != r.Policy.Namespace {
		return false
	}
	return r.Policy.Selector.Matches(labels.Set(sandbox.Labels))
}

func (r *SandboxReconciler) validate() error {
	if r.Client == nil {
		return errors.New("gateway reconciler client must not be nil")
	}
	if r.Registry == nil {
		return errors.New("gateway reconciler Registry must not be nil")
	}
	if r.LegacyFallback == nil {
		return errors.New("gateway reconciler legacy fallback must not be nil")
	}
	if r.Policy.Selector == nil || r.Policy.Include == nil {
		return errors.New("gateway reconciler policy must be initialized")
	}
	return nil
}

// StartManager starts the gateway Sandbox watch and targeted direct-reader repairer.
func StartManager(ctx context.Context, options ManagerOptions) error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	if options.Registry != nil {
		options.Registry.SetRepairEnqueuer(nil)
		defer options.Registry.SetRepairEnqueuer(nil)
	}
	policy, err := NewSandboxPolicy(options.Namespace, options.LabelSelector, options.Include)
	if err != nil {
		return err
	}
	if options.Registry == nil || options.LegacyFallback == nil {
		return errors.New("gateway manager route dependencies must not be nil")
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

	reconciler := &SandboxReconciler{
		Client:         mgr.GetClient(),
		Registry:       options.Registry,
		LegacyFallback: options.LegacyFallback,
		Policy:         policy,
	}
	if err := reconciler.validate(); err != nil {
		return err
	}
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.Sandbox{}).
		Complete(reconciler); err != nil {
		return fmt.Errorf("unable to create controller: %w", err)
	}
	if err := addJWTAuthManager(mgr.GetAPIReader(), mgr.Add, options.JWTAuthManager); err != nil {
		return err
	}

	repairer, err := sandboxroute.NewRepairer(
		options.Registry.Store(),
		func(ctx context.Context, key types.NamespacedName) (sandboxroute.AuthoritativeObservation, error) {
			return reconciler.observe(ctx, mgr.GetAPIReader(), key)
		},
		options.RepairerOptions,
	)
	if err != nil {
		return fmt.Errorf("create gateway route Repairer: %w", err)
	}
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		options.Registry.SetRepairEnqueuer(repairer.Enqueue)
		defer options.Registry.SetRepairEnqueuer(nil)
		return repairer.Start(ctx)
	})); err != nil {
		return fmt.Errorf("register gateway route Repairer: %w", err)
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
