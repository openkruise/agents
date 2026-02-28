package nativeapi

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/agent-runtime/logs"
	"github.com/openkruise/agents/pkg/agent-runtime/openapi/types"
)

func (s *OpenE2BAPIServerImpl) PostInit(c *gin.Context) {

	ctx := c.Request.Context()
	ctx = context.WithValue(ctx, types.AccessTokenAuthScopes, []string{})
	c.Request = c.Request.WithContext(ctx)

	operationID := logs.AssignOperationID()
	logContext := logs.NewLoggerContext(context.Background(), "PostInitHandler")
	logCollector := klog.FromContext(logContext)
	logCollector.V(3).Info(string(logs.OperationIDKey), operationID, "Init env")

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Service initialized successfully",
		"data": gin.H{
			"initialized": true,
			"timestamp":   time.Now().UTC(),
		},
	})
}
