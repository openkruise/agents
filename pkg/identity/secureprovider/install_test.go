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

package secureprovider

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
)

func TestInstallProviderFromEnv(t *testing.T) {
	tests := []struct {
		name        string
		source      string
		expectError string
		// expectIssueError is the error the registered provider returns from IssueToken;
		// empty means the default UUID provider is still in place.
		expectIssueError string
	}{
		{name: "empty source keeps the default provider", source: ""},
		{name: "file source without endpoint registers a failing provider", source: "File", expectIssueError: "remote identity provider unavailable"},
		{name: "secret source without endpoint returns error", source: "secret", expectError: "IDENTITY_PROVIDER_ENDPOINT not set"},
		{name: "unknown source returns error", source: "vault", expectError: "unknown IDENTITY_PROVIDER_CREDENTIAL_SOURCE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity.RegisterProvider(identity.NewDefaultIdentityProvider())
			t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })
			t.Setenv(EnvIdentityProviderCredentialSource, tt.source)
			t.Setenv(EnvIdentityProviderEndpoint, "")
			forbidUserClusterClient(t, tt.source)

			err := InstallProviderFromEnv(context.Background(), &rest.Config{Host: "https://user-cluster.example.com"})
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)

			resp, err := identity.IssueToken(context.Background(), &agentsv1alpha1.Sandbox{}, identity.TokenOptions{})
			if tt.expectIssueError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectIssueError)
				return
			}
			require.NoError(t, err)
			assert.NotEmpty(t, resp.AccessToken)
		})
	}
}
