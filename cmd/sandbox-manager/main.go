// Package main provides the main entry point for the E2B on Kubernetes server.
package main

import (
	"flag"
	"net/http"         // Added for pprof server
	_ "net/http/pprof" // Added to register pprof handlers

	"github.com/google/uuid"
	"github.com/spf13/pflag"
	zapRaw "go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/servers/e2b"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

func main() {
	// Define variables for pprof configuration
	var enablePprof bool
	var pprofAddr string

	// Define variables for server configuration
	var port int
	var e2bAdminKey string
	var e2bEnableAuth bool
	var domain string
	var e2bMaxTimeout int
	var sysNs string
	var peerSelector string
	var maxClaimWorkers int
	var maxCreateQPS int
	var extProcMaxConcurrency int
	var kubeClientQPS float64
	var kubeClientBurst int

	utilfeature.DefaultMutableFeatureGate.AddFlag(pflag.CommandLine)

	// Register the new pprof flags
	pflag.BoolVar(&enablePprof, "enable-pprof", false, "Enable pprof profiling")
	pflag.StringVar(&pprofAddr, "pprof-addr", ":6060", "The address the pprof debug maps to.")

	// Register server configuration flags
	pflag.IntVar(&port, "port", 8080, "The port the server listens on")
	pflag.StringVar(&e2bAdminKey, "e2b-admin-key", "", "E2B admin API key (if empty, a random UUID will be generated)")
	pflag.BoolVar(&e2bEnableAuth, "e2b-enable-auth", true, "Enable E2B authentication")
	pflag.StringVar(&domain, "e2b-domain", "localhost", "E2B domain")
	pflag.IntVar(&e2bMaxTimeout, "e2b-max-timeout", models.DefaultMaxTimeout, "E2B maximum timeout in seconds")
	pflag.StringVar(&sysNs, "system-namespace", "", "The namespace where the sandbox manager is running (required)")
	pflag.StringVar(&peerSelector, "peer-selector", "", "Peer selector for sandbox manager (required)")
	pflag.IntVar(&maxClaimWorkers, "max-claim-workers", 0, "Maximum number of claim workers (0 uses default)")
	pflag.IntVar(&maxCreateQPS, "max-create-qps", 0, "Maximum QPS for sandbox creation (0 uses default)")
	pflag.IntVar(&extProcMaxConcurrency, "ext-proc-max-concurrency", 0, "Maximum concurrency for external processor (0 uses default)")
	pflag.Float64Var(&kubeClientQPS, "kube-client-qps", 500, "QPS for Kubernetes client")
	pflag.IntVar(&kubeClientBurst, "kube-client-burst", 1000, "Burst for Kubernetes client")

	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	klog.InitFlags(nil)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()

	klog.SetLogger(zap.New(
		zap.UseFlagOptions(&opts),
		zap.RawZapOpts(zapRaw.AddCaller()),
		zap.StacktraceLevel(zapcore.DPanicLevel),
	))

	// Start pprof server if enabled
	if enablePprof {
		go func() {
			klog.Infof("Starting pprof server on %s", pprofAddr)
			if err := http.ListenAndServe(pprofAddr, nil); err != nil {
				klog.Errorf("Unable to start pprof server: %v", err)
			}
		}()
	}

	// Validate required flags
	if sysNs == "" {
		klog.Fatalf("--system-namespace is required")
	}

	if peerSelector == "" {
		klog.Fatalf("--peer-selector is required")
	}

	// Generate admin key if not provided
	if e2bAdminKey == "" {
		e2bAdminKey = uuid.NewString()
	}

	// Validate positive values
	if e2bMaxTimeout <= 0 {
		klog.Fatalf("--e2b-max-timeout must be greater than 0")
	}

	if maxClaimWorkers < 0 {
		klog.Fatalf("--max-claim-workers must be non-negative")
	}

	if maxCreateQPS < 0 {
		klog.Fatalf("--max-create-qps must be non-negative")
	}

	if extProcMaxConcurrency < 0 {
		klog.Fatalf("--ext-proc-max-concurrency must be non-negative")
	}

	if kubeClientQPS <= 0 {
		klog.Fatalf("--kube-client-qps must be greater than 0")
	}

	if kubeClientBurst <= 0 {
		klog.Fatalf("--kube-client-burst must be greater than 0")
	}

	// Initialize Kubernetes client and config
	clientSet, err := clients.NewClientSetWithOptions(float32(kubeClientQPS), kubeClientBurst)
	if err != nil {
		klog.Fatalf("Failed to initialize Kubernetes client: %v", err)
	}

	sandboxController := e2b.NewController(domain, e2bAdminKey, sysNs, e2bMaxTimeout, maxClaimWorkers, maxCreateQPS, uint32(extProcMaxConcurrency),
		port, e2bEnableAuth, clientSet)
	if err := sandboxController.Init(); err != nil {
		klog.Fatalf("Failed to initialize sandbox controller: %v", err)
	}

	// Start HTTP Server
	sandboxCtx, err := sandboxController.Run(sysNs, peerSelector)
	if err != nil {
		klog.Fatalf("Failed to start sandbox controller: %v", err)
	}
	<-sandboxCtx.Done()
	klog.Info("Sandbox controller stopped")
}
