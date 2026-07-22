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

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/pkg/tracing"
	"github.com/openkruise/agents/pkg/utils"
)

type Handler[T any] func(r *http.Request) (response ApiResponse[T], err *ApiError)

type MiddleWare func(ctx context.Context, r *http.Request) (context.Context, *ApiError)

type ApiResponse[T any] struct {
	Code    int
	Headers map[string]string
	Body    T
}

type ApiError struct {
	Code      int               `json:"code"`
	Headers   map[string]string `json:"headers"`
	Message   string            `json:"message"`
	RequestID string            `json:"request_id"`
}

func (r *ApiError) Error() string {
	j, err := json.Marshal(r)
	if err != nil {
		return err.Error()
	}
	return string(j)
}

func RegisterRoute[T any](mux *http.ServeMux, method, path string, handler Handler[T], middlewares ...MiddleWare) {
	pattern := fmt.Sprintf("%s %s", method, path)
	if len(pattern) > 1 && pattern[len(pattern)-1] == '/' {
		pattern = pattern[:len(pattern)-1]
	}
	handleFunc := func(w http.ResponseWriter, r *http.Request) {
		written := false
		safeWriteJson := func(ctx context.Context, w http.ResponseWriter, code, defaultCode int, body any, headers map[string]string, requestID string) {
			if !written {
				written = true
				writeJson(ctx, w, code, defaultCode, body, headers, requestID)
			}
		}
		rawRequestID := r.Header.Get("X-Request-ID")
		parsedRequestID, parseErr := uuid.Parse(rawRequestID)
		if parseErr != nil {
			parsedRequestID = uuid.New()
		}
		requestID := strings.ReplaceAll(parsedRequestID.String(), "-", "")
		r.Header.Set("X-Request-ID", requestID)
		// Derive context from request context to inherit cancellation when client disconnects
		ctx := logs.NewContextFrom(r.Context(),
			"requestID", requestID, "api", fmt.Sprintf("%s %s", method, path))
		log := klog.FromContext(ctx)

		// Store request ID in context so the custom IDGenerator uses it as TraceID.
		// This enables unified trace-log correlation: TraceID = request ID.
		ctx = tracing.WithRequestID(ctx, requestID)

		// Create root span at middleware layer, wrapping the entire request lifecycle.
		// The custom IDGenerator will produce a TraceID equal to the request ID.
		ctx, rootSpan := tracing.Tracer("sandbox-manager").Start(ctx, fmt.Sprintf("%s %s", method, path),
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("request.id", requestID),
			),
		)
		// apiErr carries the final middleware/handler error so the deferred
		// EndSpan can record the request outcome on the root span. The explicit
		// nil check avoids the typed-nil *ApiError turning into a non-nil error.
		var apiErr *ApiError
		defer func() {
			if apiErr != nil {
				tracing.EndSpan(ctx, rootSpan, apiErr)
			} else {
				tracing.EndSpan(ctx, rootSpan, nil)
			}
		}()

		// Store root span context so that InjectTraceContext uses the root span's SpanID
		// when propagating trace context to the controller via CR annotations.
		ctx = tracing.WithRootSpanContext(ctx)

		defer func() {
			if rec := recover(); rec != nil {
				buf := make([]byte, 4096)
				n := runtime.Stack(buf, false)
				log.Error(nil, "panic occurred in web handler",
					"pattern", pattern,
					"recover", rec,
					"stack", string(buf[:n]))
				// Surface the panic on the root span as well.
				apiErr = &ApiError{
					Code:    http.StatusInternalServerError,
					Message: "Internal Server Error",
				}
			}
			safeWriteJson(ctx, w, http.StatusInternalServerError, http.StatusInternalServerError,
				&ApiError{
					Code:    http.StatusInternalServerError,
					Message: "Internal Server Error",
				}, nil, requestID)
			return
		}()

		for _, m := range middlewares {
			if ctx, apiErr = m(ctx, r); apiErr != nil {
				safeWriteJson(ctx, w, apiErr.Code, http.StatusInternalServerError, apiErr, nil, requestID)
				return
			}
		}
		start := time.Now()
		log.V(utils.DebugLogLevel).Info("start handling request", "pattern", pattern)
		var resp ApiResponse[T]
		resp, apiErr = handler(r.WithContext(ctx))
		if apiErr != nil {
			log.Error(apiErr, "API Error", "path", r.URL.Path, "cost", time.Since(start))
			safeWriteJson(ctx, w, apiErr.Code, http.StatusInternalServerError, apiErr, apiErr.Headers, requestID)
		} else {
			log.Info("API Success", "path", r.URL.Path, "cost", time.Since(start))
			safeWriteJson(ctx, w, resp.Code, http.StatusOK, resp.Body, resp.Headers, requestID)
		}
	}
	mux.HandleFunc(pattern, handleFunc)
	mux.HandleFunc(pattern+"/", handleFunc)
}

func writeJson(ctx context.Context, w http.ResponseWriter, code, defaultCode int, body any, headers map[string]string, requestID string) {
	log := klog.FromContext(ctx).V(utils.DebugLogLevel)
	if code == 0 {
		code = defaultCode
	}
	//goland:noinspection GoTypeAssertionOnErrors
	if apiError, ok := body.(*ApiError); ok {
		apiError.RequestID = requestID
	} else {
		w.Header().Set("X-Request-ID", requestID)
	}
	w.Header().Set("Content-Type", "application/json")
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(code)
	if code == http.StatusNoContent {
		return
	}
	if jsonErr := json.NewEncoder(w).Encode(body); jsonErr != nil {
		log.Error(jsonErr, "Failed to encode response")
		http.Error(w, fmt.Sprintf("Internal Server Error: failed to encode response: %v", jsonErr),
			http.StatusInternalServerError)
	}
}
