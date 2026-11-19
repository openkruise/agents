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
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"go.uber.org/zap"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	serving "github.com/openkruise/agents/pkg/sandbox-gateway/sessionservice"
	"github.com/openkruise/agents/pkg/sandboxroute"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
	"github.com/openkruise/agents/pkg/utils"
)

const sessionCookieName = "kruise-agent-session-id"

// sessionServiceFilter owns SessionService admission and asynchronous allocation.
// Its completion state is independent of Sandbox authentication and wake handling.
type sessionServiceFilter struct {
	api.PassThroughStreamFilter
	callbacks      api.FilterCallbackHandler
	config         *Config
	servingService *serving.Service
	authority      string

	sessionFromCookie bool

	mu             sync.Mutex
	completing     bool
	completed      bool
	destroyed      bool
	cancel         context.CancelFunc
	servingSession string
}

var _ api.StreamFilter = (*sessionServiceFilter)(nil)

func (f *sessionServiceFilter) DecodeHeaders(header api.RequestHeaderMap, endStream bool) api.StatusType {
	sessionCount := 0
	sessionID := ""
	cookies := http.Header{}
	header.Range(func(key, value string) bool {
		if strings.EqualFold(key, serving.SessionHeader) {
			sessionCount++
			sessionID = value
		}
		if strings.EqualFold(key, "cookie") {
			cookies.Add("Cookie", value)
		}
		return true
	})
	uid, port, _, err := serving.ParseAuthority(f.authority)
	if err != nil {
		f.sendLocalReplyOnce(400, "invalid SessionService request", "InvalidRequest")
		return api.LocalReply
	}
	if sessionCount > 1 || sessionID != "" && !sessionbinding.ValidSessionID(sessionID) {
		f.sendLocalReplyOnce(400, "invalid SessionService request", "InvalidSessionID")
		return api.LocalReply
	}
	// An explicit header, including an empty one requesting a new session,
	// takes precedence over browser affinity. Invalid cookies act as absent.
	if sessionCount == 0 {
		request := http.Request{Header: cookies}
		if cookie, err := request.Cookie(sessionCookieName); err == nil && sessionbinding.ValidSessionID(cookie.Value) {
			sessionID = cookie.Value
			f.sessionFromCookie = true
		}
	}
	return f.decodeSessionService(header, uid, sessionID, port)
}

func (f *sessionServiceFilter) decodeSessionService(header api.RequestHeaderMap, uid, sessionID string, port int) api.StatusType {
	if !registry.GetRegistry().Ready() || f.servingService == nil {
		f.sendLocalReplyOnce(503, "SessionManagerUnavailable", "SessionManagerUnavailable")
		return api.LocalReply
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	f.mu.Lock()
	if f.destroyed {
		f.mu.Unlock()
		cancel()
		return api.Running
	}
	f.cancel = cancel
	f.mu.Unlock()
	go func() {
		defer cancel()
		f.servingAndContinue(ctx, header, uid, sessionID, port)
	}()
	return api.Running
}

func (f *sessionServiceFilter) servingAndContinue(ctx context.Context, header api.RequestHeaderMap, uid, sessionID string, port int) {
	defer func() {
		if recovered := recover(); recovered != nil && !isEnvoyStreamGonePanic(recovered) {
			klog.FromContext(ctx).Error(fmt.Errorf("panic of type %T", recovered), "SessionService allocation panicked",
				"sessionServiceUID", uid, "sessionID", sessionID)
			f.sendLocalReplyOnce(503, "SessionManagerUnavailable", "SessionManagerUnavailable")
		}
	}()
	result, err := f.servingService.ResolveRoute(ctx, uid, sessionID, port)
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	if err != nil {
		var rejection *serving.Error
		if errors.As(err, &rejection) {
			f.sendLocalReplyOnce(rejection.Status, rejection.Reason, rejection.Reason)
		} else {
			f.sendLocalReplyOnce(503, "SessionManagerUnavailable", "SessionManagerUnavailable")
		}
		return
	}
	// SessionService domains currently allow unauthenticated access to owned members.
	// The member runtime credential remains separate from domain authentication.
	prepare, ok := f.prepareSessionServiceRuntime(header, result.Route, port)
	if !ok {
		f.sendLocalReplyOnce(503, "SessionManagerUnavailable", "SessionManagerUnavailable")
		return
	}
	f.completeWithContinue(result.Route, port, func() {
		if prepare != nil {
			prepare()
		}
		header.Set(serving.SessionHeader, result.SessionID)
		f.servingSession = result.SessionID
	})
}

// EncodeHeaders echoes the confirmed Session ID in the header and host-only
// browser cookie, even on upstream errors. Business response status never triggers
// another admission or replay.
func (f *sessionServiceFilter) EncodeHeaders(header api.ResponseHeaderMap, endStream bool) api.StatusType {
	f.mu.Lock()
	sessionID := f.servingSession
	f.mu.Unlock()
	if sessionID != "" {
		header.Set(serving.SessionHeader, sessionID)
		// #nosec G124 -- Secure is intentionally omitted to support plain HTTP SessionService endpoints.
		cookie := http.Cookie{Name: sessionCookieName, Value: sessionID, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode}
		header.Add("set-cookie", cookie.String())
	}
	header.Set("cache-control", "no-store")
	if sessionID == "" {
		header.Del(serving.SessionHeader)
	}
	return api.Continue
}

// prepareSessionServiceRuntime supplies only agent-runtime's separate static credential.
// Business ports retain the caller's headers and never receive a runtime secret.
func (f *sessionServiceFilter) prepareSessionServiceRuntime(header api.RequestHeaderMap, route sandboxroute.Route, port int) (func(), bool) {
	if port != utils.RuntimePort {
		return nil, true
	}
	routes := registry.GetRegistry()
	if !routes.Ready() {
		return nil, false
	}
	cached, ok := routes.Get(route.ID)
	if !ok || cached.ID != route.ID || cached.UID != route.UID || cached.Namespace != route.Namespace || cached.Name != route.Name {
		return nil, false
	}
	// The completion helper invokes this only after claiming the completion,
	// while holding the same destruction guard as the upstream metadata writes.
	return func() {
		if cached.AccessToken != "" {
			header.Set(accessTokenHeader, cached.AccessToken)
		} else {
			// An empty route token retains the existing no-static-credential policy;
			// do not forward an unrelated caller token as a runtime credential.
			header.Del(accessTokenHeader)
		}
	}, true
}

func (f *sessionServiceFilter) applyUpstreamOverrides(route sandboxroute.Route, sandboxPort int) {
	upstreamHost := net.JoinHostPort(route.IP, strconv.Itoa(sandboxPort))
	f.callbacks.StreamInfo().DynamicMetadata().Set("envoy.lb.original_dst", "host", upstreamHost)
	if f.config.EnableRuntimeMTLS && sandboxPort == utils.RuntimePort {
		f.callbacks.StreamInfo().DynamicMetadata().Set(runtimeMTLSMetadataNamespace, runtimeMTLSMetadataKey, true)
		f.callbacks.ClearRouteCache()
	}
	logger.Debug("Upstream override set successfully", zap.String("upstreamHost", upstreamHost))
}

// sendLocalReplyOnce sends a LocalReply, but only if no other completion
// action has been taken. This prevents double-reply when the async goroutine
// and Destroy race.
func (f *sessionServiceFilter) sendLocalReplyOnce(code int, body string, details string) {
	if !f.claimCompletion() {
		return
	}
	headers := map[string][]string{"cache-control": {"no-store"}}
	// Clear only a stale browser session. Explicit headers and transient failures
	// must not discard browser affinity; this request still returns 404.
	if f.sessionFromCookie && code == http.StatusNotFound && details == "SessionNotFound" {
		cookie := http.Cookie{Name: sessionCookieName, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode}
		headers["set-cookie"] = []string{cookie.String()}
	}
	f.callbacks.DecoderFilterCallbacks().SendLocalReply(code, body, headers, -1, details)
}

// completeWithContinue sets upstream metadata and calls Continue to resume
// Envoy request processing after a successful SessionService allocation.
func (f *sessionServiceFilter) completeWithContinue(route sandboxroute.Route, sandboxPort int, prepareHeaders func()) {
	if !f.beginCompletion() {
		return
	}

	// Set upstream metadata. This may panic if Envoy has already destroyed
	// the stream, so we recover and abort.
	if !f.setUpstreamMetadata(route, sandboxPort, prepareHeaders) {
		return
	}

	if !f.claimPreparedCompletion() {
		return
	}
	f.callbacks.DecoderFilterCallbacks().Continue(api.Continue)
}

// setUpstreamMetadata applies the upstream overrides (original_dst and, when
// applicable, the runtime mTLS selection) after SessionService allocation. Returns false if the stream was destroyed during the call.
func (f *sessionServiceFilter) setUpstreamMetadata(route sandboxroute.Route, sandboxPort int, prepareHeaders func()) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			if isEnvoyStreamGonePanic(r) {
				f.abortCompletion(true)
				ok = false
				return
			}
			logger.Error("panic before SessionService allocation continue",
				zap.String("sandboxID", route.ID), zap.Any("recover", r))
			if f.abortCompletion(false) {
				f.sendLocalReplyOnce(503, "SessionManagerUnavailable", "SessionManagerUnavailable")
			}
			ok = false
		}
	}()
	// Serialize metadata writes with OnDestroy. A completion claimed before
	// destruction must recheck the stream before touching Envoy state.
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.destroyed || f.completed {
		f.completing = false
		return false
	}
	if prepareHeaders != nil {
		prepareHeaders()
	}
	f.applyUpstreamOverrides(route, sandboxPort)
	return true
}

// beginCompletion marks the start of the completion phase.
// Returns false if the filter is already completed or destroyed.
func (f *sessionServiceFilter) beginCompletion() bool {
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
func (f *sessionServiceFilter) claimPreparedCompletion() bool {
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
func (f *sessionServiceFilter) claimCompletion() bool {
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
func (f *sessionServiceFilter) abortCompletion(markDestroyed bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completing = false
	if markDestroyed {
		f.destroyed = true
	}
	return !f.destroyed && !f.completed
}

// OnDestroy cancels any in-flight SessionService allocation context. Called by Envoy when
// the filter/stream is destroyed (e.g., stream reset, connection close).
func (f *sessionServiceFilter) OnDestroy(reason api.DestroyReason) {
	f.mu.Lock()
	cancel := f.cancel
	f.destroyed = true
	f.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
