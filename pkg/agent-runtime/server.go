package agent_runtime

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/agent-runtime/auth"
	"github.com/openkruise/agents/pkg/agent-runtime/logs"
	"github.com/openkruise/agents/pkg/agent-runtime/openapi/types"
	"github.com/openkruise/agents/pkg/agent-runtime/permissions"
	"github.com/openkruise/agents/pkg/utils"
)

const (
	SandboxRuntimeHttpServer        = "SandboxRuntimeHttpServer"
	SandboxRuntimeHttpServerVersion = "v0.0.1"
)

// Server struct defines the SandboxRuntime HTTP server configuration
type Server struct {
	engine       *gin.Engine
	config       ServerConfig
	startTime    time.Time
	workspaceDir string
}

type ServerConfig struct {
	Port       int             `json:"port"`
	Workspace  string          `json:"workspace"`
	AuthConfig AuthConfig      `json:"authConfig"`
	FlagConfig FlagConfig      `json:"flagConfig"`
	Defaults   *types.Defaults `json:"defaults"`
}

type FlagConfig struct {
	VersionFlag  bool
	CommitFlag   bool
	StartCmdFlag string
}

type AuthConfig struct {
	ValidTokens   []string `json:"validTokens"`
	AllowedPaths  []string `json:"allowedPaths"`
	EnableSigning bool     `json:"enableSigning"`
}

// NewHttpServer creates a new SandboxRuntime server instance
func NewHttpServer(config ServerConfig) *Server {

	logContext := logs.NewLoggerContext(context.Background(), "NewHttpServer", utils.DumpJson(config))
	logCollector := klog.FromContext(logContext)

	logCollector.V(3).Info("Init agent runtime http server")

	authenticationConfig := auth.AuthenticationConfig{
		ValidTokens:   config.AuthConfig.ValidTokens,
		AllowedPaths:  config.AuthConfig.AllowedPaths,
		EnableSigning: config.AuthConfig.EnableSigning,
	}

	// create a new gin http server engine
	serverInstance := &Server{
		config:    config,
		startTime: time.Now(),
	}

	// Disable Gin debug output in production mode
	gin.SetMode(gin.ReleaseMode)

	// gin http server engine with logger\recovery middleware configuration
	httpServerEngine := gin.New()
	httpServerEngine.Use(gin.Logger())
	httpServerEngine.Use(gin.Recovery())
	httpServerEngine.Use(setupCORS())
	httpServerEngine.Use(permissions.WithGinAuthenticateUserName(&permissions.RealUserProvider{}))
	httpServerEngine.Use(auth.BearerAuthMiddleware(authenticationConfig))
	serverInstance.engine = httpServerEngine

	// register http routers
	serverInstance.setupRoutes()

	logCollector.Info("Finished init agent runtime http server")
	return serverInstance
}
