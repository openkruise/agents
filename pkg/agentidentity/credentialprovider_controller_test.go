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

package agentidentity

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
)

const (
	providerNamespace = "team-a"
	providerName      = "github"
	clientSecretName  = "github-oauth"
	clientSecretKey   = "clientSecret"
)

// credentialProvider returns a provider that is valid apart from whatever a
// case changes.
func credentialProvider(callbackBaseURL string) *securityv1alpha1.CredentialProvider {
	return &securityv1alpha1.CredentialProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:       providerName,
			Namespace:  providerNamespace,
			Generation: 3,
		},
		Spec: securityv1alpha1.CredentialProviderSpec{
			Type: "oauth2",
			OAuth2: &securityv1alpha1.OAuth2Provider{
				GrantType: "userFederation",
				Discovery: securityv1alpha1.OAuth2Discovery{
					Issuer:                "https://github.com",
					AuthorizationEndpoint: "https://github.com/login/oauth/authorize",
					TokenEndpoint:         "https://github.com/login/oauth/access_token",
				},
				Client: securityv1alpha1.OAuth2Client{
					ClientID: "Iv1.0123456789abcdef",
					ClientSecret: securityv1alpha1.SecretKeyReference{
						SecretKeyRef: securityv1alpha1.SecretKeySelector{
							Name: clientSecretName,
							Key:  clientSecretKey,
						},
					},
				},
				UserFederation: &securityv1alpha1.OAuth2UserFederation{
					CallbackBaseURL: callbackBaseURL,
					DefaultScopes:   []string{"repo:status"},
				},
			},
		},
	}
}

func clientSecret(value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: clientSecretName, Namespace: providerNamespace},
		Data:       map[string][]byte{clientSecretKey: []byte(value)},
	}
}

// reconcileProvider runs one reconcile over the given objects and returns the
// provider as it stands afterwards.
func reconcileProvider(t *testing.T, objects ...client.Object) *securityv1alpha1.CredentialProvider {
	t.Helper()
	return reconcileProviderWith(t, interceptor.Funcs{}, objects...)
}

func reconcileProviderWith(t *testing.T, funcs interceptor.Funcs,
	objects ...client.Object) *securityv1alpha1.CredentialProvider {
	t.Helper()

	fake := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&securityv1alpha1.CredentialProvider{}).
		WithInterceptorFuncs(funcs).
		Build()

	reconciler := &CredentialProviderReconciler{Client: fake}
	key := types.NamespacedName{Namespace: providerNamespace, Name: providerName}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	observed := &securityv1alpha1.CredentialProvider{}
	if err := fake.Get(context.Background(), key, observed); err != nil {
		t.Fatalf("get provider: %v", err)
	}
	return observed
}

// TestCredentialProviderReadiness covers the verdicts an operator sees and the
// callback URL recorded with each.
func TestCredentialProviderReadiness(t *testing.T) {
	tests := []struct {
		name            string
		provider        *securityv1alpha1.CredentialProvider
		secret          *corev1.Secret
		wantStatus      metav1.ConditionStatus
		wantReason      string
		wantCallbackURL string
	}{
		{
			name:            "a provider with its client secret is ready",
			provider:        credentialProvider("https://agents.example.com"),
			secret:          clientSecret("s3cr3t"),
			wantStatus:      metav1.ConditionTrue,
			wantReason:      reasonProviderReady,
			wantCallbackURL: "https://agents.example.com/oauth2/callback",
		},
		{
			// Admission permits a base with a path, and the registered redirect
			// URI has to match whatever it produces.
			name:            "a base URL with a path keeps the path",
			provider:        credentialProvider("https://agents.example.com/agents/oauth"),
			secret:          clientSecret("s3cr3t"),
			wantStatus:      metav1.ConditionTrue,
			wantReason:      reasonProviderReady,
			wantCallbackURL: "https://agents.example.com/agents/oauth/oauth2/callback",
		},
		{
			// The case that would otherwise produce a doubled slash and fail
			// the third party's exact match on the redirect URI.
			name:            "a trailing slash does not double up",
			provider:        credentialProvider("https://agents.example.com/"),
			secret:          clientSecret("s3cr3t"),
			wantStatus:      metav1.ConditionTrue,
			wantReason:      reasonProviderReady,
			wantCallbackURL: "https://agents.example.com/oauth2/callback",
		},
		{
			name:       "a missing Secret is reported, not assumed",
			provider:   credentialProvider("https://agents.example.com"),
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonClientSecretNotFound,
		},
		{
			name:     "a Secret without the named key is not usable",
			provider: credentialProvider("https://agents.example.com"),
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: clientSecretName, Namespace: providerNamespace},
				Data:       map[string][]byte{"somethingElse": []byte("s3cr3t")},
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonClientSecretKeyNotFound,
		},
		{
			// An empty value is as unusable as an absent one, and an operator
			// who created the key but left it blank needs telling.
			name:       "an empty client secret is not usable",
			provider:   credentialProvider("https://agents.example.com"),
			secret:     clientSecret(""),
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonClientSecretKeyNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := []client.Object{tt.provider}
			if tt.secret != nil {
				objects = append(objects, tt.secret)
			}
			observed := reconcileProvider(t, objects...)

			ready := apiMeta.FindStatusCondition(observed.Status.Conditions, securityv1alpha1.ConditionReady)
			if ready == nil {
				t.Fatal("no Ready condition was set")
			}
			if ready.Status != tt.wantStatus {
				t.Errorf("Ready is %s, want %s (reason %s, message %q)",
					ready.Status, tt.wantStatus, ready.Reason, ready.Message)
			}
			if ready.Reason != tt.wantReason {
				t.Errorf("reason is %q, want %q", ready.Reason, tt.wantReason)
			}
			if observed.Status.CallbackURL != tt.wantCallbackURL {
				t.Errorf("callbackUrl is %q, want %q", observed.Status.CallbackURL, tt.wantCallbackURL)
			}
			if observed.Status.ObservedGeneration != tt.provider.Generation {
				t.Errorf("observedGeneration is %d, want %d",
					observed.Status.ObservedGeneration, tt.provider.Generation)
			}
		})
	}
}

// TestCredentialProviderFailsClosedOnSpecsThatPredateAdmission covers the
// blocks admission now requires.
//
// Admission rejects these going forward, but a provider created before those
// rules existed is still in the cluster and still readable, so the controller
// has to refuse rather than dereference.
func TestCredentialProviderFailsClosedOnSpecsThatPredateAdmission(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(provider *securityv1alpha1.CredentialProvider)
	}{
		{
			name: "no oauth2 block",
			mutate: func(p *securityv1alpha1.CredentialProvider) {
				p.Spec.OAuth2 = nil
			},
		},
		{
			name: "no userFederation block",
			mutate: func(p *securityv1alpha1.CredentialProvider) {
				p.Spec.OAuth2.UserFederation = nil
			},
		},
		{
			name: "no callback base URL",
			mutate: func(p *securityv1alpha1.CredentialProvider) {
				p.Spec.OAuth2.UserFederation.CallbackBaseURL = ""
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := credentialProvider("https://agents.example.com")
			tt.mutate(provider)

			observed := reconcileProvider(t, provider, clientSecret("s3cr3t"))

			ready := apiMeta.FindStatusCondition(observed.Status.Conditions, securityv1alpha1.ConditionReady)
			if ready == nil {
				t.Fatal("no Ready condition was set")
			}
			if ready.Status != metav1.ConditionFalse {
				t.Errorf("Ready is %s, want False", ready.Status)
			}
			if ready.Reason != reasonProviderInvalidSpec {
				t.Errorf("reason is %q, want %q", ready.Reason, reasonProviderInvalidSpec)
			}
			if observed.Status.CallbackURL != "" {
				t.Errorf("callbackUrl is %q on an unusable provider, want it empty",
					observed.Status.CallbackURL)
			}
		})
	}
}

// TestAReadFailureIsRetriedNotReportedAsAMissingSecret separates the two ways
// a Secret read can fail.
//
// Reporting a transient read failure as ClientSecretNotFound would tell an
// operator their Secret is missing when it may be sitting there untouched, and
// would overwrite a correct Ready=True with a wrong verdict.
func TestAReadFailureIsRetriedNotReportedAsAMissingSecret(t *testing.T) {
	provider := credentialProvider("https://agents.example.com")
	fake := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(provider, clientSecret("s3cr3t")).
		WithStatusSubresource(&securityv1alpha1.CredentialProvider{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
				object client.Object, opts ...client.GetOption) error {
				if _, isSecret := object.(*corev1.Secret); isSecret {
					return apierrors.NewInternalError(errors.New("etcd is unavailable"))
				}
				return c.Get(ctx, key, object, opts...)
			},
		}).
		Build()

	reconciler := &CredentialProviderReconciler{Client: fake}
	key := types.NamespacedName{Namespace: providerNamespace, Name: providerName}
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err == nil {
		t.Fatal("a failed Secret read produced a verdict instead of an error")
	}
	if !strings.Contains(err.Error(), "read client secret") {
		t.Errorf("error does not name what failed: %v", err)
	}

	// And no verdict was written, so a previously correct status survives.
	observed := &securityv1alpha1.CredentialProvider{}
	if err := fake.Get(context.Background(), key, observed); err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if ready := apiMeta.FindStatusCondition(observed.Status.Conditions,
		securityv1alpha1.ConditionReady); ready != nil {
		t.Errorf("a condition was written despite the read failing: %s/%s", ready.Status, ready.Reason)
	}
}

// TestTheClientSecretNeverReachesStatus is the §4 requirement. Status, events
// and logs must not carry a client secret, and a condition message is the
// easiest place for one to end up.
func TestTheClientSecretNeverReachesStatus(t *testing.T) {
	const value = "ghs_theActualClientSecretValue"

	for _, secret := range []*corev1.Secret{clientSecret(value), nil} {
		objects := []client.Object{credentialProvider("https://agents.example.com")}
		if secret != nil {
			objects = append(objects, secret)
		}
		observed := reconcileProvider(t, objects...)

		for _, condition := range observed.Status.Conditions {
			if strings.Contains(condition.Message, value) {
				t.Errorf("the client secret appears in a condition message: %s", condition.Message)
			}
		}
		if strings.Contains(observed.Status.CallbackURL, value) {
			t.Error("the client secret appears in the callback URL")
		}
	}
}

// TestADeletedProviderIsLeftAlone covers the deletion path: a provider on its
// way out must not be given a fresh Ready=True that outlives the decision to
// delete it.
func TestADeletedProviderIsLeftAlone(t *testing.T) {
	provider := credentialProvider("https://agents.example.com")
	now := metav1.Now()
	provider.DeletionTimestamp = &now
	provider.Finalizers = []string{"example.com/hold"}

	observed := reconcileProvider(t, provider, clientSecret("s3cr3t"))

	if ready := apiMeta.FindStatusCondition(observed.Status.Conditions,
		securityv1alpha1.ConditionReady); ready != nil {
		t.Errorf("a deleted provider was given a Ready condition: %s/%s", ready.Status, ready.Reason)
	}
	if observed.Status.CallbackURL != "" {
		t.Errorf("a deleted provider was given a callback URL: %q", observed.Status.CallbackURL)
	}
}

// TestProvidersForSecret covers the fan-out that lets a provider recover when
// its Secret is finally created.
func TestProvidersForSecret(t *testing.T) {
	matching := credentialProvider("https://agents.example.com")

	other := credentialProvider("https://agents.example.com")
	other.Name = "gitlab"
	other.Spec.OAuth2.Client.ClientSecret.SecretKeyRef.Name = "gitlab-oauth"

	elsewhere := credentialProvider("https://agents.example.com")
	elsewhere.Name = "github"
	elsewhere.Namespace = "team-b"

	legacy := credentialProvider("https://agents.example.com")
	legacy.Name = "legacy"
	legacy.Spec.OAuth2 = nil

	fake := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(matching, other, elsewhere, legacy).
		Build()
	reconciler := &CredentialProviderReconciler{Client: fake}

	requests := reconciler.providersForSecret(context.Background(), clientSecret("s3cr3t"))

	if len(requests) != 1 {
		t.Fatalf("got %d requests, want 1: %v", len(requests), requests)
	}
	if requests[0].Name != providerName || requests[0].Namespace != providerNamespace {
		t.Errorf("enqueued %s/%s, want %s/%s",
			requests[0].Namespace, requests[0].Name, providerNamespace, providerName)
	}
}

// TestCallbackURLFor pins the derivation on its own, since the exact string is
// what has to match the third-party app registration.
func TestCallbackURLFor(t *testing.T) {
	tests := []struct {
		base string
		want string
	}{
		{"https://agents.example.com", "https://agents.example.com/oauth2/callback"},
		{"https://agents.example.com/", "https://agents.example.com/oauth2/callback"},
		{"https://agents.example.com/agents", "https://agents.example.com/agents/oauth2/callback"},
		{"https://agents.example.com/agents/", "https://agents.example.com/agents/oauth2/callback"},
		{"https://agents.example.com:8443", "https://agents.example.com:8443/oauth2/callback"},
		// Case is left as the operator wrote it: the registered redirect URI is
		// whatever they typed into the third party, not a canonical form.
		{"https://Agents.Example.com", "https://Agents.Example.com/oauth2/callback"},
	}

	for _, tt := range tests {
		t.Run(tt.base, func(t *testing.T) {
			got := CallbackURLFor(tt.base)
			if got != tt.want {
				t.Errorf("CallbackURLFor(%q) = %q, want %q", tt.base, got, tt.want)
			}
			if strings.Contains(got, "//oauth2") {
				t.Errorf("CallbackURLFor(%q) produced a doubled slash: %q", tt.base, got)
			}
		})
	}
}
