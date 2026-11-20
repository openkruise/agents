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
	"sync"
	"testing"
	"time"

	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
)

// mutableClientFor is authorizerFor's counterpart for the cache tests: it
// returns the fake client.Client itself rather than wrapping it, so a test
// can mutate the backing store directly and prove the Authorizer did, or did
// not, notice.
func mutableClientFor(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// TestAuthorizeServesCacheWithoutRereading is the central cache-hit proof: the
// fake client has no informer or watch layer at all, so if the second call
// reflects a deletion the first call's Authorizer never read again, the only
// explanation is that it was served from cache.
func TestAuthorizeServesCacheWithoutRereading(t *testing.T) {
	ctx := context.Background()
	c := mutableClientFor(
		role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "*")),
		binding("b", "r", "sample-agent"),
	)
	authz := New(c)
	req := exchangeRequest("sample-agent", "AgentIdentity/sample-agent")

	first, err := authz.Authorize(ctx, req)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !first.Allowed {
		t.Fatalf("first call: got %s, want allowed", first)
	}

	// Bypasses any informer: this is a direct write to the fake client's own
	// store, which WatchForInvalidation was never even told about.
	if err := c.Delete(ctx, binding("b", "r", "sample-agent")); err != nil {
		t.Fatalf("delete binding: %v", err)
	}

	second, err := authz.Authorize(ctx, req)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if second != first {
		t.Errorf("second call: got %s, want the cached %s unchanged", second, first)
	}
}

// TestAuthorizeCacheTTL covers expiry driven purely by the clock, with no
// informer event at all: an entry is still served from cache inside its TTL,
// and recomputed once the injected clock has moved past it.
func TestAuthorizeCacheTTL(t *testing.T) {
	const ttl = 5 * time.Second

	tests := []struct {
		name    string
		advance time.Duration
		// wantFresh is whether the second call must reflect the deletion
		// (cache expired) rather than repeat the first decision (cache hit).
		wantFresh bool
	}{
		{name: "still within TTL", advance: ttl - time.Second, wantFresh: false},
		{name: "exactly at TTL boundary is no longer fresh", advance: ttl, wantFresh: true},
		{name: "past TTL", advance: ttl + time.Second, wantFresh: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			c := mutableClientFor(
				role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "*")),
				binding("b", "r", "sample-agent"),
			)

			now := time.Now()
			clock := func() time.Time { return now }
			authz := New(c, WithTTL(ttl), withClock(clock))
			req := exchangeRequest("sample-agent", "AgentIdentity/sample-agent")

			first, err := authz.Authorize(ctx, req)
			if err != nil {
				t.Fatalf("Authorize: %v", err)
			}
			if !first.Allowed {
				t.Fatalf("first call: got %s, want allowed", first)
			}

			if err := c.Delete(ctx, binding("b", "r", "sample-agent")); err != nil {
				t.Fatalf("delete binding: %v", err)
			}
			now = now.Add(tt.advance)

			second, err := authz.Authorize(ctx, req)
			if err != nil {
				t.Fatalf("Authorize: %v", err)
			}
			if tt.wantFresh && second.Allowed {
				t.Errorf("second call: got %s, want the deletion reflected after the TTL elapsed", second)
			}
			if !tt.wantFresh && second != first {
				t.Errorf("second call: got %s, want the cached %s unchanged within the TTL", second, first)
			}
		})
	}
}

// fakeInvalidationCache returns a cache.Cache usable with WatchForInvalidation
// that delivers events synchronously when a test calls Add/Update/Delete on
// the FakeInformer it returns for a kind, via
// sigs.k8s.io/controller-runtime/pkg/cache/informertest. This is the
// framework's own supported seam for faking informer registration and event
// delivery without a real API server or cluster, which is why it is used here
// instead of a hand-rolled fake.
func fakeInvalidationCache() *informertest.FakeInformers {
	return &informertest.FakeInformers{Scheme: scheme}
}

// TestWatchForInvalidationEvictsOnChange covers all three informer events:
// a change the cache was never told about directly (an add, an update or a
// delete of an AgentRole or an AgentRoleBinding) must evict the affected
// namespace's cached decisions, so the next Authorize call recomputes and
// reflects the change rather than repeating a stale verdict.
func TestWatchForInvalidationEvictsOnChange(t *testing.T) {
	tests := []struct {
		name string
		// seed is the state the fake client and the request are evaluated
		// against before the triggering event.
		seed []client.Object
		// wantBeforeAllowed is the decision the seeded state must produce, so
		// a bug in the seeding itself fails loudly rather than being masked by
		// the eviction this test exists to check.
		wantBeforeAllowed bool
		// mutate changes the fake client's backing store to what the event
		// below announces, then fires the matching informer event.
		mutate func(t *testing.T, ctx context.Context, c client.Client, fc *informertest.FakeInformers)
		// wantAfterAllowed is the decision the mutated state must produce once
		// the stale cache entry has been evicted.
		wantAfterAllowed bool
	}{
		{
			name:              "an AgentRoleBinding add grants what nothing granted before",
			seed:              []client.Object{role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "*"))},
			wantBeforeAllowed: false,
			mutate: func(t *testing.T, ctx context.Context, c client.Client, fc *informertest.FakeInformers) {
				newBinding := binding("b", "r", "sample-agent")
				if err := c.Create(ctx, newBinding); err != nil {
					t.Fatalf("create binding: %v", err)
				}
				informer, err := fc.FakeInformerFor(ctx, &securityv1alpha1.AgentRoleBinding{})
				if err != nil {
					t.Fatalf("fake informer: %v", err)
				}
				informer.Add(newBinding)
			},
			wantAfterAllowed: true,
		},
		{
			name: "an AgentRole update revokes what it granted before",
			seed: []client.Object{
				role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "*")),
				binding("b", "r", "sample-agent"),
			},
			wantBeforeAllowed: true,
			mutate: func(t *testing.T, ctx context.Context, c client.Client, fc *informertest.FakeInformers) {
				oldRole := role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "*"))
				var current securityv1alpha1.AgentRole
				if err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "r"}, &current); err != nil {
					t.Fatalf("get role: %v", err)
				}
				updated := current.DeepCopy()
				updated.Spec.Rules = []securityv1alpha1.AgentRoleRule{
					rule(securityv1alpha1.ActionExchangePrincipalToken, "AgentIdentity/other-agent"),
				}
				if err := c.Update(ctx, updated); err != nil {
					t.Fatalf("update role: %v", err)
				}
				informer, err := fc.FakeInformerFor(ctx, &securityv1alpha1.AgentRole{})
				if err != nil {
					t.Fatalf("fake informer: %v", err)
				}
				informer.Update(oldRole, updated)
			},
			wantAfterAllowed: false,
		},
		{
			name: "an AgentRoleBinding delete revokes what it granted before",
			seed: []client.Object{
				role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "*")),
				binding("b", "r", "sample-agent"),
			},
			wantBeforeAllowed: true,
			mutate: func(t *testing.T, ctx context.Context, c client.Client, fc *informertest.FakeInformers) {
				toDelete := binding("b", "r", "sample-agent")
				if err := c.Delete(ctx, toDelete); err != nil {
					t.Fatalf("delete binding: %v", err)
				}
				informer, err := fc.FakeInformerFor(ctx, &securityv1alpha1.AgentRoleBinding{})
				if err != nil {
					t.Fatalf("fake informer: %v", err)
				}
				informer.Delete(toDelete)
			},
			wantAfterAllowed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			c := mutableClientFor(tt.seed...)
			authz := New(c)
			fc := fakeInvalidationCache()
			if err := authz.WatchForInvalidation(ctx, fc); err != nil {
				t.Fatalf("WatchForInvalidation: %v", err)
			}
			req := exchangeRequest("sample-agent", "AgentIdentity/sample-agent")

			before, err := authz.Authorize(ctx, req)
			if err != nil {
				t.Fatalf("Authorize (before): %v", err)
			}
			if before.Allowed != tt.wantBeforeAllowed {
				t.Fatalf("before: got %s, want allowed=%v", before, tt.wantBeforeAllowed)
			}

			tt.mutate(t, ctx, c, fc)

			after, err := authz.Authorize(ctx, req)
			if err != nil {
				t.Fatalf("Authorize (after): %v", err)
			}
			if after.Allowed != tt.wantAfterAllowed {
				t.Errorf("after: got %s, want allowed=%v (the event should have evicted the stale cache entry)",
					after, tt.wantAfterAllowed)
			}
		})
	}
}

// TestWatchForInvalidationIsNamespaceScoped checks the granularity
// WatchForInvalidation deliberately keeps simple: an event evicts the
// namespace it names and nothing else.
func TestWatchForInvalidationIsNamespaceScoped(t *testing.T) {
	ctx := context.Background()
	const otherNamespace = "team-b"

	teamA := binding("b", "r", "sample-agent")
	teamB := binding("b", "r", "sample-agent")
	teamB.Namespace = otherNamespace
	teamBRole := role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "*"))
	teamBRole.Namespace = otherNamespace

	c := mutableClientFor(
		role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "*")),
		teamA,
		teamBRole,
		teamB,
	)
	authz := New(c)
	fc := fakeInvalidationCache()
	if err := authz.WatchForInvalidation(ctx, fc); err != nil {
		t.Fatalf("WatchForInvalidation: %v", err)
	}

	reqA := exchangeRequest("sample-agent", "AgentIdentity/sample-agent")
	reqB := Request{
		Namespace:     otherNamespace,
		AgentIdentity: "sample-agent",
		Action:        securityv1alpha1.ActionExchangePrincipalToken,
		Resource:      "AgentIdentity/sample-agent",
	}

	for _, req := range []Request{reqA, reqB} {
		got, err := authz.Authorize(ctx, req)
		if err != nil {
			t.Fatalf("Authorize (seed %s): %v", req.Namespace, err)
		}
		if !got.Allowed {
			t.Fatalf("Authorize (seed %s): got %s, want allowed", req.Namespace, got)
		}
	}

	// Delete team-a's binding and announce it, which must evict team-a alone.
	if err := c.Delete(ctx, teamA); err != nil {
		t.Fatalf("delete team-a binding: %v", err)
	}
	informer, err := fc.FakeInformerFor(ctx, &securityv1alpha1.AgentRoleBinding{})
	if err != nil {
		t.Fatalf("fake informer: %v", err)
	}
	informer.Delete(teamA)

	// Delete team-b's binding too, but announce nothing for it. If the team-a
	// event reached team-b's cache entry, this call would recompute and see
	// the deletion; it must not.
	if err := c.Delete(ctx, teamB); err != nil {
		t.Fatalf("delete team-b binding: %v", err)
	}

	gotA, err := authz.Authorize(ctx, reqA)
	if err != nil {
		t.Fatalf("Authorize (team-a): %v", err)
	}
	if gotA.Allowed {
		t.Errorf("team-a: got %s, want the deletion reflected", gotA)
	}

	gotB, err := authz.Authorize(ctx, reqB)
	if err != nil {
		t.Fatalf("Authorize (team-b): %v", err)
	}
	if !gotB.Allowed {
		t.Errorf("team-b: got %s, want the stale cached decision (team-a's event must not evict team-b)", gotB)
	}
}

// TestNamespaceOf covers the helper WatchForInvalidation's handlers use to
// read an informer event's namespace, including the DeletedFinalStateUnknown
// tombstone a delete event delivers when the informer did not observe the
// deletion directly.
func TestNamespaceOf(t *testing.T) {
	obj := role("r", rule(securityv1alpha1.ActionExchangePrincipalToken, "*"))

	tests := []struct {
		name          string
		obj           any
		wantNamespace string
		wantOK        bool
	}{
		{name: "a typed object", obj: obj, wantNamespace: testNamespace, wantOK: true},
		{
			name:          "a deleted-final-state-unknown tombstone",
			obj:           toolscache.DeletedFinalStateUnknown{Key: "team-a/r", Obj: obj},
			wantNamespace: testNamespace,
			wantOK:        true,
		},
		{name: "neither a client.Object nor a tombstone", obj: "not-an-object", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			namespace, ok := namespaceOf(tt.obj)
			if ok != tt.wantOK || namespace != tt.wantNamespace {
				t.Errorf("namespaceOf(%v) = (%q, %v), want (%q, %v)",
					tt.obj, namespace, ok, tt.wantNamespace, tt.wantOK)
			}
		})
	}
}

// TestAuthorizeCacheConcurrentAccess exercises Authorize from many goroutines
// at once, against a mix of shared and distinct cache keys, under -race. The
// scenario holds no mutation after seeding, so every call must agree with the
// decision computed once up front; a data race corrupting the cache map is
// what -race is here to catch, and a wrong decision would show the corruption
// even without it.
func TestAuthorizeCacheConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	c := mutableClientFor(
		role("exchange", rule(securityv1alpha1.ActionExchangePrincipalToken, "*")),
		binding("b1", "exchange", "sample-agent"),
		binding("b2", "exchange", "other-agent"),
	)
	authz := New(c)

	requests := []Request{
		exchangeRequest("sample-agent", "AgentIdentity/sample-agent"),
		exchangeRequest("other-agent", "AgentIdentity/other-agent"),
		exchangeRequest("unbound-agent", "AgentIdentity/unbound-agent"),
	}
	want := []bool{true, true, false}

	const goroutines = 16
	const iterations = 50

	// t.Errorf is safe to call from multiple goroutines; only FailNow (and
	// thus Fatalf) is not, so every goroutine reports through it directly
	// rather than through a channel this test would have to drain itself.
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				idx := (g + i) % len(requests)
				got, err := authz.Authorize(ctx, requests[idx])
				if err != nil {
					t.Errorf("Authorize(%v): %v", requests[idx], err)
					return
				}
				if got.Allowed != want[idx] {
					t.Errorf("Authorize(%v) = %s, want allowed=%v", requests[idx], got, want[idx])
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
