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
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	agentsruntime "github.com/openkruise/agents/pkg/utils/runtime"
)

// fakeProcessProvider is an IdentityProvider stub that records issue/propagate
// calls and lets the test deterministically control the returned response and
// per-phase errors, so ProcessSandboxToken can be exercised without a real
// provider backend.
type fakeProcessProvider struct {
	issueCalls     int
	propagateCalls int
	resp           *TokenResponse
	issueErr       error
	propagateErr   error
	// gotPropOpts records how many transport options reached the propagation
	// phase, so the tests can assert that ProcessSandboxToken forwards the
	// caller-resolved transport verbatim instead of dropping it.
	gotPropOpts int
}

func (f *fakeProcessProvider) IssueToken(_ context.Context, _ *agentsv1alpha1.Sandbox, _ TokenOptions) (*TokenResponse, error) {
	f.issueCalls++
	if f.issueErr != nil {
		return nil, f.issueErr
	}
	return f.resp, nil
}

func (f *fakeProcessProvider) PropagateSecurityToken(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse,
	rtOpts ...agentsruntime.Option) error {
	f.propagateCalls++
	f.gotPropOpts = len(rtOpts)
	return f.propagateErr
}

func newProcessScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(sch))
	return sch
}

// TestProcessSandboxToken exercises the full issue -> propagate -> record
// lifecycle shared by the claim/clone flows and the post-resume reinitializer.
// The opt-in gate now lives in the callers, so this test always drives the full
// lifecycle and only verifies per-phase error wrapping/short-circuiting and the
// annotation-recorded-only-after-successful-propagation invariant.
func TestProcessSandboxToken(t *testing.T) {
	const expiration = "2030-01-01T00:00:00Z"

	tests := []struct {
		name         string
		agentName    string // sets the agent-name annotation to mimic an opted-in sandbox
		issueErr     error
		propagateErr error
		patchFails   bool
		// rtOpts is the transport the caller resolved for the sandbox; it must
		// reach the propagator unchanged.
		rtOpts          []agentsruntime.Option
		expectError     string
		expectIssue     int
		expectPropagate int
		expectPropOpts  int
		expectAnnotated bool // whether the token-status annotation should be persisted
	}{
		{
			name:            "success - issue, propagate and record",
			agentName:       "my-agent",
			expectIssue:     1,
			expectPropagate: 1,
			expectAnnotated: true,
		},
		{
			// The claim/clone/post-resume callers hand the resolved TLS transport
			// down; without this forwarding an enterprise propagator would deliver
			// the credential over plaintext to a TLS-capable sandbox.
			name:            "success - resolved transport is forwarded to the propagator",
			agentName:       "my-agent",
			rtOpts:          []agentsruntime.Option{agentsruntime.WithTLSPort(49984), agentsruntime.WithAuthority("runtime.example")},
			expectIssue:     1,
			expectPropagate: 1,
			expectPropOpts:  2,
			expectAnnotated: true,
		},
		{
			name:        "issue failure - no propagate, no record",
			agentName:   "my-agent",
			issueErr:    fmt.Errorf("issuer down"),
			expectError: "failed to issue security token",
			expectIssue: 1,
		},
		{
			name:            "propagate failure - no record",
			agentName:       "my-agent",
			propagateErr:    fmt.Errorf("runtime unreachable"),
			expectError:     "failed to propagate security token",
			expectIssue:     1,
			expectPropagate: 1,
		},
		{
			name:            "record patch failure",
			agentName:       "my-agent",
			patchFails:      true,
			expectError:     "failed to record security token refresh status",
			expectIssue:     1,
			expectPropagate: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fp := &fakeProcessProvider{
				resp: &TokenResponse{
					AccessToken:           "tok",
					AccessTokenExpiration: expiration,
				},
				issueErr:     tt.issueErr,
				propagateErr: tt.propagateErr,
			}
			RegisterProvider(fp)
			t.Cleanup(func() { RegisterProvider(NewDefaultIdentityProvider()) })

			sbx := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx",
					Namespace: "default",
				},
			}
			if tt.agentName != "" {
				sbx.Annotations = map[string]string{AnnotationAgentName: tt.agentName}
			}

			sch := newProcessScheme(t)
			builder := fakeclient.NewClientBuilder().WithScheme(sch).WithObjects(sbx.DeepCopy())
			if tt.patchFails {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if _, ok := obj.(*agentsv1alpha1.Sandbox); ok {
							return apierrors.NewInternalError(fmt.Errorf("boom"))
						}
						return c.Patch(ctx, obj, patch, opts...)
					},
				})
			}
			c := builder.Build()

			cost, err := ProcessSandboxToken(context.Background(), c, sbx, tt.rtOpts...)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
				assert.Greater(t, int64(cost), int64(0), "total lifecycle cost should be recorded on success")
			}
			assert.Equal(t, tt.expectIssue, fp.issueCalls, "unexpected IssueToken call count")
			assert.Equal(t, tt.expectPropagate, fp.propagateCalls, "unexpected PropagateSecurityToken call count")
			assert.Equal(t, tt.expectPropOpts, fp.gotPropOpts,
				"the propagator must receive exactly the transport options the caller passed in")

			// In-memory annotation mirroring: only present on a fully successful run.
			raw, present := sbx.GetAnnotations()[AgentKeyTokenRefreshStatus]
			assert.Equal(t, tt.expectAnnotated, present, "unexpected in-memory annotation presence")
			if tt.expectAnnotated {
				assert.Contains(t, raw, expiration)
			}

			// Persisted annotation must match the in-memory expectation.
			got := &agentsv1alpha1.Sandbox{}
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(sbx), got))
			_, persisted := got.GetAnnotations()[AgentKeyTokenRefreshStatus]
			assert.Equal(t, tt.expectAnnotated, persisted, "unexpected persisted annotation presence")
		})
	}
}
