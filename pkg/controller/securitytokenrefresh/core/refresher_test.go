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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/resourceaccess"
	utilruntime "github.com/openkruise/agents/pkg/utils/runtime"
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
	// gotPropOpts records how many transport options the propagation phase
	// received, which is how the tests observe that the refresher forwards the
	// resolved transport instead of silently delivering the token in plaintext.
	gotPropOpts       int
	issuedSandbox     *agentsv1alpha1.Sandbox
	propagatedSandbox *agentsv1alpha1.Sandbox
}

func (s *stubIdentityProvider) IssueToken(_ context.Context, sbx *agentsv1alpha1.Sandbox, _ identity.TokenOptions) (*identity.TokenResponse, error) {
	s.issueCalls++
	s.issuedSandbox = sbx
	if s.issueErr != nil {
		return nil, s.issueErr
	}
	return s.resp, nil
}

func (s *stubIdentityProvider) PropagateSecurityToken(_ context.Context, sbx *agentsv1alpha1.Sandbox, _ *identity.TokenResponse,
	rtOpts ...utilruntime.Option) error {
	s.propagateCalls++
	s.propagatedSandbox = sbx
	s.gotPropOpts = len(rtOpts)
	return s.propagateErr
}

// withProvider installs stub as the global identity provider for the test.
// It is intentionally not parallelisable: the registry is a process-wide singleton.
func withProvider(t *testing.T, stub *stubIdentityProvider) {
	t.Helper()
	identity.RegisterProvider(stub)
	t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })
}

func TestDefaultRefresher_PersistedResourcePolicy(t *testing.T) {
	for _, tt := range []struct {
		name, policy                    string
		marked, mounts, static, wantErr bool
		issuerDenied                    bool
	}{
		{name: "legacy ignores CSI metadata", mounts: true},
		{name: "trusted ignores CSI metadata", marked: true, policy: "trusted-domain-v1", mounts: true},
		{name: "empty marker rejected", marked: true, wantErr: true},
		{name: "unknown marker rejected", marked: true, policy: "unknown", wantErr: true},
		{name: "empty isolated scope clears stale metadata", marked: true, policy: "isolated-v1"},
		{name: "isolated rebuilds shared PV scope without PVC or Secret reads", marked: true, policy: "isolated-v1", mounts: true},
		{name: "static PV stops issuance", marked: true, policy: "isolated-v1", mounts: true, static: true, wantErr: true},
		{name: "external IdP denial proves requested metadata is not a local grant", marked: true, policy: "isolated-v1", mounts: true, issuerDenied: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubIdentityProvider{resp: &identity.TokenResponse{AccessToken: "token", AccessTokenExpiration: "2099-01-01T00:00:00Z"}}
			issuerDenial := errors.New("external IdP denied customer access to requested storage scope")
			if tt.issuerDenied {
				stub.issueErr = issuerDenial
			}
			withProvider(t, stub)
			sbx := newSandbox("policy")
			sbx.Annotations[agentsv1alpha1.AnnotationOwner] = "owner"
			sbx.Annotations[resourceaccess.StorageAuthAnnotation] = "stale"
			if tt.marked {
				sbx.Annotations[resourceaccess.AnnotationPolicy] = tt.policy
			}
			if tt.mounts {
				sbx.Annotations[agentsv1alpha1.AnnotationCSIVolumeConfig] = `[{"pvName":"volume","mountPath":"/mnt/data"}]`
				if tt.policy == "isolated-v1" {
					sbx.Annotations[agentsv1alpha1.AnnotationCSIVolumeConfig] = `[{"pvName":"volume","mountPath":"/mnt/data","subPath":"customer","attributes":{"bucket":"requested-bucket"}}]`
				}
			}
			pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "volume"}, Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
					Driver: "ossplugin.csi.alibabacloud.com", VolumeHandle: "volume", NodePublishSecretRef: &corev1.SecretReference{Name: "missing"},
					VolumeAttributes: map[string]string{"authType": "agent-identity", "sandboxCredProviderName": "provider", "bucket": "default-bucket", "path": "/base"},
				}},
			}}
			if tt.static {
				delete(pv.Spec.CSI.VolumeAttributes, "authType")
			}
			reads, patches := 0, 0
			base := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sbx, pv).Build()
			c := interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					require.IsType(t, &corev1.PersistentVolume{}, obj, "refresh must never read PVCs or Secrets")
					assert.Zero(t, stub.issueCalls, "scope validation must finish before issuance")
					reads++
					return c.Get(ctx, key, obj, opts...)
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					assert.Equal(t, 1, stub.propagateCalls)
					raw, err := patch.Data(obj)
					require.NoError(t, err)
					assert.NotContains(t, string(raw), resourceaccess.StorageAuthAnnotation)
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			before := sbx.DeepCopy()
			resp, err := NewDefaultRefresher(c, nil).Refresh(t.Context(), sbx)
			assert.Equal(t, before, sbx)
			if tt.policy == "isolated-v1" && tt.mounts {
				assert.Equal(t, 1, reads)
			} else {
				assert.Zero(t, reads)
			}
			if tt.wantErr {
				require.Error(t, err)
				assert.Zero(t, stub.issueCalls)
				assert.Zero(t, stub.propagateCalls)
				assert.Zero(t, patches)
				return
			}
			require.Equal(t, 1, stub.issueCalls)
			require.NotNil(t, stub.issuedSandbox)
			if tt.policy == "isolated-v1" {
				assert.NotSame(t, sbx, stub.issuedSandbox)
				assert.Equal(t, sbx.Namespace, stub.issuedSandbox.Namespace)
				assert.Equal(t, "owner", stub.issuedSandbox.Annotations[agentsv1alpha1.AnnotationOwner])
				if tt.mounts {
					assert.JSONEq(t, `[{"credentialProviderName":"provider","attributes":{"bucket-name":"requested-bucket","sub-path":"base/customer"}}]`, stub.issuedSandbox.Annotations[resourceaccess.StorageAuthAnnotation])
				} else {
					assert.NotContains(t, stub.issuedSandbox.Annotations, resourceaccess.StorageAuthAnnotation)
				}
			} else {
				assert.Same(t, sbx, stub.issuedSandbox)
			}
			if tt.issuerDenied {
				require.ErrorIs(t, err, issuerDenial)
				assert.Nil(t, resp)
				assert.Zero(t, stub.propagateCalls)
				assert.Zero(t, patches)
				persisted := &agentsv1alpha1.Sandbox{}
				require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(sbx), persisted))
				assert.Equal(t, before, persisted, "issuer denial must leave expiration and all persisted metadata unchanged")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 1, patches)
			assert.Equal(t, 1, stub.propagateCalls)
			assert.Same(t, stub.issuedSandbox, stub.propagatedSandbox)
		})
	}
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
		// tlsPortAnnotation, when set, makes the sandbox advertise the runtime TLS
		// capability so the transport resolution branch is exercised.
		tlsPortAnnotation string
		// tlsBundle is the client certificate material handed to the refresher.
		tlsBundle *utilruntime.TLSBundle
		// expectErr is the substring expected in the returned error (empty == no error).
		expectErr string
		// expectNoIssue asserts the failure happened before any token was minted,
		// so a misconfiguration never burns an identity-provider issuance call.
		expectNoIssue bool
		// expectPropOpts is the number of transport options the propagation phase
		// must receive. Zero means the legacy plaintext path.
		expectPropOpts int
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
		{
			// A sandbox advertising the TLS capability while this controller holds
			// no certificates must fail loudly: refreshing over plaintext instead
			// would hand the credential to the network in clear text. The failure
			// has to precede issuance so the misconfiguration costs nothing.
			name: "TLS-capable sandbox without a bundle -> transport error before issuance",
			stub: &stubIdentityProvider{
				resp: &identity.TokenResponse{AccessToken: "fresh", AccessTokenExpiration: newExpire},
			},
			tlsPortAnnotation: "49984",
			expectErr:         "resolve runtime transport",
			expectNoIssue:     true,
		},
		{
			// The resolved TLS options (WithTLS + WithTLSPort) must reach the
			// propagator so the refreshed credential rides the same HTTPS
			// transport as the claim and post-resume flows.
			name: "TLS-capable sandbox with a bundle -> propagation receives the TLS options",
			stub: &stubIdentityProvider{
				resp: &identity.TokenResponse{AccessToken: "fresh", AccessTokenExpiration: newExpire},
			},
			tlsPortAnnotation: "49984",
			tlsBundle:         &utilruntime.TLSBundle{CABundle: []byte("pem")},
			expectPropOpts:    2,
			expectAnnotation:  `{"accessTokenExpiration":"` + newExpire + `"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withProvider(t, tt.stub)

			sbx := newSandbox("sbx-1")
			if tt.tlsPortAnnotation != "" {
				sbx.Annotations[agentsv1alpha1.AnnotationRuntimeTLSPort] = tt.tlsPortAnnotation
			}
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sbx).Build()
			r := NewDefaultRefresher(c, tt.tlsBundle)

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
				if tt.expectNoIssue {
					assert.Zero(t, tt.stub.issueCalls, "no token may be minted when the transport cannot be resolved")
					assert.Zero(t, tt.stub.propagateCalls, "propagation must not run when the transport cannot be resolved")
				}
				assert.Equal(t, sbx.Annotations[identity.AgentKeyTokenRefreshStatus],
					got.Annotations[identity.AgentKeyTokenRefreshStatus],
					"annotation must NOT change when an upstream step fails")
				return
			}
			assert.Equal(t, tt.expectPropOpts, tt.stub.gotPropOpts,
				"the propagation phase must receive exactly the transport resolved for the sandbox")
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
	r := NewDefaultRefresher(c, nil)

	_, err := r.Refresh(context.Background(), sbx.DeepCopy())
	require.NoError(t, err)

	got := &agentsv1alpha1.Sandbox{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "sbx-2", Namespace: "default"}, got))
	assert.Equal(t, `{"accessTokenExpiration":"2099-12-31T23:59:59Z"}`, got.Annotations[identity.AgentKeyTokenRefreshStatus])
}
