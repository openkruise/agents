/*
Copyright 2026 The Kruise Authors.

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

package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"k8s.io/klog/v2"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	sandbox_manager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
)

// MCPServer represents the MCP server (similar to e2b.Controller)
type MCPServer struct {
	config    *ServerConfig
	mcpServer *server.MCPServer
	handler   *Handler
	auth      *Auth

	// Session manager for lifecycle and peer sync
	sessionManager *SessionManager

	// HTTP server components (similar to e2b.Controller)
	mux        *http.ServeMux
	httpServer *http.Server
}

// NewMCPServer creates a new MCP server (similar to e2b.NewController)
func NewMCPServer(
	config *ServerConfig,
	manager *sandbox_manager.SandboxManager,
	keyStorage *keys.SecretKeyStorage,
) *MCPServer {
	s := &MCPServer{
		config: config,
		mux:    http.NewServeMux(),
	}

	s.httpServer = &http.Server{
		Addr:              fmt.Sprintf(":%d", config.Port),
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Create auth (always needed, even for anonymous users)
	s.auth = NewAuth(keyStorage)

	// Create session manager with production dependencies
	deps := NewDefaultSessionDeps(manager, config.SessionSyncPort)
	s.sessionManager = NewSessionManager(deps, config)

	// Register session manager as event handler to receive sandbox events
	// This must be done before Infra.Run() to avoid missing events
	manager.GetInfra().SetSandboxEventHandler(s.sessionManager)

	// Create handler
	s.handler = NewHandler(s.sessionManager, manager, config)

	// Create MCP server
	s.mcpServer = server.NewMCPServer(
		"kruise-agents-mcp-server",
		"0.0.1",
		server.WithToolCapabilities(true),
	)

	// Register tools and routes
	s.registerTools()
	s.registerRoutes()

	return s
}

// registerRoutes registers HTTP routes (similar to e2b.Controller.registerRoutes)
func (s *MCPServer) registerRoutes() {
	// Health check endpoint
	s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "OK")
	})

	// Create streamable HTTP handler for MCP protocol
	var httpHandler http.Handler
	httpHandler = server.NewStreamableHTTPServer(s.mcpServer,
		server.WithStateLess(false),
	)

	// Apply authentication middleware (provides user context, anonymous if keys == nil)
	httpHandler = s.auth.HTTPAuthMiddleware(httpHandler)

	// Register MCP endpoint
	s.mux.Handle(s.config.MCPEndpointPath, httpHandler)
}

// Run starts the MCP server (similar to e2b.Controller.Run)
func (s *MCPServer) Run(ctx context.Context) error {
	log := klog.FromContext(ctx).WithValues("component", "MCPServer")

	if err := s.sessionManager.Start(); err != nil {
		return fmt.Errorf("failed to start session manager: %w", err)
	}

	go func() {
		log.Info("Starting MCP server", "address", s.httpServer.Addr)
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error(err, "MCP HTTP server failed")
		}
	}()

	log.Info("MCP server started successfully")
	return nil
}

func (s *MCPServer) Stop(ctx context.Context) error {
	log := klog.FromContext(ctx).WithValues("component", "MCPServer")

	s.sessionManager.Stop()

	if err := s.httpServer.Shutdown(ctx); err != nil {
		log.Error(err, "Failed to shutdown MCP HTTP server")
		return err
	}

	log.Info("MCP server stopped")
	return nil
}

// InitPeers seeds SessionManager peers from discovered peer IPs
// This reuses sandbox-manager's peer discovery results to avoid duplicate Kubernetes lookups
func (s *MCPServer) InitPeers(ips []string) {
	if s.sessionManager == nil {
		return
	}
	for _, ip := range ips {
		if ip == "" {
			continue
		}
		s.sessionManager.SetPeer(ip)
	}
}

func (s *MCPServer) registerTools() {
	toolRegistry := []struct {
		definition mcp.Tool
		handler    server.ToolHandlerFunc
	}{
		{
			definition: mcp.NewTool(
				ToolRunCode,
				mcp.WithDescription("Execute code in a secure sandbox environment using Jupyter Notebook semantics. "+
					"The code execution follows E2B Code Interpreter standards. "+
					"You can reference previously defined variables, imports, and functions in the code. "+
					"Returns structured response with error, logs (stdout/stderr arrays), and results (rich output)."),
				mcp.WithInputSchema[RunCodeRequest](),
				mcp.WithOutputSchema[RunCodeResponse](),
			),
			handler: mcp.NewStructuredToolHandler(s.handler.HandleRunCode),
		},
		{
			definition: mcp.NewTool(
				ToolRunCodeOnce,
				mcp.WithDescription("Execute code in a one-time sandbox environment. "+
					"Each call creates a new isolated sandbox, executes the code, and automatically cleans up the sandbox after execution. "+
					"Ideal for stateless code execution where no session persistence is needed. "+
					"Returns structured response with error, logs (stdout/stderr arrays), and results (rich output)."),
				mcp.WithInputSchema[RunCodeOnceRequest](),
				mcp.WithOutputSchema[RunCodeResponse](),
			),
			handler: mcp.NewStructuredToolHandler(s.handler.HandleRunCodeOnce),
		},
		{
			definition: mcp.NewTool(
				ToolRunCommand,
				mcp.WithDescription("Execute a shell command in the sandbox environment. "+
					"Commands are run via /bin/bash -l -c, following E2B Commands interface. "+
					"Supports environment variables, working directory. "+
					"Returns command result with stdout, stderr, and exit code."),
				mcp.WithInputSchema[RunCommandRequest](),
				mcp.WithOutputSchema[RunCommandResponse](),
			),
			handler: mcp.NewStructuredToolHandler(s.handler.HandleRunCommand),
		},
	}

	// Register each tool with its handler (wrapped with middlewares)
	for _, entry := range toolRegistry {
		s.mcpServer.AddTool(entry.definition, s.applyMiddlewares(entry.handler))
	}
}
