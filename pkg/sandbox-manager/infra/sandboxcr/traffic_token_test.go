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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
)

type trafficTokenSequenceReader struct {
	mu      sync.Mutex
	objects []*v1alpha1.Sandbox
	calls   int
}

func (r *trafficTokenSequenceReader) Get(ctx context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	index := r.calls
	r.calls++
	if index >= len(r.objects) {
		index = len(r.objects) - 1
	}
	target := obj.(*v1alpha1.Sandbox)
	*target = *r.objects[index].DeepCopy()
	return nil
}

func (r *trafficTokenSequenceReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("unexpected list")
}

func TestInfraIssueTrafficAccessToken(t *testing.T) {
	expiration := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	rootErr := errors.New("provider unavailable")
	tests := []struct {
		name          string
		mutateAfter   func(*v1alpha1.Sandbox)
		providerError error
		validate      func(int, infra.Sandbox) error
		expectError   string
	}{
		{name: "success"},
		{name: "replacement UID is rejected", mutateAfter: func(s *v1alpha1.Sandbox) { s.UID = "replacement" }, expectError: "sandbox changed"},
		{name: "sandbox ID change is rejected", mutateAfter: func(s *v1alpha1.Sandbox) { s.Name = "replacement" }, expectError: "identity changed"},
		{name: "owner change is rejected", mutateAfter: func(s *v1alpha1.Sandbox) { s.Annotations[v1alpha1.AnnotationOwner] = "other" }, expectError: "owner changed"},
		{name: "state change is rejected", mutateAfter: func(s *v1alpha1.Sandbox) { s.Status.Phase = v1alpha1.SandboxPaused }, expectError: "state changed"},
		{name: "JWT opt out is rejected", mutateAfter: func(s *v1alpha1.Sandbox) { delete(s.Annotations, identity.AnnotationEnableJwtAuth) }, expectError: "does not enable"},
		{name: "deletion is rejected", mutateAfter: func(s *v1alpha1.Sandbox) { now := metav1.Now(); s.DeletionTimestamp = &now }, expectError: "being deleted"},
		{name: "provider failure is returned", providerError: rootErr, expectError: rootErr.Error()},
		{
			name: "post-issuance policy rejection discards token",
			validate: func(call int, _ infra.Sandbox) error {
				if call == 2 {
					return errors.New("no longer authorized")
				}
				return nil
			},
			expectError: "no longer authorized",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infraInstance, cachedClient := NewTestInfra(t, config.SandboxManagerOptions{DisableRouteReconciliation: true})
			before := makeClaimedSandbox("default", "traffic-token", "10.0.0.1")
			before.Annotations[identity.AnnotationEnableJwtAuth] = v1alpha1.True
			CreateSandboxWithStatus(t, cachedClient, before)
			sandboxID := utils.GetSandboxID(before)
			require.Eventually(t, func() bool {
				_, err := infraInstance.Cache.GetClaimedSandbox(t.Context(), infracache.GetClaimedSandboxOptions{SandboxID: sandboxID})
				return err == nil
			}, time.Second, 10*time.Millisecond)

			after := before.DeepCopy()
			if tt.mutateAfter != nil {
				tt.mutateAfter(after)
			}
			reader := &trafficTokenSequenceReader{objects: []*v1alpha1.Sandbox{before, after}}
			infraInstance.APIReader = reader

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
			validate := func(sbx infra.Sandbox) error {
				validateCalls++
				if tt.validate != nil {
					return tt.validate(validateCalls, sbx)
				}
				return nil
			}
			result, err := infraInstance.IssueTrafficAccessToken(t.Context(), infra.IssueTrafficAccessTokenOptions{
				SandboxID: sandboxID,
				TokenOptions: identity.TokenOptions{
					RequestedValidity: time.Hour,
				},
				Validate: validate,
			})
			if tt.expectError != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tt.expectError)
				assert.Empty(t, result)
			} else {
				require.NoError(t, err)
				assert.Equal(t, "signed-token", result.Token)
				assert.Equal(t, expiration, result.Expiration)
				assert.Equal(t, 2, validateCalls)
				assert.Equal(t, 2, reader.calls)
			}
			assert.Equal(t, 1, providerCalls)
			assert.Equal(t, identity.TokenKindAccessToken, gotOpts.Kind)
			assert.Equal(t, time.Hour, gotOpts.RequestedValidity)
			assert.NotContains(t, before.Annotations, "trafficAccessToken")
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
		{
			name:        "API reader is required",
			opts:        infra.IssueTrafficAccessTokenOptions{SandboxID: "default--sandbox", Validate: func(infra.Sandbox) error { return nil }},
			expectError: "API reader is not configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := infraInstance.IssueTrafficAccessToken(t.Context(), tt.opts)
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.expectError)
		})
	}
}
