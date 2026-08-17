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
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/openkruise/agents/pkg/servers/web"
)

// decodeJSONBody decodes r's JSON body into v, translating a decode failure
// into the spec's 400 Bad Request. A missing body decodes to v's zero value
// rather than erroring, since several OpenSandbox endpoints (pause, resume)
// accept an empty body and a couple of others (renew-expiration) are decoded
// through this same helper for consistency.
func decodeJSONBody(r *http.Request, v any) *web.ApiError {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return apiError(http.StatusBadRequest, err.Error())
	}
	return nil
}

// specErrorCode maps an HTTP status this adapter returns to the OpenSandbox
// lifecycle spec's ErrorResponse.code values. The spec documents code as a
// free-form machine-readable string with examples (INVALID_REQUEST,
// NOT_FOUND, INTERNAL_ERROR) rather than a fixed enum; these five statuses
// are exactly the ones the spec gives named response components for
// (BadRequest, Unauthorized, Forbidden, NotFound, Conflict,
// InternalServerError), so status <-> code is a stable 1:1 mapping here.
func specErrorCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "INVALID_REQUEST"
	case http.StatusUnauthorized:
		return "UNAUTHORIZED"
	case http.StatusForbidden:
		return "FORBIDDEN"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusConflict:
		return "CONFLICT"
	default:
		return "INTERNAL_ERROR"
	}
}

// apiError builds a web.ApiError whose JSON body follows the OpenSandbox
// spec's ErrorResponse shape ({code:<string>, message}) instead of E2B's
// ({code:<http status>, headers, message, request_id}) — see
// web.ApiError.SpecCode. message is used verbatim, never as a format string,
// so caller-controlled or Kubernetes-error text can never be misread as
// format verbs.
func apiError(status int, message string) *web.ApiError {
	return &web.ApiError{Code: status, SpecCode: specErrorCode(status), Message: message}
}

// apiErrorf is apiError with fmt.Sprintf formatting for call sites that
// build the message from multiple parts.
func apiErrorf(status int, format string, args ...any) *web.ApiError {
	return apiError(status, fmt.Sprintf(format, args...))
}
