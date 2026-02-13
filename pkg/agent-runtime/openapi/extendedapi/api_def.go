package extendedapi

import (
	"github.com/gin-gonic/gin"

	"github.com/openkruise/agents/pkg/agent-runtime/openapi/types"
)

type ExtendedAPIServerImpl struct {
	Defaults *types.Defaults // default environment variables
}

func (w *ExtendedAPIServerImpl) WrapAPIAsExtendedRoutes(engine *gin.Engine, extendedApiServer *ExtendedAPIServerImpl) {
	extendedApiGroup := engine.Group("/v1")
	{
		// using protocol buffer to init storage mount request function
		extendedApiGroup.POST("/storage/mounts", extendedApiServer.CreateMount)
	}
}
