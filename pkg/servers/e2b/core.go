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
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/cache"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/utils/network"
	utilruntime "github.com/openkruise/agents/pkg/utils/runtime"
)

// Controller handles sandbox-related operations
type Controller struct {
	// E2B API surface
	maxTimeout int
	domain     string
	keyCfg     *keys.Config

	// mgrOpts is handed to the sandbox-manager builder unchanged. It also carries
	// the system namespace the API handlers fall back to, so it is the single
	// place a manager-level knob has to be threaded through.
	mgrOpts config.SandboxManagerOptions
	// runtimeTLSBundle is the client TLS bundle for reaching TLS-capable
	// agent-runtimes; nil disables runtime TLS for this manager, so every
	// sandbox is served over the legacy plaintext paths.
	runtimeTLSBundle *utilruntime.TLSBundle

	// fields
	mux             *http.ServeMux
	server          *http.Server
	metricsServer   *http.Server
	cache           cache.Provider
	storageRegistry storages.VolumeMountProviderRegistry
	adapter         *adapters.E2BAdapter
	manager         *sandboxmanager.SandboxManager
	keys            keys.KeyStorage
}

// ControllerOptions carries everything NewController needs. Passing a struct
// instead of a long positional parameter list keeps every value named at the
// call site, so adding a knob cannot silently shift an argument.
type ControllerOptions struct {
	// Domain is the static E2B domain. When empty the domain is resolved per
	// request from the HTTP Host header.
	Domain string
	// Port is the port the E2B HTTP server listens on.
	Port int
	// MetricsPort is the port for GET /metrics. 0 or the same value as Port
	// serves it on the control API listener; any other positive port starts a
	// dedicated observability listener. Negative values are invalid and
	// rejected by startup validation.
	MetricsPort int
	// MaxTimeout is the E2B maximum sandbox timeout in seconds.
	MaxTimeout int
	// KeyConfig configures API key storage. Nil disables E2B authentication.
	KeyConfig *keys.Config

	// Manager is passed to the sandbox-manager builder unchanged.
	Manager config.SandboxManagerOptions
	// RuntimeTLSBundle is the client TLS bundle used to reach TLS-capable
	// agent-runtimes during claim and clone post-processing. Nil keeps every
	// runtime call on the legacy plaintext paths.
	RuntimeTLSBundle *utilruntime.TLSBundle
}

// NewController creates a new E2B Controller from opts.
func NewController(opts ControllerOptions) *Controller {
	sc := &Controller{
		mux:              http.NewServeMux(),
		domain:           opts.Domain,
		adapter:          adapters.DefaultAdapterFactory(opts.Port, opts.Manager.BindAddress),
		maxTimeout:       opts.MaxTimeout,
		keyCfg:           opts.KeyConfig,
		mgrOpts:          opts.Manager,
		runtimeTLSBundle: opts.RuntimeTLSBundle,
	}

	sc.server = &http.Server{
		Addr:              network.ListenAddress(opts.Manager.BindAddress, opts.Port),
		Handler:           sc.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if opts.MetricsPort > 0 && opts.MetricsPort != opts.Port {
		metricsMux := http.NewServeMux()
		registerObservabilityRoutes(metricsMux)
		sc.metricsServer = &http.Server{
			Addr:              fmt.Sprintf(":%d", opts.MetricsPort),
			Handler:           metricsMux,
			ReadHeaderTimeout: 5 * time.Second,
		}
	} else {
		registerObservabilityRoutes(sc.mux)
	}

	return sc
}

func (sc *Controller) Init() error {
	ctx := logs.NewContext()
	log := klog.FromContext(ctx)
	log.Info("init controller")

	sandboxManager, err := sandboxmanager.NewSandboxManagerBuilder(sc.mgrOpts).
		WithSandboxInfra().
		WithMemberlistPeers().
		WithRequestAdapter(sc.adapter).
		WithRuntimeTLSBundle(sc.runtimeTLSBundle).
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
		if err := sc.manager.InitQuota(ctx, sc.mgrOpts.Quota, keys.NewQuotaSubjectLister(sc.keys)); err != nil {
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

// Run starts the controller in two phases.
//
// stop delivers the termination signal and must be buffered (capacity >= 1)
// so a signal racing startup is not dropped by a non-blocking sender.
//
// The returned context is canceled when the controller is done: immediately
// after an interrupted or failed startup, and only after the graceful
// shutdown chain completes in steady state.
//
// An interrupted startup skips memberlist Leave and leader lease release;
// peers converge through the memberlist suspicion timeout (a few seconds with
// the tuned probe settings) and the lease expires via TTL. Both self-heal
// without operator action. Upgrade path: run the shutdown chain in the
// interrupted branch once graceful leaf cleanup during startup is required.
func (sc *Controller) Run(stop <-chan os.Signal) (context.Context, error) {
	ctx, cancel := context.WithCancel(logs.NewContext())

	startupDone := make(chan error, 1)
	go func() {
		startupDone <- sc.startComponents(ctx)
	}()

	outcome, err := awaitStartup(startupDone, stop)
	switch outcome {
	case startupInterrupted:
		klog.FromContext(ctx).Info("termination signal during startup, exiting without graceful cleanup")
		cancel()
		return ctx, nil
	case startupFailed:
		klog.FromContext(ctx).Error(err, "startup failed, exiting")
		cancel()
		return ctx, err
	}

	// Steady state: a signal triggers the graceful shutdown chain. A signal
	// that awaitStartup already consumed alongside a completed startup starts
	// the chain right away.
	go func() {
		if outcome != startupSignaled {
			<-stop
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(logs.NewContext("action", "shutdown"), consts.ShutdownTimeout)
		defer shutdownCancel()
		sc.shutdown(shutdownCtx, cancel)
	}()
	return ctx, nil
}

// startComponents runs the sequential startup pipeline in the background so
// Run can race it against termination signals. Key storage starts last: once
// startComponents reports success, shutdown may assume keys.Run has run and
// pair it with keys.Stop.
func (sc *Controller) startComponents(ctx context.Context) error {
	if err := sc.manager.Run(ctx); err != nil {
		return fmt.Errorf("sandbox manager failed to start: %w", err)
	}
	if err := sc.startHTTPServer(); err != nil {
		return err
	}
	if sc.metricsServer != nil {
		go serveMetrics(sc.metricsServer)
	}
	if sc.keys != nil {
		sc.keys.Run()
	}
	return nil
}

// startupOutcome classifies how controller startup ended.
type startupOutcome int

const (
	// startupCompleted: startup succeeded and no termination signal was seen.
	startupCompleted startupOutcome = iota
	// startupSignaled: startup succeeded and a termination signal was
	// consumed alongside it; the caller must start graceful shutdown itself.
	startupSignaled
	startupFailed
	// startupInterrupted: a termination signal preempted an unfinished startup.
	startupInterrupted
)

// awaitStartup races startup completion against a termination signal. A
// startup result that is already available when the signal is observed still
// wins, so a fully started controller is never torn down with crash
// semantics; only an unfinished startup is interrupted.
func awaitStartup(startupDone <-chan error, stop <-chan os.Signal) (startupOutcome, error) {
	select {
	case err := <-startupDone:
		return classifyStartup(err, startupCompleted)
	case <-stop:
		select {
		case err := <-startupDone:
			return classifyStartup(err, startupSignaled)
		default:
			return startupInterrupted, nil
		}
	}
}

func classifyStartup(err error, success startupOutcome) (startupOutcome, error) {
	if err != nil {
		return startupFailed, err
	}
	return success, nil
}

func (sc *Controller) startHTTPServer() error {
	listener, err := net.Listen("tcp", sc.server.Addr)
	if err != nil {
		return fmt.Errorf("listen for E2B API on %s: %w", sc.server.Addr, err)
	}

	go func() {
		klog.InfoS("Starting Server", "address", listener.Addr().String())
		if err := sc.server.Serve(listener); err != nil &&
			!errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			klog.Fatalf("HTTP server failed: %v", err)
		}
	}()
	return nil
}

func serveMetrics(server *http.Server) {
	klog.InfoS("Starting metrics server", "address", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		// Metrics live only on this listener once a dedicated port is configured,
		// so a bind failure is a fatal misconfiguration, matching the control API listener.
		klog.Fatalf("metrics HTTP server failed to start: %v", err)
	}
}

func shutdownHTTPServer(ctx context.Context, srv *http.Server, msg string) {
	if srv == nil {
		return
	}
	if err := srv.Shutdown(ctx); err != nil {
		klog.ErrorS(err, msg)
	}
}

func (sc *Controller) shutdown(ctx context.Context, cancel context.CancelFunc) {
	log := klog.FromContext(ctx)
	log.Info("Shutting down server...")
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		shutdownHTTPServer(ctx, sc.server, "HTTP server forced to shutdown")
	}()
	go func() {
		defer wg.Done()
		shutdownHTTPServer(ctx, sc.metricsServer, "metrics HTTP server forced to shutdown")
	}()
	wg.Wait()
	if sc.manager != nil {
		sc.manager.Stop(ctx)
	}
	// shutdown runs only after startup reported success, so keys.Run has been
	// called and keys.Stop cannot block on a worker that never started
	// (secretKeyStorage.Stop waits for its done channel).
	if sc.keys != nil {
		sc.keys.Stop()
	}
	klog.InfoS("Server exited")
}
