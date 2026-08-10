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
	"time"

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
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			// Server-generated IDs are emitted directly in the representation
			// required by the tracing scheme (32 hex chars, usable as an OTel
			// TraceID as-is), so caller-visible values never need rewriting.
			requestID = tracing.NewRequestID()
		}
		// Derive context from request context to inherit cancellation when client disconnects
		ctx := logs.NewContextFrom(r.Context(),
			"requestID", requestID, "api", fmt.Sprintf("%s %s", method, path))
		log := klog.FromContext(ctx)

		// A caller-provided X-Request-ID is never rewritten — not even
		// case-normalized. Callers set this header precisely so they can grep
		// our logs for the exact value they hold; rewriting it (e.g. lowercasing
		// uppercase hex) would break that lookup and cut the cross-system call
		// chain. Uppercase hex still decodes to the same TraceID bytes, so the
		// trace itself is unaffected; only its canonical string form in trace
		// backends is lowercase.
		//
		// When tracing is enabled the request ID doubles as the OTel TraceID, so
		// an ID that cannot serve as one is rejected with 400 instead of being
		// silently replaced. Known limitation: a caller reusing the same
		// X-Request-ID across independent requests makes them share one TraceID;
		// the configured sampling rate is unaffected because the sampling
		// decision uses a server-side random draw (see randomRatioSampler in
		// pkg/tracing).
		if tracing.Enabled() && !tracing.IsValidRequestID(requestID) {
			safeWriteJson(ctx, w, http.StatusBadRequest, http.StatusBadRequest, &ApiError{
				Code:    http.StatusBadRequest,
				Message: "invalid X-Request-ID: must be 32 hex characters and not all-zero when tracing is enabled",
			}, nil, requestID)
			return
		}

		// Create the root span wrapping the entire request lifecycle
		// (middlewares + handler). StartManagerRootSpan stores the request ID
		// in ctx so the custom IDGenerator produces TraceID == request ID,
		// enabling unified trace-log correlation.
		ctx, rootSpan := tracing.StartManagerRootSpan(ctx, fmt.Sprintf("%s %s", method, path), requestID)
		// err carries the final middleware/handler error so the deferred
		// EndSpan can record the request outcome on the root span. Keep the
		// closure: a direct defer tracing.EndSpan(ctx, rootSpan, err) would
		// evaluate err while still nil and record every failure as success.
		var err *ApiError
		defer func() {
			// Convert through a plain error so a typed-nil *ApiError does not
			// turn into a non-nil error interface.
			var spanErr error
			if err != nil {
				spanErr = err
			}
			tracing.EndSpan(ctx, rootSpan, spanErr)
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
				err = &ApiError{
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
			if ctx, err = m(ctx, r); err != nil {
				safeWriteJson(ctx, w, err.Code, http.StatusInternalServerError, err, nil, requestID)
				return
			}
		}
		start := time.Now()
		log.V(utils.DebugLogLevel).Info("start handling request", "pattern", pattern)
		resp, err := handler(r.WithContext(ctx))
		if err != nil {
			log.Error(err, "API Error", "path", r.URL.Path, "cost", time.Since(start))
			safeWriteJson(ctx, w, err.Code, http.StatusInternalServerError, err, err.Headers, requestID)
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
