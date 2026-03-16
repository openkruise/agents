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
)

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
			registry.Delete(key)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if sandbox.DeletionTimestamp != nil {
		logger.Info("sandbox being deleted, removing from registry", "key", key)
		registry.Delete(key)
		return ctrl.Result{}, nil
	}

	podIP := sandbox.Status.PodInfo.PodIP
	if podIP == "" {
		logger.V(1).Info("sandbox has no pod IP yet, skipping", "key", key)
		return ctrl.Result{}, nil
	}

	logger.Info("updating registry", "key", key, "podIP", podIP)
	registry.Update(key, podIP)
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
