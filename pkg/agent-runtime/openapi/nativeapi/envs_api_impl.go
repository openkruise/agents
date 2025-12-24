package nativeapi

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/agent-runtime/logs"
	"github.com/openkruise/agents/pkg/agent-runtime/openapi/types"
	"github.com/openkruise/agents/pkg/utils"
)

func (s *OpenE2BAPIServerImpl) GetEnvs(c *gin.Context) {
	ctx := c.Request.Context()
	ctx = context.WithValue(ctx, types.AccessTokenAuthScopes, []string{})
	c.Request = c.Request.WithContext(ctx)

	operationID := logs.AssignOperationID()
	logContext := logs.NewLoggerContext(context.Background(), "GetEnvsHandler", utils.DumpJson(s.Defaults))
	logCollector := klog.FromContext(logContext)
	logCollector.V(3).Info(string(logs.OperationIDKey), operationID, "Getting env vars")

	envs := make(types.EnvVars)
	s.Defaults.EnvVars.Range(func(key, value string) bool {
		envs[key] = value
		return true
	})

	// set headers to prevent caching
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "application/json")

	// set json response body
	c.JSON(http.StatusOK, envs)

	logCollector.V(3).Info("Successfully sent env vars", string(logs.OperationIDKey), operationID)
}
