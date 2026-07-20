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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/identity"
)

// fakeReinitProvider is a test IdentityProvider that records issue/propagate
// calls and returns canned responses / errors so the security-token
// reinitialization logic can be exercised without a real provider backend.
type fakeReinitProvider struct {
	issueCalls     int
	propagateCalls int
	gotPropSbx     *agentsv1alpha1.Sandbox
	resp           *identity.TokenResponse
	issueErr       error
	propagateErr   error
}

func (f *fakeReinitProvider) IssueToken(_ context.Context, _ *agentsv1alpha1.Sandbox, _ identity.TokenKind) (*identity.TokenResponse, error) {
	f.issueCalls++
	if f.issueErr != nil {
		return nil, f.issueErr
	}
	return f.resp, nil
}

func (f *fakeReinitProvider) PropagateSecurityToken(_ context.Context, sbx *agentsv1alpha1.Sandbox, _ *identity.TokenResponse) error {
	f.propagateCalls++
	f.gotPropSbx = sbx
	return f.propagateErr
}

func newReinitScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(sch))
	require.NoError(t, agentsv1alpha1.AddToScheme(sch))
	return sch
}

func TestReinitSecurityToken(t *testing.T) {
	const expiration = "2030-01-01T00:00:00Z"

	tests := []struct {
		name             string
		agentName        string // sets the agent-name opt-in annotation when non-empty
		issueErr         error
		propagateErr     error
		expectError      string
		expectIssueCalls int
		expectPropCalls  int
		expectAnnotation bool // whether the token-status annotation should be persisted
	}{
		{
			name:      "identity provider not requested - no-op",
			agentName: "",
		},
		{
			name:             "issue and propagate success - annotation recorded",
			agentName:        "my-agent",
			expectIssueCalls: 1,
			expectPropCalls:  1,
			expectAnnotation: true,
		},
		{
			name:             "issue failure - no propagation and no annotation",
			agentName:        "my-agent",
			issueErr:         fmt.Errorf("issuer down"),
			expectError:      "failed to issue security token",
			expectIssueCalls: 1,
		},
		{
			name:             "propagate failure - annotation not recorded",
			agentName:        "my-agent",
			propagateErr:     fmt.Errorf("runtime unreachable"),
			expectError:      "failed to propagate security token",
			expectIssueCalls: 1,
			expectPropCalls:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fp := &fakeReinitProvider{
				resp: &identity.TokenResponse{
					AccessToken:           "tok",
					AccessTokenExpiration: expiration,
				},
				issueErr:     tt.issueErr,
				propagateErr: tt.propagateErr,
			}
			identity.RegisterProvider(fp)
			t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })

			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx",
					Namespace: "default",
				},
			}
			if tt.agentName != "" {
				box.Annotations = map[string]string{identity.AnnotationAgentName: tt.agentName}
			}
			sbxForInit := &agentsv1alpha1.Sandbox{ObjectMeta: box.ObjectMeta}

			sch := newReinitScheme(t)
			c := fakeclient.NewClientBuilder().WithScheme(sch).WithObjects(box.DeepCopy()).Build()

			err := reinitSecurityToken(context.Background(), c, sbxForInit)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.expectIssueCalls, fp.issueCalls, "unexpected IssueToken call count")
			assert.Equal(t, tt.expectPropCalls, fp.propagateCalls, "unexpected PropagateSecurityToken call count")

			got := &agentsv1alpha1.Sandbox{}
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(box), got))
			raw, present := got.Annotations[identity.AgentKeyTokenRefreshStatus]
			assert.Equal(t, tt.expectAnnotation, present, "unexpected token-status annotation presence")
			if tt.expectAnnotation {
				assert.Contains(t, raw, expiration, "token-status annotation should carry the new expiration")
			}
		})
	}
}

func TestInitializeInvokesSecurityTokenReinit(t *testing.T) {
	const expiration = "2030-01-01T00:00:00Z"

	tests := []struct {
		name        string
		issueErr    error
		expectError string
		expectIssue int
	}{
		{
			name:        "reinit success is propagated as nil",
			expectIssue: 1,
		},
		{
			name:        "reinit error aborts initialization",
			issueErr:    fmt.Errorf("issuer down"),
			expectError: "failed to issue security token",
			expectIssue: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fp := &fakeReinitProvider{
				resp: &identity.TokenResponse{
					AccessToken:           "tok",
					AccessTokenExpiration: expiration,
				},
				issueErr: tt.issueErr,
			}
			identity.RegisterProvider(fp)
			t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })

			sch := newReinitScheme(t)
			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "sbx",
					Namespace:   "default",
					Annotations: map[string]string{identity.AnnotationAgentName: "my-agent"},
				},
			}
			c := fakeclient.NewClientBuilder().WithScheme(sch).WithObjects(box.DeepCopy()).Build()

			err := Initialize(context.Background(), box, &agentsv1alpha1.SandboxStatus{}, c, c, storages.NewStorageProvider())

			assert.Equal(t, tt.expectIssue, fp.issueCalls, "expected Initialize to trigger security-token reissue")
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
