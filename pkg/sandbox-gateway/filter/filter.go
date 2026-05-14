/*
Copyright 2026.

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

package filter

import (
	"context"
	"fmt"
	"sync"

	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	"github.com/openkruise/agents/pkg/sandbox-gateway/wake"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
)

var logger *zap.Logger

func init() {
	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(zapcore.InfoLevel)
	logger, _ = config.Build()
}

func FilterFactory(c interface{}, callbacks api.FilterCallbackHandler) api.StreamFilter {
	cfg := c.(*FilterConfig)
	return &sandboxFilter{
		callbacks: callbacks,
		config:    cfg.Config,
		adapter:   cfg.Adapter,
		wake:      cfg.WakeClient,
	}
}

type sandboxFilter struct {
	api.PassThroughStreamFilter
	callbacks    api.FilterCallbackHandler
	config       *Config
	adapter      *adapters.E2BAdapter
	wake         WakeClient
	completeOnce sync.Once
	contextMu    sync.Mutex
	context      context.Context
	cancel       context.CancelFunc
}

func (f *sandboxFilter) DecodeHeaders(header api.RequestHeaderMap, endStream bool) api.StatusType {
	// Step 1: Build flat headers map from the request, including pseudo-headers
	headers := make(map[string]string)
	header.Range(func(key, value string) bool {
		headers[key] = value
		return true
	})

	// Step 2: Use adapter.ParseRequest to normalize the request
	parsed := f.adapter.ParseRequest(headers)

	// Step 3: Use the unified adapter to extract sandbox ID and port
	sandboxID, sandboxPort, extraHeaders, err := f.adapter.Map(parsed)
	if err != nil {
		logger.Debug("Adapter could not extract sandbox info, continuing",
			zap.String("authority", parsed.Authority),
			zap.String("path", parsed.Path),
			zap.Error(err))
		return api.Continue
	}

	logger.Debug("DecodeHeaders: adapter mapped request",
		zap.String("sandboxID", sandboxID),
		zap.Int("sandboxPort", sandboxPort),
		zap.Any("extraHeaders", extraHeaders))

	// Look up the pod IP from registry
	route, ok := registry.GetRegistry().Get(sandboxID)
	if !ok {
		logger.Warn("Sandbox not found in registry", zap.String("sandboxID", sandboxID))
		f.callbacks.DecoderFilterCallbacks().SendLocalReply(
			502,
			"sandbox not found: "+sandboxID,
			nil,
			-1,
			"sandbox_not_found",
		)
		return api.LocalReply
	}

	if route.State == agentsv1alpha1.SandboxStateRunning {
		f.applyRoute(header, extraHeaders, route, sandboxPort)
		return api.Continue
	}

	if !(route.State == agentsv1alpha1.SandboxStatePaused && route.Pausable && route.WakeOnTraffic != "") {
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

	if f.wake == nil {
		logger.Error("Wake client is not configured", zap.String("sandboxID", sandboxID))
		f.callbacks.DecoderFilterCallbacks().SendLocalReply(
			503,
			"failed to wake sandbox",
			toReplyHeaders(toRetryAfterHeader(5)),
			-1,
			"sandbox_wake_failed",
		)
		return api.LocalReply
	}

	for k, v := range extraHeaders {
		header.Set(k, v)
	}

	filterCtx := f.wakeContext()
	go f.wakeAndContinue(filterCtx, sandboxID, sandboxPort)

	return api.Running
}

func (f *sandboxFilter) wakeAndContinue(ctx context.Context, sandboxID string, sandboxPort int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Error("Wake goroutine panicked", zap.String("sandboxID", sandboxID), zap.Any("panic", recovered))
			f.completeWithReply(500, "wake filter panic", nil, "sandbox_wake_panic")
		}
	}()

	if err := f.wake.WakeAndWait(ctx, sandboxID); err != nil {
		status, body, retryAfter, code := wake.MapErrToReply(err)
		f.completeWithReply(status, body, toRetryAfterHeader(retryAfter), code)
		return
	}

	route, ok := registry.GetRegistry().Get(sandboxID)
	if !ok {
		f.completeWithReply(502, "sandbox not found: "+sandboxID, nil, "sandbox_not_found")
		return
	}
	if route.State != agentsv1alpha1.SandboxStateRunning {
		f.completeWithReply(502, "healthy sandbox not found: "+sandboxID, nil, "sandbox_not_running")
		return
	}

	upstreamHost := fmt.Sprintf("%s:%d", route.IP, sandboxPort)
	logger.Debug("Upstream override set successfully", zap.String("upstreamHost", upstreamHost))
	f.completeWithContinue(upstreamHost)
}

func (f *sandboxFilter) applyRoute(header api.RequestHeaderMap, extraHeaders map[string]string, route proxy.Route, sandboxPort int) {
	for k, v := range extraHeaders {
		header.Set(k, v)
	}

	upstreamHost := fmt.Sprintf("%s:%d", route.IP, sandboxPort)
	f.callbacks.StreamInfo().DynamicMetadata().Set("envoy.lb.original_dst", "host", upstreamHost)

	logger.Debug("Upstream override set successfully", zap.String("upstreamHost", upstreamHost))
}
