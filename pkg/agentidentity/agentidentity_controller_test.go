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
	"flag"
	"testing"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
)

const testNamespace = "team-a"

// scheme carries this group plus the built-in types a manager needs.
var scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(securityv1alpha1.AddToScheme(s))
	return s
}()

// errBoom stands in for any non-NotFound API failure.
var errBoom = errors.New("boom")

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
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Generation: 1},
	}
	if ready == "" {
		return config
	}
	// A config built here reports on the generation it is actually at, which is
	// what makes it usable. staleConfig is the counterpart.
	config.Status.ObservedGeneration = config.Generation
	config.Status.Conditions = []metav1.Condition{{
		Type:               securityv1alpha1.ConditionReady,
		Status:             ready,
		Reason:             "Test",
		ObservedGeneration: config.Generation,
		LastTransitionTime: metav1.Now(),
	}}
	return config
}

// staleConfig is Ready=True for a generation its spec has already moved past,
// which is what a config looks like between an edit and its controller catching
// up. The condition still says True and means nothing.
func staleConfig(namespace, name string) *securityv1alpha1.AgentAuthenticationConfig {
	config := authConfig(namespace, name, metav1.ConditionTrue)
	config.Generation = 2
	return config
}

// deletingConfig is Ready=True with deletion already requested. A finalizer can
// hold it in the API indefinitely, so this is exactly the window in which a
// consumer would otherwise keep treating it as usable.
func deletingConfig(namespace, name string) *securityv1alpha1.AgentAuthenticationConfig {
	config := authConfig(namespace, name, metav1.ConditionTrue)
	now := metav1.Now()
	config.DeletionTimestamp = &now
	config.Finalizers = []string{"agents.kruise.io/test"}
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
			// Admission now rejects an empty list, so this covers an object that
			// predates the constraint. It must fail closed, not read as "nothing
			// to check, therefore fine".
			name:            "no references is not ready",
			identity:        identity("standalone-agent", 3),
			wantStatus:      metav1.ConditionFalse,
			wantReason:      reasonNoAuthenticationRefs,
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
			// A config whose spec has changed keeps its previous Ready=True until
			// its own controller catches up. That verdict describes the old spec,
			// so it cannot be trusted for the new one.
			name:            "config with a stale status is not ready",
			identity:        identity("sample-agent", 1, "keycloak"),
			configs:         []client.Object{staleConfig(testNamespace, "keycloak")},
			wantStatus:      metav1.ConditionFalse,
			wantReason:      reasonAuthenticationConfigStale,
			wantObservedGen: 1,
		},
		{
			// Deletion is treated as unusable the moment it is requested, not when
			// the object finally disappears, because a finalizer can hold it in the
			// API with its last Ready=True still visible.
			name:            "config being deleted is not ready",
			identity:        identity("sample-agent", 1, "keycloak"),
			configs:         []client.Object{deletingConfig(testNamespace, "keycloak")},
			wantStatus:      metav1.ConditionFalse,
			wantReason:      reasonAuthenticationConfigDeleting,
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

// TestReconcileAPIFailures covers the paths where the API itself fails rather
// than reporting absence. Each must surface the error so the work is retried,
// and the first must leave the condition unwritten: an identity whose
// dependencies could not be read is unknown, not broken, and reporting it as
// not-Ready would fail issuance closed on a blip.
func TestReconcileAPIFailures(t *testing.T) {
	tests := []struct {
		name  string
		funcs interceptor.Funcs
	}{
		{
			name: "config lookup fails",
			funcs: interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
					obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*securityv1alpha1.AgentAuthenticationConfig); ok {
						return errBoom
					}
					return c.Get(ctx, key, obj, opts...)
				},
			},
		},
		{
			name: "status patch fails",
			funcs: interceptor.Funcs{
				SubResourcePatch: func(_ context.Context, _ client.Client, _ string,
					_ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
					return errBoom
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&securityv1alpha1.AgentIdentity{}).
				WithObjects(identity("sample-agent", 1, "keycloak"),
					authConfig(testNamespace, "keycloak", metav1.ConditionTrue)).
				WithInterceptorFuncs(tt.funcs).Build()}
			key := types.NamespacedName{Namespace: testNamespace, Name: "sample-agent"}

			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			if !errors.Is(err, errBoom) {
				t.Fatalf("Reconcile error: got %v, want %v", err, errBoom)
			}
		})
	}
}

// TestReconcileLeavesConditionUnsetOnLookupFailure is the half of the above
// that the error return alone does not prove: nothing was written to status.
func TestReconcileLeavesConditionUnsetOnLookupFailure(t *testing.T) {
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&securityv1alpha1.AgentIdentity{}).
		WithObjects(identity("sample-agent", 1, "keycloak")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*securityv1alpha1.AgentAuthenticationConfig); ok {
					return errBoom
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()}
	key := types.NamespacedName{Namespace: testNamespace, Name: "sample-agent"}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err == nil {
		t.Fatal("expected an error from Reconcile")
	}

	got := &securityv1alpha1.AgentIdentity{}
	if err := r.Get(context.Background(), key, got); err != nil {
		t.Fatalf("getting the identity: %v", err)
	}
	if c := apiMeta.FindStatusCondition(got.Status.Conditions, securityv1alpha1.ConditionReady); c != nil {
		t.Errorf("condition was written despite a failed lookup: %+v", c)
	}
}

// TestIdentitiesForAuthConfigListFailure covers the map function's error path.
// A failed list yields no requests rather than a panic or a partial fan-out.
// TestConvergesAfterADroppedConfigEvent is the guarantee behind the map
// function being best-effort.
//
// handler.EnqueueRequestsFromMapFunc cannot return an error, so a failed list
// drops the configuration-change event with nowhere to report it. Correctness
// therefore cannot depend on that event arriving. This drops the event, then
// shows the identity still converges on the next reconcile, and that every
// successful reconcile schedules one.
func TestConvergesAfterADroppedConfigEvent(t *testing.T) {
	listFails := true
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&securityv1alpha1.AgentIdentity{}).
		WithObjects(identity("sample-agent", 1, "keycloak"),
			authConfig(testNamespace, "keycloak", metav1.ConditionTrue)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
				opts ...client.ListOption) error {
				if listFails {
					return errBoom
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
	r := &Reconciler{Client: fc}
	ctx := context.Background()
	key := types.NamespacedName{Namespace: testNamespace, Name: "sample-agent"}

	// The identity starts Ready, as its config is Ready and current.
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require(t, err)
	if res.RequeueAfter != resyncInterval {
		t.Fatalf("a successful reconcile must schedule a resync: got %v, want %v",
			res.RequeueAfter, resyncInterval)
	}
	if got := readyConditionOf(t, r, key); got == nil || got.Status != metav1.ConditionTrue {
		t.Fatalf("expected Ready=True to begin with, got %+v", got)
	}

	// The config is deleted. The fan-out that would normally notice drops the
	// event, so nothing requeues the identity and its verdict is now stale.
	config := authConfig(testNamespace, "keycloak", metav1.ConditionTrue)
	if err := fc.Delete(ctx, config); err != nil {
		t.Fatalf("deleting the config: %v", err)
	}
	if dropped := r.identitiesForAuthConfig(ctx, config); dropped != nil {
		t.Fatalf("expected the failed list to drop the event, got %v", dropped)
	}
	if got := readyConditionOf(t, r, key); got == nil || got.Status != metav1.ConditionTrue {
		t.Fatalf("the identity should still hold its stale verdict, got %+v", got)
	}

	// The resync arrives and the identity converges without the event.
	listFails = false
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile after the dropped event: %v", err)
	}
	got := readyConditionOf(t, r, key)
	if got == nil || got.Status != metav1.ConditionFalse {
		t.Fatalf("expected Ready=False after convergence, got %+v", got)
	}
	if got.Reason != reasonAuthenticationConfigNotFound {
		t.Errorf("reason: got %q, want %q", got.Reason, reasonAuthenticationConfigNotFound)
	}
}

func require(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func readyConditionOf(t *testing.T, r *Reconciler, key types.NamespacedName) *metav1.Condition {
	t.Helper()
	got := &securityv1alpha1.AgentIdentity{}
	if err := r.Get(context.Background(), key, got); err != nil {
		t.Fatalf("getting the identity: %v", err)
	}
	return apiMeta.FindStatusCondition(got.Status.Conditions, securityv1alpha1.ConditionReady)
}

// newTestManager builds a real manager over a stub REST config, following the
// pattern in the sandboxset and controller-registry tests. It is never started,
// so no apiserver is needed; only the registration wiring is exercised.
func newTestManager(t *testing.T) ctrl.Manager {
	t.Helper()
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:0"}, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr
}

func TestSetupWithManager(t *testing.T) {
	mgr := newTestManager(t)
	if err := (&Reconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		t.Fatalf("SetupWithManager: unexpected error: %v", err)
	}
}

// TestAddWithoutCRD covers the opt-in guard. With no discovery client
// registered, DiscoverGVK reports the kind absent, so Add must return nil
// without registering a controller.
func TestAddWithoutCRD(t *testing.T) {
	if err := Add(newTestManager(t)); err != nil {
		t.Fatalf("Add with the CRD absent: unexpected error: %v", err)
	}
}

// TestWorkerFlagsAreRegistered covers the concurrency wiring. Both controllers
// in this binary expose a worker count, and a flag that is declared but never
// reaches WithOptions compiles perfectly well while doing nothing.
func TestWorkerFlagsAreRegistered(t *testing.T) {
	for _, name := range []string{"agentidentity-workers", "authconfig-workers"} {
		t.Run(name, func(t *testing.T) {
			f := flag.Lookup(name)
			if f == nil {
				t.Fatalf("flag %q is not registered on the command line", name)
			}
			if f.DefValue != "3" {
				t.Errorf("default: got %q, want %q", f.DefValue, "3")
			}
		})
	}

	// The values the flags write are what SetupWithManager passes through, so a
	// zero would silently fall back to controller-runtime's default of one.
	if agentIdentityWorkers < 1 {
		t.Errorf("agentIdentityWorkers is %d, which would disable concurrency", agentIdentityWorkers)
	}
	if authConfigWorkers < 1 {
		t.Errorf("authConfigWorkers is %d, which would disable concurrency", authConfigWorkers)
	}
}
