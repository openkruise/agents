package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/google/uuid"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/logs"
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
		requestID := uuid.NewString()
		ctx := logs.NewContextWithID(requestID)
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

		// 数一下 path 中 '/' 的个数；如果 path 以 '/' 结尾则数字 -1
		if countSlashes(pattern) != countSlashes(r.URL.Path) {
			log.Info("API Not Found", "path", r.URL.Path, "pattern", pattern)
			writeJson(w, http.StatusNotFound, http.StatusNotFound, ApiError{
				Code:    http.StatusNotFound,
				Message: fmt.Sprintf("Not Found: %s", r.URL.Path),
			}, requestID)
			return
		}

		var err *ApiError
		for _, m := range middlewares {
			if ctx, err = m(ctx, r); err != nil {
				writeJson(w, err.Code, http.StatusInternalServerError, err, requestID)
				return
			}
		}

		resp, err := handler(r.WithContext(ctx))
		if err != nil {
			klog.ErrorS(err, "API Error", "path", r.URL.Path)
			writeJson(w, err.Code, http.StatusInternalServerError, err, requestID)
		} else {
			writeJson(w, resp.Code, http.StatusOK, resp.Body, requestID)
		}
	}
	mux.HandleFunc(pattern, handleFunc)
	mux.HandleFunc(pattern+"/", handleFunc)
}

func writeJson(w http.ResponseWriter, code, defaultCode int, body any, requestID string) {
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
		klog.ErrorS(jsonErr, "Failed to encode response")
		http.Error(w, fmt.Sprintf("Internal Server Error: failed to encode response: %v", jsonErr),
			http.StatusInternalServerError)
	}
}
