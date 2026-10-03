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
	"testing"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
)

const testNamespace = "team-a"

// scheme carries only this group, which is everything the controller reads.
var scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(securityv1alpha1.AddToScheme(s))
	return s
}()

// identity builds an AgentIdentity referencing the named AgentAuthenticationConfigs.
func identity(name string, generation int64, refNames ...string) *securityv1alpha1.AgentIdentity {
	refs := make([]securityv1alpha1.AuthenticationConfigReference, 0, len(refNames))
	for _, refName := range refNames {
		refs = append(refs, securityv1alpha1.AuthenticationConfigReference{
			APIGroup: "security.agents.kruise.io",
			Kind:     "AgentAuthenticationConfig",
			Name:     refName,
		})
	}
	return &securityv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  testNamespace,
			Generation: generation,
		},
		Spec: securityv1alpha1.AgentIdentitySpec{AuthenticationRefs: refs},
	}
}

// authConfig builds an AgentAuthenticationConfig. An empty ready leaves the
// status empty, which is what a config looks like before its controller has
// observed it: present in the API, not yet known to be usable.
func authConfig(namespace, name string, ready metav1.ConditionStatus) *securityv1alpha1.AgentAuthenticationConfig {
	config := &securityv1alpha1.AgentAuthenticationConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	if ready == "" {
		return config
	}
	config.Status.Conditions = []metav1.Condition{{
		Type:               securityv1alpha1.ConditionReady,
		Status:             ready,
		Reason:             "Test",
		LastTransitionTime: metav1.Now(),
	}}
	return config
}

func TestReconcile(t *testing.T) {
	deleting := identity("deleting-agent", 1, "keycloak")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	// The fake client refuses an object carrying a deletionTimestamp without a
	// finalizer, since the real API server would have removed it already.
	deleting.Finalizers = []string{"agents.kruise.io/test"}

	tests := []struct {
		name string
		// identity is the object reconciled; its name drives the request even
		// when skipCreate leaves it out of the cluster.
		identity   *securityv1alpha1.AgentIdentity
		skipCreate bool
		configs    []client.Object
		// wantNoCondition expects the Ready condition to be absent, meaning the
		// reconcile deliberately wrote nothing.
		wantNoCondition bool
		wantStatus      metav1.ConditionStatus
		wantReason      string
		wantObservedGen int64
	}{
		{
			name:            "no references is ready",
			identity:        identity("standalone-agent", 3),
			wantStatus:      metav1.ConditionTrue,
			wantReason:      reasonValidated,
			wantObservedGen: 3,
		},
		{
			name:            "reference to a ready config is ready",
			identity:        identity("sample-agent", 1, "keycloak"),
			configs:         []client.Object{authConfig(testNamespace, "keycloak", metav1.ConditionTrue)},
			wantStatus:      metav1.ConditionTrue,
			wantReason:      reasonValidated,
			wantObservedGen: 1,
		},
		{
			name:            "every reference must be ready",
			identity:        identity("sample-agent", 1, "keycloak", "okta"),
			configs:         []client.Object{authConfig(testNamespace, "keycloak", metav1.ConditionTrue), authConfig(testNamespace, "okta", metav1.ConditionTrue)},
			wantStatus:      metav1.ConditionTrue,
			wantReason:      reasonValidated,
			wantObservedGen: 1,
		},
		{
			name:            "missing config is not ready",
			identity:        identity("sample-agent", 1, "keycloak"),
			wantStatus:      metav1.ConditionFalse,
			wantReason:      reasonAuthenticationConfigNotFound,
			wantObservedGen: 1,
		},
		{
			name:            "config in another namespace does not satisfy the reference",
			identity:        identity("sample-agent", 1, "keycloak"),
			configs:         []client.Object{authConfig("team-b", "keycloak", metav1.ConditionTrue)},
			wantStatus:      metav1.ConditionFalse,
			wantReason:      reasonAuthenticationConfigNotFound,
			wantObservedGen: 1,
		},
		{
			name:            "config reporting not ready is not ready",
			identity:        identity("sample-agent", 1, "keycloak"),
			configs:         []client.Object{authConfig(testNamespace, "keycloak", metav1.ConditionFalse)},
			wantStatus:      metav1.ConditionFalse,
			wantReason:      reasonAuthenticationConfigNotReady,
			wantObservedGen: 1,
		},
		{
			name:            "config with no observed status yet is not ready",
			identity:        identity("sample-agent", 1, "keycloak"),
			configs:         []client.Object{authConfig(testNamespace, "keycloak", "")},
			wantStatus:      metav1.ConditionFalse,
			wantReason:      reasonAuthenticationConfigNotReady,
			wantObservedGen: 1,
		},
		{
			name:            "one unready reference among ready ones is not ready",
			identity:        identity("sample-agent", 1, "keycloak", "okta"),
			configs:         []client.Object{authConfig(testNamespace, "keycloak", metav1.ConditionTrue), authConfig(testNamespace, "okta", metav1.ConditionFalse)},
			wantStatus:      metav1.ConditionFalse,
			wantReason:      reasonAuthenticationConfigNotReady,
			wantObservedGen: 1,
		},
		{
			name:            "deleting identity is left alone",
			identity:        deleting,
			configs:         []client.Object{authConfig(testNamespace, "keycloak", metav1.ConditionTrue)},
			wantNoCondition: true,
		},
		{
			name:            "missing identity is not an error",
			identity:        identity("gone-agent", 1),
			skipCreate:      true,
			wantNoCondition: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&securityv1alpha1.AgentIdentity{}).
				WithObjects(tt.configs...)
			if !tt.skipCreate {
				builder = builder.WithObjects(tt.identity.DeepCopy())
			}
			r := &Reconciler{Client: builder.Build()}

			key := client.ObjectKeyFromObject(tt.identity)
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("Reconcile returned an error: %v", err)
			}

			if tt.skipCreate {
				return
			}

			got := &securityv1alpha1.AgentIdentity{}
			if err := r.Get(context.Background(), key, got); err != nil {
				t.Fatalf("getting the reconciled identity: %v", err)
			}

			condition := apiMeta.FindStatusCondition(got.Status.Conditions, securityv1alpha1.ConditionReady)
			if tt.wantNoCondition {
				if condition != nil {
					t.Fatalf("expected no Ready condition, got %+v", condition)
				}
				return
			}
			if condition == nil {
				t.Fatal("expected a Ready condition, got none")
			}
			if condition.Status != tt.wantStatus {
				t.Errorf("Ready status: got %q, want %q", condition.Status, tt.wantStatus)
			}
			if condition.Reason != tt.wantReason {
				t.Errorf("Ready reason: got %q, want %q", condition.Reason, tt.wantReason)
			}
			if condition.Message == "" {
				t.Error("Ready condition carries no message")
			}
			if condition.ObservedGeneration != tt.wantObservedGen {
				t.Errorf("condition observedGeneration: got %d, want %d",
					condition.ObservedGeneration, tt.wantObservedGen)
			}
			if got.Status.ObservedGeneration != tt.wantObservedGen {
				t.Errorf("status observedGeneration: got %d, want %d",
					got.Status.ObservedGeneration, tt.wantObservedGen)
			}
		})
	}
}

// TestReconcileIsIdempotent guards the no-op path: a second reconcile over
// unchanged state must not rewrite the status, since every write wakes every
// watcher of the resource.
func TestReconcileIsIdempotent(t *testing.T) {

	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&securityv1alpha1.AgentIdentity{}).
		WithObjects(identity("sample-agent", 1, "keycloak"),
			authConfig(testNamespace, "keycloak", metav1.ConditionTrue)).
		Build()
	r := &Reconciler{Client: fc}
	key := types.NamespacedName{Namespace: testNamespace, Name: "sample-agent"}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	first := &securityv1alpha1.AgentIdentity{}
	if err := r.Get(context.Background(), key, first); err != nil {
		t.Fatalf("getting the identity after the first reconcile: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	second := &securityv1alpha1.AgentIdentity{}
	if err := r.Get(context.Background(), key, second); err != nil {
		t.Fatalf("getting the identity after the second reconcile: %v", err)
	}

	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("status was rewritten on an unchanged reconcile: %q then %q",
			first.ResourceVersion, second.ResourceVersion)
	}
}

func TestIdentitiesForAuthConfig(t *testing.T) {
	tests := []struct {
		name       string
		identities []client.Object
		config     *securityv1alpha1.AgentAuthenticationConfig
		want       []string
	}{
		{
			name:       "referencing identity is enqueued",
			identities: []client.Object{identity("sample-agent", 1, "keycloak")},
			config:     authConfig(testNamespace, "keycloak", metav1.ConditionTrue),
			want:       []string{"sample-agent"},
		},
		{
			name: "every referencing identity is enqueued",
			identities: []client.Object{
				identity("sample-agent", 1, "keycloak"),
				identity("other-agent", 1, "keycloak"),
			},
			config: authConfig(testNamespace, "keycloak", metav1.ConditionTrue),
			want:   []string{"other-agent", "sample-agent"},
		},
		{
			name:       "identity referencing a different config is not enqueued",
			identities: []client.Object{identity("sample-agent", 1, "okta")},
			config:     authConfig(testNamespace, "keycloak", metav1.ConditionTrue),
			want:       nil,
		},
		{
			name:       "identity with no references is not enqueued",
			identities: []client.Object{identity("standalone-agent", 1)},
			config:     authConfig(testNamespace, "keycloak", metav1.ConditionTrue),
			want:       nil,
		},
		{
			name:       "identity in another namespace is not enqueued",
			identities: []client.Object{identity("sample-agent", 1, "keycloak")},
			config:     authConfig("team-b", "keycloak", metav1.ConditionTrue),
			want:       nil,
		},
		{
			name:       "an identity referencing the same config twice is enqueued once",
			identities: []client.Object{identity("sample-agent", 1, "keycloak", "keycloak")},
			config:     authConfig(testNamespace, "keycloak", metav1.ConditionTrue),
			want:       []string{"sample-agent"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(tt.identities...).Build()}

			got := r.identitiesForAuthConfig(context.Background(), tt.config)

			if len(got) != len(tt.want) {
				t.Fatalf("got %d requests %v, want %d %v", len(got), got, len(tt.want), tt.want)
			}
			for i, want := range tt.want {
				if got[i].Name != want {
					t.Errorf("request %d: got %q, want %q", i, got[i].Name, want)
				}
				if got[i].Namespace != testNamespace {
					t.Errorf("request %d namespace: got %q, want %q", i, got[i].Namespace, testNamespace)
				}
			}
		})
	}
}
