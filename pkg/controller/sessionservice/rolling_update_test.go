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

package sessionservice

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agents "github.com/openkruise/agents/api/v1alpha1"
	binding "github.com/openkruise/agents/pkg/sessionservice/binding"
)

func rollingFixture(t *testing.T, count int32) (*agents.SessionService, client.Client, *Reconciler, []agents.Sandbox) {
	t.Helper()
	parent := serving(count)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(parent).WithObjects(parent).Build()
	r := NewReconciler(c)
	reconcileRolling(t, r, parent)
	var members agents.SandboxList
	if err := c.List(context.Background(), &members); err != nil {
		t.Fatal(err)
	}
	for i := range members.Items {
		sbx := &members.Items[i]
		sbx.Status.Phase = agents.SandboxRunning
		sbx.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
		sbx.Annotations[binding.BindingsAnnotation] = `{"000000000000000000000s":{"lastRequestAt":"2026-11-14T00:00:00Z"}}`
		if err := c.Update(context.Background(), sbx); err != nil {
			t.Fatal(err)
		}
	}
	reconcileRolling(t, r, parent)
	parent.Spec.SandboxTemplateSpec.Template.Spec.Containers[0].Image = "app:v2"
	if err := c.Update(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	return parent, c, r, members.Items
}

func TestRollingReplacementRequiresReady(t *testing.T) {
	for _, phase := range []agents.SandboxPhase{agents.SandboxPending, agents.SandboxFailed, agents.SandboxRunning} {
		t.Run(string(phase), func(t *testing.T) {
			parent, c, r, old := rollingFixture(t, 1)
			initial := parent.Status.CurrentRevision
			reconcileRolling(t, r, parent)
			var members agents.SandboxList
			if err := c.List(context.Background(), &members); err != nil {
				t.Fatal(err)
			}
			if len(members.Items) != 2 {
				t.Fatalf("rollout did not create surge member: %d", len(members.Items))
			}
			if parent.Status.CurrentRevision != initial || parent.Status.UpdateRevision == initial {
				t.Fatal("revision advanced before readiness")
			}
			for i := range members.Items {
				sbx := &members.Items[i]
				if sbx.Name == old[0].Name {
					continue
				}
				sbx.Status.Phase = phase
				if phase == agents.SandboxRunning {
					sbx.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
				}
				if err := c.Update(context.Background(), sbx); err != nil {
					t.Fatal(err)
				}
			}
			reconcileRolling(t, r, parent)
			err := c.Get(context.Background(), client.ObjectKeyFromObject(&old[0]), &agents.Sandbox{})
			if phase == agents.SandboxRunning {
				if !apierrors.IsNotFound(err) {
					t.Fatalf("bound old member not replaced: %v", err)
				}
				reconcileRolling(t, r, parent)
				if parent.Status.CurrentRevision != parent.Status.UpdateRevision {
					t.Fatal("completed rollout did not advance CurrentRevision")
				}
			} else if err != nil {
				t.Fatalf("unready target authorized deleting old: %v", err)
			}
		})
	}
}

func TestRollingReadyBudgetCannotBeReused(t *testing.T) {
	parent, c, r, _ := rollingFixture(t, 3)
	reconcileRolling(t, r, parent)
	var members agents.SandboxList
	if err := c.List(context.Background(), &members); err != nil {
		t.Fatal(err)
	}
	if len(members.Items) != 4 {
		t.Fatalf("25 percent did not round up: %d", len(members.Items))
	}
	for i := range members.Items {
		sbx := &members.Items[i]
		if sbx.Labels[binding.SessionServiceRevisionLabel] != parent.Status.UpdateRevision {
			continue
		}
		sbx.Status.Phase = agents.SandboxRunning
		sbx.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
		if err := c.Update(context.Background(), sbx); err != nil {
			t.Fatal(err)
		}
	}
	for range 4 {
		reconcileRolling(t, r, parent)
	}
	if err := c.List(context.Background(), &members); err != nil {
		t.Fatal(err)
	}
	ready := 0
	for _, sbx := range members.Items {
		if sbx.Status.Phase == agents.SandboxRunning {
			ready++
		}
	}
	if ready < 3 {
		t.Fatalf("reused one replacement's Ready budget: ready=%d", ready)
	}
}

func TestRollingRejectsInvalidSurge(t *testing.T) {
	for _, value := range []intstr.IntOrString{intstr.FromInt32(0), intstr.FromString("00%"), intstr.FromString("-1%"), intstr.FromString("2.5%"), intstr.FromString("1")} {
		parent := serving(1)
		parent.Spec.UpdateStrategy.MaxSurge = &value
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(parent).WithObjects(parent).Build()
		reconcile(t, NewReconciler(c), parent)
		var members agents.SandboxList
		if err := c.List(context.Background(), &members); err != nil {
			t.Fatal(err)
		}
		if len(members.Items) != 0 {
			t.Fatalf("invalid surge created members: %v", value)
		}
	}
}

func reconcileRolling(t *testing.T, r *Reconciler, parent *agents.SessionService) {
	t.Helper()
	reconcile(t, r, parent)
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(parent), parent); err != nil {
		t.Fatal(err)
	}
}

func TestRollingChurnAndSurgeSlots(t *testing.T) {
	for _, mode := range []string{"obsolete pending", "terminating", "replicas reduced", "rollback", "terminal history"} {
		t.Run(mode, func(t *testing.T) {
			parent, c, r, old := rollingFixture(t, 1)
			reconcileRolling(t, r, parent)
			var members agents.SandboxList
			if err := c.List(context.Background(), &members); err != nil {
				t.Fatal(err)
			}
			var surge *agents.Sandbox
			for i := range members.Items {
				if members.Items[i].Name != old[0].Name {
					surge = &members.Items[i]
				}
			}
			if surge == nil {
				t.Fatal("no surge")
			}
			switch mode {
			case "terminating":
				surge.Finalizers = []string{"test/hold"}
				if err := c.Update(context.Background(), surge); err != nil {
					t.Fatal(err)
				}
				if err := c.Delete(context.Background(), surge); err != nil {
					t.Fatal(err)
				}
			case "replicas reduced":
				*parent.Spec.Replicas = 0
			case "terminal history":
				for i, phase := range []agents.SandboxPhase{agents.SandboxFailed, agents.SandboxSucceeded} {
					history := member(parent, fmt.Sprintf("history-%d", i))
					history.Status.Phase = phase
					history.ResourceVersion = ""
					if err := c.Create(context.Background(), history); err != nil {
						t.Fatal(err)
					}
				}
				parent.Spec.SandboxTemplateSpec.Template.Spec.Containers[0].Image = "app:v3"
			case "rollback":
				parent.Spec.SandboxTemplateSpec.Template.Spec.Containers[0].Image = "app:v1"
			default:
				parent.Spec.SandboxTemplateSpec.Template.Spec.Containers[0].Image = "app:v3"
			}
			if err := c.Update(context.Background(), parent); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				reconcileRolling(t, r, parent)
			}
			if err := c.List(context.Background(), &members); err != nil {
				t.Fatal(err)
			}
			if mode != "terminal history" && len(members.Items) > 2 {
				t.Fatalf("observed surge budget exceeded with a complete cache: %d", len(members.Items))
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(&old[0]), &agents.Sandbox{}); err != nil {
				t.Fatalf("bound current member removed: %v", err)
			}
			if mode == "obsolete pending" || mode == "terminal history" {
				found := false
				for _, sbx := range members.Items {
					if sbx.Spec.Template != nil && sbx.Spec.Template.Spec.Containers[0].Image == "app:v3" {
						found = true
					}
				}
				if !found {
					t.Fatal("obsolete pending surge blocked latest target")
				}
			}
			if mode == "terminating" && parent.Status.CurrentRevision == parent.Status.UpdateRevision {
				t.Fatal("completed before terminating old disappeared")
			}
			if mode == "replicas reduced" && !meta.IsStatusConditionTrue(parent.Status.Conditions, "ScaleDownBlocked") {
				t.Fatal("bound zero-scale not reported blocked")
			}
		})
	}
}

func TestRollingUnknownCreateUsesLatestSnapshot(t *testing.T) {
	parent := serving(1)
	parent.Spec.SandboxTemplateSpec.Template.Annotations = map[string]string{"snapshot": "original"}
	var attempted []*agents.Sandbox
	fail := true
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(parent).WithObjects(parent).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if sbx, ok := obj.(*agents.Sandbox); ok {
			attempted = append(attempted, sbx.DeepCopy())
			if fail {
				return fmt.Errorf("unknown create result")
			}
		}
		return c.Create(ctx, obj, opts...)
	}}).Build()
	r := NewReconciler(c)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(parent)})
	if err == nil {
		t.Fatal("expected create failure")
	}
	parent.Spec.SandboxTemplateSpec.Template.Annotations = map[string]string{"latest": "new"}
	parent.Spec.SandboxTemplateSpec.Template.Spec.Containers[0].Image = "app:v2"
	if err := c.Update(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	fail = false
	reconcileRolling(t, r, parent)
	if len(attempted) != 2 {
		t.Fatalf("attempts=%d", len(attempted))
	}
	first, retry := attempted[0], attempted[1]
	if first.Name != "" || retry.Name != "" || first.GenerateName == "" || retry.GenerateName == "" || first.Labels[binding.SessionServiceRevisionLabel] == retry.Labels[binding.SessionServiceRevisionLabel] || retry.Spec.Template.Spec.Containers[0].Image != "app:v2" || retry.Spec.Template.Annotations["snapshot"] != "" || retry.Spec.Template.Annotations["latest"] != "new" {
		t.Fatalf("unknown create result pinned the old Revision")
	}
}

func TestRollingSurgeParsing(t *testing.T) {
	for _, tt := range []struct {
		desired int32
		text    string
		want    int32
	}{{3, "025%", 1}, {0, "25%", 0}, {2147483647, "100%", 2147483647}} {
		parent := serving(tt.desired)
		value := intstr.FromString(tt.text)
		parent.Spec.UpdateStrategy.MaxSurge = &value
		got, err := maxSurge(parent)
		if err != nil || got != tt.want {
			t.Fatalf("desired=%d value=%s got=%d err=%v", tt.desired, tt.text, got, err)
		}
	}
}

func TestRollingReplicaReductionBlocksNewCreates(t *testing.T) {
	t.Run("over-cap old members", func(t *testing.T) {
		parent, c, r, old := rollingFixture(t, 3)
		reconcileRolling(t, r, parent)
		*parent.Spec.Replicas = 1
		if err := c.Update(context.Background(), parent); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			reconcileRolling(t, r, parent)
		}
		var members agents.SandboxList
		if err := c.List(context.Background(), &members); err != nil {
			t.Fatal(err)
		}
		if len(members.Items) != 3 {
			t.Fatalf("over-cap reduction added members or removed bound members: %d", len(members.Items))
		}
		for _, sbx := range old {
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(&sbx), &agents.Sandbox{}); err != nil {
				t.Fatal(err)
			}
		}
		if !meta.IsStatusConditionTrue(parent.Status.Conditions, "ScaleDownBlocked") {
			t.Fatal("bound surplus not reported blocked")
		}
	})
	for _, tt := range []struct {
		name       string
		bindings   string
		oldPresent bool
	}{
		{name: "empty target surplus", bindings: "{}"},
		{name: "bound target surplus", bindings: `{"000000000000000000000s":{"lastRequestAt":"2026-11-14T00:00:00Z"}}`},
		{name: "invalid target bindings", bindings: "{"},
		{name: "bound pending targets with Ready old", bindings: `{"000000000000000000000s":{"lastRequestAt":"2026-11-14T00:00:00Z"}}`, oldPresent: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			parent, c, r, old := rollingFixture(t, 2)
			initial := parent.Status.CurrentRevision
			surge := intstr.FromInt32(2)
			parent.Spec.UpdateStrategy.MaxSurge = &surge
			if err := c.Update(context.Background(), parent); err != nil {
				t.Fatal(err)
			}
			reconcileRolling(t, r, parent)
			var members agents.SandboxList
			if err := c.List(context.Background(), &members); err != nil {
				t.Fatal(err)
			}
			for i := range members.Items {
				sbx := &members.Items[i]
				if sbx.Labels[binding.SessionServiceRevisionLabel] != parent.Status.UpdateRevision {
					continue
				}
				sbx.Status.Phase = agents.SandboxRunning
				sbx.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
				sbx.Annotations[binding.BindingsAnnotation] = tt.bindings
				if err := c.Update(context.Background(), sbx); err != nil {
					t.Fatal(err)
				}
			}
			// This reconciliation deletes both A members, before the next snapshot can
			// advance CurrentRevision. Reduce replicas at exactly that observation cut.
			reconcileRolling(t, r, parent)
			if err := c.List(context.Background(), &members); err != nil {
				t.Fatal(err)
			}
			if len(members.Items) != 2 || parent.Status.CurrentRevision != initial || parent.Status.UpdateRevision == initial {
				t.Fatalf("did not establish pre-completion cut: members=%d status=%+v", len(members.Items), parent.Status)
			}
			if tt.oldPresent {
				for i := range members.Items {
					sbx := &members.Items[i]
					sbx.Status.Phase = agents.SandboxPending
					sbx.Status.Conditions = nil
					if err := c.Update(context.Background(), sbx); err != nil {
						t.Fatal(err)
					}
				}
				restored := old[0].DeepCopy()
				restored.Name = memberPrefix(parent) + "-old"
				restored.ResourceVersion = ""
				restored.Annotations[binding.BindingsAnnotation] = "{}"
				if err := c.Create(context.Background(), restored); err != nil {
					t.Fatal(err)
				}
				old[0] = *restored
			}
			*parent.Spec.Replicas = 1
			surge = intstr.FromInt32(1)
			parent.Spec.UpdateStrategy.MaxSurge = &surge
			if err := c.Update(context.Background(), parent); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				reconcileRolling(t, r, parent)
			}
			if err := c.List(context.Background(), &members); err != nil {
				t.Fatal(err)
			}
			if tt.bindings == "{}" {
				if len(members.Items) != 1 || parent.Status.CurrentRevision != parent.Status.UpdateRevision || meta.IsStatusConditionTrue(parent.Status.Conditions, "ScaleDownBlocked") {
					t.Fatalf("empty target surplus did not converge: members=%d status=%+v", len(members.Items), parent.Status)
				}
			} else {
				want := 2
				if tt.oldPresent {
					want++
				}
				if len(members.Items) != want || !meta.IsStatusConditionTrue(parent.Status.Conditions, "ScaleDownBlocked") {
					t.Fatalf("occupied/invalid target surplus not protected and reported: members=%d status=%+v", len(members.Items), parent.Status)
				}
				if tt.oldPresent {
					if err := c.Get(context.Background(), client.ObjectKeyFromObject(&old[0]), &agents.Sandbox{}); err != nil {
						t.Fatalf("pending targets authorized deleting Ready old: %v", err)
					}
				}
			}
		})
	}
}

func TestRollingRejectedCreateUsesCorrectedTemplate(t *testing.T) {
	for _, reason := range []metav1.StatusReason{metav1.StatusReasonInvalid, metav1.StatusReasonForbidden, metav1.StatusReasonBadRequest, metav1.StatusReasonUnauthorized} {
		t.Run(string(reason), func(t *testing.T) {
			parent := serving(1)
			var attempts []*agents.Sandbox
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(parent).WithObjects(parent).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if sbx, ok := obj.(*agents.Sandbox); ok {
					attempts = append(attempts, sbx.DeepCopy())
					if len(attempts) == 1 {
						return &apierrors.StatusError{ErrStatus: metav1.Status{Reason: reason}}
					}
				}
				return c.Create(ctx, obj, opts...)
			}}).Build()
			r := NewReconciler(c)
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(parent)}); err == nil {
				t.Fatal("expected explicit create rejection")
			}
			parent.Spec.SandboxTemplateSpec.Template.Spec.Containers[0].Image = "app:corrected"
			if err := c.Update(context.Background(), parent); err != nil {
				t.Fatal(err)
			}
			reconcileRolling(t, r, parent)
			if len(attempts) != 2 || attempts[1].Spec.Template.Spec.Containers[0].Image != "app:corrected" || attempts[0].Labels[binding.SessionServiceRevisionLabel] == attempts[1].Labels[binding.SessionServiceRevisionLabel] {
				t.Fatal("explicit rejection pinned the old rejected template")
			}
		})
	}
}

func TestRollingRollbackReusesRevisionSnapshot(t *testing.T) {
	parent, c, r, old := rollingFixture(t, 1)
	originalRevision := old[0].Labels[binding.SessionServiceRevisionLabel]
	var before appsv1.ControllerRevision
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: parent.Namespace, Name: originalRevision}, &before); err != nil {
		t.Fatal(err)
	}
	reconcileRolling(t, r, parent) // Create a pending v2 replacement.
	parent.Spec.Template.Spec.Containers[0].Image = "app:v1"
	if err := c.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		reconcileRolling(t, r, parent)
	}
	var members agents.SandboxList
	if err := c.List(t.Context(), &members); err != nil {
		t.Fatal(err)
	}
	if len(members.Items) != 1 || members.Items[0].Labels[binding.SessionServiceRevisionLabel] != originalRevision || parent.Status.CurrentRevision != originalRevision || parent.Status.UpdateRevision != originalRevision {
		t.Fatalf("rollback failed to converge on original Revision: members=%+v status=%+v", members.Items, parent.Status)
	}
	var after appsv1.ControllerRevision
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(&before), &after); err != nil {
		t.Fatal(err)
	}
	if before.UID != after.UID || !bytes.Equal(before.Data.Raw, after.Data.Raw) {
		t.Fatal("rollback replaced or mutated the immutable Revision")
	}
	var revisions appsv1.ControllerRevisionList
	if err := c.List(t.Context(), &revisions); err != nil {
		t.Fatal(err)
	}
	if len(revisions.Items) != 2 {
		t.Fatalf("expected v1 and v2 snapshots, got %d", len(revisions.Items))
	}
}
