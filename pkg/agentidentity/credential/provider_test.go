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

package credential

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/oauth2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
)

const (
	testNamespace    = "team-a"
	testProvider     = "github"
	testSecretName   = "github-oauth"
	testSecretKey    = "client-secret"
	testClientID     = "Iv1.client-id"
	testClientSecret = "client-secret-value-never-printed"
)

var scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(securityv1alpha1.AddToScheme(s))
	return s
}()

// readyProvider is a CredentialProvider the controller has reported Ready for
// its current generation, pointing its token endpoint at tokenURL.
func readyProvider(tokenURL string) *securityv1alpha1.CredentialProvider {
	return &securityv1alpha1.CredentialProvider{
		ObjectMeta: metav1.ObjectMeta{Name: testProvider, Namespace: testNamespace, Generation: 2},
		Spec: securityv1alpha1.CredentialProviderSpec{
			Type: "oauth2",
			OAuth2: &securityv1alpha1.OAuth2Provider{
				GrantType: "userFederation",
				Discovery: securityv1alpha1.OAuth2Discovery{
					Issuer:                "https://github.com",
					AuthorizationEndpoint: "https://github.com/login/oauth/authorize",
					TokenEndpoint:         tokenURL,
				},
				Client: securityv1alpha1.OAuth2Client{
					ClientID: testClientID,
					ClientSecret: securityv1alpha1.SecretKeyReference{
						SecretKeyRef: securityv1alpha1.SecretKeySelector{Name: testSecretName, Key: testSecretKey},
					},
				},
				UserFederation: &securityv1alpha1.OAuth2UserFederation{
					CallbackBaseURL: "https://identity.example.com",
					DefaultScopes:   []string{"repo"},
				},
			},
		},
		Status: securityv1alpha1.CredentialProviderStatus{
			ObservedGeneration: 2,
			Conditions: []metav1.Condition{{
				Type:               securityv1alpha1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             "Ready",
				ObservedGeneration: 2,
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

func clientSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testSecretName, Namespace: testNamespace},
		Data:       data,
	}
}

func usableSecret() *corev1.Secret {
	return clientSecret(map[string][]byte{testSecretKey: []byte(testClientSecret)})
}

func TestLoadProvider(t *testing.T) {
	const tokenURL = "https://github.com/login/oauth/access_token"
	providerWith := func(mutate func(*securityv1alpha1.CredentialProvider)) *securityv1alpha1.CredentialProvider {
		provider := readyProvider(tokenURL)
		mutate(provider)
		return provider
	}
	readFailure := errors.New("the cache is unavailable")

	tests := []struct {
		name    string
		objects []client.Object
		// failReading makes every Get of this kind fail with readFailure.
		failReading client.Object
		wantErr     error
	}{
		{
			name:    "a ready provider with its secret loads",
			objects: []client.Object{readyProvider(tokenURL), usableSecret()},
		},
		{
			name:    "an absent provider is not found",
			objects: []client.Object{usableSecret()},
			wantErr: ErrProviderNotFound,
		},
		{
			name: "a provider being deleted is not ready",
			objects: []client.Object{providerWith(func(p *securityv1alpha1.CredentialProvider) {
				p.Finalizers = []string{"example.com/hold"}
				p.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
			}), usableSecret()},
			wantErr: ErrProviderNotReady,
		},
		{
			name: "a provider reported not ready is not ready",
			objects: []client.Object{providerWith(func(p *securityv1alpha1.CredentialProvider) {
				p.Status.Conditions[0].Status = metav1.ConditionFalse
			}), usableSecret()},
			wantErr: ErrProviderNotReady,
		},
		{
			name: "a provider with no Ready condition is not ready",
			objects: []client.Object{providerWith(func(p *securityv1alpha1.CredentialProvider) {
				p.Status.Conditions = nil
			}), usableSecret()},
			wantErr: ErrProviderNotReady,
		},
		{
			name: "status for an earlier generation is not readiness for this one",
			objects: []client.Object{providerWith(func(p *securityv1alpha1.CredentialProvider) {
				p.Status.ObservedGeneration = 1
			}), usableSecret()},
			wantErr: ErrProviderNotReady,
		},
		{
			name: "a condition for an earlier generation is not readiness for this one",
			objects: []client.Object{providerWith(func(p *securityv1alpha1.CredentialProvider) {
				p.Status.Conditions[0].ObservedGeneration = 1
			}), usableSecret()},
			wantErr: ErrProviderNotReady,
		},
		{
			name: "a provider with no oauth2 block fails closed even if marked ready",
			objects: []client.Object{providerWith(func(p *securityv1alpha1.CredentialProvider) {
				p.Spec.OAuth2 = nil
			}), usableSecret()},
			wantErr: ErrProviderNotReady,
		},
		{
			name:    "a client secret deleted since the provider went ready",
			objects: []client.Object{readyProvider(tokenURL)},
			wantErr: ErrProviderNotReady,
		},
		{
			name: "a client secret without the named key",
			objects: []client.Object{readyProvider(tokenURL),
				clientSecret(map[string][]byte{"other-key": []byte(testClientSecret)})},
			wantErr: ErrProviderNotReady,
		},
		{
			name: "a client secret whose key is empty",
			objects: []client.Object{readyProvider(tokenURL),
				clientSecret(map[string][]byte{testSecretKey: {}})},
			wantErr: ErrProviderNotReady,
		},
		{
			name:        "a failed provider read is surfaced, not reported as absent",
			objects:     []client.Object{readyProvider(tokenURL), usableSecret()},
			failReading: &securityv1alpha1.CredentialProvider{},
			wantErr:     readFailure,
		},
		{
			name:        "a failed secret read is surfaced, not reported as absent",
			objects:     []client.Object{readyProvider(tokenURL), usableSecret()},
			failReading: &corev1.Secret{},
			wantErr:     readFailure,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.objects...)
			if tt.failReading != nil {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
						obj client.Object, opts ...client.GetOption) error {
						if fmt.Sprintf("%T", obj) == fmt.Sprintf("%T", tt.failReading) {
							return readFailure
						}
						return c.Get(ctx, key, obj, opts...)
					},
				})
			}

			provider, err := LoadProvider(context.Background(), builder.Build(),
				client.ObjectKey{Namespace: testNamespace, Name: testProvider})

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error: got %v, want %v", err, tt.wantErr)
				}
				if provider != nil {
					t.Error("a refused provider must not be returned")
				}
				if strings.Contains(err.Error(), testClientSecret) {
					t.Errorf("error %q carries the client secret", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadProvider: %v", err)
			}
			want := oauth2.Config{
				ClientID:     testClientID,
				ClientSecret: testClientSecret,
				Endpoint: oauth2.Endpoint{
					AuthURL:   "https://github.com/login/oauth/authorize",
					TokenURL:  tokenURL,
					AuthStyle: oauth2.AuthStyleInParams,
				},
			}
			if got := provider.config; got.ClientID != want.ClientID || got.ClientSecret != want.ClientSecret ||
				got.Endpoint != want.Endpoint {
				t.Errorf("config: got client %q endpoint %+v, want client %q endpoint %+v",
					got.ClientID, got.Endpoint, want.ClientID, want.Endpoint)
			}
		})
	}
}

// TestProviderNeverPrintsItsClientSecret covers the formatting paths a caller
// is likeliest to log a Provider through. fmt walks unexported fields for %v
// unless String intervenes, so the secret would otherwise appear in full.
func TestProviderNeverPrintsItsClientSecret(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(readyProvider("https://github.com/login/oauth/access_token"), usableSecret()).Build()
	provider, err := LoadProvider(context.Background(), fc,
		client.ObjectKey{Namespace: testNamespace, Name: testProvider})
	if err != nil {
		t.Fatalf("LoadProvider: %v", err)
	}

	rendered := map[string]string{}
	for _, verb := range []string{"%v", "%+v", "%s", "%#v"} {
		rendered[verb+" of a pointer"] = fmt.Sprintf(verb, provider)
		rendered[verb+" of a value"] = fmt.Sprintf(verb, *provider)
	}
	encoded, err := json.Marshal(provider)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	rendered["json"] = string(encoded)

	for how, text := range rendered {
		if strings.Contains(text, testClientSecret) {
			t.Errorf("%s prints the client secret: %s", how, text)
		}
	}
	if want := testNamespace + "/" + testProvider; !strings.Contains(rendered["%v of a pointer"], want) {
		t.Errorf("%%v is %q, want it to name %s", rendered["%v of a pointer"], want)
	}
}
