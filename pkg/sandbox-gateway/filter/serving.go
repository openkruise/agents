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
	"time"

	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	"github.com/openkruise/agents/pkg/sandbox-gateway/serving"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/utils"
)

func (f *sandboxFilter) decodeServing(header api.RequestHeaderMap, servingID, sessionID string, port int) api.StatusType {
	// Serving authentication is always checked locally, independently of
	// legacy UUID and Sandbox JWT options. Never forward this credential to a Pod.
	rawJWT, _ := header.Get(f.config.GetTrafficAccessTokenHeader())
	header.Del(f.config.GetTrafficAccessTokenHeader())
	if rawJWT == "" {
		f.sendLocalReplyOnce(401, "Serving token is required", "Unauthorized")
		return api.LocalReply
	}
	if !registry.GetRegistry().Ready() || f.servingService == nil {
		f.sendLocalReplyOnce(503, "SessionManagerUnavailable", "SessionManagerUnavailable")
		return api.LocalReply
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	f.mu.Lock()
	if f.destroyed {
		f.mu.Unlock()
		cancel()
		return api.Running
	}
	f.cancel = cancel
	f.servingSession = sessionID
	f.mu.Unlock()
	go func() {
		defer cancel()
		f.servingAndContinue(ctx, header, servingID, sessionID, port, rawJWT)
	}()
	return api.Running
}

func (f *sandboxFilter) servingAndContinue(ctx context.Context, header api.RequestHeaderMap, servingID, sessionID string, port int, rawJWT string) {
	defer func() {
		if recovered := recover(); recovered != nil && !isEnvoyStreamGonePanic(recovered) {
			klog.FromContext(ctx).Error(fmt.Errorf("panic of type %T", recovered), "Serving allocation panicked",
				"servingID", servingID, "sessionID", sessionID)
			f.sendLocalReplyOnce(503, "SessionManagerUnavailable", "SessionManagerUnavailable")
		}
	}()
	route, err := f.servingService.ResolveRoute(ctx, servingID, sessionID, port, rawJWT)
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
	// Local admission verified the Serving JWT and trusted UID, and the
	// shared allocator selected an owned member of that same Serving.
	// That decision authorizes this Serving request even if the member uses
	// Sandbox JWT or UUID authentication on its own domain. Never infer this
	// authority from a hostname or unverified token claims.
	prepare, ok := f.prepareServingRuntime(header, route, port)
	if !ok {
		f.sendLocalReplyOnce(503, "SessionManagerUnavailable", "SessionManagerUnavailable")
		return
	}
	f.completeWithContinue(route, port, prepare)
}

// EncodeHeaders echoes the original, validated Session ID even on upstream
// errors. Business response status never triggers another admission or replay.
func (f *sandboxFilter) EncodeHeaders(header api.ResponseHeaderMap, endStream bool) api.StatusType {
	f.mu.Lock()
	sessionID := f.servingSession
	f.mu.Unlock()
	if sessionID != "" {
		header.Set(serving.SessionHeader, sessionID)
	}
	return api.Continue
}

// prepareServingRuntime supplies only agent-runtime's separate static credential.
// Business ports retain the caller's headers and never receive a runtime secret.
func (f *sandboxFilter) prepareServingRuntime(header api.RequestHeaderMap, route sandboxroute.Route, port int) (func(), bool) {
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
