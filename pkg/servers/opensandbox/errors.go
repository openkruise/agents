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
		return &web.ApiError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	return nil
}
