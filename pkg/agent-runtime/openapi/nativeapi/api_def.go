package nativeapi

import (
	"github.com/gin-gonic/gin"

	"github.com/openkruise/agents/pkg/agent-runtime/openapi/types"
)

type OpenE2BAPIServerImpl struct {
	Defaults *types.Defaults // default environment variables
}

func (o *OpenE2BAPIServerImpl) WrapOpenAPIAsNativeE2BRoutes(engine *gin.Engine, apiServer *OpenE2BAPIServerImpl) {
	openE2BApi := engine.Group("")
	{
		// env related functions
		openE2BApi.GET("/envs", apiServer.GetEnvs)

		// file related functions
		openE2BApi.GET("/files", nil)
		openE2BApi.POST("/files", nil)

		// init environment for configuration
		openE2BApi.POST("/init", apiServer.PostInit)

		// metrics related functions
		openE2BApi.GET("/metrics", apiServer.GetMetrics)
	}
}
