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
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agents "github.com/openkruise/agents/api/v1alpha1"
	session "github.com/openkruise/agents/pkg/servingsession"
	sessionbinding "github.com/openkruise/agents/pkg/servingsession/binding"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

func TestServingAllocationSnapshotCAS(t *testing.T) {
	for _, change := range []string{"target version", "parent version", "other member version", "no change"} {
		t.Run(change, func(t *testing.T) {
			parent, box, c := servingSessionFixture(t)
			other := box.DeepCopy()
			other.Name = "other"
			other.UID = "other"
			other.ResourceVersion = ""
			other.Annotations[sessionbinding.BindingsAnnotation] = "{}"
			if err := c.Create(t.Context(), other); err != nil {
				t.Fatal(err)
			}
			backend := newServingSessions(c, func() bool { return true })
			snapshot, err := backend.ReadServingSnapshot(t.Context(), servingValue(parent))
			if err != nil {
				t.Fatal(err)
			}
			var target session.ServingMember
			for _, member := range snapshot.Members {
				if member.UID == string(box.UID) {
					target = member
				}
			}
			switch change {
			case "target version":
				box.Annotations["example.com/concurrent"] = "yes"
				err = c.Update(t.Context(), box)
			case "parent version":
				parent.Annotations = map[string]string{"example.com/concurrent": "yes"}
				err = c.Update(t.Context(), parent)
			case "other member version":
				other.Annotations["example.com/concurrent"] = "yes"
				err = c.Update(t.Context(), other)
			}
			if err != nil {
				t.Fatal(err)
			}
			bindings := sessionbinding.Bindings{}
			for id, record := range target.Bindings {
				bindings[id] = record
			}
			delete(bindings, "a")
			_, err = backend.ReplaceServingBindings(t.Context(), snapshot, target, bindings)
			if change != "no change" {
				if !errors.Is(err, session.ErrServingConflict) {
					t.Fatalf("stale snapshot wrote: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var current agents.Sandbox
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(box), &current); err != nil {
				t.Fatal(err)
			}
			records, err := sessionbinding.Decode(current.Annotations)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 || records["b"] != target.Bindings["b"] || current.Annotations["example.com/keep"] != "yes" {
				t.Fatal("conditional binding patch lost remaining binding or unrelated annotations")
			}
			if strings.Contains(current.Annotations[sessionbinding.BindingsAnnotation], `"future"`) {
				t.Fatal("conditional binding patch preserved an unknown record field")
			}
			if !reflect.DeepEqual(current.Finalizers, box.Finalizers) {
				t.Fatal("binding write changed member finalizers")
			}
		})
	}
}

func TestServingAllocationSnapshotRejectsCapturedListAfterObserve(t *testing.T) {
	parent, box, base := servingSessionFixture(t)
	stale := box.DeepCopy()
	lag := false
	c := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if err := c.List(ctx, list, opts...); err != nil {
			return err
		}
		if members, ok := list.(*agents.SandboxList); ok && lag {
			fresh := members.Items[0].DeepCopy()
			members.Items = []agents.Sandbox{*stale.DeepCopy()}
			expectations.ResourceVersionExpectationObserve(fresh)
		}
		return nil
	}})
	backend := newServingSessions(c, func() bool { return true })
	snapshot, err := backend.ReadServingSnapshot(t.Context(), servingValue(parent))
	if err != nil {
		t.Fatal(err)
	}
	target := snapshot.Members[0]
	bindings := sessionbinding.Bindings{"b": target.Bindings["b"]}
	if _, err := backend.ReplaceServingBindings(t.Context(), snapshot, target, bindings); err != nil {
		t.Fatal(err)
	}
	lag = true
	if _, err := backend.ReadServingSnapshot(t.Context(), snapshot.Serving); !errors.Is(err, session.ErrServingUnavailable) {
		t.Fatalf("captured stale snapshot passed compact write watermark after Observe: %v", err)
	}
	lag = false
	fresh, err := backend.ReadServingSnapshot(t.Context(), snapshot.Serving)
	if err != nil || len(fresh.Members[0].Bindings) != 1 {
		t.Fatalf("fresh snapshot failed: %+v %v", fresh, err)
	}
}

func TestServingAllocationSnapshotReadsParentOnce(t *testing.T) {
	for _, operation := range []string{"read", "replace bindings"} {
		t.Run(operation, func(t *testing.T) {
			parent, _, base := servingSessionFixture(t)
			parentReads := 0
			c := interceptor.NewClient(base, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*agents.SandboxServing); ok {
					parentReads++
				}
				return c.Get(ctx, key, obj, opts...)
			}})
			backend := newServingSessions(c, func() bool { return true })
			snapshot, err := backend.ReadServingSnapshot(t.Context(), servingValue(parent))
			if err != nil {
				t.Fatal(err)
			}
			if operation == "replace bindings" {
				parentReads = 0
				_, err = backend.ReplaceServingBindings(t.Context(), snapshot, snapshot.Members[0], sessionbinding.Bindings{})
				if err != nil {
					t.Fatal(err)
				}
			}
			if parentReads != 1 {
				t.Fatalf("one snapshot read the parent %d times", parentReads)
			}
		})
	}
}

func TestServingAllocationDeleteWinsBeforeBindingPatch(t *testing.T) {
	for _, timing := range []string{"before replacement snapshot", "immediately before conditional patch"} {
		t.Run(timing, func(t *testing.T) {
			parent, box, base := servingSessionFixture(t)
			box.Status.Phase = agents.SandboxRunning
			box.Status.Conditions = []metav1.Condition{{Type: string(agents.SandboxConditionReady), Status: metav1.ConditionTrue}}
			box.Finalizers = []string{"test/keep"}
			if err := base.Update(t.Context(), box); err != nil {
				t.Fatal(err)
			}
			originalAnnotations := box.DeepCopy().Annotations
			originalFinalizers := append([]string(nil), box.Finalizers...)
			deleted := false
			patchAttempts := 0
			deleteMember := func(ctx context.Context) error {
				uid, rv := box.UID, box.ResourceVersion
				if err := base.Delete(ctx, box, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil {
					return err
				}
				deleted = true
				return nil
			}
			c := interceptor.NewClient(base, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patchAttempts++
				if timing == "immediately before conditional patch" {
					if err := deleteMember(ctx); err != nil {
						return err
					}
				}
				return c.Patch(ctx, obj, patch, opts...)
			}})
			backend := newServingSessions(c, func() bool { return true })
			snapshot, err := backend.ReadServingSnapshot(t.Context(), servingValue(parent))
			if err != nil {
				t.Fatal(err)
			}
			target := snapshot.Members[0]
			if target.Deleting || !target.Ready || target.Phase != string(agents.SandboxRunning) {
				t.Fatalf("fixture is not bindable: %+v", target)
			}
			bindings := sessionbinding.Bindings{}
			for id, record := range target.Bindings {
				bindings[id] = record
			}
			bindings["new-session"] = target.Bindings["a"]
			if timing == "before replacement snapshot" {
				if err := deleteMember(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			_, err = backend.ReplaceServingBindings(t.Context(), snapshot, target, bindings)
			if !errors.Is(err, session.ErrServingConflict) && !errors.Is(err, session.ErrServingUnavailable) {
				t.Fatalf("Delete-wins accepted binding: %v", err)
			}
			if !deleted || (timing == "immediately before conditional patch" && patchAttempts != 1) {
				t.Fatalf("delete=%v patch attempts=%d", deleted, patchAttempts)
			}
			var got agents.Sandbox
			if err := base.Get(t.Context(), client.ObjectKeyFromObject(box), &got); err != nil {
				t.Fatal(err)
			}
			if got.DeletionTimestamp.IsZero() {
				t.Fatal("Delete did not persist termination")
			}
			if !reflect.DeepEqual(got.Annotations, originalAnnotations) || !reflect.DeepEqual(got.Finalizers, originalFinalizers) {
				t.Fatalf("Delete-wins changed bindings or finalizers: %+v", got.ObjectMeta)
			}
			records, err := sessionbinding.Decode(got.Annotations)
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := records["new-session"]; exists {
				t.Fatal("new Session survived losing CAS")
			}
		})
	}
}

func TestServingAllocationBackendLockCancellation(t *testing.T) {
	for _, operation := range []string{"snapshot read", "snapshot write", "session read", "session close"} {
		t.Run(operation, func(t *testing.T) {
			parent, _, base := servingSessionFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			var patches atomic.Int32
			wrapped := interceptor.NewClient(base, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if patches.Add(1) == 1 {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return c.Patch(ctx, obj, patch, opts...)
			}})
			backend := newServingSessions(wrapped, func() bool { return true })
			serving := servingValue(parent)
			snapshot, err := backend.ReadServingSnapshot(t.Context(), serving)
			if err != nil {
				t.Fatal(err)
			}
			target := snapshot.Members[0]
			holderCtx, holderCancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer holderCancel()
			closeDone := make(chan error, 1)
			go func() { closeDone <- backend.CloseServingSession(holderCtx, serving, "a") }()
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			select {
			case <-entered:
			case <-holderCtx.Done():
				t.Fatal("Close did not acquire shared backend lock")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var e error
				switch operation {
				case "snapshot read":
					_, e = backend.ReadServingSnapshot(ctx, serving)
				case "snapshot write":
					bindings := sessionbinding.Bindings{"new": target.Bindings["a"]}
					_, e = backend.ReplaceServingBindings(ctx, snapshot, target, bindings)
				case "session read":
					_, e = backend.ReadServingSessions(ctx, serving)
				case "session close":
					e = backend.CloseServingSession(ctx, serving, "b")
				}
				done <- e
			}()
			returnedWhileHeld := false
			var canceledErr error
			select {
			case canceledErr = <-done:
				returnedWhileHeld = true
			case <-time.After(250 * time.Millisecond):
			}
			once.Do(func() { close(release) })
			if err := <-closeDone; err != nil {
				t.Fatal("holding Close failed:", err)
			}
			if !returnedWhileHeld {
				canceledErr = <-done
			}
			if !returnedWhileHeld {
				t.Fatalf("%s ignored cancellation until shared lock released: %v", operation, canceledErr)
			}
			if !errors.Is(canceledErr, context.DeadlineExceeded) {
				t.Fatalf("%s cancellation=%v", operation, canceledErr)
			}
			if patches.Load() != 1 {
				t.Fatalf("canceled operation patched after lock release: %d", patches.Load())
			}
			records, err := backend.ReadServingSessions(t.Context(), serving)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 || records[0].SessionID != "b" {
				t.Fatalf("canceled operation changed surviving records: %+v", records)
			}
		})
	}
}
