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
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agents "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandboxroute"
	session "github.com/openkruise/agents/pkg/sessionservice"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
	"github.com/openkruise/agents/pkg/sessionservice/kubernetes"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func allocationFixture(t *testing.T, affinity string, limit *int32, hooks interceptor.Funcs, boxes ...*agents.Sandbox) (*Allocator, client.Client, *agents.SessionService) {
	t.Helper()
	p := &agents.SessionService{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "serving", UID: "serving-uid"}, Spec: agents.SessionServiceSpec{SessionPolicy: agents.SessionServiceSessionPolicy{Affinity: affinity, MaxSessionsPerSandbox: limit}}}
	objects := []client.Object{p}
	for _, b := range boxes {
		b.Namespace = "team"
		b.Labels = map[string]string{sessionbinding.SessionServiceUIDLabel: string(p.UID)}
		b.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(p, agents.GroupVersion.WithKind("SessionService"))}
		objects = append(objects, b)
		t.Cleanup(func() { expectations.ResourceVersionExpectationDelete(b) })
	}
	scheme := runtime.NewScheme()
	if err := agents.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	writer := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	store := kubernetes.NewStore(interceptor.NewClient(writer, hooks), func() bool { return true })
	return New(store), writer, p
}

func allocationBox(name, uid string, ids ...string) *agents.Sandbox {
	b := &agents.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)}, Status: agents.SandboxStatus{Phase: agents.SandboxRunning, PodInfo: agents.PodInfo{PodIP: "10.0.0.1"}, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}}}
	records := sessionbinding.Bindings{}
	for _, id := range ids {
		records[id] = sessionbinding.Record{CreatedAt: "2026-11-10T00:00:00Z"}
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
		{name: "best effort unavailable requests move", ready: false, bound: true, reason: "SessionRebinding"},
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
		{name: "best effort existing expired requests move", ready: true, expired: true, bound: true, reason: "SessionRebinding", projectedState: "dead"},
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
				parent, err := m.store.GetSessionService(t.Context(), "team", "serving")
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
				if tt.reason == "SessionRebinding" {
					require.Equal(t, sessionbinding.Moving, remaining["000000000000000session"].Phase)
					require.Equal(t, records["000000000000000session"].CreatedAt, remaining["000000000000000session"].CreatedAt)
				} else if bound && !reflect.DeepEqual(remaining["000000000000000session"], records["000000000000000session"]) {
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

func TestSessionServiceBestEffortMarksMoving(t *testing.T) {
	for _, outcome := range []string{"success", "unknown committed", "unknown uncommitted", "rejected"} {
		t.Run(outcome, func(t *testing.T) {
			const sid = "000000000000000session"
			old := allocationBox("old", "a", sid, "00000000000000000other")
			old.Status.Conditions = nil
			original, err := sessionbinding.Decode(old.Annotations)
			require.NoError(t, err)
			var patches []string
			a, c, _ := allocationFixture(t, "BestEffort", nil, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches = append(patches, obj.GetName())
				raw, err := patch.Data(obj)
				require.NoError(t, err)
				var proposed struct {
					Metadata metav1.ObjectMeta `json:"metadata"`
				}
				require.NoError(t, json.Unmarshal(raw, &proposed))
				require.Equal(t, old.UID, proposed.Metadata.UID)
				require.Equal(t, obj.GetResourceVersion(), proposed.Metadata.ResourceVersion)
				records, err := sessionbinding.Decode(proposed.Metadata.Annotations)
				require.NoError(t, err)
				expected := original[sid]
				expected.Phase = sessionbinding.Moving
				require.Equal(t, sessionbinding.Bindings{sid: expected, "00000000000000000other": original["00000000000000000other"]}, records)
				if outcome == "rejected" {
					return apierrors.NewForbidden(agents.Resource("sandboxes"), obj.GetName(), errors.New("denied"))
				}
				if outcome != "unknown uncommitted" {
					if err := c.Patch(ctx, obj, patch, opts...); err != nil {
						return err
					}
				}
				if strings.HasPrefix(outcome, "unknown") {
					return context.DeadlineExceeded
				}
				return nil
			}}, old, allocationBox("new", "b"))
			ctx, cancel := context.WithTimeout(t.Context(), 80*time.Millisecond)
			defer cancel()
			route, err := resolveForTest(ctx, a, sid, "serving-uid")
			require.ErrorIs(t, err, session.ErrSessionServiceUnavailable)
			require.Empty(t, route)
			require.NotEmpty(t, patches)
			for _, name := range patches {
				require.Equal(t, "old", name)
			}
			// A committed Moving is observed and stops writes. An uncommitted
			// unknown result may retry the same source from a fresh snapshot.
			if outcome == "success" || outcome == "unknown committed" {
				require.Len(t, patches, 1)
			}
			records := readAllocationBindings(t, c, "old")
			require.Contains(t, records, sid)
			require.Contains(t, records, "00000000000000000other")
			moving := outcome == "success" || outcome == "unknown committed"
			require.Equal(t, moving, records[sid].Phase == sessionbinding.Moving)
			require.Empty(t, readAllocationBindings(t, c, "new"))
		})
	}
}

func allocatorForClient(t *testing.T, c client.WithWatch, hooks interceptor.Funcs) *Allocator {
	t.Helper()
	return New(kubernetes.NewStore(interceptor.NewClient(c, hooks), func() bool { return true }))
}

func TestSessionServiceAllocationUnknownNewPinsTarget(t *testing.T) {
	for _, tt := range []struct {
		name       string
		commit     bool
		staleReads int
		change     string
	}{
		{name: "committed and visible", commit: true},
		{name: "committed with delayed cache visibility", commit: true, staleReads: 3},
		{name: "unchanged cache waits for deadline"},
		{name: "advanced version without binding fails", change: "advance"},
		{name: "disappeared target fails", change: "delete"},
		{name: "same name with new UID fails", change: "replace"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			patches := []string{}
			var cached agents.SandboxList
			readsAfterWrite := 0
			hooks := interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if err := c.List(ctx, list, opts...); err != nil {
						return err
					}
					if boxes, ok := list.(*agents.SandboxList); ok && len(patches) > 0 {
						readsAfterWrite++
						if readsAfterWrite <= tt.staleReads {
							cached.DeepCopyInto(boxes)
						}
					}
					return nil
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches = append(patches, obj.GetName())
					if err := c.List(ctx, &cached, client.InNamespace("team")); err != nil {
						return err
					}
					if tt.commit {
						if err := c.Patch(ctx, obj, patch, opts...); err != nil {
							return err
						}
					}
					switch tt.change {
					case "advance":
						updated := obj.DeepCopyObject().(client.Object)
						updated.SetAnnotations(map[string]string{"unrelated": "update"})
						if err := c.Update(ctx, updated); err != nil {
							return err
						}
					case "delete", "replace":
						if err := c.Delete(ctx, obj); err != nil {
							return err
						}
						if tt.change == "replace" {
							replacement := obj.DeepCopyObject().(client.Object)
							replacement.SetUID("replacement-uid")
							replacement.SetResourceVersion("")
							if err := c.Create(ctx, replacement); err != nil {
								return err
							}
						}
					}
					return context.DeadlineExceeded // The caller cannot tell whether the PATCH committed.
				},
			}
			m, c, _ := allocationFixture(t, "", nil, hooks, allocationBox("first", "a"), allocationBox("second", "b"), allocationBox("third", "c"))
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			result, err := createForTest(ctx, m, "000000000000000session", "serving-uid")
			if tt.commit {
				require.NoError(t, err)
				require.Equal(t, "first", result.Name)
				require.Greater(t, readsAfterWrite, tt.staleReads)
			} else {
				require.ErrorIs(t, err, session.ErrSessionServiceUnavailable)
				if tt.change == "" {
					require.ErrorIs(t, err, context.DeadlineExceeded)
					require.Greater(t, readsAfterWrite, 1)
				} else {
					require.NoError(t, ctx.Err(), "definitive observations should fail before the request deadline")
				}
			}
			require.Equal(t, []string{"first"}, patches, "an uncertain write must never select another target or be replayed")
			require.Empty(t, readAllocationBindings(t, c, "second"))
			require.Empty(t, readAllocationBindings(t, c, "third"))
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
			var first agents.Sandbox
			if err := c.Get(ctx, client.ObjectKey{Namespace: "team", Name: "first"}, &first); err != nil {
				return err
			}
			first.Annotations[sessionbinding.BindingsAnnotation] = `{"000000000000000session":{"createdAt":"2026-11-10T00:00:00Z"}}`
			if err := c.Update(ctx, &first); err != nil {
				return err
			}
		}
		return nil
	}}
	// first is initially busier, so the acknowledged new write goes to second;
	// the independently inserted first copy must cause an error on the full reread.
	m, c, _ := allocationFixture(t, "", nil, hooks, allocationBox("first", "a", "0000000000000000000old"), allocationBox("second", "b"))
	r, e := createForTest(t.Context(), m, "000000000000000session", "serving-uid")
	requireSessionError(t, e, session.ErrSessionServiceDataAmbiguous, "SessionBindingConflict")
	require.Empty(t, r)
	for _, name := range []string{"first", "second"} {
		require.Contains(t, readAllocationBindings(t, c, name), "000000000000000session")
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
				records[fmt.Sprintf("s%021d", i)] = sessionbinding.Record{CreatedAt: "2026-11-10T00:00:00Z"}
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
		{name: "admitted parent UID mismatch", expected: "old-parent", code: session.ErrSessionServiceNotFound},
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

func TestSessionServiceAllocationConcurrentFirstBindingsRejectDuplicates(t *testing.T) {
	for _, order := range []string{"first", "second"} {
		t.Run(order, func(t *testing.T) {
			_, writer, _ := allocationFixture(t, "", nil, interceptor.Funcs{}, allocationBox("first", "a"), allocationBox("second", "b"))
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
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
					_, e := createForTest(ctx, primary, "000000000000000session", "serving-uid")
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
				requireSessionError(t, <-done, session.ErrSessionServiceDataAmbiguous, "SessionBindingConflict")
			}
			if _, ok := readAllocationBindings(t, writer, "first")["000000000000000session"]; !ok {
				t.Fatal("first binding unexpectedly removed")
			}
			if _, ok := readAllocationBindings(t, writer, "second")["000000000000000session"]; !ok {
				t.Fatal("second binding unexpectedly removed")
			}
		})
	}
}

func TestSessionServiceAllocationSkipsMemberAtStorageBudget(t *testing.T) {
	first := allocationBox("first", "a", "0000000000000000000old")
	records := sessionbinding.Bindings{"0000000000000000000old": {CreatedAt: "2026-11-10T00:00:00Z"}}
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

func TestSessionServiceAllocationBackendLockCancellation(t *testing.T) {
	for _, shortCaller := range []bool{true, false} {
		name := "primary ten second budget"
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
			parent, err := m.store.GetSessionService(t.Context(), "team", "serving")
			if err != nil {
				t.Fatal(err)
			}
			holderCtx, holderCancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer holderCancel()
			closeDone := make(chan error, 1)
			go func() {
				closeDone <- func() error {
					snap, e := m.store.ReadSessionServiceSnapshot(holderCtx, parent)
					if e != nil {
						return e
					}
					_, e = m.store.ReplaceSessionServiceBindings(holderCtx, snap, snap.Members[0], snap.Members[0].Bindings)
					return e
				}()
			}()
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			select {
			case <-entered:
			case <-holderCtx.Done():
				t.Fatal("Binding write did not acquire backend lock")
			}
			caller := t.Context()
			callerCancel := func() {}
			wait := 12 * time.Second
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
				t.Fatal("holding binding write failed:", err)
			}
			if !returnedWhileHeld {
				allocationErr = <-done
			}
			if !returnedWhileHeld {
				t.Fatalf("Allocate ignored its deadline while unrelated binding write held the backend lock: %v", allocationErr)
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

func createForTest(ctx context.Context, a *Allocator, sid, uid string) (sandboxroute.Route, error) {
	parent, err := a.store.GetSessionService(ctx, "team", "serving")
	if err != nil {
		return sandboxroute.Route{}, backendError(err)
	}
	parent.UID = uid
	return a.Create(ctx, parent, sid)
}

func resolveForTest(ctx context.Context, a *Allocator, sid, uid string) (sandboxroute.Route, error) {
	parent, err := a.store.GetSessionService(ctx, "team", "serving")
	if err != nil {
		return sandboxroute.Route{}, backendError(err)
	}
	parent.UID = uid
	return a.Resolve(ctx, parent, sid)
}

func requireSessionError(t *testing.T, err error, kind error, reason string) {
	t.Helper()
	var domain *session.Error
	if !errors.Is(err, kind) || !errors.As(err, &domain) || domain.Reason != reason {
		t.Fatalf("error=%v want kind=%v reason=%s", err, kind, reason)
	}
}

// Both existing duplicates and newly observed collisions fail without repair writes.
func TestSessionServiceAllocationDuplicateBindingsRejected(t *testing.T) {
	for _, affinity := range []string{"Strict", "BestEffort"} {
		for _, count := range []int{2, 3} {
			for _, create := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/create=%t", affinity, count, create), func(t *testing.T) {
					boxes := make([]*agents.Sandbox, 0, count)
					for i := 0; i < count; i++ {
						box := allocationBox(fmt.Sprintf("member-%d", i), fmt.Sprintf("uid-%d", count-i), "000000000000000session", fmt.Sprintf("keep%018d", i))
						box.Annotations["example.com/keep"] = "unchanged"
						boxes = append(boxes, box)
					}
					patches := 0
					a, c, _ := allocationFixture(t, affinity, nil, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						patches++
						return c.Patch(ctx, obj, patch, opts...)
					}}, boxes...)
					// Repeated requests must neither arbitrate nor alter the binding records.
					for range 2 {
						var route sandboxroute.Route
						var err error
						if create {
							route, err = createForTest(t.Context(), a, "000000000000000session", "serving-uid")
						} else {
							route, err = resolveForTest(t.Context(), a, "000000000000000session", "serving-uid")
						}
						requireSessionError(t, err, session.ErrSessionServiceDataAmbiguous, "SessionBindingConflict")
						require.Empty(t, route)
					}
					require.Zero(t, patches)
					for _, box := range boxes {
						var current agents.Sandbox
						require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(box), &current))
						require.Equal(t, box.Annotations, current.Annotations)
					}
				})
			}
		}
	}
}

// Two Gateway processes may compete to request the same migration, but neither
// may unlink the source or create a replacement binding.
func TestSessionServiceBestEffortRebindAcrossAllocators(t *testing.T) {
	const sid = "000000000000000session"
	old := allocationBox("old", "a", sid)
	old.Status.Conditions = nil
	_, writer, _ := allocationFixture(t, "BestEffort", nil, interceptor.Funcs{}, old, allocationBox("new", "b"), allocationBox("third", "c"))
	var mu sync.Mutex
	var patches []string
	hooks := interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		mu.Lock()
		patches = append(patches, obj.GetName())
		mu.Unlock()
		return c.Patch(ctx, obj, patch, opts...)
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	results := make(chan error, 2)
	for range 2 {
		a := allocatorForClient(t, writer.(client.WithWatch), hooks)
		go func() { _, err := resolveForTest(ctx, a, sid, "serving-uid"); results <- err }()
	}
	for range 2 {
		require.ErrorIs(t, <-results, session.ErrSessionServiceUnavailable)
	}
	for _, name := range patches {
		require.Equal(t, "old", name)
	}
	require.Equal(t, sessionbinding.Moving, readAllocationBindings(t, writer, "old")[sid].Phase)
	require.Empty(t, readAllocationBindings(t, writer, "new"))
	require.Empty(t, readAllocationBindings(t, writer, "third"))
}

func TestSessionServiceMovingIntermediateState(t *testing.T) {
	const sid = "000000000000000session"
	for _, tc := range []struct {
		name         string
		targetUID    string
		extraActive  bool
		targetMoving bool
		converge     bool
		cancel       bool
	}{
		{name: "matching target waits", targetUID: "b"},
		{name: "previous target waits", targetUID: "previous"},
		{name: "multiple active wait", targetUID: "b", extraActive: true},
		{name: "retarget converges", targetUID: "previous", converge: true},
		{name: "duplicates converge", targetUID: "b", extraActive: true, converge: true},
		{name: "caller cancellation", targetUID: "previous", cancel: true},
		{name: "two moving wait", targetUID: "b", targetMoving: true},
		{name: "two moving converge", targetUID: "b", targetMoving: true, converge: true},
		{name: "two moving cancellation", targetUID: "b", targetMoving: true, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, target := allocationBox("source", "a", sid), allocationBox("target", "b", sid)
			records, err := sessionbinding.Decode(source.Annotations)
			require.NoError(t, err)
			record := records[sid]
			record.Phase = sessionbinding.Moving
			record.TargetSandboxUID = tc.targetUID
			records[sid] = record
			source.Annotations, err = sessionbinding.Encode(source.Annotations, records)
			require.NoError(t, err)
			if tc.targetMoving {
				targetRecords := sessionbinding.Bindings{sid: {CreatedAt: record.CreatedAt, Phase: sessionbinding.Moving, TargetSandboxUID: "a"}}
				target.Annotations, err = sessionbinding.Encode(target.Annotations, targetRecords)
				require.NoError(t, err)
			}
			boxes := []*agents.Sandbox{source, target}
			if tc.extraActive {
				boxes = append(boxes, allocationBox("third", "c", sid))
			}
			reads, patches := 0, 0
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			a, c, _ := allocationFixture(t, "BestEffort", nil, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if err := c.List(ctx, list, opts...); err != nil {
						return err
					}
					if members, ok := list.(*agents.SandboxList); ok {
						reads++
						if reads == 2 && tc.cancel {
							cancel()
						}
						if reads >= 3 && tc.converge {
							for i := range members.Items {
								if members.Items[i].Name == "target" {
									if tc.targetMoving {
										annotations, err := sessionbinding.Encode(members.Items[i].Annotations, sessionbinding.Bindings{sid: {CreatedAt: record.CreatedAt}})
										if err != nil {
											return err
										}
										members.Items[i].Annotations = annotations
									}
									continue
								}
								annotations, err := sessionbinding.Encode(members.Items[i].Annotations, sessionbinding.Bindings{})
								if err != nil {
									return err
								}
								members.Items[i].Annotations = annotations
							}
						}
					}
					return nil
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					return c.Patch(ctx, obj, patch, opts...)
				},
			}, boxes...)
			route, err := resolveForTest(ctx, a, sid, "serving-uid")
			if tc.converge {
				require.NoError(t, err)
				require.Equal(t, "target", route.Name)
				require.GreaterOrEqual(t, reads, 3)
			} else {
				requireSessionError(t, err, session.ErrSessionServiceUnavailable, "SessionRebinding")
				require.Empty(t, route)
				if tc.cancel {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.ErrorIs(t, err, context.DeadlineExceeded)
				}
			}
			require.Zero(t, patches)
			require.Contains(t, readAllocationBindings(t, c, "source"), sid)
			require.Contains(t, readAllocationBindings(t, c, "target"), sid)
		})
	}
}

func TestSessionServiceMissingBindingReturnsImmediately(t *testing.T) {
	const sid = "000000000000000session"
	for _, observedMoving := range []bool{false, true} {
		t.Run(fmt.Sprintf("observedMoving=%v", observedMoving), func(t *testing.T) {
			reads, patches := 0, 0
			a, _, _ := allocationFixture(t, "BestEffort", nil, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if err := c.List(ctx, list, opts...); err != nil {
						return err
					}
					if members, ok := list.(*agents.SandboxList); ok {
						reads++
						for i := range members.Items {
							records := sessionbinding.Bindings{}
							if observedMoving && reads == 1 {
								records[sid] = sessionbinding.Record{CreatedAt: time.Now().UTC().Format(time.RFC3339), Phase: sessionbinding.Moving}
							}
							annotations, err := sessionbinding.Encode(members.Items[i].Annotations, records)
							if err != nil {
								return err
							}
							members.Items[i].Annotations = annotations
						}
					}
					return nil
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					return c.Patch(ctx, obj, patch, opts...)
				},
			}, allocationBox("target", "a", sid))
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			route, err := resolveForTest(ctx, a, sid, "serving-uid")
			require.ErrorIs(t, err, session.ErrSessionNotFound)
			require.NoError(t, ctx.Err())
			require.Empty(t, route)
			expectedReads := 1
			if observedMoving {
				expectedReads = 2
			}
			require.Equal(t, expectedReads, reads)
			require.Zero(t, patches)
		})
	}
}

func TestSessionCreationExpiry(t *testing.T) {
	const sid = "000000000000000session"
	for _, mode := range []string{"expired Active", "expired Moving", "disabled", "not expired"} {
		t.Run(mode, func(t *testing.T) {
			box := allocationBox("source", "source-uid")
			record := sessionbinding.Record{CreatedAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)}
			if mode == "not expired" {
				record.CreatedAt = time.Now().UTC().Format(time.RFC3339)
			}
			if mode == "expired Moving" {
				record.Phase = sessionbinding.Moving
				record.TargetSandboxUID = "missing"
			}
			var err error
			box.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{sid: record})
			require.NoError(t, err)
			patches := 0
			allocator, c, parent := allocationFixture(t, "BestEffort", nil, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches++
				return c.Patch(ctx, obj, patch, opts...)
			}}, box)
			if mode != "disabled" {
				parent.Spec.SessionPolicy.ExpireAfterCreation = "1h"
			}
			require.NoError(t, c.Update(t.Context(), parent))
			admitted, err := allocator.store.GetSessionService(t.Context(), parent.Namespace, parent.Name)
			require.NoError(t, err)
			route, err := allocator.Resolve(t.Context(), admitted, sid)
			if strings.HasPrefix(mode, "expired") {
				require.ErrorIs(t, err, session.ErrSessionNotFound)
			} else {
				require.NoError(t, err)
				require.Equal(t, "source", route.Name)
			}
			require.Zero(t, patches)
			require.Equal(t, record, readAllocationBindings(t, c, "source")[sid])
		})
	}
}
