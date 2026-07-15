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
	"crypto/subtle"
	"fmt"

	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
	proxyutils "github.com/openkruise/agents/pkg/utils/proxyutils"
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

	if f.config.EnableAuth {
		if status := f.authenticate(header, route); status != api.Continue {
			return status
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

func (f *sandboxFilter) authenticate(header api.RequestHeaderMap, route proxyutils.Route) api.StatusType {
	if f.config.EnableJWTAuth {
		return f.authenticateJWT(header, route)
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

func (f *sandboxFilter) authenticateJWT(header api.RequestHeaderMap, route proxyutils.Route) api.StatusType {
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
			401,
			"unauthorized: invalid or missing traffic access token",
			nil,
			-1,
			"unauthorized",
		)
		return api.LocalReply
	}
	if claims.Sandbox.SandboxID != route.ID || claims.Sandbox.SandboxUID != string(route.UID) {
		logger.Warn("Traffic access token sandbox mismatch", zap.String("sandboxID", route.ID))
		f.callbacks.DecoderFilterCallbacks().SendLocalReply(
			401,
			"unauthorized: traffic access token does not match sandbox",
			nil,
			-1,
			"unauthorized",
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
