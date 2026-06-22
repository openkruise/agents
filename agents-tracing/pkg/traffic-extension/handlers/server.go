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
	"errors"
	"io"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openkruise/agents/pkg/traffic-extension/framework/configstore"
	"github.com/openkruise/agents/pkg/traffic-extension/plugins"
	"github.com/openkruise/agents/pkg/traffic-extension/util/auditlog"
	logutil "github.com/openkruise/agents/pkg/traffic-extension/util/logging"
)

// Server implements the Envoy external processing server.
// https://www.envoyproxy.io/docs/envoy/latest/api-v3/service/ext_proc/v3/external_processor.proto
type Server struct {
	streaming   bool
	configStore configstore.Store
	plugins     []plugins.Plugin
	auditLogger auditlog.Logger
}

// ServerDeps holds the dependencies needed by the ext-proc server.
type ServerDeps struct {
	ConfigStore configstore.Store
	// Plugins is the ordered list of request-handling plugins. The handler
	// invokes them in this order for every matching rule.
	Plugins []plugins.Plugin
	// AuditLogger receives one Entry per RequestHeaders invocation. May be
	// nil in tests; in that case audit submission is skipped via the
	// Logger's own nil-safety.
	AuditLogger auditlog.Logger
}

// NewServer creates a new ext-proc handler server with dependencies. A nil
// AuditLogger is replaced with a no-op so the dispatch path can call
// Submit unconditionally.
func NewServer(streaming bool, deps ServerDeps) *Server {
	al := deps.AuditLogger
	if al == nil {
		al = auditlog.Nop()
	}
	return &Server{
		streaming:   streaming,
		configStore: deps.ConfigStore,
		plugins:     deps.Plugins,
		auditLogger: al,
	}
}

// Process is the main gRPC streaming method for the external processor.
// It receives requests from Envoy, processes them, and sends back responses.
func (s *Server) Process(srv extProcPb.ExternalProcessor_ProcessServer) error {
	ctx := srv.Context()
	logger := log.FromContext(ctx)
	loggerD := logger.V(logutil.DEBUG)
	loggerD.Info("Processing request started")

	streamedBody := &streamedBody{}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		req, recvErr := srv.Recv()
		if recvErr == io.EOF || errors.Is(recvErr, context.Canceled) || status.Code(recvErr) == codes.Canceled {
			return nil
		}
		if recvErr != nil {
			logger.V(logutil.DEFAULT).Error(recvErr, "Cannot receive stream request")
			return status.Errorf(codes.Unknown, "cannot receive stream request: %v", recvErr)
		}

		var responses []*extProcPb.ProcessingResponse
		var err error
		switch v := req.Request.(type) {
		case *extProcPb.ProcessingRequest_RequestHeaders:
			if s.streaming && !req.GetRequestHeaders().GetEndOfStream() {
				// If streaming and the body is not empty, then headers are handled when processing request body.
				loggerD.Info("Received headers, passing off header processing until body arrives...")
			} else {
				responses, err = s.HandleRequestHeaders(ctx, req.GetRequestHeaders(), req.Attributes)
			}
		case *extProcPb.ProcessingRequest_RequestBody:
			if loggerD.Enabled() {
				loggerD.Info("Incoming body chunk", "bodyLen", len(v.RequestBody.Body), "EoS", v.RequestBody.EndOfStream)
			}
			responses, err = s.processRequestBody(ctx, req.GetRequestBody(), streamedBody, logger)
		case *extProcPb.ProcessingRequest_RequestTrailers:
			responses, err = s.HandleRequestTrailers(ctx, req.GetRequestTrailers())
		case *extProcPb.ProcessingRequest_ResponseHeaders:
			responses, err = s.HandleResponseHeaders(ctx, req.GetResponseHeaders())
		case *extProcPb.ProcessingRequest_ResponseBody:
			responses, err = s.HandleResponseBody(ctx, req.GetResponseBody())
		case *extProcPb.ProcessingRequest_ResponseTrailers:
			responses, err = s.HandleResponseTrailers(ctx, req.GetResponseTrailers())
		default:
			logger.V(logutil.DEFAULT).Error(nil, "Unknown Request type", "request", v)
			return status.Error(codes.Unknown, "unknown request type")
		}

		if err != nil {
			if loggerD.Enabled() {
				loggerD.Error(err, "Failed to process request", "request", req)
			} else {
				logger.V(logutil.DEFAULT).Error(err, "Failed to process request")
			}
			return status.Errorf(status.Code(err), "failed to handle request: %v", err)
		}

		for _, resp := range responses {
			if loggerD.Enabled() {
				loggerD.Info("Response generated", "response", resp)
			}
			if err := srv.Send(resp); err != nil {
				logger.V(logutil.DEFAULT).Error(err, "Send failed")
				return status.Errorf(codes.Unknown, "failed to send response back to Envoy: %v", err)
			}
		}
	}
}

// streamedBody accumulates body chunks when operating in streaming mode.
type streamedBody struct {
	body []byte
}

// processRequestBody handles request body processing, accumulating chunks in streaming mode.
func (s *Server) processRequestBody(ctx context.Context, body *extProcPb.HttpBody, streamedBody *streamedBody, logger logr.Logger) ([]*extProcPb.ProcessingResponse, error) {
	loggerD := logger.V(logutil.DEBUG)

	var requestBodyBytes []byte
	if s.streaming {
		streamedBody.body = append(streamedBody.body, body.Body...)
		// In the streaming case, we can receive multiple request bodies.
		if body.EndOfStream {
			loggerD.Info("Flushing stream buffer")
			requestBodyBytes = streamedBody.body
		} else {
			return nil, nil
		}
	} else {
		requestBodyBytes = body.GetBody()
	}

	return s.HandleRequestBody(ctx, requestBodyBytes)
}
