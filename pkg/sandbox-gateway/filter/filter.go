package filter

import (
	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
)

var logger *zap.Logger

func init() {
	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(zapcore.InfoLevel)
	logger, _ = config.Build()
}

func FilterFactory(c interface{}, callbacks api.FilterCallbackHandler) api.StreamFilter {
	cfg := c.(*Config)
	return &sandboxFilter{
		callbacks: callbacks,
		config:    cfg,
	}
}

type sandboxFilter struct {
	api.PassThroughStreamFilter
	callbacks api.FilterCallbackHandler
	config    *Config
}

func (f *sandboxFilter) DecodeHeaders(header api.RequestHeaderMap, endStream bool) api.StatusType {
	// Get the effective header name (with defaults applied)
	headerName := f.config.GetHeaderMatchName()

	// Get the raw header value based on the configured header name
	// For host policy with empty header-match-name, use Host() method
	var rawHeaderValue string
	if f.config.HeaderMatchPolicy == HeaderMatchPolicyHost && headerName == "" {
		rawHeaderValue = header.Host()
	} else {
		rawHeaderValue, _ = header.Get(headerName)
	}
	logger.Debug("DecodeHeaders called",
		zap.String("headerName", headerName),
		zap.String("rawValue", rawHeaderValue),
		zap.String("policy", string(f.config.HeaderMatchPolicy)))

	if rawHeaderValue == "" {
		logger.Debug("No header value found, continuing")
		return api.Continue
	}

	// Extract host key and port based on the configured policy
	var sandboxID, port string
	switch f.config.HeaderMatchPolicy {
	case HeaderMatchPolicyHost:
		// In host mode, extract both hostKey and port in one regex call
		// from the header value (<port>-<namespace>--<name>.domain)
		sandboxID, port = f.config.ExtractHostInfo(rawHeaderValue)
		if port == "" {
			logger.Warn("Failed to extract port from host header, using default",
				zap.String("rawValue", rawHeaderValue))
			port = f.config.DefaultPort
		}
	case HeaderMatchPolicySandbox:
		// In sandbox mode, hostKey is the header value directly
		sandboxID = rawHeaderValue
		// Get port from the port header
		port, _ = header.Get(f.config.HeaderSandboxPort)
		if port == "" {
			port = f.config.DefaultPort
			logger.Debug("Using default port", zap.String("port", port))
		}
	}

	// Look up the pod IP from registry
	route, ok := registry.GetRegistry().Get(sandboxID)
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

	if route.State != agentsv1alpha1.SandboxStateRunning {
		logger.Warn("Sandbox is not running", zap.String("sandboxID", sandboxID), zap.String("state", route.State))
		f.callbacks.DecoderFilterCallbacks().SendLocalReply(
			502,
			"healthy sandbox not found: "+sandboxID,
			nil,
			-1,
			"sandbox_not_running",
		)
		return api.LocalReply
	}

	upstreamHost := route.IP + ":" + port
	f.callbacks.StreamInfo().DynamicMetadata().Set("envoy.lb.original_dst", "host", upstreamHost)

	logger.Debug("Upstream override set successfully", zap.String("upstreamHost", upstreamHost))
	return api.Continue
}
