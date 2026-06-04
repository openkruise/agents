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

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildTokenRefreshStatus exercises the projection from a TokenResponse to the
// persisted TokenRefreshStatus. The contract is intentionally minimal: only the
// AccessTokenExpiration field flows through, and a nil input produces a zero-value
// status (so that callers can chain into EncodeTokenRefreshStatus without an extra
// nil check).
func TestBuildTokenRefreshStatus(t *testing.T) {
	tests := []struct {
		name   string
		resp   *TokenResponse
		expect TokenRefreshStatus
	}{
		{
			name:   "nil response yields zero-value status",
			resp:   nil,
			expect: TokenRefreshStatus{},
		},
		{
			name: "response with expiration is projected onto status",
			resp: &TokenResponse{
				RequestID:             "req-1",
				AccessToken:           "tok-1",
				SandboxClientID:       "client-1",
				AccessTokenExpiration: "2026-05-25T16:24:50Z",
			},
			expect: TokenRefreshStatus{
				AccessTokenExpiration: "2026-05-25T16:24:50Z",
			},
		},
		{
			name: "response with empty expiration yields zero-value status",
			resp: &TokenResponse{
				RequestID:   "req-2",
				AccessToken: "tok-2",
				// AccessTokenExpiration intentionally left empty
			},
			expect: TokenRefreshStatus{},
		},
		{
			name: "non-expiration fields on TokenResponse must not leak into status",
			resp: &TokenResponse{
				RequestID:             "req-3",
				AccessToken:           "tok-3",
				SandboxClientID:       "client-3",
				AccessTokenExpiration: "2099-01-01T00:00:00Z",
			},
			expect: TokenRefreshStatus{
				AccessTokenExpiration: "2099-01-01T00:00:00Z",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildTokenRefreshStatus(tt.resp)
			assert.Equal(t, tt.expect, got)
		})
	}
}

// TestEncodeTokenRefreshStatus pins down the on-wire JSON format of the persisted
// status. Both the claim flow and the standalone refresh controller rely on this
// exact output to keep readers happy, so any change here is a wire-format change.
func TestEncodeTokenRefreshStatus(t *testing.T) {
	tests := []struct {
		name        string
		status      TokenRefreshStatus
		expect      string
		expectError string
	}{
		{
			name:   "zero-value status serialises to empty object",
			status: TokenRefreshStatus{},
			// AccessTokenExpiration has json:"...,omitempty", so an empty string is dropped.
			expect: "{}",
		},
		{
			name: "status with expiration serialises with the canonical field name",
			status: TokenRefreshStatus{
				AccessTokenExpiration: "2026-05-25T16:24:50Z",
			},
			expect: `{"accessTokenExpiration":"2026-05-25T16:24:50Z"}`,
		},
		{
			name: "expiration with non-ASCII content is preserved verbatim in JSON",
			status: TokenRefreshStatus{
				AccessTokenExpiration: "2026-05-25T16:24:50+08:00",
			},
			expect: `{"accessTokenExpiration":"2026-05-25T16:24:50+08:00"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := EncodeTokenRefreshStatus(tt.status)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expect, raw)

			// Also ensure the produced payload is valid JSON that round-trips back
			// into an equivalent TokenRefreshStatus, so readers (claim flow / refresh
			// controller) see the exact same value the writer intended.
			var decoded TokenRefreshStatus
			require.NoError(t, json.Unmarshal([]byte(raw), &decoded))
			assert.Equal(t, tt.status, decoded)
		})
	}
}

// TestBuildAndEncodeTokenRefreshStatus_RoundTrip locks in the contract used by the
// production callers: the result of BuildTokenRefreshStatus, when fed through
// EncodeTokenRefreshStatus, produces the canonical annotation payload that the
// refresh controller is expected to read back.
func TestBuildAndEncodeTokenRefreshStatus_RoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		resp   *TokenResponse
		expect string
	}{
		{
			name:   "nil response round-trips to empty object",
			resp:   nil,
			expect: "{}",
		},
		{
			name: "response with expiration round-trips with canonical field name",
			resp: &TokenResponse{
				AccessToken:           "tok",
				AccessTokenExpiration: "2026-12-31T23:59:59Z",
			},
			expect: `{"accessTokenExpiration":"2026-12-31T23:59:59Z"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EncodeTokenRefreshStatus(BuildTokenRefreshStatus(tt.resp))
			require.NoError(t, err)
			assert.Equal(t, tt.expect, got)
		})
	}
}
