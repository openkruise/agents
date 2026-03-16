package filter

import (
	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
)

var logger *zap.Logger

func init() {
	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(zapcore.InfoLevel)
	logger, _ = config.Build()
}

const (
	headerSandboxID   = "e2b-sandbox-id"
	headerSandboxPort = "e2b-sandbox-port"
	defaultPort       = "80"
)

func FilterFactory(c any, callbacks api.FilterCallbackHandler) api.StreamFilter {
	return &sandboxFilter{callbacks: callbacks}
}

type sandboxFilter struct {
	api.PassThroughStreamFilter
	callbacks api.FilterCallbackHandler
}

func (f *sandboxFilter) DecodeHeaders(header api.RequestHeaderMap, endStream bool) api.StatusType {
	sandboxID, _ := header.Get(headerSandboxID)
	logger.Debug("DecodeHeaders called", zap.String("sandboxID", sandboxID))
	if sandboxID == "" {
		logger.Debug("No sandbox ID, continuing")
		return api.Continue
	}

	port, _ := header.Get(headerSandboxPort)
	if port == "" {
		port = defaultPort
		logger.Debug("Using default port", zap.String("port", port))
	}

	podIP, ok := registry.Get(sandboxID)
	if !ok {
		logger.Warn("Sandbox not found in registry", zap.String("sandboxID", sandboxID))
		f.callbacks.DecoderFilterCallbacks().SendLocalReply(
			404,
			"sandbox not found: "+sandboxID,
			nil,
			-1,
			"sandbox_not_found",
		)
		return api.LocalReply
	}

	upstreamHost := podIP + ":" + port
	f.callbacks.StreamInfo().DynamicMetadata().Set("envoy.lb.original_dst", "host", upstreamHost)

	logger.Debug("Upstream override set successfully", zap.String("upstreamHost", upstreamHost))
	return api.Continue
}
