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
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandboxid"
)

func TestInfraIssueTrafficAccessToken(t *testing.T) {
	expiration := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	rootErr := errors.New("provider unavailable")
	tests := []struct {
		name           string
		providerError  error
		validateError  error
		missingSandbox bool
		expectError    string
		expectErrorIs  error
		expectCalls    int
	}{
		{name: "success", expectCalls: 1},
		{name: "provider failure is returned", providerError: rootErr, expectError: rootErr.Error(), expectCalls: 1},
		{name: "policy rejection prevents issuance", validateError: errors.New("not authorized"), expectError: "not authorized"},
		{name: "lookup miss is translated", missingSandbox: true, expectError: infra.ErrSandboxNotFound.Error(), expectErrorIs: infra.ErrSandboxNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infraInstance, cachedClient := NewTestInfra(t, config.SandboxManagerOptions{})
			infraInstance.APIReader = nil
			sandbox := makeClaimedSandbox("default", "traffic-token", "10.0.0.1")
			sandbox.Annotations[identity.AnnotationEnableJwtAuth] = v1alpha1.True
			sandboxID := sandboxid.Resolve(sandbox)
			if !tt.missingSandbox {
				CreateSandboxWithStatus(t, cachedClient, sandbox)
				require.Eventually(t, func() bool {
					_, err := infraInstance.Cache.GetClaimedSandbox(t.Context(), infracache.GetClaimedSandboxOptions{SandboxID: sandboxID})
					return err == nil
				}, time.Second, 10*time.Millisecond)
			}

			var gotOpts identity.TokenOptions
			providerCalls := 0
			fake := &mockIdentityProvider{issueTokenWithOptsFunc: func(_ context.Context, _ *v1alpha1.Sandbox, opts identity.TokenOptions) (*identity.TokenResponse, error) {
				providerCalls++
				gotOpts = opts
				if tt.providerError != nil {
					return nil, tt.providerError
				}
				return &identity.TokenResponse{AccessToken: "signed-token", AccessTokenExpiration: expiration.Format(time.RFC3339)}, nil
			}}
			identity.RegisterProvider(fake)
			t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })

			validateCalls := 0
			result, err := infraInstance.IssueTrafficAccessToken(t.Context(), infra.IssueTrafficAccessTokenOptions{
				SandboxID: sandboxID,
				TokenOptions: identity.TokenOptions{
					RequestedValidity: time.Hour,
				},
				Validate: func(infra.Sandbox) error {
					validateCalls++
					return tt.validateError
				},
			})
			if tt.expectError != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tt.expectError)
				if tt.expectErrorIs != nil {
					assert.ErrorIs(t, err, tt.expectErrorIs)
				}
				assert.Empty(t, result)
			} else {
				require.NoError(t, err)
				assert.Equal(t, "signed-token", result.Token)
				assert.Equal(t, expiration, result.Expiration)
			}
			if tt.missingSandbox {
				assert.Zero(t, validateCalls)
			} else {
				assert.Equal(t, 1, validateCalls)
			}
			assert.Equal(t, tt.expectCalls, providerCalls)
			if tt.expectCalls > 0 {
				assert.Equal(t, identity.TokenKindAccessToken, gotOpts.Kind)
				assert.Equal(t, time.Hour, gotOpts.RequestedValidity)
			}
			assert.NotContains(t, sandbox.Annotations, "trafficAccessToken")
		})
	}
}

func TestInfraIssueTrafficAccessTokenValidation(t *testing.T) {
	infraInstance := &Infra{}
	tests := []struct {
		name        string
		opts        infra.IssueTrafficAccessTokenOptions
		expectError string
	}{
		{name: "sandbox ID is required", expectError: "sandbox ID is required"},
		{name: "validator is required", opts: infra.IssueTrafficAccessTokenOptions{SandboxID: "default--sandbox"}, expectError: "validator is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := infraInstance.IssueTrafficAccessToken(t.Context(), tt.opts)
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.expectError)
		})
	}
}
