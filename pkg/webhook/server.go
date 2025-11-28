package webhook

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/go-logr/logr"
	"github.com/openkruise/agents/pkg/webhook/pods"
	"github.com/openkruise/agents/pkg/webhook/sandboxset"
	"github.com/openkruise/agents/pkg/webhook/types"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type GateFunc func() (enabled bool)

var (
	// HandlerMap contains all admission webhook handlers.
	HandlerMap     = map[string]admission.Handler{}
	HandlerGetters []types.HandlerGetter
)

func init() {
	HandlerGetters = append(HandlerGetters, pods.GetHandlerGetters()...)
	HandlerGetters = append(HandlerGetters, sandboxset.GetHandlerGetters()...)
}

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete,namespace=sandbox-system
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingwebhookconfigurations,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations,verbs=get;list;watch;update;patch

func SetupWithManager(logger logr.Logger, mgr manager.Manager) error {
	server := mgr.GetWebhookServer()
	for _, getter := range HandlerGetters {
		handler := getter(mgr)
		if !handler.Enabled() {
			logger.Info("Skipped handler for not enabled", "type", reflect.TypeOf(handler).Name())
		} else {
			HandlerMap[handler.Path()] = handler
		}
	}
	// register admission handlers
	for path, handler := range HandlerMap {
		server.Register(path, &webhook.Admission{Handler: handler})
		logger.Info("Registered webhook handler", "path", path)
	}
	ctx := klog.NewContext(context.Background(), logger)
	err := initialize(ctx, mgr.GetConfig())
	if err != nil {
		return err
	}
	logger.Info("webhook init done")
	return nil
}

func initialize(ctx context.Context, cfg *rest.Config) error {
	c, err := New(cfg, HandlerMap)
	if err != nil {
		return err
	}
	go func() {
		c.Start(ctx)
	}()

	timer := time.NewTimer(time.Second * 20)
	defer timer.Stop()
	select {
	case <-Inited():
		return nil
	case <-timer.C:
		return fmt.Errorf("failed to start webhook controller for waiting more than 20s")
	}
}
