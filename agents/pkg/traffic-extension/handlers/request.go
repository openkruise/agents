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
	"fmt"
	"strings"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypeV3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"

	structpb "google.golang.org/protobuf/types/known/structpb"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openkruise/agents/pkg/traffic-extension/model"
	"github.com/openkruise/agents/pkg/traffic-extension/plugins"
	env "github.com/openkruise/agents/pkg/traffic-extension/util/env"
	logutil "github.com/openkruise/agents/pkg/traffic-extension/util/logging"
	"github.com/openkruise/agents/pkg/traffic-extension/util/matcher"
	"github.com/openkruise/agents/pkg/traffic-extension/util/podlabels"
)

var unauthenticatedEgressPolicy = env.Register(
	"UNAUTHENTICATED_EGRESS_POLICY", "deny",
	"Policy for egress requests with missing pod identity or corrupted sandbox labels. "+
		"\"deny\" (default) returns 403; \"allow\" passes the request through unmodified.",
).Get()

const (
	extProcAttrsKey = "envoy.filters.http.ext_proc"

	filterStateDownstreamPeerName      = "filter_state['downstream_peer'].name"
	filterStateDownstreamPeerNamespace = "filter_state['downstream_peer'].namespace"
	filterStateSandboxLabels           = "filter_state['sandbox.labels']"
)

// HandleRequestHeaders processes request headers by walking the rule chain
// and dispatching to the registered plugins.
//
// Algorithm:
//  1. Resolve the source pod from filter_state. If pod identity is missing
//     the request is passed through unmodified — the ext-proc filter cannot
//     enforce a SecurityProfile without knowing which pod is sending the
//     traffic, and failing closed would break egress for misconfigured
//     workloads.
//  2. Look up matching SecurityProfiles via dynamic label matching.
//  3. Scan rules. For each rule whose match clause succeeds, invoke each
//     plugin in registration order. A plugin returning ActionImmediate
//     short-circuits the entire request. ActionMutate accumulates the
//     responses; ActionRecord claims the rule for deferred execution. The
//     same plugin is invoked at most once across all rules (first matching
//     rule wins).
//  4. Finalize phase: for plugins that returned ActionRecord during the
//     scan and implement Finalizer, call Finalize with the recorded rule.
//     A finalize result of Immediate still short-circuits; Mutate is
//     accumulated. This phase only runs after the scan completes without
//     an Immediate, so terminal Block rules suppress deferred RPCs.
//  5. Return the accumulated mutation responses, or a passthrough response
//     when no plugin produced a mutation.
//
// Regardless of return path, a single auditlog.Entry is submitted to the
// server's audit logger via a deferred call so operators can trace which
// SecurityProfile rule / action fired (or was skipped) for each egress.
func (s *Server) HandleRequestHeaders(ctx context.Context, headers *extProcPb.HttpHeaders, attrs map[string]*structpb.Struct) (responses []*extProcPb.ProcessingResponse, err error) {
	logger := log.FromContext(ctx)
	loggerD := logger.V(logutil.DEBUG)
	loggerD.Info("Handling request headers", "req.Attributes", attrs)

	// Audit state shared between the body and the deferred submission.
	collector := newAuditCollector()
	var (
		podNN        types.NamespacedName
		reqInfo      model.RequestInfo
		profileCount int
	)
	defer func() {
		if err != nil {
			collector.noteError(err)
		}
		// Any plugin still in the Recorded state didn't materialise a
		// mutation — count it under Skipped so the audit reflects the
		// preemption (terminal action won, or the request errored).
		collector.preemptRecorded()
		s.auditLogger.Submit(collector.buildEntry(podNN, reqInfo, profileCount))
	}()

	// Step 1: extract pod info from filter_state.
	podNamespace := extractFilterStateString(attrs, filterStateDownstreamPeerNamespace)
	podName := extractFilterStateString(attrs, filterStateDownstreamPeerName)
	sandboxLabelsEncoded := extractFilterStateString(attrs, filterStateSandboxLabels)

	if podNamespace == "" || podName == "" {
		logger.Info("Pod identity missing from filter_state",
			"podNamespace", podNamespace, "podName", podName,
			"policy", unauthenticatedEgressPolicy)
		if unauthenticatedEgressPolicy != "allow" {
			unauthenticatedRequestsTotal.WithLabelValues("missing_identity", "deny").Inc()
			return denyUnauthenticated, nil
		}
		unauthenticatedRequestsTotal.WithLabelValues("missing_identity", "allow").Inc()
		return defaultPassThrough, nil
	}

	podLabels := podlabels.ParseSandboxLabels(sandboxLabelsEncoded)
	if sandboxLabelsEncoded != "" && len(podLabels) == 0 {
		logger.Info("sandbox.labels present but failed to decode",
			"encoded", sandboxLabelsEncoded, "policy", unauthenticatedEgressPolicy)
		if unauthenticatedEgressPolicy != "allow" {
			unauthenticatedRequestsTotal.WithLabelValues("label_decode_failure", "deny").Inc()
			return denyUnauthenticated, nil
		}
		unauthenticatedRequestsTotal.WithLabelValues("label_decode_failure", "allow").Inc()
	}
	loggerD.Info("Extracted pod info from attributes",
		"pod", podName, "namespace", podNamespace, "labels", podLabels)

	// Step 2: profile lookup.
	profiles := s.configStore.FindProfilesForLabels(podNamespace, podLabels)
	profileCount = len(profiles)

	// Step 3: parse request info.
	reqHeaders := extractHeaderMap(headers)
	reqInfo = matcher.ParseRequestInfo(ctx, reqHeaders)
	podNN = types.NamespacedName{Namespace: podNamespace, Name: podName}

	// Single VERBOSE summary line per request — pod identity, request identity,
	// and matched profile count. Everything else in this flow is DEBUG.
	logger.V(logutil.VERBOSE).Info("Handling request",
		"pod", podName, "namespace", podNamespace,
		"method", reqInfo.Method, "host", reqInfo.Host, "path", reqInfo.Path,
		"profiles", len(profiles))

	if len(profiles) == 0 {
		loggerD.Info("No profiles found for pod labels",
			"pod", podName, "namespace", podNamespace, "labels", podLabels)
		return defaultPassThrough, nil
	}

	// Step 4: walk rules and dispatch to plugins.
	rctx := &plugins.RequestContext{
		Headers: reqHeaders,
		Info:    reqInfo,
		PodNN:   podNN,
	}

	var accumulated []*extProcPb.ProcessingResponse
	// claimed serves both the Mutate and Record paths: once a plugin has
	// produced a mutation OR claimed a rule for deferred finalize, the
	// same plugin must not be invoked again on later matching rules
	// (first matching rule wins).
	claimed := make(map[string]bool, len(s.plugins))
	recordedRules := make(map[string]*model.SecurityRule, len(s.plugins))

	for _, profile := range profiles {
		rctx.Profile = profile
		for i := range profile.SecurityRules {
			cr := &profile.SecurityRules[i]
			if !cr.MatchesRequest(&reqInfo) {
				continue
			}
			for _, p := range s.plugins {
				if claimed[p.Name()] {
					continue
				}
				res, pErr := p.OnRequestHeaders(ctx, rctx, cr)
				if pErr != nil {
					return nil, pErr
				}
				switch res.Action {
				case plugins.ActionImmediate:
					collector.noteImmediate(p.Name(), profile, cr)
					return res.Responses, nil
				case plugins.ActionMutate:
					collector.noteMutate(p.Name(), profile, cr)
					accumulated = append(accumulated, res.Responses...)
					claimed[p.Name()] = true
				case plugins.ActionRecord:
					collector.noteRecord(p.Name(), profile, cr)
					recordedRules[p.Name()] = cr
					claimed[p.Name()] = true
				case plugins.ActionContinue:
				}
			}
		}
	}

	// Finalize phase: plugins that recorded a match now run their deferred
	// work. We iterate s.plugins (not the map) to preserve registration
	// order, which matters when multiple deferred plugins coexist.
	for _, p := range s.plugins {
		rule, ok := recordedRules[p.Name()]
		if !ok {
			continue
		}
		res, fErr := p.Finalize(ctx, rctx, rule)
		if fErr != nil {
			return nil, fErr
		}
		switch res.Action {
		case plugins.ActionImmediate:
			collector.commitRecordedAsImmediate(p.Name())
			return res.Responses, nil
		case plugins.ActionMutate:
			collector.commitRecordedAsMutate(p.Name())
			accumulated = append(accumulated, res.Responses...)
		case plugins.ActionContinue:
			collector.finalizeContinued(p.Name(), res.Err)
		case plugins.ActionRecord:
			return nil, fmt.Errorf("plugin %q returned ActionRecord from Finalize, which is not permitted", p.Name())
		}
	}

	if len(accumulated) == 0 {
		loggerD.Info("No plugin produced mutations; passthrough", "pod", podNN.String())
		return defaultPassThrough, nil
	}
	return accumulated, nil
}

// getExtProcStruct returns the top-level structpb.Struct for the ext_proc
// filter attributes without any copying or allocation.
func getExtProcStruct(attrs map[string]*structpb.Struct) *structpb.Struct {
	if attrs == nil {
		return nil
	}
	if s, ok := attrs[extProcAttrsKey]; ok {
		return s
	}
	for key, s := range attrs {
		if strings.Contains(key, "ext_proc") {
			return s
		}
	}
	return nil
}

// extractFilterStateString extracts a string value from the Envoy ext_proc
// filter_state attributes using zero-copy protobuf field access. It looks up
// the key directly, then tries the filter_state['key'] format, and finally
// falls back to a suffix match.
func extractFilterStateString(attrs map[string]*structpb.Struct, key string) string {
	s := getExtProcStruct(attrs)
	if s == nil {
		return ""
	}
	fields := s.GetFields()
	if fields == nil {
		return ""
	}
	if v, ok := fields[key]; ok {
		return valueToString(v)
	}
	fsKey := "filter_state['" + key + "']"
	if v, ok := fields[fsKey]; ok {
		return valueToString(v)
	}
	for k, v := range fields {
		if strings.HasSuffix(k, key) {
			return valueToString(v)
		}
	}
	return ""
}

// valueToString extracts a string from a structpb.Value. For simple string
// values it returns directly; for struct values it looks for a "value" field.
func valueToString(v *structpb.Value) string {
	if v == nil {
		return ""
	}
	if s := v.GetStringValue(); s != "" {
		return s
	}
	if sv := v.GetStructValue(); sv != nil {
		if inner, ok := sv.GetFields()["value"]; ok {
			return inner.GetStringValue()
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

// defaultPassThrough is the shared passthrough response returned when no
// plugin produces mutations. It is immutable and safe to share across
// goroutines — gRPC serializes but does not mutate the proto.
var defaultPassThrough = []*extProcPb.ProcessingResponse{
	{Response: &extProcPb.ProcessingResponse_RequestHeaders{
		RequestHeaders: &extProcPb.HeadersResponse{},
	}},
}

// denyUnauthenticated is returned when the unauthenticated-egress policy is
// "deny" and the request cannot be attributed to a known pod.
var denyUnauthenticated = []*extProcPb.ProcessingResponse{
	{Response: &extProcPb.ProcessingResponse_ImmediateResponse{
		ImmediateResponse: &extProcPb.ImmediateResponse{
			Status: &envoyTypeV3.HttpStatus{
				Code: envoyTypeV3.StatusCode_Forbidden,
			},
			Body: []byte("request denied: pod identity could not be resolved"),
		},
	}},
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
