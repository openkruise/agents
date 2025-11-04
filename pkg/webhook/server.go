package webhook

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type GateFunc func() (enabled bool)

var (
	// HandlerMap contains all admission webhook handlers.
	HandlerMap = map[string]admission.Handler{
		// +kubebuilder:webhook:path=/validate-agents-kruise-io-sandbox,mutating=false,failurePolicy=fail,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups=agents.kruise.io,resources=sandbox,verbs=create;update,versions=v1alpha1,name=vsandbox.kb.io
		"validate-agents-kruise-io-sandbox": &SandboxCreateUpdateHandler{},
	}
	handlerGates = map[string]GateFunc{}
)

func init() {
	addHandlers(HandlerMap)
}

func addHandlers(m map[string]admission.Handler) {
	addHandlersWithGate(m, nil)
}

func addHandlersWithGate(m map[string]admission.Handler, fn GateFunc) {
	for path, handler := range m {
		if len(path) == 0 {
			klog.Warningf("Skip handler with empty path.")
			continue
		}
		if path[0] != '/' {
			path = "/" + path
		}
		_, found := HandlerMap[path]
		if found {
			klog.V(1).Infof("conflicting webhook builder path %v in handler map", path)
		}
		klog.Infof("add webhook path(%s)", path)
		HandlerMap[path] = handler
		if fn != nil {
			handlerGates[path] = fn
		}
	}
}

func filterActiveHandlers() {
	disablePaths := sets.NewString()
	for path := range HandlerMap {
		if fn, ok := handlerGates[path]; ok {
			if !fn() {
				disablePaths.Insert(path)
			}
		}
	}
	for _, path := range disablePaths.List() {
		delete(HandlerMap, path)
	}
}

func SetupWithManager(mgr manager.Manager) error {
	server := mgr.GetWebhookServer()

	// register admission handlers
	filterActiveHandlers()
	for path, handler := range HandlerMap {
		server.Register(path, &webhook.Admission{Handler: handler})
		klog.V(3).Infof("Registered webhook handler %s", path)
	}
	err := initialize(context.TODO(), mgr.GetConfig())
	if err != nil {
		return err
	}
	klog.Infof("webhook init done")
	return nil
}

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete,namespace=sandbox-system
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingwebhookconfigurations,verbs=get;list;watch;update;patch
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
