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

package main

import (
	"testing"

	"github.com/openkruise/agents/pkg/resourceaccess"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateResourceAccessFlags(t *testing.T) {
	for _, tt := range []struct {
		value   string
		auth    bool
		want    resourceaccess.Mode
		wantErr string
	}{
		{value: "", wantErr: "--resource-access-mode must not be empty"},
		{value: "trusted-domain", want: resourceaccess.TrustedDomain},
		{value: "isolated", auth: true, want: resourceaccess.Isolated},
		{value: "isolated", wantErr: "requires --e2b-enable-auth"},
		{value: "strict", auth: true, wantErr: "invalid --resource-access-mode"},
	} {
		t.Run(tt.value+tt.wantErr, func(t *testing.T) {
			got, err := validateResourceAccessFlags(tt.value, tt.auth)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestValidateE2BTimeoutFlags(t *testing.T) {
	cases := []struct {
		name        string
		maxTimeout  int
		expectError string
	}{
		{name: "ok-default", maxTimeout: 2592000, expectError: ""},
		{name: "ok-small", maxTimeout: 60, expectError: ""},
		{name: "zero", maxTimeout: 0, expectError: "--e2b-max-timeout must be greater than 0"},
		{name: "negative", maxTimeout: -1, expectError: "--e2b-max-timeout must be greater than 0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateE2BTimeoutFlags(tc.maxTimeout)
			if tc.expectError == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.expectError)
			}
		})
	}
}

func TestValidateMetricsPort(t *testing.T) {
	cases := []struct {
		name               string
		metricsPort        int
		controlPort        int
		memberlistBindPort int
		expectError        string
	}{
		{name: "shared-zero", controlPort: 8080, memberlistBindPort: 7946},
		{name: "shared-control-listener", metricsPort: 8080, controlPort: 8080, memberlistBindPort: 7946},
		{name: "min-valid", metricsPort: 1, controlPort: 8080, memberlistBindPort: 7946},
		{name: "max-valid", metricsPort: 65535, controlPort: 8080, memberlistBindPort: 7946},
		{name: "negative", metricsPort: -1, controlPort: 8080, memberlistBindPort: 7946, expectError: "valid TCP port"},
		{name: "too-large", metricsPort: 65536, controlPort: 8080, memberlistBindPort: 7946, expectError: "valid TCP port"},
		{name: "matches-default-memberlist-port", metricsPort: 7946, controlPort: 8080, memberlistBindPort: 7946, expectError: "must differ from --memberlist-bind-port"},
		{name: "matches-custom-memberlist-port", metricsPort: 9000, controlPort: 8080, memberlistBindPort: 9000, expectError: "must differ from --memberlist-bind-port"},
		{name: "matches-normalized-default-memberlist-port", metricsPort: 7946, controlPort: 8080, memberlistBindPort: 0, expectError: "must differ from --memberlist-bind-port"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMetricsPort(tc.metricsPort, tc.controlPort, tc.memberlistBindPort)
			if tc.expectError == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.expectError)
		})
	}
}
