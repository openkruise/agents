package nativeapi

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/agent-runtime/host"
	"github.com/openkruise/agents/pkg/agent-runtime/logs"
	"github.com/openkruise/agents/pkg/agent-runtime/openapi/types"
)

func (s *OpenE2BAPIServerImpl) GetMetrics(c *gin.Context) {
	ctx := c.Request.Context()
	ctx = context.WithValue(ctx, types.AccessTokenAuthScopes, []string{})
	c.Request = c.Request.WithContext(ctx)

	logContext := logs.NewLoggerContext(context.Background(), "GetMetricsHandler", "")
	logCollector := klog.FromContext(logContext)

	logCollector.V(3).Info("Start to get runtime metrics")

	provider := host.GetMetricsProvider()
	metrics, err := provider.GetMetrics()
	if err != nil {
		logCollector.Error(err, "Failed to get metrics")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get metrics"})
		return
	}

	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, metrics)
}
