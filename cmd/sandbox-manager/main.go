// Package main provides the main entry point for the E2B on Kubernetes server.
package main

import (
	"flag"
	"net/http"         // Added for pprof server
	_ "net/http/pprof" // Added to register pprof handlers
	"os"
	"strconv"

	"github.com/google/uuid"
	"github.com/spf13/pflag"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/servers/e2b"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

func main() {
	// Define variables for pprof configuration
	var enablePprof bool
	var pprofAddr string

	utilfeature.DefaultMutableFeatureGate.AddFlag(pflag.CommandLine)

	// Register the new pprof flags
	pflag.BoolVar(&enablePprof, "enable-pprof", false, "Enable pprof profiling")
	pflag.StringVar(&pprofAddr, "pprof-addr", ":6060", "The address the pprof debug maps to.")

	klog.InitFlags(nil)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()

	// Start pprof server if enabled
	if enablePprof {
		go func() {
			klog.Infof("Starting pprof server on %s", pprofAddr)
			if err := http.ListenAndServe(pprofAddr, nil); err != nil {
				klog.Errorf("Unable to start pprof server: %v", err)
			}
		}()
	}

	// ============= Env ===============
	// Get listen address from environment variable or use default value
	port := 8080
	if portEnv, err := strconv.Atoi(os.Getenv("PORT")); err == nil {
		port = portEnv
	}

	e2bAdminKey := os.Getenv("E2B_ADMIN_KEY")
	if e2bAdminKey == "" {
		e2bAdminKey = uuid.NewString()
	}
	e2bEnableAuth := os.Getenv("E2B_ENABLE_AUTH") == "true"

	// Get domain from environment variable or use empty string
	domain := "localhost"
	if domainEnv := os.Getenv("E2B_DOMAIN"); domainEnv != "" {
		domain = domainEnv
	}

	e2bMaxTimeout := models.DefaultMaxTimeout
	if value, err := strconv.Atoi(os.Getenv("E2B_MAX_TIMEOUT")); err == nil {
		if value <= 0 {
			klog.Fatalf("E2B_MAX_TIMEOUT must be greater than 0")
		}
		e2bMaxTimeout = value
	}

	sysNs := os.Getenv("SYSTEM_NAMESPACE")
	if sysNs == "" {
		klog.Fatalf("env var SYSTEM_NAMESPACE is required")
	}

	peerSelector := os.Getenv("PEER_SELECTOR")
	if peerSelector == "" {
		klog.Fatalf("env var PEER_SELECTOR is required")
	}
	// =========== End Env =============

	// Initialize Kubernetes client and config
	clientSet, err := clients.NewClientSet()
	if err != nil {
		klog.Fatalf("Failed to initialize Kubernetes client: %v", err)
	}

	sandboxController := e2b.NewController(domain, e2bAdminKey, sysNs, e2bMaxTimeout, port, e2bEnableAuth, clientSet)
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
