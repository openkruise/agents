package agent_runtime

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/agent-runtime/common"
	"github.com/openkruise/agents/pkg/agent-runtime/logs"
	"github.com/openkruise/agents/pkg/agent-runtime/openapi/extendedapi"
	"github.com/openkruise/agents/pkg/agent-runtime/openapi/nativeapi"
)

func (s *Server) setupRoutes() {
	// Public routes - No authentication required
	s.engine.GET("/health", s.healthCheckHandler())

	// e2b native open nativeapi configuration routes (using: Bearer Token required)
	// native nativeapi for example: /init
	nativeOpenApiGroup := &nativeapi.OpenE2BAPIServerImpl{
		Defaults: s.config.Defaults,
	}
	nativeOpenApiGroup.WrapOpenAPIAsNativeE2BRoutes(s.engine, nativeOpenApiGroup)

	// extended route group (using: Bearer Token required)
	// extended nativeapi for example: /v1/storage/mounts
	extendedOpenApiGroup := &extendedapi.ExtendedAPIServerImpl{
		Defaults: s.config.Defaults,
	}
	extendedOpenApiGroup.WrapAPIAsExtendedRoutes(s.engine, extendedOpenApiGroup)
}

func (s *Server) healthCheckHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		startTime := time.Now()
		logContext := logs.NewLoggerContext(context.Background(), "PostInitHandler", "")
		logCollector := klog.FromContext(logContext)
		defer func() {
			logCollector.V(3).Info("Health check completed", "CostTime", time.Since(startTime).String())
		}()
		c.JSON(http.StatusNoContent, gin.H{
			"status":  common.StatusSuccess,
			"service": SandboxRuntimeHttpServer,
			"version": SandboxRuntimeHttpServerVersion,
			"uptime":  time.Since(s.startTime).String(),
			"time":    time.Now().UTC(),
		})
	}
}

// Run starts the server
func (s *Server) Run() error {
	addressPort := fmt.Sprintf("0.0.0.0:%v", s.config.Port)
	logContext := logs.NewLoggerContext(context.Background(), "MainRunHandler", "")
	logCollector := klog.FromContext(logContext)
	logCollector.V(3).Info("SandboxRuntime server is starting on", "addressPort", addressPort)
	server := &http.Server{
		Addr:              addressPort,
		Handler:           s.engine,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       common.IdleTimeout,
	}
	go func() {
		if err := server.ListenAndServe(); err != nil {
			logCollector.Error(err, SandboxRuntimeHttpServer, "ListenAndService error", addressPort)
		}
	}()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	logCollector.V(3).Info("Shutting down server gracefully...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logCollector.Error(err, SandboxRuntimeHttpServerVersion, "forced to shutdown")
		return err
	}

	logCollector.V(2).Info("AgentRuntime Stopped", SandboxRuntimeHttpServer)
	return nil
}

func setupCORS() gin.HandlerFunc {
	config := cors.Config{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{
			http.MethodHead,
			http.MethodGet,
			http.MethodPost,
			http.MethodPut,
			http.MethodPatch,
			http.MethodDelete,
		},
		AllowHeaders:     []string{"*"},
		ExposeHeaders:    []string{"Location", "Cache-Control", "X-Content-Type-Options"},
		AllowCredentials: false,
		MaxAge:           common.MaxAge,
	}
	return cors.New(config)
}
