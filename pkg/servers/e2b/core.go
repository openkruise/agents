package e2b

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	adapters2 "github.com/openkruise/agents/pkg/servers/e2b/adapters"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

var DebugLevel = 5

// Controller handles sandbox-related operations
type Controller struct {
	port         int
	mux          *http.ServeMux
	server       *http.Server
	stop         chan os.Signal
	client       *clients.ClientSet
	clientConfig *rest.Config
	domain       string
	manager      *sandbox_manager.SandboxManager
	keys         *keys.SecretKeyStorage
}

// NewController creates a new Controller
//
//	Params:
//	- domain: The domain name of the sandbox service, a TLS cert is required, e.g.
//	- listenAddr: The address to listen on, e.g. ":8080"
func NewController(domain, adminKey string, port int, enableAuth bool, clientSet *clients.ClientSet) *Controller {
	sc := &Controller{
		mux:          http.NewServeMux(),
		client:       clientSet,
		domain:       domain,
		clientConfig: clientSet.Config,
		port:         port,
	}

	sc.server = &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           sc.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if enableAuth {
		sc.keys = &keys.SecretKeyStorage{
			Namespace: Namespace,
			AdminKey:  adminKey,
			Client:    clientSet.K8sClient,
			Stop:      make(chan struct{}),
		}
	}
	return sc
}

func (sc *Controller) Init(infrastructure string) error {
	ctx := logs.NewContext()
	log := klog.FromContext(ctx)
	log.Info("init controller", "infra", infrastructure)
	var adapter proxy.RequestAdapter
	baseAdapter := &adapters2.CommonAdapter{Port: sc.port, Keys: sc.keys}
	if infrastructure == consts.InfraMicroVM {
		adapter = &adapters2.MicroVMAdapter{CommonAdapter: baseAdapter}
	} else {
		adapter = baseAdapter
	}
	sandboxManager, err := sandbox_manager.NewSandboxManager(Namespace, sc.client, adapter, infrastructure)
	if err != nil {
		return err
	}
	sc.manager = sandboxManager
	sc.registerRoutes()
	if sc.keys == nil {
		return nil
	} else {
		return sc.keys.Init(ctx)
	}
}

func (sc *Controller) Run(sysNs, peerSelector string) (context.Context, error) {
	if sc.stop != nil {
		return nil, errors.New("controller already started")
	}
	ctx, cancel := context.WithCancel(logs.NewContext())
	// Channel to listen for interrupt signal
	sc.stop = make(chan os.Signal, 1)
	signal.Notify(sc.stop, syscall.SIGINT, syscall.SIGTERM)
	if err := sc.manager.Run(ctx, sysNs, peerSelector); err != nil {
		klog.Fatalf("Sandbox manager failed to start: %v", err)
	}

	// Run HTTP server in a goroutine
	go func() {
		klog.InfoS("Starting Server", "address", sc.server.Addr)
		if err := sc.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			klog.Fatalf("HTTP server failed to start: %v", err)
		}
	}()

	// stopper
	go func() {
		<-sc.stop
		// Shutdown server gracefully
		klog.InfoS("Shutting down server...")
		defer cancel()
		sc.manager.Stop()
		// Shutdown HTTP server
		if err := sc.server.Shutdown(ctx); err != nil {
			klog.InfoS("HTTP server forced to shutdown: %v", err)
		}

		klog.InfoS("Server exited")
	}()
	return ctx, nil
}
