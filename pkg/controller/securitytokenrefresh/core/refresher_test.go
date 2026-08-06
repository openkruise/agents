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

package core

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
)

// stubIdentityProvider is a hand-rolled IdentityProvider that returns the configured
// TokenResponse / errors. It is installed via identity.RegisterProvider() for the
// duration of a single sub-test.
type stubIdentityProvider struct {
	resp           *identity.TokenResponse
	issueErr       error
	propagateErr   error
	propagateCalls int
	issueCalls     int
}

func (s *stubIdentityProvider) IssueToken(_ context.Context, _ *agentsv1alpha1.Sandbox, _ identity.TokenKind) (*identity.TokenResponse, error) {
	s.issueCalls++
	if s.issueErr != nil {
		return nil, s.issueErr
	}
	return s.resp, nil
}

func (s *stubIdentityProvider) PropagateSecurityToken(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *identity.TokenResponse) error {
	s.propagateCalls++
	return s.propagateErr
}

// withProvider installs stub as the global identity provider for the test.
// It is intentionally not parallelisable: the registry is a process-wide singleton.
func withProvider(t *testing.T, stub *stubIdentityProvider) {
	t.Helper()
	identity.RegisterProvider(stub)
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

func newSandbox(name string) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True,
			},
			Annotations: map[string]string{
				identity.AgentKeyTokenRefreshStatus: `{"accessTokenExpiration":"2025-01-01T00:00:00Z"}`,
			},
		},
	}
}

func TestDefaultRefresher_Refresh(t *testing.T) {
	const newExpire = "2099-12-31T23:59:59Z"

	tests := []struct {
		name string
		// stub configures the global identity provider for the duration of the case.
		stub *stubIdentityProvider
		// expectErr is the substring expected in the returned error (empty == no error).
		expectErr string
		// expectAnnotation is the value the sandbox annotation should have AFTER Refresh.
		// Empty means "annotation must remain unchanged".
		expectAnnotation string
	}{
		{
			name: "happy path: issue, propagate, patch annotation",
			stub: &stubIdentityProvider{
				resp: &identity.TokenResponse{
					AccessToken:           "fresh",
					AccessTokenExpiration: newExpire,
				},
			},
			expectAnnotation: `{"accessTokenExpiration":"` + newExpire + `"}`,
		},
		{
			name: "issue fails -> returns error directly, annotation NOT touched",
			stub: &stubIdentityProvider{
				issueErr: errors.New("issue down"),
			},
			expectErr: "issue token",
		},
		{
			name: "propagate fails -> annotation NOT touched",
			stub: &stubIdentityProvider{
				resp: &identity.TokenResponse{
					AccessToken:           "fresh",
					AccessTokenExpiration: newExpire,
				},
				propagateErr: errors.New("propagate down"),
			},
			expectErr: "propagate token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withProvider(t, tt.stub)

			sbx := newSandbox("sbx-1")
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sbx).Build()
			r := NewDefaultRefresher(c)

			_, err := r.Refresh(context.Background(), sbx.DeepCopy())
			if tt.expectErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectErr)
			}

			got := &agentsv1alpha1.Sandbox{}
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, got))

			if tt.expectErr != "" {
				assert.Equal(t, sbx.Annotations[identity.AgentKeyTokenRefreshStatus],
					got.Annotations[identity.AgentKeyTokenRefreshStatus],
					"annotation must NOT change when an upstream step fails")
				return
			}
			if tt.expectAnnotation != "" {
				assert.Equal(t, tt.expectAnnotation, got.Annotations[identity.AgentKeyTokenRefreshStatus])
			}
		})
	}
}

// TestDefaultRefresher_PatchAnnotation_NilAnnotations exercises the branch where the
// sandbox starts with a nil Annotations map: patchAnnotation must allocate it before
// inserting the token-status entry.
func TestDefaultRefresher_PatchAnnotation_NilAnnotations(t *testing.T) {
	withProvider(t, &stubIdentityProvider{
		resp: &identity.TokenResponse{
			AccessToken:           "fresh",
			AccessTokenExpiration: "2099-12-31T23:59:59Z",
		},
	})

	sbx := newSandbox("sbx-2")
	sbx.Annotations = nil

	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sbx).Build()
	r := NewDefaultRefresher(c)

	_, err := r.Refresh(context.Background(), sbx.DeepCopy())
	require.NoError(t, err)

	got := &agentsv1alpha1.Sandbox{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "sbx-2", Namespace: "default"}, got))
	assert.Equal(t, `{"accessTokenExpiration":"2099-12-31T23:59:59Z"}`, got.Annotations[identity.AgentKeyTokenRefreshStatus])
}
