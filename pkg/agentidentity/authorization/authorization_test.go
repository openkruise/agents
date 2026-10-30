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

package authorization

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
)

const testNamespace = "team-a"

var scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(securityv1alpha1.AddToScheme(s))
	return s
}()

// role builds an AgentRole from (actions, resources) pairs.
func role(name string, rules ...securityv1alpha1.AgentRoleRule) *securityv1alpha1.AgentRole {
	return &securityv1alpha1.AgentRole{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       securityv1alpha1.AgentRoleSpec{Rules: rules},
	}
}

func rule(action string, resources ...string) securityv1alpha1.AgentRoleRule {
	return securityv1alpha1.AgentRoleRule{Actions: []string{action}, Resources: resources}
}

// binding attaches a role to an identity.
func binding(name, roleName, identity string) *securityv1alpha1.AgentRoleBinding {
	return &securityv1alpha1.AgentRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: securityv1alpha1.AgentRoleBindingSpec{
			AgentRoleRef: securityv1alpha1.AgentRoleReference{
				APIGroup: "security.agents.kruise.io",
				Kind:     "AgentRole",
				Name:     roleName,
			},
			AgentIdentityRef: securityv1alpha1.AgentIdentityReference{Name: identity},
		},
	}
}

// deleting marks an object as having deletion requested. A finalizer is
// required, since the API server would otherwise have removed it already.
func deleting[T client.Object](obj T) T {
	now := metav1.Now()
	obj.SetDeletionTimestamp(&now)
	obj.SetFinalizers([]string{"agents.kruise.io/test"})
	return obj
}

func authorizerFor(objs ...client.Object) *Authorizer {
	return New(fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build())
}

func exchangeRequest(identity, resource string) Request {
	return Request{
		Namespace:     testNamespace,
		AgentIdentity: identity,
		Action:        securityv1alpha1.ActionExchangePrincipalToken,
		Resource:      resource,
	}
}

func credentialRequest(identity, resource string) Request {
	return Request{
		Namespace:     testNamespace,
		AgentIdentity: identity,
		Action:        securityv1alpha1.ActionGetResourceOAuth2Token,
		Resource:      resource,
	}
}

func TestAuthorize(t *testing.T) {
	tests := []struct {
		name       string
		objects    []client.Object
		request    Request
		wantAllow  bool
		wantReason string
	}{
		{
			// §5.4's own example: ExchangePrincipalToken on "*".
			name: "a wildcard grants exchange for any identity",
			objects: []client.Object{
				role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "*")),
				binding("b", "r", "sample-agent"),
			},
			request:    exchangeRequest("sample-agent", "AgentIdentity/sample-agent"),
			wantAllow:  true,
			wantReason: ReasonAllowed,
		},
		{
			name: "a named identity grants exchange for itself",
			objects: []client.Object{
				role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "AgentIdentity/sample-agent")),
				binding("b", "r", "sample-agent"),
			},
			request:    exchangeRequest("sample-agent", "AgentIdentity/sample-agent"),
			wantAllow:  true,
			wantReason: ReasonAllowed,
		},
		{
			name: "a named identity does not grant exchange for another",
			objects: []client.Object{
				role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "AgentIdentity/sample-agent")),
				binding("b", "r", "sample-agent"),
			},
			request:    exchangeRequest("sample-agent", "AgentIdentity/other-agent"),
			wantReason: ReasonNoMatchingRule,
		},
		{
			name:       "nothing bound denies everything",
			objects:    []client.Object{role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "*"))},
			request:    exchangeRequest("sample-agent", "AgentIdentity/sample-agent"),
			wantReason: ReasonNoBindings,
		},
		{
			name: "a binding for another identity grants nothing",
			objects: []client.Object{
				role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "*")),
				binding("b", "r", "other-agent"),
			},
			request:    exchangeRequest("sample-agent", "AgentIdentity/sample-agent"),
			wantReason: ReasonNoBindings,
		},
		{
			// A deleted binding must stop granting at once, which is why
			// authorization reads live state rather than a cached verdict.
			name: "a deleting binding stops granting",
			objects: []client.Object{
				role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "*")),
				deleting(binding("b", "r", "sample-agent")),
			},
			request:    exchangeRequest("sample-agent", "AgentIdentity/sample-agent"),
			wantReason: ReasonNoBindings,
		},
		{
			name: "a deleting role stops granting",
			objects: []client.Object{
				deleting(role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "*"))),
				binding("b", "r", "sample-agent"),
			},
			request:    exchangeRequest("sample-agent", "AgentIdentity/sample-agent"),
			wantReason: ReasonNoMatchingRule,
		},
		{
			// §5.7: deleting a role does not cascade to its bindings, so a
			// binding naming an absent role has to grant nothing.
			name:       "a binding naming an absent role grants nothing",
			objects:    []client.Object{binding("b", "missing-role", "sample-agent")},
			request:    exchangeRequest("sample-agent", "AgentIdentity/sample-agent"),
			wantReason: ReasonNoMatchingRule,
		},
		{
			// §5.5: several bindings combine, so a request only one of them
			// covers is still allowed.
			name: "rules from several bindings combine",
			objects: []client.Object{
				role("exchange", rule(securityv1alpha1.ActionExchangePrincipalToken, "*")),
				role("credential", rule(securityv1alpha1.ActionGetResourceOAuth2Token, "CredentialProvider/github-3lo")),
				binding("b1", "exchange", "sample-agent"),
				binding("b2", "credential", "sample-agent"),
			},
			request:    credentialRequest("sample-agent", "CredentialProvider/github-3lo"),
			wantAllow:  true,
			wantReason: ReasonAllowed,
		},
		{
			name: "one absent role does not stop another from granting",
			objects: []client.Object{
				role("present", rule(securityv1alpha1.ActionExchangePrincipalToken, "*")),
				binding("b1", "missing-role", "sample-agent"),
				binding("b2", "present", "sample-agent"),
			},
			request:    exchangeRequest("sample-agent", "AgentIdentity/sample-agent"),
			wantAllow:  true,
			wantReason: ReasonAllowed,
		},
		{
			name: "a provider grants only itself",
			objects: []client.Object{
				role("r", rule(securityv1alpha1.ActionGetResourceOAuth2Token, "CredentialProvider/github-3lo")),
				binding("b", "r", "sample-agent"),
			},
			request:    credentialRequest("sample-agent", "CredentialProvider/gitlab"),
			wantReason: ReasonNoMatchingRule,
		},
		{
			name: "an action the role does not list is denied",
			objects: []client.Object{
				role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "*")),
				binding("b", "r", "sample-agent"),
			},
			request:    credentialRequest("sample-agent", "CredentialProvider/github-3lo"),
			wantReason: ReasonNoMatchingRule,
		},
		{
			name: "an action this release does not define is denied",
			objects: []client.Object{
				role("r", rule("DeleteEverything", "*")),
				binding("b", "r", "sample-agent"),
			},
			request: Request{
				Namespace:     testNamespace,
				AgentIdentity: "sample-agent",
				Action:        "DeleteEverything",
				Resource:      "AgentIdentity/sample-agent",
			},
			wantReason: ReasonUnknownAction,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := authorizerFor(tt.objects...).Authorize(context.Background(), tt.request)
			if err != nil {
				t.Fatalf("Authorize: %v", err)
			}
			if got.Allowed != tt.wantAllow {
				t.Errorf("allowed: got %v, want %v (reason %q)", got.Allowed, tt.wantAllow, got.Reason)
			}
			if got.Reason != tt.wantReason {
				t.Errorf("reason: got %q, want %q", got.Reason, tt.wantReason)
			}
		})
	}
}

// TestAuthorizeFailsClosedOnRolesThatPredateAdmission is the defence-in-depth
// case.
//
// Admission now rejects a role that pairs an action with the wrong resource
// kind, or uses a wildcard where §5.4 forbids one. A role created before those
// constraints is still in the cluster and still readable, so the evaluator
// enforces the pairing itself rather than trusting that admission saw it. This
// is the same reasoning as an AgentIdentity carrying an empty
// authenticationRefs: admission closes the door going forward, the consumer
// still fails closed for what came before.
func TestAuthorizeFailsClosedOnRolesThatPredateAdmission(t *testing.T) {
	tests := []struct {
		name     string
		resource string
		request  Request
	}{
		{
			// The hole caught in review on the CRD: a wildcard here would grant
			// every provider in the namespace, including ones added later.
			name:     "a wildcard never grants a credential action",
			resource: "*",
			request:  credentialRequest("sample-agent", "CredentialProvider/github-3lo"),
		},
		{
			name:     "a bare name without its kind grants nothing",
			resource: "github-3lo",
			request:  credentialRequest("sample-agent", "CredentialProvider/github-3lo"),
		},
		{
			name:     "a provider entry does not grant an exchange",
			resource: "CredentialProvider/github-3lo",
			request:  exchangeRequest("sample-agent", "AgentIdentity/sample-agent"),
		},
		{
			name:     "an identity entry does not grant a credential action",
			resource: "AgentIdentity/sample-agent",
			request:  credentialRequest("sample-agent", "CredentialProvider/github-3lo"),
		},
		{
			name:     "a wildcard inside the name position grants nothing",
			resource: "CredentialProvider/*",
			request:  credentialRequest("sample-agent", "CredentialProvider/github-3lo"),
		},
		// The cases above all vary the rule's entry while the request stays a
		// well formed resource of the action's own kind. These vary the request
		// instead, against the one entry that is not compared against it.
		// A wildcard means every resource of one kind in the namespace, so it
		// must not answer for a request naming anything else.
		{
			name:     "a wildcard exchange grant does not reach a credential provider",
			resource: "*",
			request:  exchangeRequest("sample-agent", "CredentialProvider/github-3lo"),
		},
		{
			name:     "a wildcard exchange grant does not reach an unrelated kind",
			resource: "*",
			request:  exchangeRequest("sample-agent", "Secret/github-oauth"),
		},
		{
			name:     "a wildcard exchange grant does not reach a bare name",
			resource: "*",
			request:  exchangeRequest("sample-agent", "sample-agent"),
		},
		{
			name:     "a wildcard exchange grant does not answer a wildcard request",
			resource: "*",
			request:  exchangeRequest("sample-agent", "*"),
		},
		{
			name:     "a wildcard exchange grant does not answer a wildcard name",
			resource: "*",
			request:  exchangeRequest("sample-agent", "AgentIdentity/*"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []client.Object{
				role("r", rule(tt.request.Action, tt.resource)),
				binding("b", "r", "sample-agent"),
			}
			got, err := authorizerFor(objs...).Authorize(context.Background(), tt.request)
			if err != nil {
				t.Fatalf("Authorize: %v", err)
			}
			if got.Allowed {
				t.Errorf("a role predating admission granted %s on %s via entry %q",
					tt.request.Action, tt.request.Resource, tt.resource)
			}
		})
	}
}

// TestAuthorizeNeverCrossesNamespaces checks the boundary §5.1 draws. A role
// and binding in another namespace must not grant anything here, however the
// names line up.
func TestAuthorizeNeverCrossesNamespaces(t *testing.T) {
	elsewhere := role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "*"))
	elsewhere.Namespace = "team-b"
	elsewhereBinding := binding("b", "r", "sample-agent")
	elsewhereBinding.Namespace = "team-b"

	got, err := authorizerFor(elsewhere, elsewhereBinding).
		Authorize(context.Background(), exchangeRequest("sample-agent", "AgentIdentity/sample-agent"))
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got.Allowed {
		t.Error("a binding in another namespace granted a request in this one")
	}
	if got.Reason != ReasonNoBindings {
		t.Errorf("reason: got %q, want %q", got.Reason, ReasonNoBindings)
	}
}

// TestAuthorizeSurfacesReadFailures covers the difference between "not
// permitted" and "could not tell". A caller must refuse on the second rather
// than treating it as a denial, since it may succeed on retry.
func TestAuthorizeSurfacesReadFailures(t *testing.T) {
	errBoom := errors.New("boom")

	tests := []struct {
		name  string
		funcs interceptor.Funcs
	}{
		{
			name: "listing bindings fails",
			funcs: interceptor.Funcs{
				List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList,
					_ ...client.ListOption) error {
					return errBoom
				},
			},
		},
		{
			name: "getting the role fails for a reason other than absence",
			funcs: interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
					obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*securityv1alpha1.AgentRole); ok {
						return errBoom
					}
					return c.Get(ctx, key, obj, opts...)
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(
					role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "*")),
					binding("b", "r", "sample-agent"),
				).
				WithInterceptorFuncs(tt.funcs).Build()

			got, err := New(c).Authorize(context.Background(),
				exchangeRequest("sample-agent", "AgentIdentity/sample-agent"))
			if !errors.Is(err, errBoom) {
				t.Fatalf("error: got %v, want %v", err, errBoom)
			}
			if got.Allowed {
				t.Error("a failed read must never produce an allow")
			}
		})
	}
}

func TestAuthorizeRejectsIncompleteRequests(t *testing.T) {
	complete := exchangeRequest("sample-agent", "AgentIdentity/sample-agent")

	tests := []struct {
		name   string
		mutate func(*Request)
		want   string
	}{
		{"namespace", func(r *Request) { r.Namespace = "" }, "namespace must not be empty"},
		{"identity", func(r *Request) { r.AgentIdentity = "" }, "agent identity must not be empty"},
		{"action", func(r *Request) { r.Action = "" }, "action must not be empty"},
		{"resource", func(r *Request) { r.Resource = "" }, "resource must not be empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := complete
			tt.mutate(&req)

			got, err := authorizerFor().Authorize(context.Background(), req)
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tt.want)
			}
			if got.Allowed {
				t.Error("an incomplete request must never produce an allow")
			}
		})
	}
}

func TestDecisionString(t *testing.T) {
	if got := (Decision{Allowed: true, Reason: ReasonAllowed}).String(); got != "allowed(Allowed)" {
		t.Errorf("got %q", got)
	}
	if got := (Decision{Reason: ReasonNoBindings}).String(); got != "denied(NoBindings)" {
		t.Errorf("got %q", got)
	}
}

// TestAuthorizeReadsADuplicatedRoleOnce covers the case where two bindings name
// the same role for the same identity. The decision is unchanged, but
// authorization runs on every request, so the role is fetched once rather than
// once per binding.
func TestAuthorizeReadsADuplicatedRoleOnce(t *testing.T) {
	var roleGets int
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(
			role("r", rule(securityv1alpha1.ActionGetResourceOAuth2Token, "CredentialProvider/gitlab")),
			binding("b1", "r", "sample-agent"),
			binding("b2", "r", "sample-agent"),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*securityv1alpha1.AgentRole); ok {
					roleGets++
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()

	// Denied, so every bound role is consulted rather than short-circuiting on
	// the first match.
	got, err := New(c).Authorize(context.Background(),
		credentialRequest("sample-agent", "CredentialProvider/github-3lo"))
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got.Allowed {
		t.Fatalf("expected a denial, got %s", got)
	}
	if roleGets != 1 {
		t.Errorf("the duplicated role was fetched %d times, want 1", roleGets)
	}
}
