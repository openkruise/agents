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
	"net/http"

	"github.com/openkruise/agents/pkg/servers/web"
)

// ErrorResponse follows the pinned OpenAPI ErrorResponse schema. Transport
// headers, HTTP status and request ID must not appear as additional body fields.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func formatAPIError(err *web.ApiError) any {
	code := "INTERNAL_ERROR"
	switch err.Code {
	case http.StatusBadRequest:
		code = "INVALID_REQUEST"
	case http.StatusUnauthorized:
		code = "UNAUTHORIZED"
	case http.StatusForbidden:
		code = "FORBIDDEN"
	case http.StatusNotFound:
		code = "NOT_FOUND"
	case http.StatusConflict:
		code = "CONFLICT"
	case http.StatusTooManyRequests:
		code = "TOO_MANY_REQUESTS"
	case http.StatusGatewayTimeout:
		code = "GATEWAY_TIMEOUT"
	}
	return ErrorResponse{Code: code, Message: err.Message}
}
