package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	types "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
)

var LogLevel = consts.DebugLogLevel + 1

func (s *Server) Process(srv extProcPb.ExternalProcessor_ProcessServer) error {
	log := klog.LoggerWithValues(klog.Background(), "contextID", uuid.NewString()).V(LogLevel)
	ctx := srv.Context()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		req, err := srv.Recv()
		if err == io.EOF {
			// envoy has closed the stream. Don't return anything and close this stream entirely
			log.Info("envoy has closed the stream")
			return nil
		}
		if err != nil {
			// Check if it is a context cancellation error, which is a normal case and does not need to be recorded as an error
			if errors.Is(ctx.Err(), context.Canceled) || status.Code(err) == codes.Canceled {
				log.Info("context canceled, closing stream")
				return nil
			}
			log.Error(err, "cannot receive stream request")
			return status.Errorf(codes.Unknown, "cannot receive stream request: %v", err)
		}

		// build response based on request type
		resp := &extProcPb.ProcessingResponse{
			Response: &extProcPb.ProcessingResponse_RequestHeaders{
				RequestHeaders: &extProcPb.HeadersResponse{
					Response: &extProcPb.CommonResponse{},
				},
			},
		}
		switch v := req.Request.(type) {
		case *extProcPb.ProcessingRequest_RequestHeaders:
			h := req.Request.(*extProcPb.ProcessingRequest_RequestHeaders)
			resp = s.handleRequestHeaders(h, log)

		default:
			log.Info("Unknown Request type", "type", v)

		}

		if err = srv.Send(resp); err != nil {
			log.Error(err, "failed to send response")
			return err
		}

	}

}

var OrigDstHeader = "x-envoy-original-dst-host"

func (s *Server) handleRequestHeaders(requestHeaders *extProcPb.ProcessingRequest_RequestHeaders, log logr.Logger) *extProcPb.ProcessingResponse {
	// Step 1: Convert ext_proc headers to flat map[string]string
	headers := extProcHeadersToMap(requestHeaders.RequestHeaders)
	// Step 2: Use adapter.ParseRequest to normalize the request
	parsed := s.adapter.ParseRequest(headers)
	scheme, authority, path := parsed.Scheme, parsed.Authority, parsed.Path

	log = log.WithValues("requestID", headers["x-request-id"])
	log.Info("envoy ext processor parsed request", "scheme", scheme, "authority", authority, "path", path, "port", parsed.Port, "headers", headers)
	if !s.adapter.IsSandboxRequest(authority, path, parsed.Port) {
		return s.logAndCreateDstResponse(requestHeaders.RequestHeaders, map[string]string{
			OrigDstHeader: s.LBEntry,
		}, log)
	}
	sandboxID, sandboxPort, extraHeaders, err := s.adapter.Map(parsed)
	if err != nil {
		// Return error response instead of gRPC error
		log.Error(err, "failed to map request to sandbox")
		errorMsg := fmt.Sprintf("failed to map request to sandbox, URL=%s://%s%s", scheme, authority, path)
		return s.logAndCreateErrorResponse(http.StatusInternalServerError, errorMsg, log)
	}
	if sandboxPort < 0 || sandboxPort > 65535 {
		errorMsg := fmt.Sprintf("invalid sandbox port: %d", sandboxPort)
		return s.logAndCreateErrorResponse(http.StatusBadRequest, errorMsg, log)
	}
	log.Info("request mapped", "sandboxID", sandboxID, "sandboxPort", sandboxPort, "extraHeaders", extraHeaders)

	errorMsg := fmt.Sprintf("healthy sandbox %s not found", sandboxID)
	route, ok := s.LoadRoute(sandboxID)
	if !ok {
		log.Info("route not found", "sandboxID", sandboxID)
		return s.logAndCreateErrorResponse(http.StatusBadGateway, errorMsg, log)
	}
	if route.State != agentsv1alpha1.SandboxStateRunning {
		log.Info("sandbox is not running", "sandboxID", sandboxID, "route", route)
		return s.logAndCreateErrorResponse(http.StatusBadGateway, errorMsg, log)
	}
	if extraHeaders == nil {
		extraHeaders = make(map[string]string)
	}
	// An adapter can set "x-envoy-original-dst-host" header to force route the request to a specific destination
	if _, ok := extraHeaders[OrigDstHeader]; !ok {
		extraHeaders[OrigDstHeader] = fmt.Sprintf("%s:%d", route.IP, sandboxPort)
	}

	return s.logAndCreateDstResponse(requestHeaders.RequestHeaders, extraHeaders, log)
}

func (s *Server) logAndCreateDstResponse(requestHeaders *extProcPb.HttpHeaders,
	extraHeaders map[string]string, log logr.Logger) *extProcPb.ProcessingResponse {
	log.Info("will modify request headers", "headers", extraHeaders)
	setHeaders := make([]*configPb.HeaderValueOption, 0, len(extraHeaders))
	for k, v := range extraHeaders {
		setHeaders = append(setHeaders, &configPb.HeaderValueOption{
			Header: &configPb.HeaderValue{
				Key:      k,
				RawValue: []byte(v),
			},
		})
	}
	resp := &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: setHeaders,
					},
				},
			},
		},
	}
	resp.Response.(*extProcPb.ProcessingResponse_RequestHeaders).RequestHeaders.Response.HeaderMutation.SetHeaders = append(
		resp.Response.(*extProcPb.ProcessingResponse_RequestHeaders).RequestHeaders.Response.HeaderMutation.SetHeaders,
		headerModifiers("request-header-modifier", requestHeaders, log)...)
	return resp
}

func (s *Server) logAndCreateErrorResponse(statusCode int, message string, log logr.Logger) *extProcPb.ProcessingResponse {
	log.Error(errors.New(message), "create error response", "code", statusCode)
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extProcPb.ImmediateResponse{
				Status: &types.HttpStatus{
					Code: types.StatusCode(statusCode),
				},
				Body: []byte(fmt.Sprintf("API Error: %s", message)),
			},
		},
	}
}

// extProcHeadersToMap converts Envoy ext_proc HttpHeaders into a flat map[string]string,
// preserving pseudo-headers (:scheme, :authority, :path) so that adapter.ParseRequest
// can normalize them uniformly.
func extProcHeadersToMap(httpHeaders *extProcPb.HttpHeaders) map[string]string {
	headers := make(map[string]string, len(httpHeaders.Headers.Headers))
	for _, header := range httpHeaders.Headers.Headers {
		headers[header.Key] = string(header.RawValue)
	}
	return headers
}

func headerModifiers(key string, in *extProcPb.HttpHeaders, log logr.Logger) []*configPb.HeaderValueOption {
	var modifiers []*configPb.HeaderValueOption
	value := ""
	for _, header := range in.Headers.Headers {
		if header.Key == key {
			value = string(header.RawValue)
			break
		}
	}
	if value != "" {
		unmarshalled := map[string]string{}
		if err := json.Unmarshal([]byte(value), &unmarshalled); err != nil {
			log.Error(err, "failed to unmarshall header-modifier", "value", value)
			return modifiers
		}
		for k, v := range unmarshalled {
			modifiers = append(modifiers, &configPb.HeaderValueOption{
				Header: &configPb.HeaderValue{
					Key:      k,
					RawValue: []byte(v),
				},
			})
		}
	}
	return modifiers
}
