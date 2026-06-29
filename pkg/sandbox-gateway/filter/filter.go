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
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

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

const (
	// accessTokenHeader is the HTTP header name that clients must set
	// to carry the sandbox access token for authentication.
	accessTokenHeader = "x-access-token"
)

func FilterFactory(c interface{}, callbacks api.FilterCallbackHandler) api.StreamFilter {
	cfg := c.(*FilterConfig)
	return &sandboxFilter{
		callbacks: callbacks,
		config:    cfg.Config,
		adapter:   cfg.Adapter,
	}
}

type sandboxFilter struct {
	api.PassThroughStreamFilter
	callbacks api.FilterCallbackHandler
	config    *Config
	adapter   *adapters.E2BAdapter

	// Async wake completion state. Protected by mu.
	mu         sync.Mutex
	completing bool // wakeAndContinue is setting metadata
	completed  bool // Continue or SendLocalReply already called
	destroyed  bool // Envoy destroyed the filter (stream gone)
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

	if route.State != agentsv1alpha1.SandboxStateRunning {
		// Check if wake-on-traffic should be attempted for this sandbox.
		// route.WakeOnTraffic is the primary check (fast, from registry).
		// HasWakeAnnotation is a fallback that reads the informer cache
		// directly, covering the window between kubectl annotate and the
		// gateway controller reconciling the change into the route registry.
		waker := wake.GetWaker()
		parts := strings.SplitN(sandboxID, "--", 2)
		shouldWake := route.WakeOnTraffic
		if !shouldWake && f.config.EnableWakeOnTraffic && waker != nil &&
			len(parts) == 2 && route.State == agentsv1alpha1.SandboxStatePaused {
			shouldWake = waker.HasWakeAnnotation(context.Background(), parts[0], parts[1])
		}
		logger.Info("Wake eligibility check",
			zap.String("sandboxID", sandboxID),
			zap.String("state", route.State),
			zap.Bool("wakeOnTraffic", route.WakeOnTraffic),
			zap.Bool("shouldWake", shouldWake),
			zap.Bool("enableWakeOnTraffic", f.config.EnableWakeOnTraffic),
			zap.Bool("wakerInitialized", waker != nil))
		if f.config.EnableWakeOnTraffic && shouldWake && route.State == agentsv1alpha1.SandboxStatePaused {
			if waker != nil {
				if len(parts) == 2 {
					// Apply extra headers before returning Running so they
					// are visible to subsequent filter phases.
					for k, v := range extraHeaders {
						header.Set(k, v)
					}
					// Launch async wake with a detached context. The filter
					// returns Running to tell Envoy to suspend request
					// processing. wakeAndContinue will call Continue or
					// SendLocalReply when the wake completes.
					waitTimeout := time.Duration(f.config.GetWakeTimeoutSeconds()) * time.Second
					ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
					f.mu.Lock()
					f.cancel = cancel
					f.mu.Unlock()
					go f.wakeAndContinue(ctx, waker, parts[0], parts[1], sandboxID, sandboxPort, waitTimeout)
					return api.Running
				}
				logger.Warn("Invalid sandbox ID format for wake",
					zap.String("sandboxID", sandboxID))
			}
		}
		// Not running and not wakeable -> 502 (existing behavior)
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
	}

	// Authenticate the request if auth is enabled and the sandbox has an access token configured.
	// When EnableAuth is false (default), the gateway skips token validation for backward compatibility.
	if f.config.EnableAuth && route.AccessToken != "" {
		requestToken, _ := header.Get(accessTokenHeader)
		if subtle.ConstantTimeCompare([]byte(requestToken), []byte(route.AccessToken)) != 1 {
			logger.Warn("Access token mismatch",
				zap.String("sandboxID", sandboxID))
			f.callbacks.DecoderFilterCallbacks().SendLocalReply(
				401,
				"unauthorized: invalid or missing access token",
				nil,
				-1,
				"unauthorized",
			)
			return api.LocalReply
		}
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

// wakeAndContinue runs the wake operation asynchronously. On success it sets
// upstream metadata and calls Continue; on failure it sends a LocalReply.
// This method is launched as a goroutine from DecodeHeaders after returning
// api.Running.
func (f *sandboxFilter) wakeAndContinue(
	ctx context.Context,
	waker *wake.Waker,
	namespace, name, sandboxID string,
	sandboxPort int,
	waitTimeout time.Duration,
) {
	defer func() {
		if r := recover(); r != nil {
			if isEnvoyStreamGonePanic(r) {
				return
			}
			logger.Error("panic during async wake",
				zap.String("sandboxID", sandboxID), zap.Any("recover", r))
			f.sendLocalReplyOnce(500, "wake failed", "wake_panic")
		}
	}()

	err := waker.Wake(ctx, namespace, name, waitTimeout)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Filter was destroyed; do nothing.
			return
		}
		logger.Warn("Sandbox wake failed",
			zap.String("sandboxID", sandboxID), zap.Error(err))
		f.sendLocalReplyOnce(503, "sandbox wake failed: "+err.Error(), "sandbox_wake_failed")
		return
	}

	// Wake succeeded — verify the sandbox is now Running in the registry.
	route, ok := registry.GetRegistry().Get(sandboxID)
	if !ok || route.State != agentsv1alpha1.SandboxStateRunning {
		logger.Warn("Sandbox not running after wake", zap.String("sandboxID", sandboxID))
		f.sendLocalReplyOnce(502, "healthy sandbox not found: "+sandboxID, "sandbox_not_running")
		return
	}

	logger.Info("Sandbox woken successfully", zap.String("sandboxID", sandboxID))
	f.completeWithContinue(route, sandboxPort)
}

// sendLocalReplyOnce sends a LocalReply, but only if no other completion
// action has been taken. This prevents double-reply when the async goroutine
// and Destroy race.
func (f *sandboxFilter) sendLocalReplyOnce(code int, body string, details string) {
	if !f.claimCompletion() {
		return
	}
	f.callbacks.DecoderFilterCallbacks().SendLocalReply(code, body, nil, -1, details)
}

// completeWithContinue sets upstream metadata and calls Continue to resume
// Envoy request processing after a successful async wake.
func (f *sandboxFilter) completeWithContinue(route proxy.Route, sandboxPort int) {
	if !f.beginCompletion() {
		return
	}

	// Set upstream metadata. This may panic if Envoy has already destroyed
	// the stream, so we recover and abort.
	if !f.setUpstreamMetadata(route, sandboxPort) {
		return
	}

	if !f.claimPreparedCompletion() {
		return
	}
	f.callbacks.DecoderFilterCallbacks().Continue(api.Continue)
}

// setUpstreamMetadata sets the envoy.lb.original_dst dynamic metadata.
// Returns false if the stream was destroyed during the call.
func (f *sandboxFilter) setUpstreamMetadata(route proxy.Route, sandboxPort int) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			if isEnvoyStreamGonePanic(r) {
				f.abortCompletion(true)
				ok = false
				return
			}
			logger.Error("panic before async wake continue",
				zap.String("sandboxID", route.ID), zap.Any("recover", r))
			if f.abortCompletion(false) {
				f.sendLocalReplyOnce(500, "wake failed", "wake_panic")
			}
			ok = false
		}
	}()
	upstreamHost := fmt.Sprintf("%s:%d", route.IP, sandboxPort)
	f.callbacks.StreamInfo().DynamicMetadata().Set("envoy.lb.original_dst", "host", upstreamHost)
	logger.Debug("Upstream override set successfully (async)", zap.String("upstreamHost", upstreamHost))
	return true
}

// beginCompletion marks the start of the completion phase.
// Returns false if the filter is already completed or destroyed.
func (f *sandboxFilter) beginCompletion() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.destroyed || f.completed || f.completing {
		return false
	}
	f.completing = true
	return true
}

// claimPreparedCompletion transitions from completing to completed.
// Returns false if the filter was destroyed or completed in the meantime.
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

// claimCompletion atomically marks the filter as completed.
// Returns false if already completed or destroyed.
func (f *sandboxFilter) claimCompletion() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.destroyed || f.completed || f.completing {
		return false
	}
	f.completed = true
	return true
}

// abortCompletion resets the completing flag. If markDestroyed is true,
// also marks the filter as destroyed.
func (f *sandboxFilter) abortCompletion(markDestroyed bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completing = false
	if markDestroyed {
		f.destroyed = true
	}
	return !f.destroyed && !f.completed
}

// Destroy cancels any in-flight async wake context. Called by Envoy when
// the filter is destroyed (e.g., stream reset).
func (f *sandboxFilter) Destroy() {
	f.mu.Lock()
	cancel := f.cancel
	f.destroyed = true
	f.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// isEnvoyStreamGonePanic checks if the recovered value is a known Envoy
// panic indicating the stream has been finished or the filter destroyed.
func isEnvoyStreamGonePanic(recovered interface{}) bool {
	message, ok := recovered.(string)
	if !ok {
		return false
	}
	return strings.Contains(message, "request has been finished") ||
		strings.Contains(message, "golang filter has been destroyed")
}
