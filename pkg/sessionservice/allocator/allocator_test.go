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

package allocator

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agents "github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/sandboxroute"
	session "github.com/openkruise/agents/pkg/sessionservice"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
	"github.com/openkruise/agents/pkg/sessionservice/kubernetes"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

func allocationFixture(t *testing.T, affinity string, limit *int32, hooks interceptor.Funcs, boxes ...*agents.Sandbox) (*Allocator, client.Client, *agents.SessionService) {
	t.Helper()
	p := &agents.SessionService{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "serving", UID: "serving-uid", Labels: map[string]string{sessionbinding.SessionServiceIDLabel: "serving-id"}}, Spec: agents.SessionServiceSpec{SessionPolicy: agents.SessionServiceSessionPolicy{Affinity: affinity, MaxSessionsPerSandbox: limit}}}
	objects := []client.Object{p}
	for _, b := range boxes {
		b.Namespace = "team"
		b.Labels = map[string]string{sessionbinding.SessionServiceUIDLabel: string(p.UID)}
		b.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(p, agents.GroupVersion.WithKind("SessionService"))}
		objects = append(objects, b)
		t.Cleanup(func() { expectations.ResourceVersionExpectationDelete(b) })
	}
	cache, writer, err := cachetest.NewTestSessionServiceCache(t, objects...)
	if err != nil {
		t.Fatal(err)
	}
	if err = cache.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	watched := writer.(client.WithWatch)
	store := kubernetes.NewStore(interceptor.NewClient(watched, hooks), cache.(infracache.SessionServiceCache).SessionServiceInformerHealthy, kubernetes.Options{})
	return New(store), writer, p
}

func allocationBox(name, uid string, ids ...string) *agents.Sandbox {
	b := &agents.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)}, Status: agents.SandboxStatus{Phase: agents.SandboxRunning, PodInfo: agents.PodInfo{PodIP: "10.0.0.1"}, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}}}
	records := sessionbinding.Bindings{}
	for _, id := range ids {
		records[id] = sessionbinding.Record{LastRequestAt: "2026-11-10T00:00:00Z"}
	}
	var err error
	b.Annotations, err = sessionbinding.Encode(nil, records)
	if err != nil {
		panic(err)
	}
	return b
}

func readAllocationBindings(t *testing.T, c client.Client, name string) sessionbinding.Bindings {
	t.Helper()
	var b agents.Sandbox
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "team", Name: name}, &b); err != nil {
		t.Fatal(err)
	}
	r, e := sessionbinding.Decode(b.Annotations)
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func TestSessionServiceAllocationCreateAndResolve(t *testing.T) {
	const id = "Ab_9012345678901234567"
	for _, operation := range []string{"create", "known", "unknown", "closed", "invalid"} {
		t.Run(operation, func(t *testing.T) {
			patches := 0
			hooks := interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches++
				return c.Patch(ctx, obj, patch, opts...)
			}}
			box := allocationBox("first", "a")
			if operation == "known" || operation == "closed" {
				box = allocationBox("first", "a", id)
			}
			a, c, _ := allocationFixture(t, "BestEffort", nil, hooks, box)
			if operation == "closed" {
				require.NoError(t, closeForTest(t.Context(), a, id))
				patches = 0
			}
			var route sandboxroute.Route
			var err error
			switch operation {
			case "create":
				route, err = createForTest(t.Context(), a, id, "serving-uid")
			case "invalid":
				route, err = resolveForTest(t.Context(), a, "short", "serving-uid")
			default:
				route, err = resolveForTest(t.Context(), a, id, "serving-uid")
			}
			switch operation {
			case "create", "known":
				require.NoError(t, err)
				require.Equal(t, "first", route.Name)
				require.Contains(t, readAllocationBindings(t, c, "first"), id)
			case "invalid":
				require.ErrorIs(t, err, session.ErrInvalidSession)
			default:
				require.ErrorIs(t, err, session.ErrSessionNotFound)
				require.Empty(t, readAllocationBindings(t, c, "first"))
			}
			if operation == "create" {
				require.Equal(t, 1, patches)
			} else {
				require.Zero(t, patches)
			}
		})
	}
}

func TestSessionServiceAllocationLeastSessionsAndAffinity(t *testing.T) {
	for _, tt := range []struct {
		name, affinity, phase                           string
		paused, expired, busyAlternative, noAlternative bool
		projectedState                                  string
		ready                                           bool
		bound                                           bool
		want                                            string
		reason                                          string
	}{
		{name: "least sessions", ready: true, want: "second"},
		{name: "strict reuses despite lower capacity", affinity: "Strict", ready: true, bound: true, want: "first"},
		{name: "best effort unavailable moves", ready: false, bound: true, want: "second"},
		{name: "strict unavailable retains", affinity: "Strict", ready: false, bound: true, reason: "BoundSandboxUnavailable"},
		{name: "paused retains", ready: false, bound: true, phase: "Paused", reason: "BoundSandboxUnavailable"},
		{name: "resuming retains", ready: false, bound: true, phase: "Resuming", reason: "BoundSandboxUnavailable"},
		{name: "strict paused retains", affinity: "Strict", ready: true, bound: true, phase: "Paused", reason: "BoundSandboxUnavailable", projectedState: "paused"},
		{name: "strict resuming retains", affinity: "Strict", ready: true, bound: true, phase: "Resuming", reason: "BoundSandboxUnavailable", projectedState: "paused"},
		{name: "best effort new spec paused skips to healthy", ready: true, paused: true, busyAlternative: true, want: "second", projectedState: "paused"},
		{name: "strict new spec paused skips to healthy", affinity: "Strict", ready: true, paused: true, busyAlternative: true, want: "second", projectedState: "paused"},
		{name: "best effort new expired skips to healthy", ready: true, expired: true, busyAlternative: true, want: "second", projectedState: "dead"},
		{name: "strict new expired skips to healthy", affinity: "Strict", ready: true, expired: true, busyAlternative: true, want: "second", projectedState: "dead"},
		{name: "best effort existing spec paused retains", ready: true, paused: true, bound: true, reason: "BoundSandboxUnavailable", projectedState: "paused"},
		{name: "strict existing spec paused retains", affinity: "Strict", ready: true, paused: true, bound: true, reason: "BoundSandboxUnavailable", projectedState: "paused"},
		{name: "best effort existing expired moves", ready: true, expired: true, bound: true, want: "second", projectedState: "dead"},
		{name: "strict existing expired retains", affinity: "Strict", ready: true, expired: true, bound: true, reason: "BoundSandboxUnavailable", projectedState: "dead"},
		{name: "best effort new paused no running capacity", ready: true, paused: true, noAlternative: true, reason: "SessionCapacityExhausted", projectedState: "paused"},
		{name: "strict new expired no running capacity", affinity: "Strict", ready: true, expired: true, noAlternative: true, reason: "SessionCapacityExhausted", projectedState: "dead"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			first := allocationBox("first", "a", "00000000000000000other")
			if tt.bound {
				first = allocationBox("first", "a", "00000000000000000other", "000000000000000session")
			}
			if !tt.ready {
				first.Status.Conditions = nil
			}
			if tt.phase != "" {
				first.Status.Phase = agents.SandboxPhase(tt.phase)
			}
			first.Spec.Paused = tt.paused
			if tt.expired {
				shutdown := metav1.NewTime(time.Now().Add(-time.Hour))
				first.Spec.ShutdownTime = &shutdown
			}
			first.Annotations["example.com/keep"] = "yes"
			first.Finalizers = []string{"example.com/keep"}
			records, err := sessionbinding.Decode(first.Annotations)
			if err != nil {
				t.Fatal(err)
			}
			first.Annotations, err = sessionbinding.Encode(first.Annotations, records)
			if err != nil {
				t.Fatal(err)
			}
			boxes := []*agents.Sandbox{first}
			if !tt.noAlternative {
				second := allocationBox("second", "b")
				if tt.busyAlternative {
					second = allocationBox("second", "b", "0000000000000000keep_1", "0000000000000000keep_2")
				}
				boxes = append(boxes, second)
			}
			m, c, _ := allocationFixture(t, tt.affinity, nil, interceptor.Funcs{}, boxes...)
			if tt.projectedState != "" {
				parent, err := m.store.GetSessionService(t.Context(), "team", "serving-id")
				if err != nil {
					t.Fatal(err)
				}
				snapshot, err := m.store.ReadSessionServiceSnapshot(t.Context(), parent)
				if err != nil {
					t.Fatal(err)
				}
				for _, member := range snapshot.Members {
					if member.UID == "a" && member.Route.State != tt.projectedState {
						t.Fatalf("real Sandbox projection state=%q, want %q", member.Route.State, tt.projectedState)
					}
				}
			}
			operation := createForTest
			if tt.bound {
				operation = resolveForTest
			}
			result, err := operation(t.Context(), m, "000000000000000session", "serving-uid")
			var persisted agents.Sandbox
			if e := c.Get(t.Context(), client.ObjectKey{Namespace: "team", Name: "first"}, &persisted); e != nil {
				t.Fatal(e)
			}
			remaining, e := sessionbinding.Decode(persisted.Annotations)
			if e != nil {
				t.Fatal(e)
			}
			keptFinalizer := false
			for _, f := range persisted.Finalizers {
				keptFinalizer = keptFinalizer || f == "example.com/keep"
			}
			if persisted.Annotations["example.com/keep"] != "yes" || remaining["00000000000000000other"] != records["00000000000000000other"] || !keptFinalizer {
				t.Fatal("allocation lost unrelated member metadata or record fields")
			}
			if tt.reason != "" {
				requireSessionError(t, err, session.ErrSessionServiceUnavailable, tt.reason)
				_, bound := remaining["000000000000000session"]
				if bound != tt.bound {
					t.Fatal("failed allocation changed the existing binding")
				}
				if bound && !reflect.DeepEqual(remaining["000000000000000session"], records["000000000000000session"]) {
					t.Fatal("retained binding lost its state or activity")
				}
				return
			}
			if err != nil || result.Name != tt.want {
				t.Fatalf("allocation=%+v err=%v", result, err)
			}
			if _, ok := readAllocationBindings(t, c, tt.want)["000000000000000session"]; !ok {
				t.Fatal("successful route lacks durable binding")
			}
			if _, ok := readAllocationBindings(t, c, "first")["00000000000000000other"]; !ok {
				t.Fatal("lost unrelated session")
			}
		})
	}
}

func TestSessionServiceAllocationTieAndCapacity(t *testing.T) {
	one := int32(1)
	for _, tt := range []struct {
		name   string
		limit  *int32
		full   bool
		uid    string
		reason string
	}{
		{name: "uid tie", uid: "serving-uid"},
		{name: "capacity", limit: &one, full: true, uid: "serving-uid", reason: "SessionCapacityExhausted"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := allocationBox("second", "b")
			a := allocationBox("first", "a")
			if tt.full {
				b = allocationBox("second", "b", "000000000000000000000b")
				a = allocationBox("first", "a", "000000000000000000000a")
			}
			m, c, _ := allocationFixture(t, "Strict", tt.limit, interceptor.Funcs{}, b, a)

			r, e := createForTest(t.Context(), m, "0000000000000000000new", tt.uid)
			if tt.reason != "" {
				requireSessionError(t, e, session.ErrSessionServiceUnavailable, tt.reason)
				return
			}
			if e != nil || r.Name != "first" {
				t.Fatalf("tie=%+v %v", r, e)
			}
			var member agents.Sandbox
			if e = c.Get(t.Context(), client.ObjectKey{Namespace: "team", Name: "first"}, &member); e != nil {
				t.Fatal(e)
			}
			if len(member.Finalizers) != 0 {
				t.Fatalf("Strict allocation added deletion protection: %v", member.Finalizers)
			}
		})
	}
}

func TestSessionServiceAllocationConcurrentLastSlot(t *testing.T) {
	one := int32(1)
	m, c, _ := allocationFixture(t, "", &one, interceptor.Funcs{}, allocationBox("first", "a"))
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			_, err := createForTest(t.Context(), m, fmt.Sprintf("s%021d", i), "serving-uid")
			results <- err
		})
	}
	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		} else if !errors.Is(e, session.ErrSessionServiceUnavailable) {
			t.Fatal(e)
		}
	}
	if success != 1 || len(readAllocationBindings(t, c, "first")) != 1 {
		t.Fatalf("last slot successes=%d", success)
	}
}

func TestSessionServiceAllocationDuplicateWinner(t *testing.T) {
	a := allocationBox("first", "a", "000000000000000session", "0000000000000000keep_a")
	b := allocationBox("second", "b", "000000000000000session", "0000000000000000keep_b")
	b.Annotations[sessionbinding.BindingsAnnotation] = `{"000000000000000session":{"lastRequestAt":"2026-11-10T00:01:00Z"},"0000000000000000keep_b":{"lastRequestAt":"2026-11-10T00:00:00Z"}}`
	m, c, _ := allocationFixture(t, "", nil, interceptor.Funcs{}, b, a)
	winnerBefore := readAllocationBindings(t, c, "first")["000000000000000session"]
	r, e := resolveForTest(t.Context(), m, "000000000000000session", "serving-uid")
	if e != nil || r.Name != "first" {
		t.Fatalf("winner=%+v %v", r, e)
	}
	ra := readAllocationBindings(t, c, "first")
	rb := readAllocationBindings(t, c, "second")
	if !reflect.DeepEqual(ra["000000000000000session"], winnerBefore) || len(ra) != 2 || len(rb) != 1 || rb["0000000000000000keep_b"].LastRequestAt == "" {
		t.Fatalf("convergence=%+v %+v", ra, rb)
	}
}

func TestSessionServiceAllocationCanceledWait(t *testing.T) {
	m, _, _ := allocationFixture(t, "", nil, interceptor.Funcs{}, allocationBox("first", "a"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	_, err := createForTest(ctx, m, "000000000000000session", "serving-uid")
	if !errors.Is(err, session.ErrSessionServiceUnavailable) || time.Since(start) > time.Second {
		t.Fatalf("canceled allocation=%v", err)
	}
}

// Real conditional patches must unlink the failed old target before adding the
// Session to a new target. Unknown outcomes are tested at the client boundary.
func TestSessionServiceAllocationBestEffortWriteOrdering(t *testing.T) {
	for _, tt := range []struct {
		name         string
		unknown      bool
		commit       bool
		rejectUnlink bool
		rejectNew    bool
		wantSuccess  bool
	}{
		{name: "normal same call move", wantSuccess: true},
		{name: "unknown unlink persisted", unknown: true, commit: true, wantSuccess: true},
		{name: "unknown unlink not persisted", unknown: true},
		{name: "unlink rejected", rejectUnlink: true},
		{name: "new binding rejected after unlink", rejectNew: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			old := allocationBox("old", "a", "000000000000000session", "00000000000000000other")
			old.Status.Conditions = nil
			var patches []string
			injected := false
			hooks := interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches = append(patches, obj.GetName())
				if obj.GetName() == "old" && tt.rejectUnlink {
					return apierrors.NewForbidden(agents.Resource("sandboxes"), "old", errors.New("rejected"))
				}
				if obj.GetName() == "old" && !injected {
					injected = true
					if tt.unknown {
						if tt.commit {
							if err := c.Patch(ctx, obj, patch, opts...); err != nil {
								return err
							}
						}
						return context.DeadlineExceeded
					}
				}
				if obj.GetName() == "new" && tt.rejectNew {
					return apierrors.NewForbidden(agents.Resource("sandboxes"), "new", errors.New("rejected"))
				}
				return c.Patch(ctx, obj, patch, opts...)
			}}
			m, c, _ := allocationFixture(t, "BestEffort", nil, hooks, old, allocationBox("new", "b"))
			ctx, cancel := context.WithTimeout(t.Context(), 130*time.Millisecond)
			defer cancel()
			result, err := resolveForTest(ctx, m, "000000000000000session", "serving-uid")
			if tt.wantSuccess {
				if err != nil || result.Name != "new" {
					t.Fatalf("move=%+v %v", result, err)
				}
			} else if !errors.Is(err, session.ErrSessionServiceUnavailable) {
				t.Fatalf("failed write returned success: %v", err)
			}
			firstNew := -1
			for i, name := range patches {
				if name == "new" {
					firstNew = i
					break
				}
			}
			if tt.wantSuccess || tt.rejectNew {
				if firstNew < 1 || patches[0] != "old" {
					t.Fatalf("new write preceded unlink: %v", patches)
				}
			} else if firstNew != -1 {
				t.Fatalf("unconfirmed unlink allocated new target: %v", patches)
			}
			if readAllocationBindings(t, c, "old")["00000000000000000other"].LastRequestAt == "" {
				t.Fatal("unlink lost unrelated Session")
			}
			if tt.rejectNew {
				if _, ok := readAllocationBindings(t, c, "old")["000000000000000session"]; ok {
					t.Fatal("old binding unexpectedly rolled back")
				}
				// A fresh operation cannot recreate the unlinked binding.
				m2 := allocatorForClient(t, c.(client.WithWatch), interceptor.Funcs{})
				r, e := resolveForTest(t.Context(), m2, "000000000000000session", "serving-uid")
				if !errors.Is(e, session.ErrSessionNotFound) || r.Name != "" {
					t.Fatalf("restart recreated unknown session=%+v %v", r, e)
				}
			}
		})
	}
}

func allocatorForClient(t *testing.T, c client.WithWatch, hooks interceptor.Funcs) *Allocator {
	t.Helper()
	cache, _, err := cachetest.NewTestSessionServiceCache(t)
	if err != nil {
		t.Fatal(err)
	}
	if err = cache.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	return New(kubernetes.NewStore(interceptor.NewClient(c, hooks), cache.(infracache.SessionServiceCache).SessionServiceInformerHealthy, kubernetes.Options{}))
}

func TestSessionServiceAllocationUnknownNewPinsTarget(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(fmt.Sprint(commit), func(t *testing.T) {
			patches := []string{}
			hooks := interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches = append(patches, obj.GetName())
				if commit {
					if e := c.Patch(ctx, obj, patch, opts...); e != nil {
						return e
					}
				}
				return context.DeadlineExceeded
			}}
			m, c, _ := allocationFixture(t, "", nil, hooks, allocationBox("first", "a"), allocationBox("second", "b"), allocationBox("third", "c"))
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			result, err := createForTest(ctx, m, "000000000000000session", "serving-uid")
			if commit {
				if err != nil || result.Name != "first" {
					t.Fatalf("unknown persisted=%+v %v", result, err)
				}
			} else if !errors.Is(err, session.ErrSessionServiceUnavailable) {
				t.Fatalf("unknown not persisted=%v", err)
			}
			if !reflect.DeepEqual(patches, []string{"first"}) {
				t.Fatalf("unknown write moved to another target: %v", patches)
			}
			if len(readAllocationBindings(t, c, "second")) != 0 || len(readAllocationBindings(t, c, "third")) != 0 {
				t.Fatal("unknown outcome bound a third target")
			}
		})
	}
}

func TestSessionServiceAllocationReadAfterAcknowledgement(t *testing.T) {
	inserted := false
	hooks := interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if err := c.Patch(ctx, obj, patch, opts...); err != nil {
			return err
		}
		if !inserted {
			inserted = true
			var winner agents.Sandbox
			if err := c.Get(ctx, client.ObjectKey{Namespace: "team", Name: "first"}, &winner); err != nil {
				return err
			}
			winner.Annotations[sessionbinding.BindingsAnnotation] = `{"000000000000000session":{"lastRequestAt":"2026-11-10T00:00:00Z"}}`
			if err := c.Update(ctx, &winner); err != nil {
				return err
			}
		}
		return nil
	}}
	// first is initially busier, so the acknowledged new write goes to second;
	// the independently inserted first copy must win the complete reread by UID.
	m, c, _ := allocationFixture(t, "", nil, hooks, allocationBox("first", "a", "0000000000000000000old"), allocationBox("second", "b"))
	r, e := createForTest(t.Context(), m, "000000000000000session", "serving-uid")
	if e != nil || r.Name != "first" {
		t.Fatalf("route used own ACK instead of actual winner: %+v %v", r, e)
	}
	if _, ok := readAllocationBindings(t, c, "second")["000000000000000session"]; ok {
		t.Fatal("post-ACK duplicate not converged")
	}
}

func TestSessionServiceAllocationWinnerValidationBeforeEachLoser(t *testing.T) {
	for _, change := range []string{"remove record", "replace UID", "advance RV", "three copies winner vanishes"} {
		t.Run(change, func(t *testing.T) {
			patches := []string{}
			changed := false
			hooks := interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches = append(patches, obj.GetName())
				if err := c.Patch(ctx, obj, patch, opts...); err != nil {
					return err
				}
				cut := obj.GetName() == "first"
				if change == "three copies winner vanishes" {
					cut = obj.GetName() == "second"
				}
				if cut && !changed {
					changed = true
					var box agents.Sandbox
					if err := c.Get(ctx, client.ObjectKey{Namespace: "team", Name: "first"}, &box); err != nil {
						return err
					}
					switch change {
					case "replace UID":
						if err := c.Delete(ctx, &box); err != nil {
							return err
						}
						box.UID = "replacement"
						box.ResourceVersion = ""
						return c.Create(ctx, &box)
					case "advance RV":
						box.Annotations["example.com/keep"] = "0000000000000000000new"
					default:
						box.Annotations[sessionbinding.BindingsAnnotation] = "{}"
					}
					return c.Update(ctx, &box)
				}
				return nil
			}}
			boxes := []*agents.Sandbox{allocationBox("first", "a", "000000000000000session"), allocationBox("second", "b", "000000000000000session")}
			if change == "three copies winner vanishes" {
				boxes = append(boxes, allocationBox("third", "c", "000000000000000session"))
			}
			m, c, _ := allocationFixture(t, "", nil, hooks, boxes...)
			parent, err := m.store.GetSessionService(t.Context(), "team", "serving-id")
			if err != nil {
				t.Fatal(err)
			}
			backend := m.store
			snapshot, err := backend.ReadSessionServiceSnapshot(t.Context(), parent)
			if err != nil {
				t.Fatal(err)
			}
			err = m.convergeSessionServiceSession(t.Context(), snapshot, "000000000000000session", servingCandidates(snapshot, "000000000000000session"))
			if change != "three copies winner vanishes" && !errors.Is(err, session.ErrSessionServiceConflict) {
				t.Fatalf("stale winner validation=%v", err)
			}
			if change == "three copies winner vanishes" {
				if _, ok := readAllocationBindings(t, c, "third")["000000000000000session"]; !ok {
					t.Fatal("deleted second loser after winner vanished")
				}
				if !reflect.DeepEqual(patches, []string{"first", "second"}) {
					t.Fatalf("unexpected cleanup sequence %v", patches)
				}
			} else {
				if _, ok := readAllocationBindings(t, c, "second")["000000000000000session"]; !ok {
					t.Fatal("stale winner deleted new loser")
				}
				if !reflect.DeepEqual(patches, []string{"first"}) {
					t.Fatalf("cleanup bypassed validation %v", patches)
				}
			}
		})
	}
}

func TestSessionServiceAllocationDuplicateOrderAndWarnings(t *testing.T) {
	for _, order := range [][]string{{"c", "b", "a"}, {"b", "a", "c"}, {"a", "c", "b"}} {
		t.Run(strings.Join(order, ""), func(t *testing.T) {
			boxes := []*agents.Sandbox{}
			for _, uid := range order {
				box := allocationBox(uid, uid, "000000000000000session", fmt.Sprintf("%022s", "keep_"+uid))
				box.Annotations["example.com/keep"] = "yes"
				boxes = append(boxes, box)
			}
			m, c, _ := allocationFixture(t, "", nil, interceptor.Funcs{}, boxes...)
			var logs strings.Builder
			ctx := klog.NewContext(t.Context(), funcr.NewJSON(func(line string) { logs.WriteString(line) }, funcr.Options{}))
			r, e := resolveForTest(ctx, m, "000000000000000session", "serving-uid")
			if e != nil || r.Name != "a" {
				t.Fatalf("winner=%+v %v", r, e)
			}
			for _, uid := range order {
				records := readAllocationBindings(t, c, uid)
				_, has := records["000000000000000session"]
				if has != (uid == "a") || records[fmt.Sprintf("%022s", "keep_"+uid)].LastRequestAt == "" {
					t.Fatalf("wrong convergence on %s: %+v", uid, records)
				}
			}
			for _, event := range []string{"WARNING: SessionBindingConflict", "WARNING: SessionBindingConverged", `"severity":"warning"`} {
				if !strings.Contains(logs.String(), event) {
					t.Fatalf("missing warning %s in %s", event, logs.String())
				}
			}
		})
	}
}

func TestSessionServiceAllocationWarningAfterFreshObservation(t *testing.T) {
	for _, sweep := range []bool{false, true} {
		t.Run(map[bool]string{false: "allocation retry", true: "next maintenance sweep"}[sweep], func(t *testing.T) {
			var stale atomic.Pointer[agents.Sandbox]
			var staleReads atomic.Int32
			hooks := interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if obj.GetName() != "second" {
						return c.Patch(ctx, obj, patch, opts...)
					}
					var before agents.Sandbox
					if err := c.Get(ctx, client.ObjectKeyFromObject(obj), &before); err != nil {
						return err
					}
					if err := c.Patch(ctx, obj, patch, opts...); err != nil {
						return err
					}
					// The loser ACK precedes informer observation of its new RV.
					stale.Store(&before)
					return nil
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if err := c.List(ctx, list, opts...); err != nil {
						return err
					}
					if members, ok := list.(*agents.SandboxList); ok {
						if before := stale.Swap(nil); before != nil {
							for i := range members.Items {
								if members.Items[i].UID == before.UID {
									members.Items[i] = *before.DeepCopy()
									staleReads.Add(1)
								}
							}
						}
					}
					return nil
				},
			}
			first := allocationBox("first", "a", "000000000000000session", "000000000000keep_first")
			second := allocationBox("second", "b", "000000000000000session", "00000000000keep_second")
			records, err := sessionbinding.Decode(second.Annotations)
			if err != nil {
				t.Fatal(err)
			}
			records["000000000000000session"] = sessionbinding.Record{LastRequestAt: "2026-11-10T01:00:00Z"}
			second.Annotations, err = sessionbinding.Encode(second.Annotations, records)
			if err != nil {
				t.Fatal(err)
			}
			primary, writer, _ := allocationFixture(t, "", nil, hooks, first, second)
			var logs strings.Builder
			ctx := klog.NewContext(t.Context(), funcr.NewJSON(func(line string) { logs.WriteString(line) }, funcr.Options{}))
			if sweep {
				if err := primary.Maintain(ctx); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(logs.String(), "WARNING: SessionBindingConverged") || len(primary.warnings) == 0 {
					t.Fatal("unavailable snapshot reported success or discarded conflict history")
				}
				if err := primary.Maintain(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				result, err := resolveForTest(ctx, primary, "000000000000000session", "serving-uid")
				if err != nil || result.Name != "first" {
					t.Fatalf("allocation after cache lag: %+v %v", result, err)
				}
			}
			if staleReads.Load() != 1 || !strings.Contains(logs.String(), "waiting for fresh member second") {
				t.Fatalf("post-ACK freshness failure was not exercised: %s", logs.String())
			}
			for _, event := range []string{"SessionBindingConflict", "SessionBindingConvergenceFailed", "SessionBindingConverged"} {
				if count := strings.Count(logs.String(), "WARNING: "+event); count != 1 {
					t.Fatalf("%s count=%d: %s", event, count, logs.String())
				}
			}
			if len(primary.warnings) != 0 {
				t.Fatal("completed warning episode retained state")
			}
			winner := readAllocationBindings(t, writer, "first")
			loser := readAllocationBindings(t, writer, "second")
			if winner["000000000000000session"].LastRequestAt != "2026-11-10T00:00:00Z" || winner["000000000000keep_first"].LastRequestAt == "" || loser["00000000000keep_second"].LastRequestAt == "" {
				t.Fatal("convergence changed winner activity or lost unrelated bindings")
			}
			if _, exists := loser["000000000000000session"]; exists {
				t.Fatal("loser binding survived convergence")
			}
		})
	}
}

func TestSessionServiceAllocationWarningAfterCooperatingAllocatorCleanup(t *testing.T) {
	var collaborator *Allocator
	var finishLoser func() error
	var collaboratorLogs strings.Builder
	ctx := klog.NewContext(t.Context(), funcr.NewJSON(func(line string) { collaboratorLogs.WriteString(line) }, funcr.Options{}))
	loserPatches := 0
	hooks := interceptor.Funcs{Patch: func(outer context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if obj.GetName() != "second" {
			return c.Patch(outer, obj, patch, opts...)
		}
		// This Allocator has confirmed its winner and validated the loser patch.
		// Let the other Allocator confirm the same winner before this patch commits.
		finishLoser = func() error {
			loserPatches++
			return c.Patch(outer, obj, patch, opts...)
		}
		defer func() { finishLoser = nil }()
		backend := collaborator.store
		parent, err := collaborator.store.GetSessionService(ctx, "team", "serving-id")
		if err != nil {
			return err
		}
		snapshot, err := backend.ReadSessionServiceSnapshot(ctx, parent)
		if err != nil {
			return err
		}
		return collaborator.convergeSessionServiceSession(ctx, snapshot, "000000000000000session", servingCandidates(snapshot, "000000000000000session"))
	}}
	primary, writer, _ := allocationFixture(t, "", nil, hooks, allocationBox("first", "a", "000000000000000session"), allocationBox("second", "b", "000000000000000session"))
	collaborator = allocatorForClient(t, writer.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if err := c.Patch(ctx, obj, patch, opts...); err != nil {
			return err
		}
		if obj.GetName() != "first" || finishLoser == nil {
			return fmt.Errorf("collaborating Allocator unexpectedly attempted loser cleanup")
		}
		// The first Allocator commits deletion after this winner ACK, so the fresh
		// snapshot has the confirmed winner alone and takes the early return.
		return finishLoser()
	}})
	result, err := resolveForTest(t.Context(), primary, "000000000000000session", "serving-uid")
	if err != nil || result.Name != "first" || loserPatches != 1 {
		t.Fatalf("cooperating cleanup: route=%+v patches=%d err=%v", result, loserPatches, err)
	}
	if count := strings.Count(collaboratorLogs.String(), "WARNING: SessionBindingConverged"); count != 1 {
		t.Fatalf("early unique observation did not finish warning episode: %s", collaboratorLogs.String())
	}
	if len(collaborator.warnings) != 0 || len(primary.warnings) != 0 {
		t.Fatal("cooperating cleanup retained warning state")
	}
}

func TestSessionServiceAllocationBudgetsAndLoweredLimit(t *testing.T) {
	one := int32(1)
	for _, tt := range []struct {
		name            string
		records         int
		limit           *int32
		reuse           bool
		largeAnnotation bool
		success         bool
	}{
		{name: "platform default 512", records: 512},
		{name: "platform default 511", records: 511, success: true},
		{name: "lowered M keeps existing", records: 2, limit: &one, reuse: true, success: true},
		{name: "lowered M blocks new", records: 2, limit: &one},
		{name: "all annotation budget", records: 1, largeAnnotation: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			box := allocationBox("first", "a")
			records := sessionbinding.Bindings{}
			for i := 0; i < tt.records; i++ {
				records[fmt.Sprintf("s%021d", i)] = sessionbinding.Record{LastRequestAt: "2026-11-10T00:00:00Z"}
			}
			var err error
			box.Annotations, err = sessionbinding.Encode(nil, records)
			if err != nil {
				t.Fatal("fixture budget", err)
			}
			if tt.largeAnnotation {
				box.Annotations["example.com/large"] = strings.Repeat("x", sessionbinding.MaxAnnotationBytes-len(sessionbinding.BindingsAnnotation)-len(box.Annotations[sessionbinding.BindingsAnnotation])-len("example.com/large")-40)
			}
			m, c, _ := allocationFixture(t, "", tt.limit, interceptor.Funcs{}, box)
			sid := "0000000000000000000new"
			if tt.reuse {
				sid = fmt.Sprintf("s%021d", 0)
			}
			operation := createForTest
			if tt.reuse {
				operation = resolveForTest
			}
			r, e := operation(t.Context(), m, sid, "serving-uid")
			if tt.success {
				if e != nil || r.Name != "first" {
					t.Fatalf("allocation=%+v %v", r, e)
				}
			} else {
				requireSessionError(t, e, session.ErrSessionServiceUnavailable, "SessionCapacityExhausted")
			}
			count := tt.records
			if tt.success && !tt.reuse {
				count++
			}
			if len(readAllocationBindings(t, c, "first")) != count {
				t.Fatal("capacity change evicted existing records")
			}
		})
	}
}

func TestSessionServiceAllocationConcurrentIndependentAllocators(t *testing.T) {
	// Hold both first writes after each primary has independently read its empty
	// member. CAS makes one conflict; its full reread must preserve the other SID.
	m, c, _ := allocationFixture(t, "", nil, interceptor.Funcs{}, allocationBox("first", "a"))
	var entered atomic.Int32
	both := make(chan struct{})
	var once sync.Once
	hook := interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
		if entered.Add(1) <= 2 {
			if entered.Load() == 2 {
				once.Do(func() { close(both) })
			}
			select {
			case <-both:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return c.Patch(ctx, obj, p, opts...)
	}}
	allocators := []*Allocator{allocatorForClient(t, c.(client.WithWatch), hook), allocatorForClient(t, c.(client.WithWatch), hook)}
	_ = m
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i, primary := range allocators {
		wg.Go(func() {
			_, e := createForTest(t.Context(), primary, fmt.Sprintf("s%021d", i), "serving-uid")
			results <- e
		})
	}
	wg.Wait()
	close(results)
	for e := range results {
		if e != nil {
			t.Fatal(e)
		}
	}
	if records := readAllocationBindings(t, c, "first"); len(records) != 2 {
		t.Fatalf("concurrent CAS lost records: %+v", records)
	}
}

func TestSessionServiceAllocationUIDAndDeletingParent(t *testing.T) {
	for _, tt := range []struct {
		name, expected string
		deleting       bool
		code           error
	}{
		{name: "token parent UID mismatch", expected: "old-parent", code: session.ErrSessionServiceNotFound},
		{name: "deleting parent", expected: "serving-uid", deleting: true, code: session.ErrSessionServiceUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, c, p := allocationFixture(t, "", nil, interceptor.Funcs{}, allocationBox("first", "a"))
			if tt.deleting {
				p.Finalizers = []string{"test/keep"}
				if err := c.Update(t.Context(), p); err != nil {
					t.Fatal(err)
				}
				if err := c.Delete(t.Context(), p); err != nil {
					t.Fatal(err)
				}
			}
			_, err := createForTest(t.Context(), m, "000000000000000session", tt.expected)
			if !errors.Is(err, tt.code) {
				t.Fatalf("admission=%v", err)
			}
			if len(readAllocationBindings(t, c, "first")) != 0 {
				t.Fatal("rejected parent acquired binding")
			}
		})
	}
}

func TestSessionServiceAllocationWaitingCallerCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	hooks := interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return c.Patch(ctx, obj, p, opts...)
	}}
	m, _, _ := allocationFixture(t, "", nil, hooks, allocationBox("first", "a"))
	done := make(chan error, 1)
	go func() {
		_, err := createForTest(t.Context(), m, "000000000000000session", "serving-uid")
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first call failed to reach write")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	_, err := createForTest(ctx, m, "000000000000000session", "serving-uid")
	if !errors.Is(err, session.ErrSessionServiceUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting cancellation=%v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSessionServiceMaintenanceSkipsDeletingSessionService(t *testing.T) {
	box := allocationBox("first", "a", "000000000000000session")
	m, c, parent := allocationFixture(t, "Strict", nil, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*agents.SandboxList); ok {
			t.Fatal("Allocator read members for obsolete deletion cleanup")
		}
		return c.List(ctx, list, opts...)
	}}, box)
	parent.Finalizers = []string{"test/keep"}
	if err := c.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	if err := m.Maintain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := readAllocationBindings(t, c, "first")["000000000000000session"]; !ok {
		t.Fatal("Allocator erased a binding during garbage collection")
	}
}

func TestSessionServiceStrictBindingEndsWithSandbox(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprintf("CR still exists=%t", held), func(t *testing.T) {
			first := allocationBox("first", "a")
			if held {
				first.Finalizers = []string{"test/keep"}
			}
			m, c, _ := allocationFixture(t, "Strict", nil, interceptor.Funcs{}, first, allocationBox("second", "b"))
			initial, err := createForTest(t.Context(), m, "000000000000000session", "serving-uid")
			if err != nil || initial.Name != "first" {
				t.Fatalf("initial allocation=%+v err=%v", initial, err)
			}
			var current agents.Sandbox
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(first), &current); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(current.Finalizers, first.Finalizers) {
				t.Fatalf("allocation added deletion protection: %v", current.Finalizers)
			}
			if err := c.Delete(t.Context(), &current); err != nil {
				t.Fatal(err)
			}
			result, err := resolveForTest(t.Context(), m, "000000000000000session", "serving-uid")
			if held {
				requireSessionError(t, err, session.ErrSessionServiceUnavailable, "BoundSandboxUnavailable")
				if _, ok := readAllocationBindings(t, c, "first")["000000000000000session"]; !ok {
					t.Fatal("Strict erased an existing binding")
				}
				if _, ok := readAllocationBindings(t, c, "second")["000000000000000session"]; ok {
					t.Fatal("Strict moved a binding while the old CR still exists")
				}
				return
			}
			if !errors.Is(err, session.ErrSessionNotFound) || result.Name != "" {
				t.Fatalf("missing member recreated session: %+v %v", result, err)
			}
			if _, ok := readAllocationBindings(t, c, "second")["000000000000000session"]; ok {
				t.Fatal("missing member recreated binding")
			}
		})
	}
}

func TestSessionServiceAllocationConcurrentFirstBindingsConverge(t *testing.T) {
	for _, order := range []string{"first", "second"} {
		t.Run(order, func(t *testing.T) {
			_, writer, _ := allocationFixture(t, "", nil, interceptor.Funcs{}, allocationBox("first", "a"), allocationBox("second", "b"))
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			applied := make(chan struct{}, 2)
			release := make(chan struct{})
			firstACK := make(chan string, 2)
			enteredFirstPatch := make(chan struct{}, 2)
			allowFirstPatches := make(chan struct{})
			allocators := []*Allocator{}
			for _, hidden := range []string{"first", "second"} {
				var lag atomic.Bool
				lag.Store(true)
				hooks := interceptor.Funcs{
					List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						if err := c.List(ctx, list, opts...); err != nil {
							return err
						}
						if members, ok := list.(*agents.SandboxList); ok && lag.Load() {
							for i := range members.Items {
								if members.Items[i].Name == hidden {
									members.Items[i].ResourceVersion = "998"
									members.Items[i].Status.Conditions = nil
								}
							}
						}
						return nil
					},
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
						if !lag.Load() {
							return c.Patch(ctx, obj, p, opts...)
						}
						// Both initial writes use their own current target UID/RV; each primary
						// has only an old informer observation of the other candidate.
						select {
						case enteredFirstPatch <- struct{}{}:
						case <-ctx.Done():
							return ctx.Err()
						}
						select {
						case <-allowFirstPatches:
						case <-ctx.Done():
							return ctx.Err()
						}
						if obj.GetName() != order {
							select {
							case <-firstACK:
							case <-ctx.Done():
								return ctx.Err()
							}
						}
						if err := c.Patch(ctx, obj, p, opts...); err != nil {
							return err
						}
						if obj.GetName() == order {
							firstACK <- order
						}
						lag.Store(false)
						applied <- struct{}{}
						select {
						case <-release:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					},
				}
				allocators = append(allocators, allocatorForClient(t, writer.(client.WithWatch), hooks))
			}
			done := make(chan error, 2)
			for _, primary := range allocators {
				go func() {
					r, e := createForTest(ctx, primary, "000000000000000session", "serving-uid")
					if e == nil && r.Name != "first" {
						e = fmt.Errorf("returned noncanonical route %s", r.Name)
					}
					done <- e
				}()
			}
			// Neither binding may appear until both Allocators have validated their
			// initial snapshots; otherwise the later one can take the reuse path.
			for range 2 {
				select {
				case <-enteredFirstPatch:
				case <-ctx.Done():
					t.Fatal("both Allocators did not enter their first binding patch", ctx.Err())
				}
			}
			close(allowFirstPatches)
			for range 2 {
				select {
				case <-applied:
				case <-ctx.Done():
					t.Fatal("first bindings did not both commit", ctx.Err())
				}
			}
			close(release)
			for range 2 {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			if _, ok := readAllocationBindings(t, writer, "first")["000000000000000session"]; !ok {
				t.Fatal("canonical first binding missing")
			}
			if _, ok := readAllocationBindings(t, writer, "second")["000000000000000session"]; ok {
				t.Fatal("concurrent noncanonical binding retained")
			}
		})
	}
}

func TestSessionServiceAllocationSkipsMemberAtStorageBudget(t *testing.T) {
	first := allocationBox("first", "a", "0000000000000000000old")
	records := sessionbinding.Bindings{"0000000000000000000old": {LastRequestAt: "2026-11-10T00:00:00Z"}}
	var err error
	first.Annotations, err = sessionbinding.Encode(nil, records)
	if err != nil {
		t.Fatal(err)
	}
	first.Annotations["example.com/large"] = strings.Repeat("x", sessionbinding.MaxAnnotationBytes-len(sessionbinding.BindingsAnnotation)-len(first.Annotations[sessionbinding.BindingsAnnotation])-len("example.com/large")-40)
	m, c, _ := allocationFixture(t, "", nil, interceptor.Funcs{}, first, allocationBox("second", "b", "00000000000000000000s0", "00000000000000000000s1"))
	r, e := createForTest(t.Context(), m, "000000000000000session", "serving-uid")
	if e != nil || r.Name != "second" {
		t.Fatalf("storage-full member masked usable capacity: %+v %v", r, e)
	}
	if len(readAllocationBindings(t, c, "first")) != 1 || len(readAllocationBindings(t, c, "second")) != 3 {
		t.Fatal("storage capacity retry lost records")
	}
}

// Newer loser activity does not change the winner's record or block cleanup.
func TestSessionServiceAllocationConvergenceKeepsWinnerRecord(t *testing.T) {
	for _, tt := range []struct {
		name, member string
		newDuplicate bool
	}{
		{name: "loser advances after winner ACK", member: "second"},
		{name: "later duplicate enters after winner ACK", member: "late", newDuplicate: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const latest = "2026-11-10T00:02:00Z"
			injected, winnerWritten := false, false
			hooks := interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if err := c.Patch(ctx, obj, patch, opts...); err != nil {
						return err
					}
					if obj.GetName() == "first" {
						winnerWritten = true
					}
					return nil
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, members := list.(*agents.SandboxList); members && winnerWritten && !injected {
						// This List is reached only after the backend received
						// the winner's ACK and registered its write watermark.
						injected = true
						var loser agents.Sandbox
						if err := c.Get(ctx, client.ObjectKey{Namespace: "team", Name: "second"}, &loser); err != nil {
							return err
						}
						records, err := sessionbinding.Decode(loser.Annotations)
						if err != nil {
							return err
						}
						record := records["000000000000000session"]
						record.LastRequestAt = latest
						records["000000000000000session"] = record
						if tt.newDuplicate {
							loser.Name = "late"
							loser.UID = "aa"
							loser.ResourceVersion = ""
							delete(records, "00000000000keep_second")
							records["0000000000000keep_late"] = sessionbinding.Record{LastRequestAt: "2026-11-10T00:00:00Z"}
						}
						loser.Annotations, err = sessionbinding.Encode(loser.Annotations, records)
						if err != nil {
							return err
						}
						if tt.newDuplicate {
							err = c.Create(ctx, &loser)
						} else {
							err = c.Update(ctx, &loser)
						}
						if err != nil {
							return err
						}
					}
					return c.List(ctx, list, opts...)
				},
			}
			m, c, _ := allocationFixture(t, "", nil, hooks, allocationBox("first", "a", "000000000000000session", "000000000000keep_first"), allocationBox("second", "b", "000000000000000session", "00000000000keep_second"))
			t.Cleanup(func() { expectations.ResourceVersionExpectationDelete(&metav1.ObjectMeta{UID: "aa"}) })
			backend := m.store
			parent, err := m.store.GetSessionService(t.Context(), "team", "serving-id")
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := backend.ReadSessionServiceSnapshot(t.Context(), parent)
			if err != nil {
				t.Fatal(err)
			}
			winnerBefore := readAllocationBindings(t, c, "first")["000000000000000session"]
			err = m.convergeSessionServiceSession(t.Context(), snapshot, "000000000000000session", servingCandidates(snapshot, "000000000000000session"))
			if err != nil {
				t.Fatalf("newer loser activity blocked convergence: %v", err)
			}
			if !injected {
				t.Fatal("newer loser activity was not injected after winner confirmation")
			}
			if _, bound := readAllocationBindings(t, c, tt.member)["000000000000000session"]; bound {
				t.Fatalf("newer loser binding survived cleanup: %s", tt.member)
			}
			// A later allocation finishes any remaining duplicate without changing
			// the winner's record or removing unrelated Session records.
			result, err := resolveForTest(t.Context(), m, "000000000000000session", "serving-uid")
			if err != nil || result.Name != "first" || !reflect.DeepEqual(readAllocationBindings(t, c, "first")["000000000000000session"], winnerBefore) {
				t.Fatalf("fresh convergence=%+v %v", result, err)
			}
			members := []string{"first", "second"}
			if tt.newDuplicate {
				members = append(members, "late")
			}
			for _, name := range members {
				records := readAllocationBindings(t, c, name)
				_, bound := records["000000000000000session"]
				if bound != (name == "first") || records[fmt.Sprintf("%022s", "keep_"+name)].LastRequestAt == "" {
					t.Fatalf("convergence lost identity/other records on %s: %+v", name, records)
				}
			}
		})
	}
}

func TestSessionServiceAllocationBackendLockCancellation(t *testing.T) {
	for _, shortCaller := range []bool{true, false} {
		name := "primary two second budget"
		if shortCaller {
			name = "short caller deadline"
		}
		t.Run(name, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			var patches atomic.Int32
			hooks := interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if patches.Add(1) == 1 {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return c.Patch(ctx, obj, patch, opts...)
			}}
			m, c, _ := allocationFixture(t, "", nil, hooks, allocationBox("first", "a", "00000000000000000other"), allocationBox("second", "b"))
			parent, err := m.store.GetSessionService(t.Context(), "team", "serving-id")
			if err != nil {
				t.Fatal(err)
			}
			holderCtx, holderCancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer holderCancel()
			closeDone := make(chan error, 1)
			go func() {
				closeDone <- m.store.(session.SessionServiceSessionBackend).CloseSessionServiceSession(holderCtx, parent, "00000000000000000other")
			}()
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			select {
			case <-entered:
			case <-holderCtx.Done():
				t.Fatal("Close did not acquire backend lock")
			}
			caller := t.Context()
			callerCancel := func() {}
			wait := 3 * time.Second
			if shortCaller {
				caller, callerCancel = context.WithTimeout(t.Context(), 30*time.Millisecond)
				wait = 500 * time.Millisecond
			}
			defer callerCancel()
			done := make(chan error, 1)
			go func() {
				_, e := createForTest(caller, m, "000000000000000session", "serving-uid")
				done <- e
			}()
			var allocationErr error
			returnedWhileHeld := false
			select {
			case allocationErr = <-done:
				returnedWhileHeld = true
			case <-time.After(wait):
			}
			once.Do(func() { close(release) })
			if err := <-closeDone; err != nil {
				t.Fatal("holding Close failed:", err)
			}
			if !returnedWhileHeld {
				allocationErr = <-done
			}
			if !returnedWhileHeld {
				t.Fatalf("Allocate ignored its deadline while unrelated Close held the backend lock: %v", allocationErr)
			}
			if !errors.Is(allocationErr, session.ErrSessionServiceUnavailable) || !errors.Is(allocationErr, context.DeadlineExceeded) {
				t.Fatalf("canceled allocation=%v", allocationErr)
			}
			if patches.Load() != 1 {
				t.Fatalf("canceled allocation patched after lock release: patches=%d", patches.Load())
			}
			if _, ok := readAllocationBindings(t, c, "second")["000000000000000session"]; ok {
				t.Fatal("canceled Allocate left a background binding")
			}
		})
	}
}

func TestSessionServiceMaintenanceSkipsHealthySessions(t *testing.T) {
	memberLists := 0
	hooks := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*agents.SandboxList); ok {
			memberLists++
		}
		return c.List(ctx, list, opts...)
	}}
	primary, writer, _ := allocationFixture(t, "", nil, hooks,
		allocationBox("first", "a", "0000000000000000000one", "0000000000000000000two"), allocationBox("second", "b", "00000000000000000three"))
	if err := primary.Maintain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if memberLists != 1 {
		t.Fatalf("healthy Sessions caused %d full membership reads, want one", memberLists)
	}
	if len(readAllocationBindings(t, writer, "first")) != 2 || len(readAllocationBindings(t, writer, "second")) != 1 {
		t.Fatal("maintenance changed healthy bindings")
	}
}

// This fixes a process-local scheduling cut, not a real Kubernetes race:
// another operation records a conflict after the sweep captures an empty List.
func TestSessionServiceWarningPruneAfterCapturedEmptySnapshot(t *testing.T) {
	var primary *Allocator
	captured := false
	recordedBeforeListReturn := false
	hooks := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if err := c.List(ctx, list, opts...); err != nil {
			return err
		}
		members, ok := list.(*agents.SandboxList)
		if !ok || captured {
			return nil
		}
		captured = true
		if len(members.Items) != 1 {
			return fmt.Errorf("expected one member in captured List")
		}
		records, err := sessionbinding.Decode(members.Items[0].Annotations)
		if err != nil {
			return err
		}
		if len(records) != 0 {
			return fmt.Errorf("captured List unexpectedly contains a binding")
		}
		// Durable state advances after capture. The returned List stays empty.
		current := members.Items[0].DeepCopy()
		current.Annotations, err = sessionbinding.Encode(current.Annotations, sessionbinding.Bindings{
			"000000000000000session": {LastRequestAt: "2026-11-10T00:00:00Z"},
		})
		if err != nil {
			return err
		}
		if err := c.Update(ctx, current); err != nil {
			return err
		}
		// Simulate only the real warning-entry cut of a concurrent duplicate
		// observer; this does not pretend that this fixture contained duplicates.
		parent := session.SessionService{Namespace: "team", Name: "serving", UID: "serving-uid", ID: "serving-id"}
		primary.servingWarning(ctx, "SessionBindingConflict", parent, "000000000000000session", nil, "concurrent duplicate observation after sweep List capture")
		primary.mu.Lock()
		_, recordedBeforeListReturn = primary.warnings["serving-uid/000000000000000session/SessionBindingConflict"]
		primary.mu.Unlock()
		return nil
	}}
	primary, writer, _ := allocationFixture(t, "", nil, hooks, allocationBox("first", "a"))
	var logs strings.Builder
	ctx := klog.NewContext(t.Context(), funcr.NewJSON(func(line string) { logs.WriteString(line) }, funcr.Options{}))
	if err := primary.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if !captured || !recordedBeforeListReturn {
		t.Fatal("did not exercise conflict after empty List capture")
	}
	primary.mu.Lock()
	_, retained := primary.warnings["serving-uid/000000000000000session/SessionBindingConflict"]
	primary.mu.Unlock()
	t.Logf("capturedNoSession=true conflictRecordedBeforeListReturn=%v conflictRetainedAfterSweep=%v convergedAfterSweep=%d", recordedBeforeListReturn, retained, strings.Count(logs.String(), "WARNING: SessionBindingConverged"))
	result, err := resolveForTest(ctx, primary, "000000000000000session", "serving-uid")
	if err != nil || result.Name != "first" {
		t.Fatalf("later real allocation did not read sole binding: %+v %v", result, err)
	}
	if records := readAllocationBindings(t, writer, "first"); len(records) != 1 || records["000000000000000session"].LastRequestAt == "" {
		t.Fatal("sole durable binding was not preserved")
	}
	if count := strings.Count(logs.String(), "WARNING: SessionBindingConflict"); count != 1 {
		t.Fatalf("expected real warning entry once, got %d", count)
	}
	if count := strings.Count(logs.String(), "WARNING: SessionBindingConverged"); count != 1 {
		t.Fatalf("fresh sole binding lost convergence warning after stale sweep prune: count=%d logs=%s", count, logs.String())
	}
}

func TestSessionServiceWarningLifecycle(t *testing.T) {
	primary, writer, parent := allocationFixture(t, "", nil, interceptor.Funcs{}, allocationBox("first", "a", "000000000000000session"), allocationBox("second", "b", "000000000000000session"))
	var logs strings.Builder
	ctx := klog.NewContext(t.Context(), funcr.NewJSON(func(line string) { logs.WriteString(line) }, funcr.Options{}))
	for round := 0; round < 2; round++ {
		if round > 0 {
			var box agents.Sandbox
			require.NoError(t, writer.Get(ctx, client.ObjectKey{Namespace: "team", Name: "second"}, &box))
			var err error
			box.Annotations, err = sessionbinding.Encode(box.Annotations, sessionbinding.Bindings{"000000000000000session": {LastRequestAt: "2026-11-10T00:00:00Z"}})
			require.NoError(t, err)
			require.NoError(t, writer.Update(ctx, &box))
			require.Eventually(t, func() bool {
				snapshot, err := primary.store.ReadSessionServiceSnapshot(ctx, session.SessionService{Namespace: "team", Name: "serving", UID: "serving-uid", ID: "serving-id"})
				return err == nil && len(servingCandidates(snapshot, "000000000000000session")) == 2
			}, time.Second, 10*time.Millisecond)
		}
		_, err := resolveForTest(ctx, primary, "000000000000000session", "serving-uid")
		require.NoError(t, err)
		require.Empty(t, primary.warnings)
	}
	require.Equal(t, 2, strings.Count(logs.String(), "WARNING: SessionBindingConflict"))
	require.Equal(t, 2, strings.Count(logs.String(), "WARNING: SessionBindingConverged"))
	primary.servingWarning(ctx, "SessionBindingConflict", session.SessionService{UID: "serving-uid"}, "000000000000000session", nil, "test")
	err := closeForTest(ctx, primary, "000000000000000session")
	require.NoError(t, err)
	require.Empty(t, primary.warnings)
	primary.servingWarning(ctx, "SessionBindingConflict", session.SessionService{UID: "serving-uid"}, "000000000000000session", nil, "test")
	parent.Finalizers = nil
	require.NoError(t, writer.Update(ctx, parent))
	require.NoError(t, writer.Delete(ctx, parent))
	require.Eventually(t, func() bool {
		parents, err := primary.store.ListSessionServices(ctx)
		return err == nil && len(parents) == 0
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, primary.Maintain(ctx))
	require.Empty(t, primary.warnings)
}

func TestSessionServiceWarningResolutionObservation(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		bound, priorConflict bool
		wantConverged        int
	}{
		{name: "ordinary single binding", bound: true},
		{name: "ordinary empty binding"},
		{name: "conflict followed by unique", bound: true, priorConflict: true, wantConverged: 1},
		{name: "conflict followed by empty", priorConflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := allocationBox("first", "a")
			if tc.bound {
				box = allocationBox("first", "a", "000000000000000session")
			} else {
				// Keep an empty observation from allocating a new binding.
				box.Status.Conditions = nil
			}
			primary, _, _ := allocationFixture(t, "", nil, interceptor.Funcs{}, box)
			var logs strings.Builder
			ctx := klog.NewContext(t.Context(), funcr.NewJSON(func(line string) { logs.WriteString(line) }, funcr.Options{}))
			if tc.priorConflict {
				primary.servingWarning(ctx, "SessionBindingConflict", session.SessionService{UID: "serving-uid"}, "000000000000000session", nil, "previous duplicate observation")
			}
			_, err := createForTest(ctx, primary, "000000000000000session", "serving-uid")
			if tc.bound {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "SessionCapacityExhausted")
			}
			require.Equal(t, tc.wantConverged, strings.Count(logs.String(), "WARNING: SessionBindingConverged"))
			require.Empty(t, primary.warnings)
		})
	}
}

// Resolve through the real Store to exercise the same admission input as callers.
func createForTest(ctx context.Context, a *Allocator, sid, uid string) (sandboxroute.Route, error) {
	parent, err := a.store.GetSessionService(ctx, "team", "serving-id")
	if err != nil {
		return sandboxroute.Route{}, backendError(err)
	}
	parent.UID = uid
	return a.Create(ctx, parent, sid)
}

func resolveForTest(ctx context.Context, a *Allocator, sid, uid string) (sandboxroute.Route, error) {
	parent, err := a.store.GetSessionService(ctx, "team", "serving-id")
	if err != nil {
		return sandboxroute.Route{}, backendError(err)
	}
	parent.UID = uid
	return a.Resolve(ctx, parent, sid)
}

func closeForTest(ctx context.Context, a *Allocator, sid string) error {
	parent, err := a.store.GetSessionService(ctx, "team", "serving-id")
	if err != nil {
		return err
	}
	if err := a.store.(session.SessionServiceSessionBackend).CloseSessionServiceSession(ctx, parent, sid); err != nil {
		return err
	}
	a.ForgetSession(parent.UID, sid)
	return nil
}

func requireSessionError(t *testing.T, err error, kind error, reason string) {
	t.Helper()
	var domain *session.Error
	if !errors.Is(err, kind) || !errors.As(err, &domain) || domain.Reason != reason {
		t.Fatalf("error=%v want kind=%v reason=%s", err, kind, reason)
	}
}
