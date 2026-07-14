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

package identity

import "encoding/json"

// BuildTokenRefreshStatus derives the persisted refresh status from a successful
// token issuance response. It centralises the projection so that the
// ProcessSandboxToken lifecycle and the standalone refresh controller
// produce the exact same payload.
//
// A nil resp results in a zero-value TokenRefreshStatus, which serialises to "{}".
func BuildTokenRefreshStatus(resp *TokenResponse) TokenRefreshStatus {
	if resp == nil {
		return TokenRefreshStatus{}
	}
	return TokenRefreshStatus{
		AccessTokenExpiration: resp.AccessTokenExpiration,
	}
}

// EncodeTokenRefreshStatus serialises a TokenRefreshStatus into the JSON payload
// stored on the sandbox annotation AgentKeyTokenRefreshStatus. Keeping the
// encoding here ensures every writer (claim flow / refresh controller) emits the
// identical wire format expected by readers.
func EncodeTokenRefreshStatus(s TokenRefreshStatus) (string, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
