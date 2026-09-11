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
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	"github.com/openkruise/agents/pkg/sandbox-gateway/wake"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
	"github.com/openkruise/agents/pkg/utils"
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
	// runtimeMTLSMetadataNamespace and runtimeMTLSMetadataKey select the mTLS ORIGINAL_DST cluster.
	runtimeMTLSMetadataNamespace = "agents.kruise.io/sandbox-gateway"
	runtimeMTLSMetadataKey       = "upstream-mtls"
)

// Compile-time assertion that sandboxFilter implements the api.StreamFilter interface.
// This catches mismatches like implementing Destroy() (Config interface) instead of
// OnDestroy(DestroyReason) (StreamFilter interface).
var _ api.StreamFilter = (*sandboxFilter)(nil)

// FilterFactory creates a new sandbox filter instance for each stream.
func FilterFactory(c interface{}, callbacks api.FilterCallbackHandler) api.StreamFilter {
	cfg := c.(*FilterConfig)
	return &sandboxFilter{
		callbacks:      callbacks,
		config:         cfg.Config,
		adapter:        cfg.Adapter,
		jwtAuthManager: cfg.jwtAuthManager,
	}
}

type sandboxFilter struct {
	api.PassThroughStreamFilter
	callbacks      api.FilterCallbackHandler
	config         *Config
	adapter        *adapters.E2BAdapter
	jwtAuthManager JWTAuthManager

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

	log := logger.With(zap.String("sandboxID", sandboxID))

	log.Debug("DecodeHeaders: adapter mapped request",
		zap.Int("sandboxPort", sandboxPort),
		zap.Any("extraHeaders", extraHeaders))

	// Look up the pod IP from registry. Readiness is read separately from the
	// route lookup, so a concurrent SetReady may flip between the two. ready
	// only moves false->true once at startup and back to false on shutdown, so
	// the worst case is one extra successful read during teardown, which is
	// harmless.
	routeRegistry := registry.GetRegistry()
	if !routeRegistry.Ready() {
		logger.Warn("Sandbox gateway route registry is not ready")
		f.callbacks.DecoderFilterCallbacks().SendLocalReply(
			503,
			"sandbox gateway is not ready",
			nil,
			-1,
			"gateway_not_ready",
		)
		return api.LocalReply
	}
	route, ok := routeRegistry.Get(sandboxID)
	if !ok {
		log.Warn("Sandbox not found in registry")
		f.callbacks.DecoderFilterCallbacks().SendLocalReply(
			502,
			"sandbox not found: "+sandboxID,
			nil,
			-1,
			"sandbox_not_found",
		)
		return api.LocalReply
	}

	// Authenticate the request before any state/wake handling so that
	// paused sandboxes are not woken by unauthorized requests.
	if status := f.authenticate(header, route); status != api.Continue {
		return status
	}

	if route.State != agentsv1alpha1.SandboxStateRunning {
		waker := wake.GetWaker()
		if f.shouldWakeSandbox(route, waker) {
			// Apply extra headers before returning Running so they
			// are visible to subsequent filter phases.
			for k, v := range extraHeaders {
				header.Set(k, v)
			}
			// Launch async wake with a detached context. The filter
			// returns Running to tell Envoy to suspend request
			// processing. wakeAndContinue will call Continue or
			// SendLocalReply when the wake completes.
			//
			// This context carries the sole wake deadline.
			waitTimeout := time.Duration(f.config.GetWakeTimeoutSeconds()) * time.Second
			ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
			f.mu.Lock()
			f.cancel = cancel
			f.mu.Unlock()
			go f.wakeAndContinue(ctx, waker, route.Namespace, route.Name, sandboxID, sandboxPort)
			return api.Running
		}
		// Not running and not wakeable -> 502 (existing behavior)
		log.Warn("Sandbox is not running", zap.String("state", route.State))
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

	f.applyUpstreamOverrides(route, sandboxPort)
	return api.Continue
}

// applyUpstreamOverrides routes the request to the sandbox upstream: it sets
// the envoy.lb.original_dst override and, for runtime-port traffic when
// runtime mTLS is enabled, selects the mTLS upstream cluster and re-resolves
// the route. The normal Running path and the async wake completion path must
// apply identical overrides so a request that triggered a wake continues
// exactly like a request that arrived on a Running sandbox.
func (f *sandboxFilter) applyUpstreamOverrides(route sandboxroute.Route, sandboxPort int) {
	upstreamHost := net.JoinHostPort(route.IP, strconv.Itoa(sandboxPort))
	f.callbacks.StreamInfo().DynamicMetadata().Set("envoy.lb.original_dst", "host", upstreamHost)
	if f.config.EnableRuntimeMTLS && sandboxPort == utils.RuntimePort {
		f.callbacks.StreamInfo().DynamicMetadata().Set(runtimeMTLSMetadataNamespace, runtimeMTLSMetadataKey, true)
		f.callbacks.ClearRouteCache()
	}
	logger.Debug("Upstream override set successfully", zap.String("upstreamHost", upstreamHost))
}

func (f *sandboxFilter) authenticate(header api.RequestHeaderMap, route sandboxroute.Route) api.StatusType {
	if route.RequireTrafficAuth {
		if !f.config.EnableJWTAuth {
			return f.verifierUnavailable(route.ID)
		}
		return f.authenticateJWT(header, route)
	}
	// A route that has not opted into JWT enforcement is authenticated by the
	// EnableAuth baseline below, exactly as it would be with JWT mode off. The two
	// switches govern disjoint sets of routes, so enabling JWT neither exposes a
	// route that UUID mode protected nor starts rejecting traffic that an
	// unauthenticated deployment allowed.
	//
	// Stripping the traffic token is scoped to JWT mode rather than unconditional:
	// only a gateway that verifies these tokens owns the header, so only it can
	// claim the one arriving here is meaningless. With the capability off the
	// gateway never reads that header and forwards it untouched, so a workload must
	// not treat it as gateway-asserted identity.
	if f.config.EnableJWTAuth {
		header.Del(f.config.GetTrafficAccessTokenHeader())
	}
	if !f.config.EnableAuth {
		return api.Continue
	}
	if route.AccessToken == "" {
		return api.Continue
	}
	requestToken, _ := header.Get(accessTokenHeader)
	if subtle.ConstantTimeCompare([]byte(requestToken), []byte(route.AccessToken)) == 1 {
		return api.Continue
	}
	logger.Warn("Access token mismatch", zap.String("sandboxID", route.ID))
	f.callbacks.DecoderFilterCallbacks().SendLocalReply(
		401,
		"unauthorized: invalid or missing access token",
		nil,
		-1,
		"unauthorized",
	)
	return api.LocalReply
}

func (f *sandboxFilter) authenticateJWT(header api.RequestHeaderMap, route sandboxroute.Route) api.StatusType {
	if f.jwtAuthManager == nil {
		return f.verifierUnavailable(route.ID)
	}
	verifier := f.jwtAuthManager.Current()
	if verifier == nil {
		return f.verifierUnavailable(route.ID)
	}
	headerName := f.config.GetTrafficAccessTokenHeader()
	rawJWT, _ := header.Get(headerName)
	claims, err := verifier.Verify(rawJWT)
	if err != nil {
		logger.Warn("Traffic access token verification failed", zap.String("sandboxID", route.ID), zap.Error(err))
		f.callbacks.DecoderFilterCallbacks().SendLocalReply(
			403,
			"forbidden: invalid or missing traffic access token",
			nil,
			-1,
			"forbidden",
		)
		return api.LocalReply
	}
	if claims.Sandbox.SandboxID != route.ID || claims.Sandbox.SandboxUID != string(route.UID) {
		logger.Warn("Traffic access token sandbox mismatch", zap.String("sandboxID", route.ID))
		f.callbacks.DecoderFilterCallbacks().SendLocalReply(
			403,
			"forbidden: traffic access token does not match sandbox",
			nil,
			-1,
			"forbidden",
		)
		return api.LocalReply
	}
	header.Del(headerName)
	return api.Continue
}

func (f *sandboxFilter) verifierUnavailable(sandboxID string) api.StatusType {
	logger.Warn("Traffic access token verifier is unavailable", zap.String("sandboxID", sandboxID))
	f.callbacks.DecoderFilterCallbacks().SendLocalReply(
		503,
		"service unavailable: traffic access token verifier is not ready",
		nil,
		-1,
		"jwt_verifier_not_ready",
	)
	return api.LocalReply
}

// shouldWakeSandbox determines whether a non-Running sandbox should be woken
// by traffic. Returns true only when wake-on-traffic is enabled, the sandbox
// is Paused, the waker is initialized, the route carries a full ObjectKey,
// the informer still holds a sandbox with the route's UID (stale-route
// fence), and either the route registry already has WakeOnTraffic set or
// WakeEnabled reads the spec directly from the informer cache (covering the
// window between a spec patch and the gateway controller reconciling the
// change into the route registry).
func (f *sandboxFilter) shouldWakeSandbox(route sandboxroute.Route, waker *wake.Waker) bool {
	if route.State != agentsv1alpha1.SandboxStatePaused {
		return false
	}
	if !f.config.EnableWakeOnTraffic {
		return false
	}
	if waker == nil {
		return false
	}
	key, ok := route.ObjectKey()
	if !ok {
		return false
	}
	// UID fence: the route registry can lag behind deletions, so the route
	// may reference a sandbox that was deleted and recreated under the same
	// name. The request was authenticated against the route's identity, so a
	// stale route must neither wake the new object nor inherit its wake
	// configuration. Verify the informer still holds the exact object.
	if !waker.SandboxUIDMatches(context.Background(), key.Namespace, key.Name, route.UID) {
		logger.Warn("Stale wake route: sandbox UID mismatch or object gone",
			zap.String("sandboxID", route.ID),
			zap.String("namespace", key.Namespace),
			zap.String("name", key.Name),
			zap.String("routeUID", string(route.UID)))
		return false
	}
	// route.WakeOnTraffic is the primary check (fast, from registry).
	if route.WakeOnTraffic {
		return true
	}
	// WakeEnabled is a fallback that reads the informer cache directly,
	// covering the window between a spec patch and the gateway controller
	// reconciling the change into the route registry.
	return waker.WakeEnabled(context.Background(), key.Namespace, key.Name)
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
) {
	log := logger.With(zap.String("sandboxID", sandboxID))

	defer func() {
		if r := recover(); r != nil {
			if isEnvoyStreamGonePanic(r) {
				return
			}
			log.Error("panic during async wake", zap.Any("recover", r))
			f.sendLocalReplyOnce(500, "wake failed", "wake_panic")
		}
	}()

	err := waker.Wake(ctx, namespace, name)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Filter was destroyed; do nothing.
			return
		}
		log.Warn("Sandbox wake failed", zap.Error(err))
		// Keep the reply body generic: err can carry Kubernetes API error
		// strings (namespaces, names, conflict details) that must not be
		// echoed to external callers. The detail is logged above.
		f.sendLocalReplyOnce(503, "sandbox wake failed", "sandbox_wake_failed")
		return
	}

	// Wake succeeded — verify the sandbox is now Running in the registry.
	route, ok := registry.GetRegistry().Get(sandboxID)
	if !ok || route.State != agentsv1alpha1.SandboxStateRunning {
		log.Warn("Sandbox not running after wake")
		f.sendLocalReplyOnce(502, "healthy sandbox not found: "+sandboxID, "sandbox_not_running")
		return
	}

	log.Info("Sandbox woken successfully")
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
func (f *sandboxFilter) completeWithContinue(route sandboxroute.Route, sandboxPort int) {
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

// setUpstreamMetadata applies the upstream overrides (original_dst and, when
// applicable, the runtime mTLS selection) for a request resumed after an
// async wake. Returns false if the stream was destroyed during the call.
func (f *sandboxFilter) setUpstreamMetadata(route sandboxroute.Route, sandboxPort int) (ok bool) {
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
	f.applyUpstreamOverrides(route, sandboxPort)
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

// OnDestroy cancels any in-flight async wake context. Called by Envoy when
// the filter/stream is destroyed (e.g., stream reset, connection close).
func (f *sandboxFilter) OnDestroy(reason api.DestroyReason) {
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
