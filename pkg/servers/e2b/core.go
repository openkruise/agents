/*
Copyright 2025.

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
	"github.com/openkruise/agents/pkg/cache"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
)

// Controller handles sandbox-related operations
type Controller struct {
	port                  int
	maxTimeout            int
	minResumeTimeoutValue int

	// manager params
	systemNamespace       string // the namespace where the sandbox manager is running
	peerSelector          string
	maxClaimWorkers       int
	maxCreateQPS          int
	extProcMaxConcurrency uint32
	sandboxLabelSelector  string
	sandboxNamespace      string
	memberlistBindPort    int
	keyCfg                *keys.Config
	quotaOpts             config.QuotaOptions

	// fields
	mux             *http.ServeMux
	server          *http.Server
	stop            chan os.Signal
	cache           cache.Provider
	storageRegistry storages.VolumeMountProviderRegistry
	clientConfig    *rest.Config
	domain          string
	adapter         *adapters.E2BAdapter
	manager         *sandboxmanager.SandboxManager
	keys            keys.KeyStorage
}

// NewController creates a new E2B Controller
func NewController(domain, sysNs, peerSelector, sandboxNamespace, sandboxLabelSelector string, maxTimeout, minResumeTimeout, maxClaimWorkers, maxCreateQPS int, extProcMaxConcurrency uint32, port, memberlistBindPort int, keyCfg *keys.Config, clientConfig *rest.Config, quotaOpts config.QuotaOptions) *Controller {
	sc := &Controller{
		mux:                   http.NewServeMux(),
		domain:                domain,
		adapter:               adapters.DefaultAdapterFactory(port),
		clientConfig:          clientConfig,
		port:                  port,
		maxTimeout:            maxTimeout,
		minResumeTimeoutValue: minResumeTimeout,
		systemNamespace:       sysNs, // the namespace where the sandbox manager is running
		peerSelector:          peerSelector,
		sandboxNamespace:      sandboxNamespace,
		sandboxLabelSelector:  sandboxLabelSelector,
		maxClaimWorkers:       maxClaimWorkers,
		maxCreateQPS:          maxCreateQPS,
		extProcMaxConcurrency: extProcMaxConcurrency,
		memberlistBindPort:    memberlistBindPort,
		keyCfg:                keyCfg,
		quotaOpts:             quotaOpts,
	}

	sc.server = &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           sc.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	return sc
}

func (sc *Controller) Init() error {
	ctx := logs.NewContext()
	log := klog.FromContext(ctx)
	log.Info("init controller")

	sandboxManager, err := sandboxmanager.NewSandboxManagerBuilder(sc.sandboxManagerOptions()).
		WithSandboxInfra().
		WithMemberlistPeers().
		WithRequestAdapter(sc.adapter).
		Build()

	if err != nil {
		return err
	}

	sc.manager = sandboxManager
	sc.cache = sandboxManager.GetInfra().GetCache()
	sc.storageRegistry = storages.NewStorageProvider()
	sc.registerRoutes()

	if err := sc.initKeyStorage(ctx); err != nil {
		return err
	}

	// Initialize quota through the sandbox-manager, which owns the runtime lifecycle.
	if sc.keys != nil {
		log.Info("will init quota management with quota options")
		if err := sc.manager.InitQuota(ctx, sc.quotaOpts, keys.NewQuotaSubjectLister(sc.keys)); err != nil {
			return err
		}
	} else {
		log.Info("api-key quota is unenforced because E2B auth is disabled")
		if err := sc.manager.InitQuota(ctx, config.QuotaOptions{}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (sc *Controller) sandboxManagerOptions() config.SandboxManagerOptions {
	return config.SandboxManagerOptions{
		SystemNamespace:       sc.systemNamespace,
		PeerSelector:          sc.peerSelector,
		SandboxNamespace:      sc.sandboxNamespace,
		SandboxLabelSelector:  sc.sandboxLabelSelector,
		MaxClaimWorkers:       sc.maxClaimWorkers,
		ExtProcMaxConcurrency: sc.extProcMaxConcurrency,
		MaxCreateQPS:          sc.maxCreateQPS,
		MemberlistBindPort:    sc.memberlistBindPort,
		RestConfig:            sc.clientConfig,
		Quota:                 sc.quotaOpts,
	}
}

func (sc *Controller) initKeyStorage(ctx context.Context) error {
	// Initialize key storage if key config is provided
	if sc.keyCfg != nil {
		var err error
		if sc.cache != nil {
			sc.keyCfg.Client = sc.cache.GetClient()
			sc.keyCfg.APIReader = sc.cache.GetAPIReader()
			sc.keyCfg.Cache = sc.cache.GetCache()
		}
		if sc.keys, err = keys.NewKeyStorage(*sc.keyCfg); err != nil {
			return err
		}
		if err = sc.keys.Init(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (sc *Controller) Run() (context.Context, error) {
	if sc.stop != nil {
		return nil, errors.New("controller already started")
	}
	ctx, cancel := context.WithCancel(logs.NewContext())
	// Channel to listen for interrupt signal
	sc.stop = make(chan os.Signal, 1)
	signal.Notify(sc.stop, syscall.SIGINT, syscall.SIGTERM)
	if err := sc.manager.Run(ctx); err != nil {
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
		shutdownCtx, shutdownCancel := context.WithTimeout(logs.NewContext("action", "shutdown"), consts.ShutdownTimeout)
		defer shutdownCancel()
		sc.shutdown(shutdownCtx, cancel)
	}()

	if sc.keys != nil {
		sc.keys.Run()
	}
	return ctx, nil
}

func (sc *Controller) shutdown(ctx context.Context, cancel context.CancelFunc) {
	log := klog.FromContext(ctx)
	log.Info("Shutting down server...")
	defer cancel()

	if sc.server != nil {
		if err := sc.server.Shutdown(ctx); err != nil {
			klog.ErrorS(err, "HTTP server forced to shutdown")
		}
	}
	if sc.manager != nil {
		sc.manager.Stop(ctx)
	}
	if sc.keys != nil {
		sc.keys.Stop()
	}
	klog.InfoS("Server exited")
}
