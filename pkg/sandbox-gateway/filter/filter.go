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
	"errors"
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
	timeoututils "github.com/openkruise/agents/pkg/utils/timeout"
)

var logger *zap.Logger

const (
	envoyRequestFinishedPanic = "request has been finished"
	envoyFilterDestroyedPanic = "golang filter has been destroyed"
)

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
		waker:     cfg.Waker,
	}
}

type sandboxFilter struct {
	api.PassThroughStreamFilter
	callbacks api.FilterCallbackHandler
	config    *Config
	adapter   *adapters.E2BAdapter
	waker     WakeAndWaiter

	mu         sync.Mutex
	completing bool
	completed  bool
	destroyed  bool
	cancel     context.CancelFunc
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

	_, wakeEnabled := timeoututils.ParseWakeOnTraffic(route.WakeOnTraffic)
	if route.State == agentsv1alpha1.SandboxStatePaused && route.WakeOnTraffic != "" && wakeEnabled && f.waker != nil {
		for k, v := range extraHeaders {
			header.Set(k, v)
		}
		ctx, cancel := context.WithCancel(context.Background())
		f.mu.Lock()
		f.cancel = cancel
		f.mu.Unlock()
		go f.wakeAndContinue(ctx, sandboxID, sandboxPort, route.WakeOnTraffic)
		return api.Running
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

	// Apply extra headers from the adapter (e.g., :path rewrite for kruise custom protocol)
	for k, v := range extraHeaders {
		header.Set(k, v)
	}

	upstreamHost := fmt.Sprintf("%s:%d", route.IP, sandboxPort)
	f.callbacks.StreamInfo().DynamicMetadata().Set("envoy.lb.original_dst", "host", upstreamHost)

	logger.Debug("Upstream override set successfully", zap.String("upstreamHost", upstreamHost))
	return api.Continue
}

func (f *sandboxFilter) wakeAndContinue(ctx context.Context, sandboxID string, sandboxPort int, annotation string) {
	defer func() {
		if r := recover(); r != nil {
			if isEnvoyStreamGonePanic(r) {
				return
			}
			logger.Error("panic during async wake", zap.String("sandboxID", sandboxID), zap.Any("recover", r))
			f.sendLocalReplyOnce(500, "wake failed", "wake_panic")
		}
	}()

	err := f.waker.WakeAndWait(ctx, sandboxID, annotation)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		if errors.Is(err, wake.ErrSandboxNotFound) {
			f.sendLocalReplyOnce(502, "sandbox not found: "+sandboxID, "sandbox_not_found")
			return
		}
		f.sendLocalReplyOnce(502, "healthy sandbox not found: "+sandboxID, "sandbox_not_running")
		return
	}

	route, ok := registry.GetRegistry().Get(sandboxID)
	if !ok {
		f.sendLocalReplyOnce(502, "sandbox not found: "+sandboxID, "sandbox_not_found")
		return
	}
	if route.State != agentsv1alpha1.SandboxStateRunning {
		f.sendLocalReplyOnce(502, "healthy sandbox not found: "+sandboxID, "sandbox_not_running")
		return
	}

	f.completeWithContinue(route, sandboxPort)
}

func (f *sandboxFilter) sendLocalReplyOnce(code int, body string, details string) {
	if !f.claimCompletion() {
		return
	}
	f.callbacks.DecoderFilterCallbacks().SendLocalReply(code, body, nil, -1, details)
}

func (f *sandboxFilter) completeWithContinue(route proxy.Route, sandboxPort int) {
	if !f.beginCompletion() {
		return
	}
	if !f.setUpstreamMetadata(route, sandboxPort) {
		return
	}
	if !f.claimPreparedCompletion() {
		return
	}
	f.callbacks.DecoderFilterCallbacks().Continue(api.Continue)
}

func (f *sandboxFilter) setUpstreamMetadata(route proxy.Route, sandboxPort int) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			if isEnvoyStreamGonePanic(r) {
				f.abortCompletion(true)
				ok = false
				return
			}
			logger.Error("panic before async wake continue", zap.String("sandboxID", route.ID), zap.Any("recover", r))
			if f.abortCompletion(false) {
				f.sendLocalReplyOnce(500, "wake failed", "wake_panic")
			}
			ok = false
		}
	}()
	upstreamHost := fmt.Sprintf("%s:%d", route.IP, sandboxPort)
	f.callbacks.StreamInfo().DynamicMetadata().Set("envoy.lb.original_dst", "host", upstreamHost)
	return true
}

func (f *sandboxFilter) beginCompletion() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.destroyed || f.completed || f.completing {
		return false
	}
	f.completing = true
	return true
}

func (f *sandboxFilter) claimPreparedCompletion() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.destroyed || f.completed || !f.completing {
		f.completing = false
		return false
	}
	f.completing = false
	f.completed = true
	return true
}

func (f *sandboxFilter) claimCompletion() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.destroyed || f.completed || f.completing {
		return false
	}
	f.completed = true
	return true
}

func (f *sandboxFilter) abortCompletion(markDestroyed bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completing = false
	if markDestroyed {
		f.destroyed = true
	}
	return !f.destroyed && !f.completed
}

func isEnvoyStreamGonePanic(recovered interface{}) bool {
	message, ok := recovered.(string)
	if !ok {
		return false
	}
	return message == envoyRequestFinishedPanic || message == envoyFilterDestroyedPanic
}

func (f *sandboxFilter) OnDestroy(reason api.DestroyReason) {
	f.mu.Lock()
	if !f.completed {
		f.destroyed = true
	}
	cancel := f.cancel
	f.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
