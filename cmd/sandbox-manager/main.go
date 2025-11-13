// Package main provides the main entry point for the E2B on Kubernetes server.
package main

import (
	"flag"
	"os"
	"strconv"

	"github.com/openkruise/agents/pkg/sandbox-manager/controllers/e2b"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/clients"
	"k8s.io/klog/v2"
)

func main() {
	// ============= Env ===============
	// Get listen address from environment variable or use default value
	port := 8080
	if portEnv, err := strconv.Atoi(os.Getenv("PORT")); err == nil {
		port = portEnv
	}

	templateDir := os.Getenv("TEMPLATE_DIR")
	if templateDir == "" {
		templateDir = "/root/builtin_templates"
	}

	tlsSecret := os.Getenv("TLS_SECRET")
	if tlsSecret == "" {
		tlsSecret = "kruise-sandbox-manager-tls"
	}

	infra := os.Getenv("INFRA")
	if infra == "" {
		klog.Fatalf("env var INFRA is required")
	}

	e2bAdminKey := os.Getenv("E2B_ADMIN_KEY")
	e2bEnableAuth := os.Getenv("E2B_ENABLE_AUTH") == "true"
	// =========== End Env =============

	// Initialize Kubernetes client and config
	clientSet, err := clients.NewClientSet(infra)
	if err != nil {
		klog.Fatalf("Failed to initialize Kubernetes client: %v", err)
	}

	klog.InitFlags(nil)
	flag.Parse()

	// Get domain from environment variable or use empty string
	domain := "localhost"
	if domainEnv := os.Getenv("E2B_DOMAIN"); domainEnv != "" {
		domain = domainEnv
	}

	// Get namespace from environment variable or use "default"
	if ns := os.Getenv("NAMESPACE"); ns != "" {
		e2b.Namespace = ns
	}

	sandboxController := e2b.NewController(domain, tlsSecret, e2bAdminKey, port, e2bEnableAuth, clientSet)
	if err := sandboxController.Init(templateDir, infra); err != nil {
		klog.Fatalf("Failed to initialize sandbox controller: %v", err)
	}

	// Start HTTP Server
	sandboxCtx, err := sandboxController.Run()
	if err != nil {
		klog.Fatalf("Failed to start sandbox controller: %v", err)
	}
	<-sandboxCtx.Done()
	klog.Info("Sandbox controller stopped")
}
