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

	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	sandbox_manager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
)

// Controller handles sandbox-related operations
type Controller struct {
	port       int
	maxTimeout int

	// manager params
	systemNamespace       string // the namespace where the sandbox manager is running
	maxClaimWorkers       int
	maxCreateQPS          int
	extProcMaxConcurrency uint32
	sandboxLabelSelector  string
	sandboxNamespace      string
	memberlistBindPort    int

	// fields
	mux             *http.ServeMux
	server          *http.Server
	stop            chan os.Signal
	client          *clients.ClientSet
	cache           infra.CacheProvider
	storageRegistry storages.VolumeMountProviderRegistry
	clientConfig    *rest.Config
	domain          string
	manager         *sandbox_manager.SandboxManager
	keys            keys.KeyStorage
}

// NewController creates a new E2B Controller.
// When keyCfg is not nil, authentication is enabled and the provided config is used to initialize key storage.
func NewController(domain string, sysNs, sandboxNamespace, sandboxLabelSelector string, maxTimeout, maxClaimWorkers, maxCreateQPS int, extProcMaxConcurrency uint32,
	port, memberlistBindPort int, keyCfg *keys.Config, clientSet *clients.ClientSet) *Controller {
	sc := &Controller{
		mux:                   http.NewServeMux(),
		client:                clientSet,
		domain:                domain,
		clientConfig:          clientSet.Config,
		port:                  port,
		maxTimeout:            maxTimeout,
		systemNamespace:       sysNs, // the namespace where the sandbox manager is running
		sandboxNamespace:      sandboxNamespace,
		sandboxLabelSelector:  sandboxLabelSelector,
		maxClaimWorkers:       maxClaimWorkers,
		maxCreateQPS:          maxCreateQPS,
		extProcMaxConcurrency: extProcMaxConcurrency,
		memberlistBindPort:    memberlistBindPort,
	}

	sc.server = &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           sc.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if keyCfg != nil {
		storage, err := keys.NewKeyStorage(*keyCfg)
		if err != nil {
			klog.Fatalf("Failed to initialize key storage: %v", err)
		}
		sc.keys = storage
	}
	return sc
}

func (sc *Controller) Init() error {
	ctx := logs.NewContext()
	log := klog.FromContext(ctx)
	log.Info("init controller")
	adapter := adapters.DefaultAdapterFactory(sc.port)
	sandboxManager, err := sandbox_manager.NewSandboxManager(sc.client, adapter, config.SandboxManagerOptions{
		SystemNamespace:       sc.systemNamespace,
		SandboxNamespace:      sc.sandboxNamespace,
		SandboxLabelSelector:  sc.sandboxLabelSelector,
		MaxClaimWorkers:       sc.maxClaimWorkers,
		ExtProcMaxConcurrency: sc.extProcMaxConcurrency,
		MaxCreateQPS:          sc.maxCreateQPS,
		MemberlistBindPort:    sc.memberlistBindPort,
	})
	if err != nil {
		return err
	}

	infraWithCache := sandboxManager.GetInfra()
	if infraWithCache != nil {
		sc.cache = infraWithCache.GetCache()
	}
	sc.manager = sandboxManager
	sc.storageRegistry = storages.NewStorageProvider()
	sc.registerRoutes()
	if sc.keys == nil {
		return nil
	}

	return sc.keys.Init(ctx)
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
		shutdownCtx, shutdownCancel := context.WithTimeout(logs.NewContext("action", "shutdown"), consts.ShutdownTimeout)
		log := klog.FromContext(shutdownCtx)
		log.Info("Shutting down server...")
		defer cancel()
		sc.manager.Stop(shutdownCtx)
		// Shutdown HTTP server with timeout
		defer shutdownCancel()
		if err := sc.server.Shutdown(shutdownCtx); err != nil {
			klog.ErrorS(err, "HTTP server forced to shutdown")
		}
		if sc.keys != nil {
			sc.keys.Stop()
		}
		klog.InfoS("Server exited")
	}()

	if sc.keys != nil {
		sc.keys.Run()
	}
	return ctx, nil
}
