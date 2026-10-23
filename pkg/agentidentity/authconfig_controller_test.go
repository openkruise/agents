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
	"testing"
	"time"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
	"github.com/openkruise/agents/pkg/identity/oidc"
)

// unresolvedConfig is an AgentAuthenticationConfig as an operator writes it:
// a discovery URL and audiences, with status left for the controller.
func unresolvedConfig(name, discoveryURL string) *securityv1alpha1.AgentAuthenticationConfig {
	return &securityv1alpha1.AgentAuthenticationConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Generation: 1},
		Spec: securityv1alpha1.AgentAuthenticationConfigSpec{
			Type: "jwt",
			JWT: &securityv1alpha1.JWTAuthentication{
				DiscoveryURL:    discoveryURL,
				AllowedAudience: []string{"sample-agent"},
			},
		},
	}
}

// staticDiscovery returns a discover func yielding fixed values, standing in
// for a reachable issuer without making a network call.
func staticDiscovery(issuer, jwksURI string) func(context.Context, string, oidc.Options) (oidc.Discovered, error) {
	return func(context.Context, string, oidc.Options) (oidc.Discovered, error) {
		return oidc.Discovered{Issuer: issuer, JWKSURI: jwksURI}, nil
	}
}

func failingDiscovery(err error) func(context.Context, string, oidc.Options) (oidc.Discovered, error) {
	return func(context.Context, string, oidc.Options) (oidc.Discovered, error) {
		return oidc.Discovered{}, err
	}
}

func TestAuthConfigReconcile(t *testing.T) {
	const discoveryURL = "https://keycloak.example.com/realms/agents/.well-known/openid-configuration"

	noJWT := unresolvedConfig("no-jwt", discoveryURL)
	noJWT.Spec.JWT = nil

	emptyURL := unresolvedConfig("empty-url", "")

	deleting := unresolvedConfig("deleting", discoveryURL)
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{"agents.kruise.io/test"}

	tests := []struct {
		name     string
		config   *securityv1alpha1.AgentAuthenticationConfig
		discover func(context.Context, string, oidc.Options) (oidc.Discovered, error)
		// wantNoCondition expects nothing to be written at all.
		wantNoCondition bool
		wantStatus      metav1.ConditionStatus
		wantReason      string
		wantIssuer      string
		wantJWKSURI     string
		wantRequeue     time.Duration
	}{
		{
			name:        "a reachable issuer is resolved into status",
			config:      unresolvedConfig("keycloak", discoveryURL),
			discover:    staticDiscovery("https://keycloak.example.com/realms/agents", "https://keycloak.example.com/realms/agents/protocol/openid-connect/certs"),
			wantStatus:  metav1.ConditionTrue,
			wantReason:  reasonDiscovered,
			wantIssuer:  "https://keycloak.example.com/realms/agents",
			wantJWKSURI: "https://keycloak.example.com/realms/agents/protocol/openid-connect/certs",
			wantRequeue: 0,
		},
		{
			name:        "an unreachable issuer is reported and retried",
			config:      unresolvedConfig("keycloak", discoveryURL),
			discover:    failingDiscovery(errors.New("connection refused")),
			wantStatus:  metav1.ConditionFalse,
			wantReason:  reasonDiscoveryFailed,
			wantRequeue: discoveryRetryBackoff,
		},
		{
			// Retrying a spec that cannot be acted on would be pointless: only
			// an edit changes it, and that arrives as an event.
			name:        "a missing jwt block is not retried",
			config:      noJWT,
			discover:    staticDiscovery("https://issuer.example", "https://issuer.example/keys"),
			wantStatus:  metav1.ConditionFalse,
			wantReason:  reasonInvalidSpec,
			wantRequeue: 0,
		},
		{
			name:        "an empty discovery URL is not retried",
			config:      emptyURL,
			discover:    staticDiscovery("https://issuer.example", "https://issuer.example/keys"),
			wantStatus:  metav1.ConditionFalse,
			wantReason:  reasonInvalidSpec,
			wantRequeue: 0,
		},
		{
			name:            "a deleting config is left alone",
			config:          deleting,
			discover:        staticDiscovery("https://issuer.example", "https://issuer.example/keys"),
			wantNoCondition: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&securityv1alpha1.AgentAuthenticationConfig{}).
				WithObjects(tt.config.DeepCopy()).Build()
			r := &AuthConfigReconciler{Client: fc, discover: tt.discover}
			key := client.ObjectKeyFromObject(tt.config)

			result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if result.RequeueAfter != tt.wantRequeue {
				t.Errorf("requeueAfter: got %v, want %v", result.RequeueAfter, tt.wantRequeue)
			}

			got := &securityv1alpha1.AgentAuthenticationConfig{}
			if err := r.Get(context.Background(), key, got); err != nil {
				t.Fatalf("getting the config: %v", err)
			}

			condition := apiMeta.FindStatusCondition(got.Status.Conditions, securityv1alpha1.ConditionReady)
			if tt.wantNoCondition {
				if condition != nil {
					t.Fatalf("expected no condition, got %+v", condition)
				}
				return
			}
			if condition == nil {
				t.Fatal("expected a Ready condition, got none")
			}
			if condition.Status != tt.wantStatus {
				t.Errorf("status: got %q, want %q", condition.Status, tt.wantStatus)
			}
			if condition.Reason != tt.wantReason {
				t.Errorf("reason: got %q, want %q", condition.Reason, tt.wantReason)
			}
			if condition.Message == "" {
				t.Error("condition carries no message")
			}
			if condition.ObservedGeneration != tt.config.Generation {
				t.Errorf("condition observedGeneration: got %d, want %d",
					condition.ObservedGeneration, tt.config.Generation)
			}
			if got.Status.Issuer != tt.wantIssuer {
				t.Errorf("status.issuer: got %q, want %q", got.Status.Issuer, tt.wantIssuer)
			}
			if got.Status.JWKSURI != tt.wantJWKSURI {
				t.Errorf("status.jwksUri: got %q, want %q", got.Status.JWKSURI, tt.wantJWKSURI)
			}
		})
	}
}

// TestAuthConfigClearsResolutionWhenANewGenerationFails covers the case where a
// stale resolution would actually be dangerous.
//
// Discovery runs once per generation, so a transient outage no longer re-opens
// a settled answer: the issuer identity recorded for a generation is a fact
// about that spec, not about whether the network is up. What must not survive
// is an edit. If the spec is repointed and the new discoveryUrl does not
// resolve, the previous issuer has to be cleared, or status would keep
// advertising an issuer the current spec never named.
func TestAuthConfigClearsResolutionWhenANewGenerationFails(t *testing.T) {
	config := unresolvedConfig("keycloak", "https://keycloak.example.com/.well-known/openid-configuration")
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&securityv1alpha1.AgentAuthenticationConfig{}).
		WithObjects(config.DeepCopy()).Build()
	key := client.ObjectKeyFromObject(config)
	ctx := context.Background()

	resolving := &AuthConfigReconciler{Client: fc,
		discover: staticDiscovery("https://keycloak.example.com", "https://keycloak.example.com/keys")}
	if _, err := resolving.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	got := &securityv1alpha1.AgentAuthenticationConfig{}
	if err := fc.Get(ctx, key, got); err != nil {
		t.Fatalf("get after resolving: %v", err)
	}
	if got.Status.Issuer == "" {
		t.Fatal("expected the issuer to be recorded on success")
	}

	// A settled generation is not re-resolved, so a failing discover changes
	// nothing while the spec is unchanged.
	broken := &AuthConfigReconciler{Client: fc, discover: failingDiscovery(errors.New("dns failure"))}
	if _, err := broken.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile on an unchanged generation: %v", err)
	}
	if err := fc.Get(ctx, key, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Issuer == "" {
		t.Error("an outage must not discard a resolution that belongs to an unchanged spec")
	}

	// The spec is now repointed somewhere that does not resolve.
	got.Spec.JWT.DiscoveryURL = "https://elsewhere.example/.well-known/openid-configuration"
	got.Generation = 2
	if err := fc.Update(ctx, got); err != nil {
		t.Fatalf("update the spec: %v", err)
	}
	if _, err := broken.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile on the new generation: %v", err)
	}

	if err := fc.Get(ctx, key, got); err != nil {
		t.Fatalf("get after the failed generation: %v", err)
	}
	if got.Status.Issuer != "" || got.Status.JWKSURI != "" {
		t.Errorf("a resolution from the previous spec survived: issuer=%q jwksUri=%q",
			got.Status.Issuer, got.Status.JWKSURI)
	}
	if c := apiMeta.FindStatusCondition(got.Status.Conditions, securityv1alpha1.ConditionReady); c == nil ||
		c.Status != metav1.ConditionFalse {
		t.Errorf("expected Ready=False for the unresolvable generation, got %+v", c)
	}
}

// TestAgentIdentityBecomesReadyOnceItsConfigResolves is the reason this
// controller exists.
//
// authenticationRefs is required with MinItems=1 and the AgentIdentity
// controller requires every referenced config to be Ready for its current
// generation. With nothing producing that condition, no AgentIdentity could
// ever reach Ready. This drives both controllers in sequence and shows the
// identity going from blocked to Ready.
func TestAgentIdentityBecomesReadyOnceItsConfigResolves(t *testing.T) {
	config := unresolvedConfig("keycloak", "https://keycloak.example.com/.well-known/openid-configuration")
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&securityv1alpha1.AgentIdentity{}).
		WithStatusSubresource(&securityv1alpha1.AgentAuthenticationConfig{}).
		WithObjects(identity("sample-agent", 1, "keycloak"), config.DeepCopy()).Build()
	ctx := context.Background()

	identityKey := types.NamespacedName{Namespace: testNamespace, Name: "sample-agent"}
	configKey := types.NamespacedName{Namespace: testNamespace, Name: "keycloak"}

	identityReconciler := &Reconciler{Client: fc}
	configReconciler := &AuthConfigReconciler{Client: fc,
		discover: staticDiscovery("https://keycloak.example.com", "https://keycloak.example.com/keys")}

	// Before the config is reconciled the identity is blocked, which is the
	// state master is in today.
	if _, err := identityReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: identityKey}); err != nil {
		t.Fatalf("identity reconcile before: %v", err)
	}
	blocked := readyConditionOf(t, identityReconciler, identityKey)
	if blocked == nil || blocked.Status != metav1.ConditionFalse {
		t.Fatalf("expected the identity to start blocked, got %+v", blocked)
	}
	if blocked.Reason != reasonAuthenticationConfigNotReady {
		t.Errorf("blocked reason: got %q, want %q", blocked.Reason, reasonAuthenticationConfigNotReady)
	}

	if _, err := configReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: configKey}); err != nil {
		t.Fatalf("config reconcile: %v", err)
	}

	if _, err := identityReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: identityKey}); err != nil {
		t.Fatalf("identity reconcile after: %v", err)
	}
	unblocked := readyConditionOf(t, identityReconciler, identityKey)
	if unblocked == nil || unblocked.Status != metav1.ConditionTrue {
		t.Fatalf("expected the identity to become Ready, got %+v", unblocked)
	}
	if unblocked.Reason != reasonValidated {
		t.Errorf("ready reason: got %q, want %q", unblocked.Reason, reasonValidated)
	}
}

func TestAddAuthConfigWithoutCRD(t *testing.T) {
	if err := AddAuthConfig(newTestManager(t)); err != nil {
		t.Fatalf("AddAuthConfig with the CRD absent: %v", err)
	}
}

func TestAuthConfigSetupWithManager(t *testing.T) {
	mgr := newTestManager(t)
	r := &AuthConfigReconciler{Client: mgr.GetClient(), discover: oidc.Discover}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("SetupWithManager: %v", err)
	}
}

// TestAuthConfigResolvesOncePerGeneration counts the outbound requests.
// Discovery reaches somebody else's server, so a settled generation must not
// keep fetching, and an edit must.
func TestAuthConfigResolvesOncePerGeneration(t *testing.T) {
	config := unresolvedConfig("keycloak", "https://keycloak.example.com/.well-known/openid-configuration")
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&securityv1alpha1.AgentAuthenticationConfig{}).
		WithObjects(config.DeepCopy()).Build()
	key := client.ObjectKeyFromObject(config)
	ctx := context.Background()

	var fetches int
	r := &AuthConfigReconciler{Client: fc,
		discover: func(context.Context, string, oidc.Options) (oidc.Discovered, error) {
			fetches++
			return oidc.Discovered{
				Issuer:  "https://keycloak.example.com",
				JWKSURI: "https://keycloak.example.com/keys",
			}, nil
		}}

	for i := 0; i < 4; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if fetches != 1 {
		t.Errorf("an unchanged generation fetched discovery %d times, want 1", fetches)
	}

	// An edit bumps the generation, which has to resolve again.
	got := &securityv1alpha1.AgentAuthenticationConfig{}
	if err := fc.Get(ctx, key, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	got.Generation = 2
	got.Spec.JWT.AllowedAudience = []string{"another-agent"}
	if err := fc.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile after the edit: %v", err)
	}
	if fetches != 2 {
		t.Errorf("a changed generation fetched discovery %d times total, want 2", fetches)
	}
}
