// Package main provides the main entry point for the E2B on Kubernetes server.
package main

import (
	"flag"
	"os"
	"strconv"

	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/servers/e2b"
	"k8s.io/klog/v2"
)

func main() {
	// ============= Env ===============
	// Get listen address from environment variable or use default value
	port := 8080
	if portEnv, err := strconv.Atoi(os.Getenv("PORT")); err == nil {
		port = portEnv
	}

	infra := os.Getenv("INFRA")
	if infra == "" {
		klog.Fatalf("env var INFRA is required")
	}

	e2bAdminKey := os.Getenv("E2B_ADMIN_KEY")
	e2bEnableAuth := os.Getenv("E2B_ENABLE_AUTH") == "true"

	// Get domain from environment variable or use empty string
	domain := "localhost"
	if domainEnv := os.Getenv("E2B_DOMAIN"); domainEnv != "" {
		domain = domainEnv
	}

	// Get namespace from environment variable or use "default"
	if ns := os.Getenv("NAMESPACE"); ns != "" {
		e2b.Namespace = ns
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
	clientSet, err := clients.NewClientSet(infra)
	if err != nil {
		klog.Fatalf("Failed to initialize Kubernetes client: %v", err)
	}

	klog.InitFlags(nil)
	flag.Parse()

	sandboxController := e2b.NewController(domain, e2bAdminKey, port, e2bEnableAuth, clientSet)
	if err := sandboxController.Init(infra); err != nil {
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
