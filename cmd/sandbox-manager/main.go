// Package main provides the main entry point for the E2B on Kubernetes server.
package main

import (
	"flag"
	"fmt"
	"net/http"         // Added for pprof server
	_ "net/http/pprof" // Added to register pprof handlers
	"os"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/spf13/pflag"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/servers/e2b"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
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

	// Start Sandbox Manager (Background Worker)
	sandboxCtx, err := sandboxController.Run(sysNs, peerSelector)
	if err != nil {
		klog.Fatalf("Failed to start sandbox controller: %v", err)
	}

	// --- START OF NEW WEB SERVER WIRING ---

	// Create the address string from the port variable
	addr := fmt.Sprintf(":%d", port)

	// Wrap the sandboxController in our adapter to bridge the interface gap.
	// We use the adapter defined at the bottom of this file.
	adapter := &ControllerAdapter{controller: sandboxController}

	// Initialize the NEW web server (Gin + OTEL) using the adapter
	webServer := web.NewServer(addr, adapter)

	// Run the web server in a separate goroutine so it doesn't block the rest of the startup
	go func() {
		klog.Infof("Starting web server on %s", addr)
		if err := webServer.Run(); err != nil {
			klog.Fatalf("Failed to start web server: %v", err)
		}
	}()
	// --- END OF NEW WEB SERVER WIRING ---

	<-sandboxCtx.Done()
	klog.Info("Sandbox controller stopped")
}

// ControllerAdapter wraps the e2b.Controller to satisfy the web.Service interface.
// It translates Gin contexts into the raw http.Requests that e2b.Controller expects.
type ControllerAdapter struct {
	controller *e2b.Controller
}

func (a *ControllerAdapter) CreateSandbox(c *gin.Context) {
	resp, apiErr := a.controller.CreateSandbox(c.Request)
	if apiErr != nil {
		c.JSON(apiErr.Code, apiErr)
		return
	}
	c.JSON(http.StatusCreated, resp.Body)
}

func (a *ControllerAdapter) ListSandboxes(c *gin.Context) {
	// Returning 501 Not Implemented or empty list to satisfy the interface.
	c.JSON(http.StatusNotImplemented, gin.H{"message": "ListSandboxes is not implemented in e2b.Controller"})
}

func (a *ControllerAdapter) GetSandbox(c *gin.Context) {
	resp, apiErr := a.controller.DescribeSandbox(c.Request)
	if apiErr != nil {
		c.JSON(apiErr.Code, apiErr)
		return
	}
	c.JSON(http.StatusOK, resp.Body)
}

func (a *ControllerAdapter) RefreshSandbox(c *gin.Context) {
	resp, apiErr := a.controller.SetSandboxTimeout(c.Request)
	if apiErr != nil {
		c.JSON(apiErr.Code, apiErr)
		return
	}
	c.JSON(http.StatusOK, resp.Body)
}

func (a *ControllerAdapter) KillSandbox(c *gin.Context) {
	resp, apiErr := a.controller.DeleteSandbox(c.Request)
	if apiErr != nil {
		c.JSON(apiErr.Code, apiErr)
		return
	}
	// DeleteSandbox returns web.ApiResponse[struct{}], so we just check for error
	// and return No Content on success.
	c.Status(resp.Code)
}
