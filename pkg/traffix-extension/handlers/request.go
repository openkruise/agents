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

package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/go-logr/logr"
	structpb "google.golang.org/protobuf/types/known/structpb"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/traffix-extension/framework/credential"
	"github.com/openkruise/agents/pkg/traffix-extension/plugins"
	logutil "github.com/openkruise/agents/pkg/traffix-extension/util/logging"
	"github.com/openkruise/agents/pkg/traffix-extension/util/matcher"
	"github.com/openkruise/agents/pkg/traffix-extension/util/podlabels"
)

const (
	extProcAttrsKey = "envoy.filters.http.ext_proc"

	filterStateDownstreamPeerName      = "filter_state['downstream_peer'].name"
	filterStateDownstreamPeerNamespace = "filter_state['downstream_peer'].namespace"
	filterStateSandboxToken            = "filter_state['sandbox.token']"
	filterStateSandboxLabels           = "filter_state['sandbox.labels']"

	defaultSandboxTokenEnvVar = "DEFAULT_SANDBOX_TOKEN"
	defaultPodName            = "sample"
	defaultPodNs              = "default"
)

// getFallbackToken reads the sandbox token from environment variable DEFAULT_SANDBOX_TOKEN.
// Falls back to empty string if not set.
func getFallbackToken() string {
	if v := os.Getenv(defaultSandboxTokenEnvVar); v != "" {
		return v
	}
	return ""
}

// HandleRequestHeaders processes request headers by walking the rule chain
// and dispatching to the registered plugins.
//
// Algorithm:
//  1. Resolve the source pod from filter_state (with E2E test fallbacks).
//  2. Look up matching SecurityProfiles via dynamic label matching.
//  3. For each rule whose match clause succeeds, invoke each plugin in
//     registration order. A plugin returning ActionImmediate short-circuits
//     the entire request. A plugin returning ActionMutate has its responses
//     accumulated; the same plugin is invoked at most once across all rules
//     (first matching rule wins).
//  4. Return the accumulated mutation responses, or a passthrough response
//     when no plugin produced a mutation.
func (s *Server) HandleRequestHeaders(ctx context.Context, headers *extProcPb.HttpHeaders, attrs map[string]*structpb.Struct) ([]*extProcPb.ProcessingResponse, error) {
	logger := log.FromContext(ctx)
	loggerD := logger.V(logutil.DEBUG)
	loggerD.Info("Handling request headers", "req.Attributes", attrs)

	// Step 1: extract pod info & sandbox token from filter_state.
	extProcAttrs := extractExtProcAttrs(attrs)
	podNamespace := extractFilterStateValueFromStruct(extProcAttrs, filterStateDownstreamPeerNamespace)
	podName := extractFilterStateValueFromStruct(extProcAttrs, filterStateDownstreamPeerName)
	sandboxTokenRaw := extractFilterStateValueFromStruct(extProcAttrs, filterStateSandboxToken)
	sandboxLabelsEncoded := extractFilterStateValueFromStruct(extProcAttrs, filterStateSandboxLabels)

	if podNamespace == "" || podName == "" {
		loggerD.Info("No pod info in filter_state, falling back to E2E defaults",
			"podNamespace", podNamespace, "podName", podName)
		podNamespace = defaultPodNs
		podName = defaultPodName
	}

	podLabels := podlabels.ParseSandboxLabels(sandboxLabelsEncoded)
	if sandboxLabelsEncoded != "" && len(podLabels) == 0 {
		loggerD.Info("sandbox.labels was present but failed to decode", "encoded", sandboxLabelsEncoded)
	}
	loggerD.Info("Extracted pod info from attributes",
		"pod", podName, "namespace", podNamespace, "labels", podLabels)

	// Step 2: profile lookup.
	profiles := s.configStore.FindProfilesForLabels(podNamespace, podLabels)

	// Step 3: parse request info & resolve sandbox token (best-effort).
	reqHeaders := extractHeaderMap(headers)
	reqInfo := matcher.ParseRequestInfo(reqHeaders)

	// Single VERBOSE summary line per request — pod identity, request identity,
	// and matched profile count. Everything else in this flow is DEBUG.
	logger.V(logutil.VERBOSE).Info("Handling request",
		"pod", podName, "namespace", podNamespace,
		"method", reqInfo.Method, "host", reqInfo.Host, "path", reqInfo.Path,
		"profiles", len(profiles))

	if len(profiles) == 0 {
		loggerD.Info("No profiles found for pod labels",
			"pod", podName, "namespace", podNamespace, "labels", podLabels)
		return passThroughResponse(), nil
	}

	sandboxToken := resolveSandboxToken(ctx, sandboxTokenRaw)
	podNN := types.NamespacedName{Namespace: podNamespace, Name: podName}

	// Step 4: walk rules and dispatch to plugins.
	rctx := &plugins.RequestContext{
		Headers:      reqHeaders,
		Info:         reqInfo,
		PodNN:        podNN,
		SandboxToken: sandboxToken,
		CredClient:   s.credClient,
	}

	var accumulated []*extProcPb.ProcessingResponse
	mutated := make(map[string]bool, len(s.plugins))

	for _, profile := range profiles {
		rctx.Profile = profile
		for i := range profile.Spec.Rules {
			rule := &profile.Spec.Rules[i]
			if !matcher.MatchesRule(reqInfo, *rule) || rule.Actions == nil {
				continue
			}
			warnUnimplementedActions(logger, profile, rule)
			for _, p := range s.plugins {
				if mutated[p.Name()] {
					continue
				}
				res, err := p.OnRequestHeaders(ctx, rctx, rule)
				if err != nil {
					return nil, err
				}
				switch res.Action {
				case plugins.ActionImmediate:
					return res.Responses, nil
				case plugins.ActionMutate:
					accumulated = append(accumulated, res.Responses...)
					mutated[p.Name()] = true
				case plugins.ActionContinue:
				}
			}
		}
	}

	if len(accumulated) == 0 {
		loggerD.Info("No plugin produced mutations; passthrough", "pod", podNN.String())
		return passThroughResponse(), nil
	}
	return accumulated, nil
}

// resolveSandboxToken parses the sandbox token from filter_state, falling
// back to the DEFAULT_SANDBOX_TOKEN env var. Returns nil when no usable token
// is available — the sentinel "-" value is also treated as "no token".
func resolveSandboxToken(ctx context.Context, raw string) *credential.SandboxToken {
	logger := log.FromContext(ctx)
	loggerD := logger.V(logutil.DEBUG)

	if raw == "" {
		raw = getFallbackToken()
		if raw == "" {
			loggerD.Info("No sandbox token in filter_state and no DEFAULT_SANDBOX_TOKEN env var set")
			return nil
		}
		loggerD.Info("No sandbox token in filter_state, falling back to DEFAULT_SANDBOX_TOKEN env var")
	}
	if raw == "-" {
		loggerD.Info("Sandbox token sentinel '-' present, treating as no token")
		return nil
	}

	parsed, err := ParseSandboxToken(raw)
	if err != nil {
		logger.Error(err, "Failed to parse sandbox token; treating as no token")
		return nil
	}
	if parsed.AccessToken == "" {
		loggerD.Info("Sandbox token has no accessToken; treating as no token")
		return nil
	}
	if parsed.SandboxClientID == "" {
		loggerD.Info("sandboxClientId is empty; token injection will use empty resourceId")
	}
	return parsed
}

// extractExtProcAttrs extracts the nested map from envoy.filters.http.ext_proc.
// The filter_state data (pod name, namespace, sandbox token) is stored within this nested struct.
func extractExtProcAttrs(attrs map[string]*structpb.Struct) map[string]*structpb.Struct {
	if attrs == nil {
		return nil
	}

	if s, ok := attrs[extProcAttrsKey]; ok {
		return extractNestedMap(s)
	}

	for key, s := range attrs {
		if strings.Contains(key, "ext_proc") {
			return extractNestedMap(s)
		}
	}

	return nil
}

// extractNestedMap extracts a nested map from a structpb.Struct.
// The struct may contain nested maps that hold the actual filter_state values.
func extractNestedMap(s *structpb.Struct) map[string]*structpb.Struct {
	if s == nil {
		return nil
	}

	m := s.AsMap()
	if m == nil {
		return nil
	}

	result := make(map[string]*structpb.Struct)
	for k, v := range m {
		if nested, ok := v.(*structpb.Struct); ok {
			result[k] = nested
			continue
		}

		if nestedMap, ok := v.(map[string]interface{}); ok {
			if nestedStruct, err := structpb.NewStruct(nestedMap); err == nil {
				result[k] = nestedStruct
			}
			continue
		}

		// For primitive values (strings, etc.), wrap them in a struct with a "value" field.
		if v != nil {
			if wrapped, err := structpb.NewStruct(map[string]interface{}{"value": v}); err == nil {
				result[k] = wrapped
			}
		}
	}

	return result
}

// extractFilterStateValueFromStruct extracts a string value from the nested filter_state attributes.
// The attrs map contains the ext_proc attributes, with keys like filter_state['downstream_peer'].name.
func extractFilterStateValueFromStruct(attrs map[string]*structpb.Struct, key string) string {
	if attrs == nil {
		return ""
	}

	if s, ok := attrs[key]; ok {
		return extractStringValue(s)
	}

	fsKey := "filter_state['" + key + "']"
	if s, ok := attrs[fsKey]; ok {
		return extractStringValue(s)
	}

	for attrKey, s := range attrs {
		if strings.HasSuffix(attrKey, key) {
			return extractStringValue(s)
		}
	}

	return ""
}

// extractStringValue extracts a string value from a structpb.Struct.
func extractStringValue(s *structpb.Struct) string {
	if s == nil {
		return ""
	}

	m := s.AsMap()
	if m == nil {
		return ""
	}

	if v, ok := m["value"]; ok {
		if str, ok := v.(string); ok {
			return str
		}
	}

	for _, v := range m {
		if str, ok := v.(string); ok {
			return str
		}
	}

	return ""
}

// extractHeaderMap converts Envoy's HeaderMap to a plain string map for easier processing.
// Envoy normalizes header names to lowercase per HTTP/2 spec, so keys are stored in lowercase.
func extractHeaderMap(headers *extProcPb.HttpHeaders) map[string]string {
	result := make(map[string]string)
	if headers == nil || headers.GetHeaders() == nil {
		return result
	}
	for _, h := range headers.GetHeaders().GetHeaders() {
		if h.Key != "" {
			result[strings.ToLower(h.Key)] = string(h.RawValue)
		}
	}
	return result
}

// warnUnimplementedActions logs a warning for each action type that is
// declared on a matched rule but does not yet have a corresponding plugin
// implementation. The request is still passed through; the warning makes
// it visible that the policy author wrote a rule the data plane cannot
// honor (Bypass, Forwarding, IdentityInjection, SecurityCheck, Mirroring,
// RateLimit are reserved for future plugins).
func warnUnimplementedActions(logger logr.Logger, profile *v1alpha1.SecurityProfile, rule *v1alpha1.SecurityRule) {
	a := rule.Actions
	if a == nil {
		return
	}
	var unimplemented []string
	if a.Bypass {
		unimplemented = append(unimplemented, "bypass")
	}
	if a.Forwarding != nil {
		unimplemented = append(unimplemented, "forwarding")
	}
	if a.IdentityInjection != nil {
		unimplemented = append(unimplemented, "identityInjection")
	}
	if a.SecurityCheck != nil {
		unimplemented = append(unimplemented, "securityCheck")
	}
	if a.Mirroring != nil {
		unimplemented = append(unimplemented, "mirroring")
	}
	if a.RateLimit != nil {
		unimplemented = append(unimplemented, "rateLimit")
	}
	if len(unimplemented) == 0 {
		return
	}
	logger.V(logutil.DEFAULT).Info(
		"SecurityProfile rule declares actions that are not yet implemented; ignoring",
		"profile", profile.Namespace+"/"+profile.Name,
		"rule", rule.Name,
		"actions", unimplemented)
}

// passThroughResponse returns a simple passthrough response with no header modifications.
func passThroughResponse() []*extProcPb.ProcessingResponse {
	return []*extProcPb.ProcessingResponse{
		{Response: &extProcPb.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extProcPb.HeadersResponse{},
		}},
	}
}

// ParseSandboxToken parses the sandbox.token filter_state value as a base64-encoded JSON string.
// The sandbox.token is base64-encoded; after decoding, it becomes a JSON string with fields:
// requestId, accessToken, and sandboxClientId.
// Returns an error if the input is empty, not valid base64, or not valid JSON.
func ParseSandboxToken(raw string) (*credential.SandboxToken, error) {
	if raw == "" {
		return nil, fmt.Errorf("sandbox token is empty")
	}

	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("failed to base64-decode sandbox token: %w", err)
	}

	var parsed credential.SandboxToken
	if err := json.Unmarshal(decoded, &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse sandbox token: %w", err)
	}
	return &parsed, nil
}

// HandleRequestBody processes request body.
// Currently a pass-through stub -- returns an empty response.
func (s *Server) HandleRequestBody(ctx context.Context, body []byte) ([]*extProcPb.ProcessingResponse, error) {
	return []*extProcPb.ProcessingResponse{
		{
			Response: &extProcPb.ProcessingResponse_RequestBody{
				RequestBody: &extProcPb.BodyResponse{},
			},
		},
	}, nil
}

// HandleRequestTrailers processes request trailers.
// Currently a pass-through stub -- returns an empty response.
func (s *Server) HandleRequestTrailers(ctx context.Context, trailers *extProcPb.HttpTrailers) ([]*extProcPb.ProcessingResponse, error) {
	return []*extProcPb.ProcessingResponse{
		{
			Response: &extProcPb.ProcessingResponse_RequestTrailers{
				RequestTrailers: &extProcPb.TrailersResponse{},
			},
		},
	}, nil
}

// HandleResponseHeaders processes response headers.
// Currently a pass-through stub -- returns an empty response.
func (s *Server) HandleResponseHeaders(ctx context.Context, headers *extProcPb.HttpHeaders) ([]*extProcPb.ProcessingResponse, error) {
	return []*extProcPb.ProcessingResponse{
		{
			Response: &extProcPb.ProcessingResponse_ResponseHeaders{
				ResponseHeaders: &extProcPb.HeadersResponse{},
			},
		},
	}, nil
}

// HandleResponseBody processes response body.
// Currently a pass-through stub -- returns an empty response.
func (s *Server) HandleResponseBody(ctx context.Context, body *extProcPb.HttpBody) ([]*extProcPb.ProcessingResponse, error) {
	return []*extProcPb.ProcessingResponse{
		{
			Response: &extProcPb.ProcessingResponse_ResponseBody{
				ResponseBody: &extProcPb.BodyResponse{},
			},
		},
	}, nil
}

// HandleResponseTrailers processes response trailers.
// Currently a pass-through stub -- returns an empty response.
func (s *Server) HandleResponseTrailers(ctx context.Context, trailers *extProcPb.HttpTrailers) ([]*extProcPb.ProcessingResponse, error) {
	return []*extProcPb.ProcessingResponse{
		{
			Response: &extProcPb.ProcessingResponse_ResponseTrailers{
				ResponseTrailers: &extProcPb.TrailersResponse{},
			},
		},
	}, nil
}
