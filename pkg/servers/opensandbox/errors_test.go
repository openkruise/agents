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
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestApiError verifies apiError/apiErrorf produce the OpenSandbox lifecycle
// spec's ErrorResponse envelope ({code:<string>, message:<string>}) on the
// wire, for every HTTP status this adapter actually returns as an error —
// see the spec's named error response components (BadRequest, Unauthorized,
// Forbidden, NotFound, Conflict, InternalServerError).
func TestApiError(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantCode   string
		wantStatus int
	}{
		{name: "bad request", status: http.StatusBadRequest, wantCode: "INVALID_REQUEST"},
		{name: "unauthorized", status: http.StatusUnauthorized, wantCode: "UNAUTHORIZED"},
		{name: "forbidden", status: http.StatusForbidden, wantCode: "FORBIDDEN"},
		{name: "not found", status: http.StatusNotFound, wantCode: "NOT_FOUND"},
		{name: "conflict", status: http.StatusConflict, wantCode: "CONFLICT"},
		{name: "internal server error", status: http.StatusInternalServerError, wantCode: "INTERNAL_ERROR"},
		{name: "unmapped status falls back to INTERNAL_ERROR", status: http.StatusTeapot, wantCode: "INTERNAL_ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := apiError(tt.status, "boom")
			assert.Equal(t, tt.status, err.Code, "HTTP status must be preserved for RegisterRoute's w.WriteHeader")
			assert.Equal(t, tt.wantCode, err.SpecCode)
			assert.Equal(t, "boom", err.Message)

			body, marshalErr := json.Marshal(err)
			require.NoError(t, marshalErr)
			assert.JSONEq(t, `{"code":"`+tt.wantCode+`","message":"boom"}`, string(body))
		})
	}
}

// TestApiErrorf_NeverInterpretsMessageAsFormatString guards the reason
// apiError takes message as a plain string rather than doing the
// fmt.Sprintf(err.Error()) callers used to write directly: an upstream error
// string containing a literal "%" (e.g. from a Kubernetes quantity parse
// error) must never be misread as a format verb.
func TestApiErrorf_NeverInterpretsMessageAsFormatString(t *testing.T) {
	err := apiError(http.StatusBadRequest, "invalid quantity: 100%!s(MISSING)")
	assert.Equal(t, "invalid quantity: 100%!s(MISSING)", err.Message)

	formatted := apiErrorf(http.StatusBadRequest, "sandbox %s not found", "abc-100%")
	assert.Equal(t, "sandbox abc-100% not found", formatted.Message)
}
