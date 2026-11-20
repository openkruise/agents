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

// Package v1alpha1_test exercises §5.7 of the agent identity and
// user-delegated credentials proposal ("Admission validates structure,
// reference types, URLs, actions and resources, and OAuth2 field
// combinations. It does not perform network calls.") against a real
// Kubernetes API server.
//
// The CEL rules under test live in the sibling *_types.go files as
// +kubebuilder:validation:XValidation markers. The fake client used
// elsewhere in this repo's unit tests does not evaluate CRD schema or CEL
// validation at all, so only a real API server (envtest, here) can prove
// these rules behave as written. A rule that reads correctly can still have
// a one-sided hole that only the server's CEL evaluator catches, which is
// why every pairing below is tested from both directions rather than only
// the direction that is expected to fail.
package v1alpha1_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
)

// admissionClient returns a client talking to a real API server with the
// security CRDs installed from the generated manifests, the same ones a
// cluster is deployed with. There is no controller running: every case here
// is decided by the API server's own schema and CEL evaluation, which is
// exactly what §5.7 attributes to admission rather than to the controller.
func admissionClient(t *testing.T) client.Client {
	t.Helper()

	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run under make test or point it at envtest binaries")
	}
	ctrl.SetLogger(klog.Background())

	scheme := apiruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("register the core scheme: %v", err)
	}
	if err := securityv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("register the security scheme: %v", err)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		Scheme:                scheme,
	}
	restConfig, err := env.Start()
	if err != nil {
		t.Fatalf("start the API server: %v", err)
	}
	t.Cleanup(func() { stopEnvironment(t, env) })

	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}
	return c
}

// stopEnvironment tears down the API server. envtest signals SIGTERM, which
// Windows does not have, so there the processes are killed outright instead
// of being left behind. Mirrors pkg/agentidentity's stopEnvironment helper.
func stopEnvironment(t *testing.T, env *envtest.Environment) {
	t.Helper()
	err := env.Stop()
	if runtime.GOOS != "windows" {
		if err != nil {
			t.Errorf("stop the API server: %v", err)
		}
		return
	}
	for _, image := range []string{"kube-apiserver.exe", "etcd.exe"} {
		_ = exec.Command("taskkill", "/F", "/IM", image).Run()
	}
}

func createTestNamespace(t *testing.T, ctx context.Context, c client.Client, prefix string) string {
	t.Helper()
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: prefix + "-"},
	}
	if err := c.Create(ctx, namespace); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return namespace.Name
}

// wantAdmission asserts that creating obj is accepted when wantErrSubstring
// is empty, or rejected with an error containing wantErrSubstring otherwise.
func wantAdmission(t *testing.T, ctx context.Context, c client.Client, obj client.Object, wantErrSubstring string) {
	t.Helper()
	err := c.Create(ctx, obj)
	if wantErrSubstring == "" {
		if err != nil {
			t.Fatalf("create: got error %v, want none", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("create: got no error, want one containing %q", wantErrSubstring)
	}
	if !apierrors.IsInvalid(err) && !apierrors.IsForbidden(err) {
		t.Fatalf("create: got %v, want a validation error containing %q", err, wantErrSubstring)
	}
	if !strings.Contains(err.Error(), wantErrSubstring) {
		t.Fatalf("create: error %q does not contain %q", err.Error(), wantErrSubstring)
	}
}

// TestAgentAuthenticationConfigAdmission covers §5.7 "structure", "URLs" and
// the type/jwt field combination for AgentAuthenticationConfig (§5.2).
func TestAgentAuthenticationConfigAdmission(t *testing.T) {
	ctx := context.Background()
	c := admissionClient(t)
	namespace := createTestNamespace(t, ctx, c, "aac")

	tests := []struct {
		name    string
		spec    securityv1alpha1.AgentAuthenticationConfigSpec
		wantErr string
	}{
		{
			name: "valid jwt config",
			spec: securityv1alpha1.AgentAuthenticationConfigSpec{
				Type: "jwt",
				JWT: &securityv1alpha1.JWTAuthentication{
					DiscoveryURL:    "https://keycloak.example.com/.well-known/openid-configuration",
					AllowedAudience: []string{"sample-agent"},
				},
			},
		},
		{
			name: "non-https discovery URL is rejected",
			spec: securityv1alpha1.AgentAuthenticationConfigSpec{
				Type: "jwt",
				JWT: &securityv1alpha1.JWTAuthentication{
					DiscoveryURL:    "http://keycloak.example.com/.well-known/openid-configuration",
					AllowedAudience: []string{"sample-agent"},
				},
			},
			wantErr: "discoveryUrl",
		},
		{
			name: "unsupported type is rejected",
			spec: securityv1alpha1.AgentAuthenticationConfigSpec{
				Type: "oidc",
				JWT: &securityv1alpha1.JWTAuthentication{
					DiscoveryURL:    "https://keycloak.example.com/.well-known/openid-configuration",
					AllowedAudience: []string{"sample-agent"},
				},
			},
			wantErr: "Unsupported value",
		},
		{
			name: "empty allowedAudience is rejected",
			spec: securityv1alpha1.AgentAuthenticationConfigSpec{
				Type: "jwt",
				JWT: &securityv1alpha1.JWTAuthentication{
					DiscoveryURL:    "https://keycloak.example.com/.well-known/openid-configuration",
					AllowedAudience: []string{},
				},
			},
			wantErr: "allowedAudience",
		},
		{
			// Gap closed in this change: type=jwt with no jwt block used to be
			// structurally valid. §5.2 treats the jwt block as the
			// configuration for the only supported type, so omitting it is an
			// incomplete object that admission, not the controller, should
			// reject.
			name: "type jwt without a jwt block is rejected",
			spec: securityv1alpha1.AgentAuthenticationConfigSpec{
				Type: "jwt",
			},
			wantErr: "jwt is required when type is jwt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &securityv1alpha1.AgentAuthenticationConfig{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "aac-", Namespace: namespace},
				Spec:       tt.spec,
			}
			wantAdmission(t, ctx, c, obj, tt.wantErr)
		})
	}
}

// TestAgentIdentityAdmission covers §5.7 "structure" and "reference types"
// for AgentIdentity (§5.3).
func TestAgentIdentityAdmission(t *testing.T) {
	ctx := context.Background()
	c := admissionClient(t)
	namespace := createTestNamespace(t, ctx, c, "aid")

	tests := []struct {
		name    string
		spec    securityv1alpha1.AgentIdentitySpec
		wantErr string
	}{
		{
			name: "valid identity",
			spec: securityv1alpha1.AgentIdentitySpec{
				AuthenticationRefs: []securityv1alpha1.AuthenticationConfigReference{{
					APIGroup: "security.agents.kruise.io",
					Kind:     "AgentAuthenticationConfig",
					Name:     "keycloak",
				}},
			},
		},
		{
			name:    "empty authenticationRefs is rejected",
			spec:    securityv1alpha1.AgentIdentitySpec{AuthenticationRefs: []securityv1alpha1.AuthenticationConfigReference{}},
			wantErr: "authenticationRefs",
		},
		{
			name: "wrong apiGroup is rejected",
			spec: securityv1alpha1.AgentIdentitySpec{
				AuthenticationRefs: []securityv1alpha1.AuthenticationConfigReference{{
					APIGroup: "apps",
					Kind:     "AgentAuthenticationConfig",
					Name:     "keycloak",
				}},
			},
			wantErr: "Unsupported value",
		},
		{
			name: "wrong kind is rejected",
			spec: securityv1alpha1.AgentIdentitySpec{
				AuthenticationRefs: []securityv1alpha1.AuthenticationConfigReference{{
					APIGroup: "security.agents.kruise.io",
					Kind:     "ConfigMap",
					Name:     "keycloak",
				}},
			},
			wantErr: "Unsupported value",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &securityv1alpha1.AgentIdentity{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "aid-", Namespace: namespace},
				Spec:       tt.spec,
			}
			wantAdmission(t, ctx, c, obj, tt.wantErr)
		})
	}
}

// TestAgentRoleAdmission covers §5.7 "actions and resources" for AgentRole
// (§5.4): each action is pinned to the resource kind it is allowed to name,
// CredentialProvider resources may never be a wildcard, and a rule naming
// both actions together can never be satisfied because no resource value
// matches both kind constraints at once.
func TestAgentRoleAdmission(t *testing.T) {
	ctx := context.Background()
	c := admissionClient(t)
	namespace := createTestNamespace(t, ctx, c, "arole")

	tests := []struct {
		name    string
		rules   []securityv1alpha1.AgentRoleRule
		wantErr string
	}{
		{
			name: "ExchangePrincipalToken with wildcard resource",
			rules: []securityv1alpha1.AgentRoleRule{{
				Actions:   []string{"ExchangePrincipalToken"},
				Resources: []string{"*"},
			}},
		},
		{
			name: "ExchangePrincipalToken with a named AgentIdentity resource",
			rules: []securityv1alpha1.AgentRoleRule{{
				Actions:   []string{"ExchangePrincipalToken"},
				Resources: []string{"AgentIdentity/checkout-agent"},
			}},
		},
		{
			name: "GetResourceOAuth2Token with a named CredentialProvider resource",
			rules: []securityv1alpha1.AgentRoleRule{{
				Actions:   []string{"GetResourceOAuth2Token"},
				Resources: []string{"CredentialProvider/github-3lo"},
			}},
		},
		{
			name: "GetResourceOAuth2Token with an AgentIdentity resource is rejected",
			rules: []securityv1alpha1.AgentRoleRule{{
				Actions:   []string{"GetResourceOAuth2Token"},
				Resources: []string{"AgentIdentity/checkout-agent"},
			}},
			wantErr: "GetResourceOAuth2Token resources must be CredentialProvider/<name>",
		},
		{
			name: "GetResourceOAuth2Token with a wildcard resource is rejected",
			rules: []securityv1alpha1.AgentRoleRule{{
				Actions:   []string{"GetResourceOAuth2Token"},
				Resources: []string{"*"},
			}},
			wantErr: "GetResourceOAuth2Token resources must be CredentialProvider/<name>",
		},
		{
			name: "ExchangePrincipalToken with a CredentialProvider resource is rejected",
			rules: []securityv1alpha1.AgentRoleRule{{
				Actions:   []string{"ExchangePrincipalToken"},
				Resources: []string{"CredentialProvider/github-3lo"},
			}},
			wantErr: "ExchangePrincipalToken resources must be * or AgentIdentity/<name>",
		},
		{
			name: "both actions in one rule can never be satisfied",
			rules: []securityv1alpha1.AgentRoleRule{{
				Actions:   []string{"ExchangePrincipalToken", "GetResourceOAuth2Token"},
				Resources: []string{"*"},
			}},
			wantErr: "GetResourceOAuth2Token resources must be CredentialProvider/<name>",
		},
		{
			name: "unknown action is rejected",
			rules: []securityv1alpha1.AgentRoleRule{{
				Actions:   []string{"DeleteEverything"},
				Resources: []string{"*"},
			}},
			wantErr: "Unsupported value",
		},
		{
			name:    "empty rules is rejected",
			rules:   []securityv1alpha1.AgentRoleRule{},
			wantErr: "rules",
		},
		{
			name: "a resource naming an unknown kind is rejected",
			rules: []securityv1alpha1.AgentRoleRule{{
				Actions:   []string{"ExchangePrincipalToken"},
				Resources: []string{"Secret/github-oauth-secret"},
			}},
			wantErr: "Invalid value",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &securityv1alpha1.AgentRole{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "arole-", Namespace: namespace},
				Spec:       securityv1alpha1.AgentRoleSpec{Rules: tt.rules},
			}
			wantAdmission(t, ctx, c, obj, tt.wantErr)
		})
	}
}

// TestAgentRoleBindingAdmission covers §5.7 "reference types" and the
// immutability of both reference fields for AgentRoleBinding (§5.5).
func TestAgentRoleBindingAdmission(t *testing.T) {
	ctx := context.Background()
	c := admissionClient(t)
	namespace := createTestNamespace(t, ctx, c, "arb")

	t.Run("valid binding is accepted", func(t *testing.T) {
		obj := &securityv1alpha1.AgentRoleBinding{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "arb-", Namespace: namespace},
			Spec: securityv1alpha1.AgentRoleBindingSpec{
				AgentRoleRef:     securityv1alpha1.AgentRoleReference{APIGroup: "security.agents.kruise.io", Kind: "AgentRole", Name: "sample-agent-role"},
				AgentIdentityRef: securityv1alpha1.AgentIdentityReference{Name: "sample-agent"},
			},
		}
		wantAdmission(t, ctx, c, obj, "")
	})

	t.Run("wrong agentRoleRef kind is rejected", func(t *testing.T) {
		obj := &securityv1alpha1.AgentRoleBinding{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "arb-", Namespace: namespace},
			Spec: securityv1alpha1.AgentRoleBindingSpec{
				AgentRoleRef:     securityv1alpha1.AgentRoleReference{APIGroup: "security.agents.kruise.io", Kind: "ClusterRole", Name: "sample-agent-role"},
				AgentIdentityRef: securityv1alpha1.AgentIdentityReference{Name: "sample-agent"},
			},
		}
		wantAdmission(t, ctx, c, obj, "Unsupported value")
	})

	t.Run("agentRoleRef is immutable", func(t *testing.T) {
		obj := &securityv1alpha1.AgentRoleBinding{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "arb-", Namespace: namespace},
			Spec: securityv1alpha1.AgentRoleBindingSpec{
				AgentRoleRef:     securityv1alpha1.AgentRoleReference{APIGroup: "security.agents.kruise.io", Kind: "AgentRole", Name: "sample-agent-role"},
				AgentIdentityRef: securityv1alpha1.AgentIdentityReference{Name: "sample-agent"},
			},
		}
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("create: %v", err)
		}
		obj.Spec.AgentRoleRef.Name = "a-different-role"
		err := c.Update(ctx, obj)
		if err == nil || !strings.Contains(err.Error(), "agentRoleRef is immutable") {
			t.Fatalf("update agentRoleRef.name: got %v, want an error containing %q", err, "agentRoleRef is immutable")
		}
	})

	t.Run("agentIdentityRef is immutable", func(t *testing.T) {
		obj := &securityv1alpha1.AgentRoleBinding{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "arb-", Namespace: namespace},
			Spec: securityv1alpha1.AgentRoleBindingSpec{
				AgentRoleRef:     securityv1alpha1.AgentRoleReference{APIGroup: "security.agents.kruise.io", Kind: "AgentRole", Name: "sample-agent-role"},
				AgentIdentityRef: securityv1alpha1.AgentIdentityReference{Name: "sample-agent"},
			},
		}
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("create: %v", err)
		}
		obj.Spec.AgentIdentityRef.Name = "a-different-agent"
		err := c.Update(ctx, obj)
		if err == nil || !strings.Contains(err.Error(), "agentIdentityRef is immutable") {
			t.Fatalf("update agentIdentityRef.name: got %v, want an error containing %q", err, "agentIdentityRef is immutable")
		}
	})

	t.Run("a status-only update is not blocked by the immutability rules", func(t *testing.T) {
		obj := &securityv1alpha1.AgentRoleBinding{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "arb-", Namespace: namespace},
			Spec: securityv1alpha1.AgentRoleBindingSpec{
				AgentRoleRef:     securityv1alpha1.AgentRoleReference{APIGroup: "security.agents.kruise.io", Kind: "AgentRole", Name: "sample-agent-role"},
				AgentIdentityRef: securityv1alpha1.AgentIdentityReference{Name: "sample-agent"},
			},
		}
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("create: %v", err)
		}
		obj.Status.Conditions = []metav1.Condition{{
			Type: securityv1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: "Validated", Message: "ok", LastTransitionTime: metav1.Now(),
		}}
		if err := c.Status().Update(ctx, obj); err != nil {
			t.Fatalf("status update with spec references unchanged: got %v, want none", err)
		}
	})
}

// TestCredentialProviderAdmission covers §5.7 "structure", "URLs", "actions
// and resources" (indirectly, via AgentRole) and "OAuth2 field combinations"
// for CredentialProvider (§5.6). The callbackBaseUrl cases in particular are
// the kind of one-sided CEL/pattern hole the real API server, and not the
// fake client, is needed to catch.
func TestCredentialProviderAdmission(t *testing.T) {
	ctx := context.Background()
	c := admissionClient(t)
	namespace := createTestNamespace(t, ctx, c, "acp")

	validOAuth2 := func() securityv1alpha1.OAuth2Provider {
		return securityv1alpha1.OAuth2Provider{
			GrantType: "userFederation",
			Discovery: securityv1alpha1.OAuth2Discovery{
				Issuer:                "https://github.com",
				AuthorizationEndpoint: "https://github.com/login/oauth/authorize",
				TokenEndpoint:         "https://github.com/login/oauth/access_token",
			},
			Client: securityv1alpha1.OAuth2Client{
				ClientID: "client-id",
				ClientSecret: securityv1alpha1.SecretKeyReference{
					SecretKeyRef: securityv1alpha1.SecretKeySelector{Name: "github-oauth-secret", Key: "client-secret"},
				},
			},
			UserFederation: &securityv1alpha1.OAuth2UserFederation{
				CallbackBaseURL: "https://identity.example.com",
				DefaultScopes:   []string{"repo", "read:user"},
			},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*securityv1alpha1.CredentialProviderSpec)
		wantErr string
	}{
		{
			name:   "valid provider",
			mutate: func(s *securityv1alpha1.CredentialProviderSpec) {},
		},
		{
			name: "valid provider with a callback path and no trailing slash",
			mutate: func(s *securityv1alpha1.CredentialProviderSpec) {
				s.OAuth2.UserFederation.CallbackBaseURL = "https://identity.example.com/agents"
			},
		},
		{
			name: "type oauth2 without an oauth2 block is rejected",
			mutate: func(s *securityv1alpha1.CredentialProviderSpec) {
				s.OAuth2 = nil
			},
			wantErr: "oauth2 is required when type is oauth2",
		},
		{
			name: "grantType userFederation without a userFederation block is rejected",
			mutate: func(s *securityv1alpha1.CredentialProviderSpec) {
				s.OAuth2.UserFederation = nil
			},
			wantErr: "userFederation is required when grantType is userFederation",
		},
		{
			name: "unsupported type is rejected",
			mutate: func(s *securityv1alpha1.CredentialProviderSpec) {
				s.Type = "basic"
			},
			wantErr: "Unsupported value",
		},
		{
			name: "non-https discovery issuer is rejected",
			mutate: func(s *securityv1alpha1.CredentialProviderSpec) {
				s.OAuth2.Discovery.Issuer = "http://github.com"
			},
			wantErr: "issuer",
		},
		{
			name: "non-https authorization endpoint is rejected",
			mutate: func(s *securityv1alpha1.CredentialProviderSpec) {
				s.OAuth2.Discovery.AuthorizationEndpoint = "http://github.com/login/oauth/authorize"
			},
			wantErr: "authorizationEndpoint",
		},
		{
			name: "non-https token endpoint is rejected",
			mutate: func(s *securityv1alpha1.CredentialProviderSpec) {
				s.OAuth2.Discovery.TokenEndpoint = "http://github.com/login/oauth/access_token"
			},
			wantErr: "tokenEndpoint",
		},
		{
			name: "a callback URL with a query string is rejected",
			mutate: func(s *securityv1alpha1.CredentialProviderSpec) {
				s.OAuth2.UserFederation.CallbackBaseURL = "https://identity.example.com/oauth2/callback?x=1"
			},
			wantErr: "callbackBaseUrl",
		},
		{
			name: "a callback URL with a fragment is rejected",
			mutate: func(s *securityv1alpha1.CredentialProviderSpec) {
				s.OAuth2.UserFederation.CallbackBaseURL = "https://identity.example.com/callback#frag"
			},
			wantErr: "callbackBaseUrl",
		},
		{
			name: "a callback URL carrying user information is rejected",
			mutate: func(s *securityv1alpha1.CredentialProviderSpec) {
				s.OAuth2.UserFederation.CallbackBaseURL = "https://attacker:secret@identity.example.com"
			},
			wantErr: "callbackBaseUrl",
		},
		{
			name: "a non-https callback URL is rejected",
			mutate: func(s *securityv1alpha1.CredentialProviderSpec) {
				s.OAuth2.UserFederation.CallbackBaseURL = "http://identity.example.com"
			},
			wantErr: "callbackBaseUrl",
		},
		{
			name: "empty defaultScopes is rejected",
			mutate: func(s *securityv1alpha1.CredentialProviderSpec) {
				s.OAuth2.UserFederation.DefaultScopes = []string{}
			},
			wantErr: "defaultScopes",
		},
		{
			name: "unsupported grantType is rejected",
			mutate: func(s *securityv1alpha1.CredentialProviderSpec) {
				s.OAuth2.GrantType = "clientCredentials"
			},
			wantErr: "Unsupported value",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oauth2 := validOAuth2()
			spec := securityv1alpha1.CredentialProviderSpec{Type: "oauth2", OAuth2: &oauth2}
			tt.mutate(&spec)
			obj := &securityv1alpha1.CredentialProvider{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "acp-", Namespace: namespace},
				Spec:       spec,
			}
			wantAdmission(t, ctx, c, obj, tt.wantErr)
		})
	}
}
