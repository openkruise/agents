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

package sandboxcr

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

func TestValidateAndInitClaimOptions_AccessToken(t *testing.T) {
	for _, tt := range []struct {
		name       string
		outerToken string
		innerToken string
		initialize bool
		wantToken  string
		wantError  bool
	}{
		{name: "skip initialization with token", outerToken: "outer-token", wantToken: "outer-token"},
		{name: "skip initialization without token"},
		{name: "initialization without tokens", initialize: true},
		{name: "default inner token", initialize: true, outerToken: "outer-token", wantToken: "outer-token"},
		{name: "default outer token", initialize: true, innerToken: "runtime-token", wantToken: "runtime-token"},
		{name: "matching tokens", initialize: true, outerToken: "same-token", innerToken: "same-token", wantToken: "same-token"},
		{name: "conflicting tokens", initialize: true, outerToken: "outer-token", innerToken: "runtime-token", wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Given independently supplied claim and runtime credentials.
			input := infra.ClaimSandboxOptions{
				User: "test-user", Template: "test-template", AccessToken: tt.outerToken,
			}
			if tt.initialize {
				input.InitRuntime = &config.InitRuntimeOptions{
					EnvVars: map[string]string{"KEY": "value"}, AccessToken: tt.innerToken,
					ReInit: true, SkipRefresh: true,
				}
			}

			// When validation normalizes the options before claim execution.
			got, err := ValidateAndInitClaimOptions(input)

			// Then the input remains unchanged and inconsistent credentials fail safely.
			require.Equal(t, tt.outerToken, input.AccessToken)
			if input.InitRuntime != nil {
				require.Equal(t, tt.innerToken, input.InitRuntime.AccessToken)
			}
			if tt.wantError {
				require.Error(t, err)
				require.Equal(t, managererrors.ErrorBadRequest, managererrors.GetErrCode(err))
				require.NotContains(t, err.Error(), tt.outerToken)
				require.NotContains(t, err.Error(), tt.innerToken)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantToken, got.AccessToken)
			if !tt.initialize {
				require.Nil(t, got.InitRuntime)
			} else {
				require.NotNil(t, got.InitRuntime)
				expected := *input.InitRuntime
				expected.AccessToken = tt.wantToken
				require.Equal(t, expected, *got.InitRuntime)
				if tt.outerToken != "" && tt.innerToken == "" {
					require.NotSame(t, input.InitRuntime, got.InitRuntime)
				}
			}
			validatedAgain, err := ValidateAndInitClaimOptions(got)
			require.NoError(t, err)
			require.Equal(t, got, validatedAgain)
		})
	}
}
