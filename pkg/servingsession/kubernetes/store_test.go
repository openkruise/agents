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

package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agents "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/sandboxid"
	session "github.com/openkruise/agents/pkg/servingsession"
	sessionbinding "github.com/openkruise/agents/pkg/servingsession/binding"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

func newServingSessions(c client.Client, healthy func() bool) *Store {
	return NewStore(c, healthy, Options{})
}

func servingSessionFixture(t *testing.T) (*agents.SandboxServing, *agents.Sandbox, client.WithWatch) {
	t.Helper()
	parent := &agents.SandboxServing{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "serving", UID: "serving-uid", Labels: map[string]string{sessionbinding.ServingIDLabel: "serving-id"}}}
	box := &agents.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "member", UID: "member-uid", Labels: map[string]string{sessionbinding.ServingUIDLabel: string(parent.UID)}, Annotations: map[string]string{"example.com/keep": "yes", sessionbinding.BindingsAnnotation: `{"a":{"state":"Active","lastRequestAt":"2026-11-10T00:00:00Z"},"b":{"state":"Active","lastRequestAt":"2026-11-10T00:00:00Z"}}`}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, agents.GroupVersion.WithKind("SandboxServing"))}}, Status: agents.SandboxStatus{Phase: agents.SandboxFailed}}
	scheme := runtime.NewScheme()
	if err := agents.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithIndex(&agents.SandboxServing{}, cache.IndexServingID, func(obj client.Object) []string { return []string{obj.GetLabels()[sessionbinding.ServingIDLabel]} }).WithObjects(parent, box).Build()
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(parent), parent); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(box), box); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		expectations.ResourceVersionExpectationDelete(box)
		// Fake clients have no informer to retire expectations for extra members.
		var members agents.SandboxList
		if err := c.List(context.Background(), &members); err != nil {
			t.Error(err)
			return
		}
		for i := range members.Items {
			expectations.ResourceVersionExpectationDelete(&members.Items[i])
		}
	})
	return parent, box, c
}

func TestServingSessionsOwnershipAndReadiness(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*agents.Sandbox)
		want   int
	}{
		{name: "unclaimed terminal", want: 2},
		{name: "foreign UID", mutate: func(b *agents.Sandbox) { b.OwnerReferences[0].UID = "other" }},
		{name: "foreign name", mutate: func(b *agents.Sandbox) { b.OwnerReferences[0].Name = "other" }},
		{name: "foreign label", mutate: func(b *agents.Sandbox) { b.Labels[sessionbinding.ServingUIDLabel] = "other" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			parent, box, c := servingSessionFixture(t)
			if tt.mutate != nil {
				tt.mutate(box)
				if err := c.Update(t.Context(), box); err != nil {
					t.Fatal(err)
				}
			}
			healthy := false
			backend := newServingSessions(c, func() bool { return healthy })
			if _, err := backend.GetServing(t.Context(), "team", "serving-id"); !errors.Is(err, session.ErrServingUnavailable) {
				t.Fatal("unsynced cache became 404")
			}
			healthy = true
			serving, err := backend.GetServing(t.Context(), "team", "serving-id")
			if err != nil {
				t.Fatal(err)
			}
			sessions, err := backend.ReadServingSessions(t.Context(), serving)
			if err != nil {
				t.Fatal(err)
			}
			if len(sessions) != tt.want {
				t.Fatalf("ownership filter returned %d, want %d", len(sessions), tt.want)
			}
			if tt.want > 0 && sessions[0].SandboxID != sandboxid.Resolve(box) {
				t.Fatal("member identity did not use sandboxid.Resolve")
			}
			if _, err := backend.GetServing(t.Context(), "other", "serving-id"); !errors.Is(err, session.ErrServingNotFound) {
				t.Fatal("cross-namespace Serving exposed")
			}
			replacement := parent.DeepCopy()
			replacement.UID = "new-parent"
			if err := c.Delete(t.Context(), parent); err != nil {
				t.Fatal(err)
			}
			replacement.ResourceVersion = ""
			if err := c.Create(t.Context(), replacement); err != nil {
				t.Fatal(err)
			}
			if _, err := backend.ReadServingSessions(t.Context(), serving); !errors.Is(err, session.ErrServingNotFound) {
				t.Fatal("new parent UID inherited old session scope")
			}
		})
	}
}

func TestServingCloseWaitsForMemberExpectation(t *testing.T) {
	parent, box, base := servingSessionFixture(t)
	stale := box.DeepCopy()
	lag := true
	lagged := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if err := c.List(ctx, list, opts...); err != nil {
			return err
		}
		if members, ok := list.(*agents.SandboxList); ok && lag {
			members.Items = []agents.Sandbox{*stale.DeepCopy()}
		}
		return nil
	}})
	backend := newServingSessions(lagged, func() bool { return true })
	serving := servingValue(parent)
	if err := backend.CloseServingSession(t.Context(), serving, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ReadServingSessions(t.Context(), serving); !errors.Is(err, session.ErrServingUnavailable) {
		t.Fatalf("stale member returned a successful Session snapshot: %v", err)
	}
	if err := backend.CloseServingSession(t.Context(), serving, "b"); !errors.Is(err, session.ErrServingUnavailable) {
		t.Fatalf("Close used a member older than the acknowledged write: %v", err)
	}
	// The expectation is shared by backend instances in the same process.
	other := newServingSessions(lagged, func() bool { return true })
	if _, err := other.ReadServingSessions(t.Context(), serving); !errors.Is(err, session.ErrServingUnavailable) {
		t.Fatalf("another backend ignored the shared member expectation: %v", err)
	}
	lag = false
	sessions, err := backend.ReadServingSessions(t.Context(), serving)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].SessionID != "b" {
		t.Fatalf("old cache revived closed record: %+v", sessions)
	}
	if err := backend.CloseServingSession(t.Context(), serving, "a"); err != nil {
		t.Fatal(err)
	}
	var remaining agents.Sandbox
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(box), &remaining); err != nil {
		t.Fatal(err)
	}
	records, err := sessionbinding.Decode(remaining.Annotations)
	if err != nil {
		t.Fatal(err)
	}
	if records["b"].State != sessionbinding.Active || records["b"].LastRequestAt != "2026-11-10T00:00:00Z" || remaining.Annotations["example.com/keep"] != "yes" {
		t.Fatal("Close lost unrelated fields or remaining record metadata")
	}
	if err := backend.CloseServingSession(t.Context(), serving, "b"); err != nil {
		t.Fatal(err)
	}
	var current agents.Sandbox
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(box), &current); err != nil {
		t.Fatal(err)
	}
	if current.Annotations["example.com/keep"] != "yes" || current.Annotations[sessionbinding.BindingsAnnotation] != "{}" {
		t.Fatalf("close lost unrelated data: %+v", current.Annotations)
	}
}

func TestServingCloseRejectsCapturedSnapshotAfterObserve(t *testing.T) {
	for _, externalObserve := range []bool{false, true} {
		name := "expectation-retained-control"
		if externalObserve {
			name = "fresh-observed-after-stale-list-captured"
		}
		t.Run(name, func(t *testing.T) {
			parent, box, base := servingSessionFixture(t)
			stale := box.DeepCopy()
			afterClose := false
			lag := true
			c := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if err := c.List(ctx, list, opts...); err != nil {
					return err
				}
				if members, ok := list.(*agents.SandboxList); ok && lag {
					fresh := members.Items[0].DeepCopy()
					members.Items = []agents.Sandbox{*stale.DeepCopy()}
					// Model the cache wait controller observing the new object
					// after this request has already captured the old List.
					if afterClose && externalObserve {
						expectations.ResourceVersionExpectationObserve(fresh)
					}
				}
				return nil
			}})
			backend := newServingSessions(c, func() bool { return true })
			serving := servingValue(parent)
			if err := backend.CloseServingSession(t.Context(), serving, "a"); err != nil {
				t.Fatal(err)
			}
			var persisted agents.Sandbox
			if err := base.Get(t.Context(), client.ObjectKeyFromObject(box), &persisted); err != nil {
				t.Fatal(err)
			}
			bindings, err := sessionbinding.Decode(persisted.Annotations)
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := bindings["a"]; exists {
				t.Fatal("fixture: Close did not persist deletion")
			}
			afterClose = true
			records, err := backend.ReadServingSessions(t.Context(), serving)
			if !errors.Is(err, session.ErrServingUnavailable) {
				t.Fatalf("stale List accepted after acknowledged Close: sessions=%+v err=%v; persisted a is absent", records, err)
			}
			if err := backend.CloseServingSession(t.Context(), serving, "b"); !errors.Is(err, session.ErrServingUnavailable) {
				t.Fatalf("Close accepted the captured stale member: %v", err)
			}
			lag = false
			records, err = backend.ReadServingSessions(t.Context(), serving)
			if err != nil || len(records) != 1 || records[0].SessionID != "b" {
				t.Fatalf("fresh snapshot did not recover remaining binding: records=%+v err=%v", records, err)
			}
		})
	}
}

func TestServingCloseWatermarkCleanup(t *testing.T) {
	for _, mode := range []string{"fresh snapshot", "expired wait", "replacement UID", "foreign owner", "left label scope", "deleted member"} {
		t.Run(mode, func(t *testing.T) {
			parent, box, base := servingSessionFixture(t)
			stale := box.DeepCopy()
			lag := true
			c := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if err := c.List(ctx, list, opts...); err != nil {
					return err
				}
				if members, ok := list.(*agents.SandboxList); ok && lag {
					members.Items = []agents.Sandbox{*stale.DeepCopy()}
				}
				return nil
			}})
			backend := newServingSessions(c, func() bool { return true })
			serving := servingValue(parent)
			if err := backend.CloseServingSession(t.Context(), serving, "a"); err != nil {
				t.Fatal(err)
			}
			var current agents.Sandbox
			if err := base.Get(t.Context(), client.ObjectKeyFromObject(box), &current); err != nil {
				t.Fatal(err)
			}
			expectations.ResourceVersionExpectationObserve(&current)
			if _, err := backend.ReadServingSessions(t.Context(), serving); !errors.Is(err, session.ErrServingUnavailable) {
				t.Fatalf("stale snapshot did not start the bounded wait: %v", err)
			}
			lag = false
			want := 0
			switch mode {
			case "fresh snapshot":
				want = 1
			case "expired wait":
				write := backend.memberWrites[box.UID]
				write.firstUnsatisfied = time.Now().Add(-2 * expectations.ExpectationTimeout)
				backend.memberWrites[box.UID] = write
				lag = true
				want = 2 // Preserve the existing bounded fail-open policy.
			case "replacement UID":
				if err := base.Delete(t.Context(), &current); err != nil {
					t.Fatal(err)
				}
				replacement := stale.DeepCopy()
				replacement.UID = "replacement-uid"
				replacement.ResourceVersion = ""
				if err := base.Create(t.Context(), replacement); err != nil {
					t.Fatal(err)
				}
				want = 2
			case "foreign owner":
				current.OwnerReferences[0].UID = "other-serving"
				if err := base.Update(t.Context(), &current); err != nil {
					t.Fatal(err)
				}
			case "left label scope":
				delete(current.Labels, sessionbinding.ServingUIDLabel)
				if err := base.Update(t.Context(), &current); err != nil {
					t.Fatal(err)
				}
			case "deleted member":
				if err := base.Delete(t.Context(), &current); err != nil {
					t.Fatal(err)
				}
			}
			sessions, err := backend.ReadServingSessions(t.Context(), serving)
			if err != nil || len(sessions) != want {
				t.Fatalf("watermark blocked recovery: sessions=%+v want=%d err=%v", sessions, want, err)
			}
			if len(backend.memberWrites) != 0 {
				t.Fatalf("obsolete member watermarks retained: %+v", backend.memberWrites)
			}
		})
	}
}

func TestServingCloseUnknownWriteRecovery(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint("committed=", committed), func(t *testing.T) {
			parent, box, base := servingSessionFixture(t)
			stale := box.DeepCopy()
			lag := true
			fail := true
			transport := interceptor.NewClient(base, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if err := c.List(ctx, list, opts...); err != nil {
						return err
					}
					if members, ok := list.(*agents.SandboxList); ok && lag {
						members.Items = []agents.Sandbox{*stale.DeepCopy()}
					}
					return nil
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
					if fail {
						fail = false
						if committed {
							if err := c.Patch(ctx, obj, p, opts...); err != nil {
								return err
							}
						}
						return apierrors.NewServerTimeout(agents.Resource("sandboxes"), "patch", 1)
					}
					return c.Patch(ctx, obj, p, opts...)
				},
			})
			backend := newServingSessions(transport, func() bool { return true })
			serving := servingValue(parent)
			if err := backend.CloseServingSession(t.Context(), serving, "a"); !errors.Is(err, session.ErrServingUnavailable) {
				t.Fatalf("unknown write reported Closed: %v", err)
			}
			if _, err := backend.ReadServingSessions(t.Context(), serving); !errors.Is(err, session.ErrServingUnavailable) {
				t.Fatal("unknown write yielded successful query")
			}
			if err := backend.CloseServingSession(t.Context(), serving, "a"); !errors.Is(err, session.ErrServingUnavailable) {
				t.Fatalf("retry used an unresolved member version: %v", err)
			}
			if !committed {
				// An unrelated persisted update lets the informer observe a newer RV
				// even when the failed request never reached the API server.
				var current agents.Sandbox
				if err := base.Get(t.Context(), client.ObjectKeyFromObject(box), &current); err != nil {
					t.Fatal(err)
				}
				current.Annotations["example.com/concurrent"] = "observed"
				if err := base.Update(t.Context(), &current); err != nil {
					t.Fatal(err)
				}
			}
			lag = false
			if err := backend.CloseServingSession(t.Context(), serving, "a"); err != nil {
				t.Fatalf("explicit retry did not resolve uncertain conditional write: %v", err)
			}
			sessions, err := backend.ReadServingSessions(t.Context(), serving)
			if err != nil {
				t.Fatal(err)
			}
			if len(sessions) != 1 || sessions[0].SessionID != "b" {
				t.Fatalf("unknown recovery wrong result: %+v", sessions)
			}
		})
	}
}

func TestServingCloseConflictAndConcurrentRecords(t *testing.T) {
	parent, box, base := servingSessionFixture(t)
	conflict := true
	patches := 0
	c := interceptor.NewClient(base, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
		patches++
		if conflict {
			conflict = false
			var current agents.Sandbox
			if err := c.Get(ctx, client.ObjectKeyFromObject(box), &current); err != nil {
				return err
			}
			current.Annotations["example.com/concurrent"] = "won"
			if err := c.Update(ctx, &current); err != nil {
				return err
			}
			return apierrors.NewConflict(agents.Resource("sandboxes"), box.Name, fmt.Errorf("concurrent annotation update"))
		}
		return c.Patch(ctx, obj, p, opts...)
	}})
	backend := newServingSessions(c, func() bool { return true })
	serving := servingValue(parent)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []string{"a", "b"} {
		wg.Go(func() { errs <- backend.CloseServingSession(t.Context(), serving, id) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if patches != 3 {
		t.Fatalf("unexpected stale retries: %d", patches)
	}
	var current agents.Sandbox
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(box), &current); err != nil {
		t.Fatal(err)
	}
	if current.Annotations["example.com/concurrent"] != "won" || current.Annotations[sessionbinding.BindingsAnnotation] != "{}" {
		t.Fatal("concurrent Close lost a winning write")
	}
}

func TestServingSessionsRejectAmbiguousData(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprint(malformed), func(t *testing.T) {
			parent, box, c := servingSessionFixture(t)
			if malformed {
				box.Annotations[sessionbinding.BindingsAnnotation] = `{"a":invalid}`
				if err := c.Update(t.Context(), box); err != nil {
					t.Fatal(err)
				}
			} else {
				other := box.DeepCopy()
				other.Name = "other"
				other.UID = "other"
				other.ResourceVersion = ""
				if err := c.Create(t.Context(), other); err != nil {
					t.Fatal(err)
				}
			}
			backend := newServingSessions(c, func() bool { return true })
			if _, err := backend.ReadServingSessions(t.Context(), servingValue(parent)); !errors.Is(err, session.ErrServingDataAmbiguous) {
				t.Fatalf("ambiguous binding returned winner: %v", err)
			}
			err := backend.CloseServingSession(t.Context(), servingValue(parent), "a")
			if malformed {
				if !errors.Is(err, session.ErrServingDataAmbiguous) {
					t.Fatal("malformed Close mutated a member")
				}
			} else {
				if err != nil {
					t.Fatal("duplicate Close did not remove all copies:", err)
				}
				var members agents.SandboxList
				if err := c.List(t.Context(), &members); err != nil {
					t.Fatal(err)
				}
				for _, member := range members.Items {
					records, err := sessionbinding.Decode(member.Annotations)
					if err != nil {
						t.Fatal(err)
					}
					if _, ok := records["a"]; ok {
						t.Fatal("duplicate Close retained target")
					}
					if _, ok := records["b"]; !ok {
						t.Fatal("duplicate Close removed unrelated Session")
					}
				}
			}
		})
	}
}

func TestServingIDAssignmentPatchesOnlyMetadata(t *testing.T) {
	parent, _, base := servingSessionFixture(t)
	delete(parent.Labels, sessionbinding.ServingIDLabel)
	parent.Labels["example.com/keep"] = "yes"
	parent.Spec.Ports = []int32{8080}
	if err := base.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	observed := servingValue(parent)
	patches := 0
	wrapped := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			t.Fatal("identity assignment sent a full typed object Update")
			return nil
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patches++
			data, err := patch.Data(obj)
			if err != nil {
				return err
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(data, &body); err != nil {
				return err
			}
			if len(body) != 1 || body["metadata"] == nil {
				t.Fatalf("identity patch included non-metadata fields: %s", data)
			}
			var metadata struct {
				UID     string            `json:"uid"`
				Version string            `json:"resourceVersion"`
				Labels  map[string]string `json:"labels"`
			}
			if err := json.Unmarshal(body["metadata"], &metadata); err != nil {
				return err
			}
			if metadata.UID != observed.UID || metadata.Version != observed.Version || len(metadata.Labels) != 1 {
				t.Fatalf("identity patch lost conditional identity: %s", data)
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	backend := newServingSessions(wrapped, func() bool { return true })
	assigned, err := backend.AssignServingID(t.Context(), observed, "new-id")
	if err != nil || assigned.ID != "new-id" || patches != 1 {
		t.Fatalf("identity assignment failed: %+v %v", assigned, err)
	}
	var current agents.SandboxServing
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(parent), &current); err != nil {
		t.Fatal(err)
	}
	if current.Labels["example.com/keep"] != "yes" || len(current.Spec.Ports) != 1 || current.Spec.Ports[0] != 8080 {
		t.Fatal("identity patch changed unrelated metadata/spec")
	}
	again, err := backend.AssignServingID(t.Context(), observed, "different-id")
	if err != nil || again.ID != "new-id" || patches != 1 {
		t.Fatal("identity assignment overwrote an existing stable ID")
	}
}

func TestServingCloseConflictWaitsForFreshCache(t *testing.T) {
	parent, box, base := servingSessionFixture(t)
	patches := 0
	c := interceptor.NewClient(base, interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
		patches++
		return apierrors.NewConflict(agents.Resource("sandboxes"), box.Name, fmt.Errorf("cache stays stale"))
	}})
	backend := newServingSessions(c, func() bool { return true })
	ctx, cancel := context.WithTimeout(t.Context(), 55*time.Millisecond)
	defer cancel()
	if err := backend.CloseServingSession(ctx, servingValue(parent), "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close ignored cancellation: %v", err)
	}
	if patches != 1 {
		t.Fatalf("Close retried stale observation %d times", patches)
	}
}

func TestServingCloseExpectationDoesNotCrossMemberUID(t *testing.T) {
	parent, box, c := servingSessionFixture(t)
	backend := newServingSessions(c, func() bool { return true })
	serving := servingValue(parent)
	if err := backend.CloseServingSession(t.Context(), serving, "a"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), box); err != nil {
		t.Fatal(err)
	}
	replacement := box.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = "replacement-uid"
	if err := c.Create(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	sessions, err := backend.ReadServingSessions(t.Context(), serving)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatal("old member expectation affected a new member UID")
	}
}

func TestServingSessionsFilteredMemberCache(t *testing.T) {
	for _, tt := range []struct {
		name         string
		memberScope  string
		wantSessions int
		wantWarning  bool
	}{
		{name: "matching template", memberScope: "serving", wantSessions: 2},
		{name: "mismatched template", memberScope: "other", wantWarning: true},
		{name: "missing template label", wantWarning: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			parent, box, base := servingSessionFixture(t)
			parent.Labels["scope"] = "serving"
			templateLabels := map[string]string{}
			if tt.memberScope != "" {
				templateLabels["scope"] = tt.memberScope
				box.Labels["scope"] = tt.memberScope
			}
			parent.Spec.Provision.SandboxTemplate = &agents.SandboxTemplateSpec{
				Template: &corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: templateLabels}},
			}
			for _, obj := range []client.Object{parent, box} {
				if err := base.Update(t.Context(), obj); err != nil {
					t.Fatal(err)
				}
			}
			selector := labels.SelectorFromSet(labels.Set{"scope": "serving"})
			filtered := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				return c.List(ctx, list, append(opts, client.MatchingLabelsSelector{Selector: selector})...)
			}})
			backend := newServingSessions(filtered, func() bool { return true })
			backend.labelSelector = "scope=serving"
			var logs strings.Builder
			ctx := klog.NewContext(t.Context(), funcr.NewJSON(func(line string) { logs.WriteString(line) }, funcr.Options{}))
			serving, err := backend.GetServing(ctx, parent.Namespace, "serving-id")
			if err != nil {
				t.Fatal(err)
			}
			sessions, err := backend.ReadServingSessions(ctx, serving)
			if err != nil || len(sessions) != tt.wantSessions {
				t.Fatalf("read with selector: sessions=%v, error=%v", sessions, err)
			}
			if err := backend.CloseServingSession(ctx, serving, "a"); err != nil {
				t.Fatalf("close with selector: %v", err)
			}
			sessions, err = backend.ReadServingSessions(ctx, serving)
			wantRemaining := 0
			if tt.wantSessions > 0 {
				wantRemaining = tt.wantSessions - 1
			}
			if err != nil || len(sessions) != wantRemaining {
				t.Fatalf("read after close: sessions=%v, error=%v", sessions, err)
			}
			if warned := strings.Contains(logs.String(), `"severity":"warning"`); warned != tt.wantWarning {
				t.Fatalf("warning=%v, want %v; logs=%s", warned, tt.wantWarning, logs.String())
			}
			if err := base.Get(ctx, client.ObjectKeyFromObject(box), box); err != nil {
				t.Fatal(err)
			}
			bindings, err := sessionbinding.Decode(box.Annotations)
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := bindings["a"]; exists != (tt.wantSessions == 0) {
				t.Fatal("Close must only remove bindings from visible members")
			}
		})
	}
}

// The write is held before the fake API server applies its original patch.
// Recreating the name must not let that old UID/version mutate a new delivery.
func TestServingClosePendingPatchDoesNotCrossMemberUID(t *testing.T) {
	parent, box, base := servingSessionFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	applied := make(chan error, 1)
	patches := 0
	wrapped := interceptor.NewClient(base, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		patches++
		data, err := patch.Data(obj)
		if err != nil {
			return err
		}
		var body struct {
			Metadata struct {
				UID     string `json:"uid"`
				Version string `json:"resourceVersion"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(data, &body); err != nil {
			return err
		}
		if body.Metadata.UID != string(box.UID) || body.Metadata.Version != box.ResourceVersion {
			return fmt.Errorf("pending patch lost original UID/version: %s", data)
		}
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		err = c.Patch(ctx, obj, patch, opts...)
		applied <- err
		return err
	}})
	backend := newServingSessions(wrapped, func() bool { return true })
	done := make(chan error, 1)
	go func() { done <- backend.CloseServingSession(ctx, servingValue(parent), "a") }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("Close did not reach pending patch:", ctx.Err())
	}
	if err := base.Delete(ctx, box); err != nil {
		t.Fatal(err)
	}
	replacement := box.DeepCopy()
	replacement.UID = "new-member-uid"
	replacement.ResourceVersion = ""
	replacement.Labels[sessionbinding.ServingUIDLabel] = "other-serving-uid"
	replacement.OwnerReferences[0].UID = "other-serving-uid"
	replacement.OwnerReferences[0].Name = "other-serving"
	replacement.Annotations[sessionbinding.BindingsAnnotation] = `{"a":{"state":"Active","lastRequestAt":"2026-11-10T00:01:00Z","generation":"new"},"new":{"state":"Active","lastRequestAt":"2026-11-10T00:01:00Z"}}`
	if err := base.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	var before agents.Sandbox
	if err := base.Get(ctx, client.ObjectKeyFromObject(replacement), &before); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal("Close did not rejudge the departed member:", err)
	}
	if err := <-applied; !apierrors.IsConflict(err) {
		t.Fatalf("old UID/version patch did not conflict after replacement: %v", err)
	}
	if patches != 1 {
		t.Fatalf("Close sent %d patches across a new delivery", patches)
	}
	var after agents.Sandbox
	if err := base.Get(ctx, client.ObjectKeyFromObject(replacement), &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("old request changed replacement delivery: before=%+v after=%+v", before, after)
	}
}

func TestServingCloseConflictRejudgesMemberOwnership(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*agents.Sandbox)
	}{
		{"controller owner UID", func(box *agents.Sandbox) { box.OwnerReferences[0].UID = "other-serving-uid" }},
		{"controller owner name", func(box *agents.Sandbox) { box.OwnerReferences[0].Name = "other-serving" }},
		{"serving UID label", func(box *agents.Sandbox) { box.Labels[sessionbinding.ServingUIDLabel] = "other-serving-uid" }},
		{"target belongs to foreign member", func(box *agents.Sandbox) {
			box.Annotations[sessionbinding.BindingsAnnotation] = `{"b":{"state":"Active","lastRequestAt":"2026-11-10T00:00:00Z"}}`
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			parent, box, base := servingSessionFixture(t)
			foreign := box.DeepCopy()
			foreign.Name = "foreign-member"
			foreign.UID = "foreign-member-uid"
			foreign.ResourceVersion = ""
			foreign.Labels[sessionbinding.ServingUIDLabel] = "other-serving-uid"
			foreign.OwnerReferences[0].UID = "other-serving-uid"
			foreign.OwnerReferences[0].Name = "other-serving"
			if err := base.Create(t.Context(), foreign); err != nil {
				t.Fatal(err)
			}
			var expectedForeign agents.Sandbox
			if err := base.Get(t.Context(), client.ObjectKeyFromObject(foreign), &expectedForeign); err != nil {
				t.Fatal(err)
			}
			var expectedCurrent agents.Sandbox
			patches := 0
			var stalePatchError error
			wrapped := interceptor.NewClient(base, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches++
				if patches != 1 {
					return fmt.Errorf("Close patched a new binding owner after conflict")
				}
				var current agents.Sandbox
				if err := c.Get(ctx, client.ObjectKeyFromObject(box), &current); err != nil {
					return err
				}
				tt.change(&current)
				current.Annotations["example.com/new-owner"] = "preserve"
				if err := c.Update(ctx, &current); err != nil {
					return err
				}
				if err := c.Get(ctx, client.ObjectKeyFromObject(box), &expectedCurrent); err != nil {
					return err
				}
				stalePatchError = c.Patch(ctx, obj, patch, opts...)
				return stalePatchError
			}})
			backend := newServingSessions(wrapped, func() bool { return true })
			if err := backend.CloseServingSession(t.Context(), servingValue(parent), "a"); err != nil {
				t.Fatal("Close did not rejudge fresh ownership:", err)
			}
			if !apierrors.IsConflict(stalePatchError) {
				t.Fatalf("concurrent ownership write did not conflict: %v", stalePatchError)
			}
			if patches != 1 {
				t.Fatalf("Close did not stop after ownership changed: %d patches", patches)
			}
			for _, expected := range []*agents.Sandbox{&expectedCurrent, &expectedForeign} {
				var actual agents.Sandbox
				if err := base.Get(t.Context(), client.ObjectKeyFromObject(expected), &actual); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(*expected, actual) {
					t.Fatalf("Close changed records after ownership transition: before=%+v after=%+v", expected, actual)
				}
			}
		})
	}
}

func TestServingIDLookupRejectsAmbiguousIndex(t *testing.T) {
	for _, namespace := range []string{"team", "other-team"} {
		t.Run(namespace, func(t *testing.T) {
			parent, _, base := servingSessionFixture(t)
			duplicate := parent.DeepCopy()
			duplicate.Name = "other-serving"
			duplicate.Namespace = namespace
			duplicate.UID = "other-serving-uid"
			duplicate.ResourceVersion = ""
			if err := base.Create(t.Context(), duplicate); err != nil {
				t.Fatal(err)
			}
			backend := newServingSessions(base, func() bool { return true })
			if _, err := backend.GetServing(t.Context(), "", "serving-id"); !errors.Is(err, session.ErrServingDataAmbiguous) {
				t.Fatalf("admin lookup selected duplicate ID: %v", err)
			}
			if namespace == parent.Namespace {
				if _, err := backend.GetServing(t.Context(), namespace, "serving-id"); !errors.Is(err, session.ErrServingDataAmbiguous) {
					t.Fatalf("namespace lookup selected duplicate ID: %v", err)
				}
				return
			}
			for _, expected := range []*agents.SandboxServing{parent, duplicate} {
				got, err := backend.GetServing(t.Context(), expected.Namespace, "serving-id")
				if err != nil || got.UID != string(expected.UID) {
					t.Fatalf("namespace lookup lost exact parent: %+v %v", got, err)
				}
			}
		})
	}
}

func TestGatewayScopeMismatchRejectsIncompleteMembership(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			parent, _, c := servingSessionFixture(t)
			if !missing {
				parent.Spec.Provision.SandboxTemplate = &agents.SandboxTemplateSpec{Template: &corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"scope": "other"}}}}
			}
			if err := c.Update(t.Context(), parent); err != nil {
				t.Fatal(err)
			}
			backend := NewStore(c, func() bool { return true }, Options{LabelSelector: "scope=gateway", RequireMatchingTemplateScope: true})
			_, err := backend.ReadServingSnapshot(t.Context(), servingValue(parent))
			if !errors.Is(err, session.ErrServingUnavailable) {
				t.Fatalf("incomplete scope accepted: %v", err)
			}
		})
	}
}
