package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/google/uuid"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"k8s.io/klog/v2"
)

type Handler[T any] func(r *http.Request) (response ApiResponse[T], err *ApiError)

type MiddleWare func(ctx context.Context, r *http.Request) (context.Context, *ApiError)

type ApiResponse[T any] struct {
	Code int
	Body T
}

type ApiError struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func (r *ApiError) Error() string {
	j, err := json.Marshal(r)
	if err != nil {
		return err.Error()
	}
	return string(j)
}

func countSlashes(path string) int {
	count := strings.Count(path, "/")
	if strings.HasSuffix(path, "/") {
		count--
	}
	return count
}

func RegisterRoute[T any](mux *http.ServeMux, pattern string, handler Handler[T], middlewares ...MiddleWare) {
	if len(pattern) > 1 && pattern[len(pattern)-1] == '/' {
		pattern = pattern[:len(pattern)-1]
	}
	handleFunc := func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = uuid.NewString()
		}
		ctx := logs.NewContext("requestID", requestID)
		log := klog.FromContext(ctx)

		defer func() {
			if rec := recover(); rec != nil {
				buf := make([]byte, 4096)
				n := runtime.Stack(buf, false)
				log.Error(nil, "panic occurred in web handler",
					"pattern", pattern,
					"recover", rec,
					"stack", string(buf[:n]))
			}
		}()

		// Count the number of '/' in the path; if path ends with '/', subtract 1 from the count
		if countSlashes(pattern) != countSlashes(r.URL.Path) {
			log.V(consts.DebugLogLevel).Info("API Not Found", "path", r.URL.Path, "pattern", pattern)
			writeJson(ctx, w, http.StatusNotFound, http.StatusNotFound, ApiError{
				Code:    http.StatusNotFound,
				Message: fmt.Sprintf("Not Found: %s", r.URL.Path),
			}, requestID)
			return
		}

		var err *ApiError
		for _, m := range middlewares {
			if ctx, err = m(ctx, r); err != nil {
				writeJson(ctx, w, err.Code, http.StatusInternalServerError, err, requestID)
				return
			}
		}
		log.V(consts.DebugLogLevel).Info("start handling request", "pattern", pattern)
		resp, err := handler(r.WithContext(ctx))
		if err != nil {
			log.Error(err, "API Error", "path", r.URL.Path)
			writeJson(ctx, w, err.Code, http.StatusInternalServerError, err, requestID)
		} else {
			writeJson(ctx, w, resp.Code, http.StatusOK, resp.Body, requestID)
		}
	}
	mux.HandleFunc(pattern, handleFunc)
	mux.HandleFunc(pattern+"/", handleFunc)
}

func writeJson(ctx context.Context, w http.ResponseWriter, code, defaultCode int, body any, requestID string) {
	log := klog.FromContext(ctx).V(consts.DebugLogLevel)
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
