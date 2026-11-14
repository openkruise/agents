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
package opensandbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIErrorHTTP(t *testing.T) {
	for status, code := range map[int]string{400: "INVALID_REQUEST", 401: "UNAUTHORIZED", 403: "FORBIDDEN", 404: "NOT_FOUND", 409: "CONFLICT", 429: "TOO_MANY_REQUESTS", 500: "INTERNAL_ERROR", 504: "GATEWAY_TIMEOUT"} {
		for _, middleware := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/middleware=%t", status, middleware), func(t *testing.T) {
				mux := http.NewServeMux()
				apiErr := &web.ApiError{Code: status, Message: "test failure", Headers: map[string]string{"Retry-After": "7"}}
				var chain []web.MiddleWare
				if middleware {
					chain = append(chain, func(ctx context.Context, _ *http.Request) (context.Context, *web.ApiError) { return ctx, apiErr })
				}
				web.RegisterRouteWithErrorFormatter(mux, http.MethodGet, "/test", func(*http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
					return web.ApiResponse[struct{}]{}, apiErr
				}, formatAPIError, chain...)
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodGet, "/test", nil)
				r.Header.Set("X-Request-ID", "0123456789abcdef0123456789abcdef")
				mux.ServeHTTP(w, r)
				require.Equal(t, status, w.Code)
				assert.JSONEq(t, fmt.Sprintf(`{"code":%q,"message":"test failure"}`, code), w.Body.String())
				if !middleware {
					assert.Equal(t, "7", w.Header().Get("Retry-After"))
				}
				assert.Equal(t, r.Header.Get("X-Request-ID"), w.Header().Get("X-Request-ID"))
			})
		}
	}
	t.Run("panic also uses protocol error shape", func(t *testing.T) {
		mux := http.NewServeMux()
		web.RegisterRouteWithErrorFormatter(mux, http.MethodGet, "/test", func(*http.Request) (web.ApiResponse[struct{}], *web.ApiError) { panic("test panic") }, formatAPIError)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))
		require.Equal(t, 500, w.Code)
		assert.JSONEq(t, `{"code":"INTERNAL_ERROR","message":"Internal Server Error"}`, w.Body.String())
	})
}
