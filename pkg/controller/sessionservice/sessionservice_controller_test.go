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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	agents "github.com/openkruise/agents/api/v1alpha1"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{agents.AddToScheme, corev1.AddToScheme, appsv1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func serving(replicas int32) *agents.SessionService {
	return &agents.SessionService{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "test",
			Name:       "serving",
			UID:        "serving-uid",
			Generation: 1,
		},
		Spec: agents.SessionServiceSpec{
			Replicas: &replicas,
			SandboxTemplateSpec: agents.SandboxTemplateSpec{Template: &corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "app:v1"}},
				},
			},
			},
			Ports: []int32{8080},
		},
	}
}

func member(parent *agents.SessionService, name string) *agents.Sandbox {
	data, err := json.Marshal(parent.Spec.SandboxTemplateSpec)
	if err != nil {
		panic(err)
	}
	return &agents.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       parent.Namespace,
			Name:            name,
			UID:             types.UID(name + "-uid"),
			ResourceVersion: "1",
			Labels: map[string]string{
				sessionbinding.SessionServiceUIDLabel:      string(parent.UID),
				sessionbinding.SessionServiceRevisionLabel: provisionRevisionName(parent, data),
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, controllerKind)},
		},
	}
}

func populatedTemplate() *agents.SandboxTemplateSpec {
	template := serving(0).Spec.SandboxTemplateSpec.DeepCopy()
	template.StartupMode = agents.StartupModeDeferred
	template.Backend = "k8s"
	template.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	template.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Labels: map[string]string{"storage": "test"}},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		},
	}}
	template.PersistentContents = []string{"filesystem"}
	template.Runtimes = []agents.RuntimeConfig{{Name: "agent-runtime"}}
	template.PauseStrategy = &agents.PauseStrategy{Type: agents.PauseStrategyStop}
	template.Probes = []agents.Probe{{Name: "idle", Probe: corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"check-idle"}}}}}}
	template.AutoPausePolicy = &agents.AutoPausePolicy{Pause: &agents.PausePolicy{WhenProbedIdleState: &agents.ProbedIdleStateRule{
		Probe: "idle", MessageRegex: "^idle$", ThresholdDuration: &metav1.Duration{Duration: time.Minute},
	}}}
	template.Lifecycle = &agents.SandboxLifecycle{
		PrePause:   &agents.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"flush"}}, TimeoutSeconds: 1},
		PostResume: &agents.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"restore"}}, TimeoutSeconds: 60},
	}
	return template
}

func reconcile(t *testing.T, r *Reconciler, parent *agents.SessionService) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(parent)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestEffectiveMembers(t *testing.T) {
	parent := serving(0)
	for _, tt := range []struct {
		name       string
		phase      agents.SandboxPhase
		deleting   bool
		wrongOwner bool
		wrongLabel bool
		want       bool
	}{
		{name: "creating", want: true}, {name: "pending", phase: agents.SandboxPending, want: true},
		{name: "running", phase: agents.SandboxRunning, want: true},
		{name: "paused", phase: agents.SandboxPaused, want: true}, {name: "resuming", phase: agents.SandboxResuming, want: true},
		{name: "failed", phase: agents.SandboxFailed}, {name: "succeeded", phase: agents.SandboxSucceeded},
		{name: "terminating", phase: agents.SandboxTerminating}, {name: "deleting", deleting: true},
		{name: "wrong owner", wrongOwner: true}, {name: "wrong UID label", wrongLabel: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sbx := member(parent, "member")
			sbx.Status.Phase = tt.phase
			if tt.deleting {
				now := metav1.Now()
				sbx.DeletionTimestamp = &now
			}
			if tt.wrongOwner {
				sbx.OwnerReferences[0].UID = "old-serving"
			}
			if tt.wrongLabel {
				sbx.Labels[sessionbinding.SessionServiceUIDLabel] = "old-serving"
			}
			if got := effectiveMember(parent, sbx); got != tt.want {
				t.Fatalf("effective=%v want=%v", got, tt.want)
			}
		})
	}
}

func TestScaleDownBindingPolicy(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  *string
	}{
		{name: "absent"}, {name: "empty", raw: ptr("{}")},
		{name: "bound", raw: ptr(`{"000000000000000000000s":{"createdAt":"2026-11-03T10:00:00Z"}}`)},
		{name: "malformed", raw: ptr(`{`)},
		{name: "invalid activity", raw: ptr(`{"000000000000000000000s":{"createdAt":"invalid"}}`)},
	} {
		for _, affinity := range []string{"BestEffort", "Strict"} {
			t.Run(fmt.Sprintf("%s/%s", tt.name, affinity), func(t *testing.T) {
				parent := serving(0)
				parent.Spec.SessionPolicy.Affinity = affinity
				sbx := member(parent, "member")
				if tt.raw != nil {
					sbx.Annotations = map[string]string{sessionbinding.BindingsAnnotation: *tt.raw}
				}
				deleted := 0
				c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent, sbx).WithInterceptorFuncs(interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					deleted++
					del := &client.DeleteOptions{}
					del.ApplyOptions(opts)
					if del.Preconditions == nil || del.Preconditions.UID == nil || *del.Preconditions.UID != sbx.UID || del.Preconditions.ResourceVersion == nil || *del.Preconditions.ResourceVersion != sbx.ResourceVersion {
						t.Fatalf("missing snapshot preconditions: %+v", del.Preconditions)
					}
					return c.Delete(ctx, obj, opts...)
				}}).Build()
				reconcile(t, NewReconciler(c), parent)
				if deleted != 1 {
					t.Fatalf("deleted=%d, want 1 regardless of bindings and affinity", deleted)
				}
				var actual agents.Sandbox
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(sbx), &actual); !apierrors.IsNotFound(err) {
					t.Fatalf("expected member deleted, err=%v", err)
				}
				var got agents.SessionService
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(parent), &got); err != nil {
					t.Fatal(err)
				}
				if meta.IsStatusConditionTrue(got.Status.Conditions, "ScaleDownBlocked") {
					t.Fatalf("bindings unexpectedly blocked deletion: %+v", got.Status.Conditions)
				}
			})
		}
	}
}

func TestConflictRejudgesFreshSnapshot(t *testing.T) {
	parent := serving(0)
	sbx := member(parent, "member")
	s := testScheme(t)
	attempts := 0
	c := fake.NewClientBuilder().WithScheme(s).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent, sbx).WithInterceptorFuncs(interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		attempts++
		if attempts > 1 {
			return c.Delete(ctx, obj, opts...)
		}
		var fresh agents.Sandbox
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), &fresh); err != nil {
			return err
		}
		fresh.Annotations = map[string]string{sessionbinding.BindingsAnnotation: `{"000000000000000000000s":{"createdAt":"2026-11-03T10:00:00Z"}}`}
		if err := c.Update(ctx, &fresh); err != nil {
			return err
		}
		return apierrors.NewConflict(agents.Resource("sandboxes"), obj.GetName(), fmt.Errorf("binding won"))
	}}).Build()
	r := NewReconciler(c)
	if result := reconcile(t, r, parent); result.RequeueAfter <= 0 || result.RequeueAfter > time.Second*10 {
		t.Fatalf("unbounded or missing conflict requeue: %v", result)
	}
	reconcile(t, r, parent)
	if attempts != 2 {
		t.Fatalf("delete did not retry from the fresh snapshot: %d", attempts)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(sbx), &agents.Sandbox{}); !apierrors.IsNotFound(err) {
		t.Fatalf("fresh binding unexpectedly blocked deletion: %v", err)
	}
}

func TestInitialRevisionAndTemplateCopy(t *testing.T) {
	parent := serving(2)
	s := testScheme(t)
	parent.Spec.SandboxTemplateSpec = *populatedTemplate()
	templateAnnotations := map[string]string{"example.com/fixture": "preserved"}
	parent.Spec.SandboxTemplateSpec.Template.Annotations = maps.Clone(templateAnnotations)
	memberAnnotations := maps.Clone(templateAnnotations)
	memberAnnotations[sessionbinding.BindingsAnnotation] = "{}"
	c := fake.NewClientBuilder().WithScheme(s).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent).Build()
	r := NewReconciler(c)
	reconcile(t, r, parent)
	var members agents.SandboxList
	if err := c.List(context.Background(), &members); err != nil {
		t.Fatal(err)
	}
	if len(members.Items) != 2 {
		t.Fatalf("created %d members", len(members.Items))
	}
	for _, sbx := range members.Items {
		if !maps.Equal(sbx.Annotations, memberAnnotations) || !maps.Equal(sbx.Spec.Template.Annotations, templateAnnotations) {
			t.Fatalf("member annotations must preserve the template and initialize only bindings: sandbox=%v template=%v", sbx.Annotations, sbx.Spec.Template.Annotations)
		}
		if sbx.Spec.Template.Labels[sessionbinding.SessionServiceUIDLabel] != string(parent.UID) || sbx.Labels[sessionbinding.SessionServiceRevisionLabel] == "" || sbx.Spec.StartupMode != parent.Spec.StartupMode || sbx.Spec.PauseStrategy.Type != agents.PauseStrategyStop || len(sbx.Spec.Runtimes) != 1 || len(sbx.Spec.PersistentContents) != 1 {
			t.Fatalf("bad member: %+v", sbx)
		}
		template := parent.Spec.SandboxTemplateSpec
		if !reflect.DeepEqual(sbx.Spec.VolumeClaimTemplates, template.VolumeClaimTemplates) || !reflect.DeepEqual(sbx.Spec.Probes, template.Probes) || !reflect.DeepEqual(sbx.Spec.AutoPausePolicy, template.AutoPausePolicy) || !reflect.DeepEqual(sbx.Spec.Lifecycle, template.Lifecycle) {
			t.Fatalf("creation fields not copied: %+v", sbx.Spec)
		}
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(parent), parent); err != nil {
		t.Fatal(err)
	}
	initial := parent.Status.CurrentRevision
	if initial == "" || parent.Spec.SandboxTemplateSpec.Template.Labels[sessionbinding.SessionServiceUIDLabel] != "" {
		t.Fatalf("revision/input mutation: %+v", parent)
	}
	parent.Spec.Ports = []int32{8081}
	parent.Spec.SessionPolicy.Affinity = "Strict"
	*parent.Spec.Replicas = 3
	if err := c.Update(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, parent)
	var revisions appsv1.ControllerRevisionList
	if err := c.List(context.Background(), &revisions); err != nil {
		t.Fatal(err)
	}
	if len(revisions.Items) != 1 || revisions.Items[0].Name != initial {
		t.Fatalf("mutable policy created a revision: %+v", revisions.Items)
	}
	var payload agents.SandboxTemplateSpec
	if err := json.Unmarshal(revisions.Items[0].Data.Raw, &payload); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(payload, parent.Spec.SandboxTemplateSpec) {
		t.Fatalf("Revision omitted or changed creation input: %+v", payload)
	}
	// Restart with status missing must recover the same deterministic initial revision.
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(parent), parent); err != nil {
		t.Fatal(err)
	}
	parent.Status = agents.SessionServiceStatus{}
	if err := c.Status().Update(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	reconcile(t, NewReconciler(c), parent)
	if err := c.List(context.Background(), &revisions); err != nil {
		t.Fatal(err)
	}
	if len(revisions.Items) != 1 {
		t.Fatalf("restart duplicated revision: %d", len(revisions.Items))
	}
	for _, tt := range []struct {
		name   string
		mutate func(*agents.Sandbox)
	}{
		{name: "template metadata", mutate: func(s *agents.Sandbox) { s.Spec.Template.Labels["app"] = "changed" }},
		{name: "template containers", mutate: func(s *agents.Sandbox) { s.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath = "/changed" }},
		{name: "PVC metadata", mutate: func(s *agents.Sandbox) { s.Spec.VolumeClaimTemplates[0].Labels["storage"] = "changed" }},
		{name: "PVC resources", mutate: func(s *agents.Sandbox) {
			s.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("2Gi")
		}},
		{name: "persistent contents", mutate: func(s *agents.Sandbox) { s.Spec.PersistentContents[0] = "memory" }},
		{name: "runtimes", mutate: func(s *agents.Sandbox) { s.Spec.Runtimes[0].Name = "changed" }},
		{name: "pause strategy", mutate: func(s *agents.Sandbox) { s.Spec.PauseStrategy.Type = agents.PauseStrategyHibernate }},
		{name: "probes", mutate: func(s *agents.Sandbox) { s.Spec.Probes[0].Exec.Command[0] = "changed" }},
		{name: "auto pause policy", mutate: func(s *agents.Sandbox) {
			s.Spec.AutoPausePolicy.Pause.WhenProbedIdleState.ThresholdDuration.Duration = 0
		}},
		{name: "lifecycle", mutate: func(s *agents.Sandbox) { s.Spec.Lifecycle.PrePause.Exec.Command[0] = "changed" }},
	} {
		t.Run(tt.name+" does not alias", func(t *testing.T) {
			before := parent.Spec.SandboxTemplateSpec.DeepCopy()
			tt.mutate(newMember(parent, initial))
			if !reflect.DeepEqual(before, &parent.Spec.SandboxTemplateSpec) {
				t.Fatal("member mutation changed SessionService creation input")
			}
		})
	}
}

func TestUnsupportedMakesNoMemberChanges(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*agents.SessionService)
	}{
		{name: "SessionService idle policy", mutate: func(p *agents.SessionService) { p.Spec.SessionPolicy.PauseAfterIdle = "1m" }},
		{name: "missing Pod template", mutate: func(p *agents.SessionService) { p.Spec.Template = nil }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			parent := serving(0)
			parent.Spec.SandboxTemplateSpec = *populatedTemplate()
			tt.mutate(parent)
			sbx := member(parent, "member")
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent, sbx).Build()
			reconcile(t, NewReconciler(c), parent)
			var got agents.SessionService
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(parent), &got); err != nil {
				t.Fatal(err)
			}
			condition := meta.FindStatusCondition(got.Status.Conditions, "Supported")
			if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "Unsupported" {
				t.Fatalf("unsupported not explicit: %+v", got.Status)
			}
			var members agents.SandboxList
			if err := c.List(context.Background(), &members); err != nil {
				t.Fatal(err)
			}
			if len(members.Items) != 1 || !reflect.DeepEqual(members.Items[0], *sbx) {
				t.Fatalf("unsupported changed members: %+v", members.Items)
			}
			var revisions appsv1.ControllerRevisionList
			if err := c.List(context.Background(), &revisions); err != nil {
				t.Fatal(err)
			}
			if len(revisions.Items) != 0 {
				t.Fatalf("unsupported wrote %d revisions", len(revisions.Items))
			}
		})
	}
}

func TestCreateExpectationsSurviveCacheDelayAndParentReplacement(t *testing.T) {
	parent := serving(2)
	s := testScheme(t)
	hide := true
	creates := 0
	c := fake.NewClientBuilder().WithScheme(s).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if sandboxes, ok := list.(*agents.SandboxList); ok && hide {
				sandboxes.Items = nil
			}
			return nil
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*agents.Sandbox); ok {
				creates++
			}
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
	r := NewReconciler(c)
	reconcile(t, r, parent)
	reconcile(t, r, parent)
	if creates != 2 {
		t.Fatalf("cache lag duplicated creation: %d", creates)
	}
	hide = false
	reconcile(t, r, parent)
	if ok, _, _ := r.Expectations.SatisfiedExpectations(expectationKey(parent)); !ok {
		t.Fatal("cache observation did not release expectations")
	}
	r.Expectations.ExpectScale(expectationKey(parent), expectations.Create, "old-pending")
	oldParent := parent.DeepCopy()
	parent.UID = "new-serving-uid"
	r.parentExpectationCleanup().Update(event.UpdateEvent{ObjectOld: oldParent, ObjectNew: parent})
	if ok, _, _ := r.Expectations.SatisfiedExpectations(expectationKey(parent)); !ok {
		t.Fatal("new SessionService inherited old expectations")
	}
}

func ptr(value string) *string { return &value }

func TestObservedCountsAndReplacement(t *testing.T) {
	parent := serving(5)
	s := testScheme(t)
	objects := []client.Object{parent}
	for index, phase := range []agents.SandboxPhase{agents.SandboxPending, agents.SandboxRunning, agents.SandboxPaused, agents.SandboxResuming, agents.SandboxRunning, agents.SandboxFailed, agents.SandboxSucceeded, agents.SandboxTerminating} {
		sbx := member(parent, fmt.Sprintf("member-%d", index))
		sbx.Status.Phase = phase
		if index == 1 || index == 2 {
			sbx.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
		}
		objects = append(objects, sbx)
	}
	deleting := member(parent, "deleting")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{"test/finalizer"}
	objects = append(objects, deleting)
	c := fake.NewClientBuilder().WithScheme(s).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(objects...).Build()
	r := NewReconciler(c)
	reconcile(t, r, parent)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(parent), parent); err != nil {
		t.Fatal(err)
	}
	if parent.Status.Replicas != 5 || parent.Status.ReadyReplicas != 1 || parent.Status.ObservedGeneration != 1 {
		t.Fatalf("incorrect observed counts: %+v", parent.Status)
	}
	var members agents.SandboxList
	if err := c.List(context.Background(), &members); err != nil {
		t.Fatal(err)
	}
	if len(members.Items) != 9 {
		t.Fatalf("counted paused/notready as absent: %d", len(members.Items))
	}
	// A higher target replaces terminal/deleting capacity without deleting it.
	*parent.Spec.Replicas = 6
	if err := c.Update(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, parent)
	if err := c.List(context.Background(), &members); err != nil {
		t.Fatal(err)
	}
	if len(members.Items) != 10 {
		t.Fatalf("terminal/deleting member blocked replacement: %d", len(members.Items))
	}
}

func TestMemberOwnershipAndScaleDownOrdering(t *testing.T) {
	parent := serving(0)
	for _, tt := range []struct {
		name   string
		mutate func(*agents.Sandbox)
	}{
		{name: "wrong owner UID", mutate: func(s *agents.Sandbox) { s.OwnerReferences[0].UID = "old" }},
		{name: "wrong owner kind", mutate: func(s *agents.Sandbox) { s.OwnerReferences[0].Kind = "SandboxSet" }},
		{name: "wrong owner API", mutate: func(s *agents.Sandbox) { s.OwnerReferences[0].APIVersion = "other/v1" }},
		{name: "wrong SessionService UID label", mutate: func(s *agents.Sandbox) { s.Labels[sessionbinding.SessionServiceUIDLabel] = "old" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sbx := member(parent, "same-name")
			tt.mutate(sbx)
			if effectiveMember(parent, sbx) {
				t.Fatal("member with incorrect ownership was counted")
			}
		})
	}
	parent = serving(1)
	first, second := member(parent, "a"), member(parent, "b")
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent, first, second).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if members, ok := list.(*agents.SandboxList); ok {
				slices.Reverse(members.Items)
			}
			return nil
		},
	}).Build()
	reconcile(t, NewReconciler(c), parent)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(first), &agents.Sandbox{}); !apierrors.IsNotFound(err) {
		t.Fatalf("first eligible member by name retained: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(second), &agents.Sandbox{}); err != nil {
		t.Fatalf("later member by name deleted: %v", err)
	}
}

func TestRevisionCollisionsAndSupportedTemplateMutation(t *testing.T) {
	for _, tt := range []struct {
		name          string
		mutate        func(*agents.SessionService, *appsv1.ControllerRevision)
		extraRevision bool
	}{
		{name: "extra owned Revision", mutate: func(*agents.SessionService, *appsv1.ControllerRevision) {}, extraRevision: true},
		{name: "wrong Revision owner", mutate: func(_ *agents.SessionService, r *appsv1.ControllerRevision) { r.OwnerReferences[0].UID = "other" }},
		{name: "wrong Revision content", mutate: func(_ *agents.SessionService, r *appsv1.ControllerRevision) { r.Data.Raw = []byte(`{"other":true}`) }},

		{name: "PVC mutation", mutate: func(p *agents.SessionService, _ *appsv1.ControllerRevision) {
			p.Spec.SandboxTemplateSpec.VolumeClaimTemplates = populatedTemplate().VolumeClaimTemplates
			p.Spec.SandboxTemplateSpec.Template.Spec.Containers[0].VolumeMounts = populatedTemplate().Template.Spec.Containers[0].VolumeMounts
		}},
		{name: "persistent contents mutation", mutate: func(p *agents.SessionService, _ *appsv1.ControllerRevision) {
			p.Spec.SandboxTemplateSpec.PersistentContents = []string{"filesystem"}
		}},
		{name: "runtime mutation", mutate: func(p *agents.SessionService, _ *appsv1.ControllerRevision) {
			p.Spec.SandboxTemplateSpec.Runtimes = populatedTemplate().Runtimes
		}},
		{name: "pause strategy mutation", mutate: func(p *agents.SessionService, _ *appsv1.ControllerRevision) {
			p.Spec.SandboxTemplateSpec.PauseStrategy = populatedTemplate().PauseStrategy
		}},
		{name: "probe mutation", mutate: func(p *agents.SessionService, _ *appsv1.ControllerRevision) {
			p.Spec.SandboxTemplateSpec.Probes = populatedTemplate().Probes
		}},
		{name: "auto pause policy mutation", mutate: func(p *agents.SessionService, _ *appsv1.ControllerRevision) {
			p.Spec.SandboxTemplateSpec.AutoPausePolicy = &agents.AutoPausePolicy{Resume: &agents.ResumePolicy{OnIngressTraffic: &agents.IngressTrafficRule{}}}
		}},
		{name: "lifecycle mutation", mutate: func(p *agents.SessionService, _ *appsv1.ControllerRevision) {
			p.Spec.SandboxTemplateSpec.Lifecycle = populatedTemplate().Lifecycle
		}},
		{name: "backend is not normalized out of Revision", mutate: func(p *agents.SessionService, _ *appsv1.ControllerRevision) {
			p.Spec.SandboxTemplateSpec.Backend = "k8s"
		}},
		{name: "template mutation", mutate: func(p *agents.SessionService, _ *appsv1.ControllerRevision) {
			p.Spec.SandboxTemplateSpec.Template.Spec.Containers[0].Image = "app:v2"
		}},
		{name: "template metadata mutation", mutate: func(p *agents.SessionService, _ *appsv1.ControllerRevision) {
			p.Spec.SandboxTemplateSpec.Template.Labels["app"] = "changed"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			parent := serving(0)
			sbx := member(parent, "keep")
			s := testScheme(t)
			c := fake.NewClientBuilder().WithScheme(s).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent, sbx).Build()
			r := NewReconciler(c)
			name, unsupported, err := r.ensureRevision(context.Background(), parent)
			if err != nil || unsupported != "" {
				t.Fatalf("initial Revision: %q %v", unsupported, err)
			}
			var revision appsv1.ControllerRevision
			if err := c.Get(context.Background(), client.ObjectKey{Namespace: parent.Namespace, Name: name}, &revision); err != nil {
				t.Fatal(err)
			}
			tt.mutate(parent, &revision)
			if err := c.Update(context.Background(), &revision); err != nil {
				t.Fatal(err)
			}
			if err := c.Update(context.Background(), parent); err != nil {
				t.Fatal(err)
			}
			if tt.extraRevision {
				extra := revision.DeepCopy()
				extra.Name = "zz-extra-revision"
				extra.ResourceVersion = ""
				extra.UID = ""
				if err := c.Create(context.Background(), extra); err != nil {
					t.Fatal(err)
				}
			}
			reconcile(t, r, parent)
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(parent), parent); err != nil {
				t.Fatal(err)
			}
			condition := meta.FindStatusCondition(parent.Status.Conditions, "Supported")
			if tt.name != "wrong Revision owner" && tt.name != "wrong Revision content" {
				if condition == nil || condition.Status != metav1.ConditionTrue {
					t.Fatalf("supported mutation rejected: %+v", parent.Status)
				}
				return
			}
			if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "Unsupported" {
				t.Fatalf("unsafe Revision accepted: %+v", parent.Status)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(sbx), &agents.Sandbox{}); err != nil {
				t.Fatalf("unsupported Revision deleted member: %v", err)
			}
		})
	}
}

func TestFailedCreatesReleaseExpectationsAndUseGenerateName(t *testing.T) {
	parent := serving(1)
	s := testScheme(t)
	var names []string
	c := fake.NewClientBuilder().WithScheme(s).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*agents.Sandbox); ok {
			if obj.GetName() != "" || obj.GetGenerateName() != memberPrefix(parent) {
				t.Fatalf("create must use generateName: %+v", obj.GetObjectKind())
			}
			names = append(names, obj.GetGenerateName())
			return fmt.Errorf("create failed")
		}
		return c.Create(ctx, obj, opts...)
	}}).Build()
	r := NewReconciler(c)
	for i := 0; i < 2; i++ {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(parent)})
		if err == nil {
			t.Fatal("ignored create failure")
		}
		if ok, _, _ := r.Expectations.SatisfiedExpectations(expectationKey(parent)); !ok {
			t.Fatal("failed creation retained expectation")
		}
	}
	if len(names) != 2 || names[0] != names[1] {
		t.Fatalf("failed attempts did not use the same generateName prefix: %v", names)
	}
}

func TestPartialCreateBatchWaitsForSuccessfulMembers(t *testing.T) {
	parent := serving(3)
	hide, attempts := true, 0
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if members, ok := list.(*agents.SandboxList); ok && hide {
				members.Items = nil
			}
			return nil
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*agents.Sandbox); ok {
				attempts++
				if attempts == 2 {
					return fmt.Errorf("second create failed")
				}
			}
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
	r := NewReconciler(c)
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(parent)}); err == nil {
		t.Fatal("expected partial batch failure")
	}
	if pending := r.Expectations.GetExpectations(expectationKey(parent))[expectations.Create]; pending.Len() != 1 {
		t.Fatalf("expected one successful create to wait for: %v", pending)
	}
	reconcile(t, r, parent)
	if attempts != 2 {
		t.Fatal("continued creating before the successful member was observed")
	}
	hide = false
	reconcile(t, r, parent)
	var members agents.SandboxList
	if err := c.List(t.Context(), &members); err != nil {
		t.Fatal(err)
	}
	if len(members.Items) != 3 || attempts != 4 {
		t.Fatalf("partial batch did not converge: members=%d attempts=%d", len(members.Items), attempts)
	}
}

func TestConcurrentSessionServiceIsolation(t *testing.T) {
	s := testScheme(t)
	parents := make([]*agents.SessionService, 8)
	objects := make([]client.Object, 8)
	for i := range parents {
		parent := serving(2)
		parent.Name = fmt.Sprintf("serving-%d", i)
		parent.UID = types.UID(parent.Name)
		parents[i] = parent
		objects[i] = parent
	}
	c := fake.NewClientBuilder().WithScheme(s).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(&agents.SessionService{}).WithObjects(objects...).Build()
	r := NewReconciler(c)
	var wg sync.WaitGroup
	errors := make(chan error, len(parents))
	for _, parent := range parents {
		wg.Go(func() {
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(parent)})
			errors <- err
		})
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var members agents.SandboxList
	if err := c.List(context.Background(), &members); err != nil {
		t.Fatal(err)
	}
	if len(members.Items) != 16 {
		t.Fatalf("wrong concurrent member count: %d", len(members.Items))
	}
	for _, sbx := range members.Items {
		owner := metav1.GetControllerOf(&sbx)
		if owner == nil || sbx.Labels[sessionbinding.SessionServiceUIDLabel] != string(owner.UID) {
			t.Fatalf("cross-SessionService identity: %+v", sbx.ObjectMeta)
		}
	}
}

func TestLostCreateResponseConvergesWithoutNameReplay(t *testing.T) {
	for _, restart := range []bool{false, true} {
		for _, bindings := range []string{"{}", `{"000000000000000000000s":{"createdAt":"2026-11-03T10:00:00Z"}}`, "invalid"} {
			t.Run(fmt.Sprintf("restart=%t/bindings=%s", restart, bindings), func(t *testing.T) {
				parent := serving(1)
				hide, attempts := true, 0
				c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent).WithInterceptorFuncs(interceptor.Funcs{
					List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						if err := c.List(ctx, list, opts...); err != nil {
							return err
						}
						if members, ok := list.(*agents.SandboxList); ok && hide {
							members.Items = nil
						}
						return nil
					},
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						err := c.Create(ctx, obj, opts...)
						if _, ok := obj.(*agents.Sandbox); ok {
							attempts++
							if attempts == 1 && err == nil {
								return fmt.Errorf("response lost after commit")
							}
						}
						return err
					},
				}).Build()
				r := NewReconciler(c)
				if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(parent)}); err == nil {
					t.Fatal("expected lost response")
				}
				if restart {
					r = NewReconciler(c)
				}
				reconcile(t, r, parent)
				hide = false
				var members agents.SandboxList
				if err := c.List(t.Context(), &members); err != nil {
					t.Fatal(err)
				}
				if len(members.Items) != 2 || members.Items[0].Name == members.Items[1].Name {
					t.Fatalf("expected two independently generated members: %+v", members.Items)
				}
				for i := range members.Items {
					// Give each member a distinct Session; duplicate repair is tested separately.
					members.Items[i].Annotations[sessionbinding.BindingsAnnotation] = strings.ReplaceAll(bindings, "000000000000000000000s", fmt.Sprintf("%022d", i))
					if err := c.Update(t.Context(), &members.Items[i]); err != nil {
						t.Fatal(err)
					}
				}
				reconcile(t, r, parent)
				if err := c.List(t.Context(), &members); err != nil {
					t.Fatal(err)
				}

				if len(members.Items) != 1 {
					t.Fatalf("surplus did not converge regardless of bindings: %d", len(members.Items))
				}
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(parent), parent); err != nil {
					t.Fatal(err)
				}

			})
		}
	}
}

func immediateExpectationTimeout(t *testing.T) {
	t.Helper()
	previous := expectations.ExpectationTimeout
	expectations.ExpectationTimeout = 0
	t.Cleanup(func() { expectations.ExpectationTimeout = previous })
}

func TestExpiredCreateWaitUsesLatestDesiredState(t *testing.T) {
	immediateExpectationTimeout(t)
	for _, desired := range []int32{0, 1, 2} {
		t.Run(fmt.Sprint(desired), func(t *testing.T) {
			parent := serving(1)
			store := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent).Build()
			hide := false
			c := interceptor.NewClient(store, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if err := c.List(ctx, list, opts...); err != nil {
					return err
				}
				if members, ok := list.(*agents.SandboxList); ok && hide {
					members.Items = nil
				}
				return nil
			}})
			r := NewReconciler(c)
			reconcile(t, r, parent)
			var members agents.SandboxList
			if err := store.List(t.Context(), &members); err != nil {
				t.Fatal(err)
			}
			original := members.Items[0]
			if err := store.Delete(t.Context(), &original); err != nil {
				t.Fatal(err)
			}
			if err := store.Get(t.Context(), client.ObjectKeyFromObject(parent), parent); err != nil {
				t.Fatal(err)
			}
			*parent.Spec.Replicas = desired
			parent.Spec.Template.Spec.Containers[0].Image = "app:latest"
			if err := store.Update(t.Context(), parent); err != nil {
				t.Fatal(err)
			}
			hide = true
			reconcile(t, r, parent) // Expire the old wait without writing any members.
			if err := store.List(t.Context(), &members); err != nil {
				t.Fatal(err)
			}
			if len(members.Items) != 0 {
				t.Fatal("expiration replayed an old create")
			}
			reconcile(t, r, parent)
			hide = false
			if err := store.List(t.Context(), &members); err != nil {
				t.Fatal(err)
			}
			if len(members.Items) != int(desired) {
				t.Fatalf("members=%d desired=%d", len(members.Items), desired)
			}
			for _, sbx := range members.Items {
				if sbx.Name == original.Name || sbx.Spec.Template.Spec.Containers[0].Image != "app:latest" || sbx.Labels[sessionbinding.SessionServiceRevisionLabel] == original.Labels[sessionbinding.SessionServiceRevisionLabel] {
					t.Fatalf("old name or Revision replayed: %+v", sbx)
				}
			}
		})
	}
}

func TestPartialCacheCanTemporarilyOvercreate(t *testing.T) {
	immediateExpectationTimeout(t)
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint(restart), func(t *testing.T) {
			parent := serving(2)
			store := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent).Build()
			hidden := ""
			c := interceptor.NewClient(store, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if err := c.List(ctx, list, opts...); err != nil {
					return err
				}
				if members, ok := list.(*agents.SandboxList); ok {
					items := members.Items[:0]
					for _, sbx := range members.Items {
						if sbx.Name != hidden {
							items = append(items, sbx)
						}
					}
					members.Items = items
				}
				return nil
			}})
			r := NewReconciler(c)
			reconcile(t, r, parent)
			var members agents.SandboxList
			if err := store.List(t.Context(), &members); err != nil {
				t.Fatal(err)
			}
			hidden = members.Items[0].Name
			if restart {
				r = NewReconciler(c)
			} else {
				reconcile(t, r, parent)
			}
			reconcile(t, r, parent)
			if err := store.List(t.Context(), &members); err != nil {
				t.Fatal(err)
			}
			if len(members.Items) != 3 {
				t.Fatalf("partial cache should permit one extra member: %d", len(members.Items))
			}
			hidden = ""
			reconcile(t, r, parent)
			reconcile(t, r, parent)
			if err := store.List(t.Context(), &members); err != nil {
				t.Fatal(err)
			}
			if len(members.Items) != 2 {
				t.Fatalf("complete observation did not converge: %d", len(members.Items))
			}
		})
	}
}

func TestTransferredMemberIsNotModified(t *testing.T) {
	immediateExpectationTimeout(t)
	parent := serving(1)
	s := testScheme(t)
	hide := true
	c := fake.NewClientBuilder().WithScheme(s).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent).WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if err := c.List(ctx, list, opts...); err != nil {
			return err
		}
		if members, ok := list.(*agents.SandboxList); ok && hide {
			members.Items = nil
		}
		return nil
	}}).Build()
	r := NewReconciler(c)
	reconcile(t, r, parent)
	hide = false
	var initial agents.SandboxList
	if err := c.List(context.Background(), &initial); err != nil {
		t.Fatal(err)
	}
	if len(initial.Items) != 1 {
		t.Fatalf("initial members=%d", len(initial.Items))
	}
	sbx := initial.Items[0]
	sbx.OwnerReferences[0].UID = "other-serving"
	sbx.Labels[sessionbinding.SessionServiceUIDLabel] = "other-serving"
	if err := c.Update(context.Background(), &sbx); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, parent)
	var members agents.SandboxList
	if err := c.List(context.Background(), &members); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, parent)
	if err := c.List(t.Context(), &members); err != nil {
		t.Fatal(err)
	}
	if len(members.Items) != 2 {
		t.Fatalf("expected a new owned member without modifying the transferred member: %+v", members.Items)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(&sbx), &sbx); err != nil || sbx.OwnerReferences[0].UID != "other-serving" {
		t.Fatalf("changed foreign object: %v %+v", err, sbx)
	}
}

func TestDeleteExpectationReplacementWaitsForTimeout(t *testing.T) {
	parent := serving(1)
	s := testScheme(t)
	old, other := member(parent, "a"), member(parent, "b")
	c := fake.NewClientBuilder().WithScheme(s).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent, old, other).Build()
	r := NewReconciler(c)
	reconcile(t, r, parent)
	replacement := member(parent, "a")
	replacement.UID = "new-member-uid"
	replacement.ResourceVersion = ""
	replacement.Annotations = map[string]string{sessionbinding.BindingsAnnotation: `{"000000000000000000000s":{"createdAt":"2026-11-03T10:00:00Z"}}`}
	if err := c.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, parent)
	if ok, _, _ := r.Expectations.SatisfiedExpectations(expectationKey(parent)); ok {
		t.Fatal("same-name replacement should wait for observation timeout")
	}
	immediateExpectationTimeout(t)
	reconcile(t, r, parent)
	if ok, _, _ := r.Expectations.SatisfiedExpectations(expectationKey(parent)); !ok {
		t.Fatal("expired deletion wait was not released")
	}
	reconcile(t, r, parent)
	pending := r.Expectations.GetExpectations(expectationKey(parent))[expectations.Delete]
	if len(pending) != 1 || !pending.Has(other.Name) {
		t.Fatalf("expected a deletion wait for the unbound member from the fresh snapshot, got %v", pending)
	}
	if result := reconcile(t, r, parent); result.RequeueAfter != 0 {
		t.Fatalf("new deletion must be observed without another timeout: %+v", result)
	}
	if ok, _, _ := r.Expectations.SatisfiedExpectations(expectationKey(parent)); !ok {
		t.Fatal("new deletion was not observed")
	}
	var members agents.SandboxList
	if err := c.List(context.Background(), &members); err != nil {
		t.Fatal(err)
	}
	if len(members.Items) != 1 || members.Items[0].UID != replacement.UID {
		t.Fatalf("fresh binding preference was not applied after expectation timeout: %+v", members.Items)
	}
}

func TestDeleteExpectationTimeoutRejudgesCachedSnapshot(t *testing.T) {
	immediateExpectationTimeout(t)
	parent := serving(0)
	sbx := member(parent, "old")
	s := testScheme(t)
	stale := true
	deletes := 0
	c := fake.NewClientBuilder().WithScheme(s).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent, sbx).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if members, ok := list.(*agents.SandboxList); ok && stale {
				members.Items = []agents.Sandbox{*sbx.DeepCopy()}
			}
			return nil
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*agents.Sandbox); ok {
				deletes++
				options := &client.DeleteOptions{}
				options.ApplyOptions(opts)
				if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != sbx.UID {
					t.Fatal("delete recovery lost UID precondition")
				}
			}
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()
	r := NewReconciler(c)
	reconcile(t, r, parent)
	reconcile(t, r, parent)
	reconcile(t, r, parent)
	if deletes != 2 {
		t.Fatalf("expired delete did not rejudge and retry the cached snapshot: %d", deletes)
	}
	stale = false
	reconcile(t, r, parent)
	if ok, _, _ := r.Expectations.SatisfiedExpectations(expectationKey(parent)); !ok {
		t.Fatal("cache observation did not settle delete retry")
	}
}

func TestExpectationBookkeepingCleanup(t *testing.T) {
	for _, operation := range []string{"deleted", "delete-event", "recreated"} {
		t.Run(operation, func(t *testing.T) {
			parent := serving(0)
			sbx := member(parent, "old")
			s := testScheme(t)
			c := fake.NewClientBuilder().WithScheme(s).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent, sbx).Build()
			r := NewReconciler(c)
			reconcile(t, r, parent)
			oldKey := expectationKey(parent)
			r.Expectations.ExpectScale(oldKey, expectations.Create, "unobserved")
			if operation == "recreated" {
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(parent), parent); err != nil {
					t.Fatal(err)
				}
				oldParent := parent.DeepCopy()
				parent.UID = "new-serving-uid"
				r.parentExpectationCleanup().Update(event.UpdateEvent{ObjectOld: oldParent, ObjectNew: parent})
				if err := c.Update(context.Background(), parent); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(parent), parent); err != nil {
					t.Fatal(err)
				}
				parent.Finalizers = nil
				if err := c.Update(context.Background(), parent); err != nil {
					t.Fatal(err)
				}
				if err := c.Delete(context.Background(), parent); err != nil {
					t.Fatal(err)
				}
				if operation == "delete-event" {
					r.parentExpectationCleanup().Delete(event.DeleteEvent{Object: parent, DeleteStateUnknown: true})
					if ok, _, _ := r.Expectations.SatisfiedExpectations(oldKey); !ok {
						t.Fatal("delete event retained expectations")
					}
				}
			}
			reconcile(t, r, parent)
			if ok, _, _ := r.Expectations.SatisfiedExpectations(oldKey); !ok {
				t.Fatal("old SessionService kept pending expectations")
			}

		})
	}
}

func TestSessionServiceMembersUseGarbageCollection(t *testing.T) {
	for _, affinity := range []string{"", "BestEffort", "Strict"} {
		t.Run(affinity, func(t *testing.T) {
			parent := serving(1)
			parent.Spec.SessionPolicy.Affinity = affinity
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent).Build()
			reconcile(t, NewReconciler(c), parent)
			var current agents.SessionService
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(parent), &current); err != nil {
				t.Fatal(err)
			}
			if len(current.Finalizers) != 0 {
				t.Fatalf("SessionService deletion depends on cleanup finalizers: %v", current.Finalizers)
			}
			var members agents.SandboxList
			if err := c.List(t.Context(), &members); err != nil {
				t.Fatal(err)
			}
			if len(members.Items) != 1 {
				t.Fatalf("members=%d", len(members.Items))
			}
			box := &members.Items[0]
			owner := metav1.GetControllerOf(box)
			if owner == nil || owner.UID != parent.UID || owner.Kind != "SessionService" || owner.APIVersion != agents.GroupVersion.String() {
				t.Fatalf("member lacks SessionService GC ownership: %+v", owner)
			}
			if len(box.Finalizers) != 0 {
				t.Fatalf("member deletion depends on Session retention: %v", box.Finalizers)
			}
		})
	}
}

func TestDeletingSessionServiceLeavesMembersToGarbageCollector(t *testing.T) {
	parent := serving(1)
	now := metav1.Now()
	parent.DeletionTimestamp = &now
	parent.Finalizers = []string{"test/keep"}
	box := member(parent, "bound")
	box.Annotations = map[string]string{sessionbinding.BindingsAnnotation: `{"000000000000000000000s":{"createdAt":"2026-11-03T10:00:00Z"}}`}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithObjects(parent, box).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			t.Fatal("created during SessionService deletion")
			return nil
		},
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			t.Fatal("controller bypassed garbage collection")
			return nil
		},
	}).Build()
	r := NewReconciler(c)
	r.Expectations.ExpectScale(expectationKey(parent), expectations.Create, "pending")
	if result := reconcile(t, r, parent); result.RequeueAfter != 0 || result.Requeue {
		t.Fatalf("controller waits for GC: %+v", result)
	}
	if ok, _, _ := r.Expectations.SatisfiedExpectations(expectationKey(parent)); !ok {
		t.Fatal("deleting SessionService kept creation expectations")
	}
	var current agents.Sandbox
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(box), &current); err != nil {
		t.Fatal(err)
	}
	if !current.DeletionTimestamp.IsZero() || current.Annotations[sessionbinding.BindingsAnnotation] != box.Annotations[sessionbinding.BindingsAnnotation] {
		t.Fatal("controller changed a member while GC owns deletion")
	}
}

func rollingFixture(t *testing.T, count int32) (*agents.SessionService, client.Client, *Reconciler, []agents.Sandbox) {
	t.Helper()
	parent := serving(count)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent).Build()
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
		sbx.Annotations[sessionbinding.BindingsAnnotation] = `{"000000000000000000000s":{"createdAt":"2026-11-14T00:00:00Z"}}`
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
		if sbx.Labels[sessionbinding.SessionServiceRevisionLabel] != parent.Status.UpdateRevision {
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
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent).Build()
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
			if mode == "obsolete pending" {
				// Obsolete unavailable members must not retain a surge slot due to bindings.
				surge.Annotations[sessionbinding.BindingsAnnotation] = "{"
				if err := c.Update(context.Background(), surge); err != nil {
					t.Fatal(err)
				}
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
			if mode != "replicas reduced" {
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(&old[0]), &agents.Sandbox{}); err != nil {
					t.Fatalf("Ready current member removed without replacement capacity: %v", err)
				}
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
			if mode == "replicas reduced" && len(members.Items) != 0 {
				t.Fatalf("bound zero-scale did not converge: members=%d status=%+v", len(members.Items), parent.Status)
			}
		})
	}
}

func TestRollingUnknownCreateUsesLatestSnapshot(t *testing.T) {
	parent := serving(1)
	parent.Spec.SandboxTemplateSpec.Template.Annotations = map[string]string{"snapshot": "original"}
	var attempted []*agents.Sandbox
	fail := true
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
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
	if first.Name != "" || retry.Name != "" || first.GenerateName == "" || retry.GenerateName == "" || first.Labels[sessionbinding.SessionServiceRevisionLabel] == retry.Labels[sessionbinding.SessionServiceRevisionLabel] || retry.Spec.Template.Spec.Containers[0].Image != "app:v2" || retry.Spec.Template.Annotations["snapshot"] != "" || retry.Spec.Template.Annotations["latest"] != "new" {
		t.Fatalf("unknown create result pinned the old Revision")
	}
}

func TestRollingReplicaReductionBlocksNewCreates(t *testing.T) {
	t.Run("create before delete when surge has space", func(t *testing.T) {
		parent, c, r, _ := rollingFixture(t, 2)
		*parent.Spec.Replicas = 1
		surge := intstr.FromInt32(2)
		parent.Spec.UpdateStrategy.MaxSurge = &surge
		if err := c.Update(t.Context(), parent); err != nil {
			t.Fatal(err)
		}
		reconcileRolling(t, r, parent)
		var members agents.SandboxList
		if err := c.List(t.Context(), &members); err != nil {
			t.Fatal(err)
		}
		ready := 0
		for i := range members.Items {
			if readyMember(&members.Items[i]) {
				ready++
			}
		}
		if len(members.Items) != 3 || ready != 2 {
			t.Fatalf("one reconcile must create without deleting old members: total=%d ready=%d", len(members.Items), ready)
		}
	})

	t.Run("over-capacity keeps Ready old members", func(t *testing.T) {
		parent := serving(2)
		parent.Status.CurrentRevision = "old-revision"
		surge := intstr.FromInt32(1)
		parent.Spec.UpdateStrategy.MaxSurge = &surge
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent).Build()
		r := NewReconciler(c)
		revision, unsupported, err := r.ensureRevision(t.Context(), parent)
		if err != nil || unsupported != "" {
			t.Fatalf("revision: %s %v", unsupported, err)
		}
		for _, name := range []string{"a-old", "b-old", "y-target", "z-target"} {
			sbx := member(parent, name)
			sbx.ResourceVersion = ""
			if name == "a-old" || name == "b-old" {
				sbx.Labels[sessionbinding.SessionServiceRevisionLabel] = "old-revision"
				sbx.Status.Phase = agents.SandboxRunning
				sbx.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
			} else {
				sbx.Labels[sessionbinding.SessionServiceRevisionLabel] = revision
				sbx.Status.Phase = agents.SandboxPending
			}
			if err := c.Create(t.Context(), sbx); err != nil {
				t.Fatal(err)
			}
		}
		reconcileRolling(t, r, parent)
		var members agents.SandboxList
		if err := c.List(t.Context(), &members); err != nil {
			t.Fatal(err)
		}
		ready, targets := 0, 0
		for _, sbx := range members.Items {
			if readyMember(&sbx) {
				ready++
			}
			if sbx.Labels[sessionbinding.SessionServiceRevisionLabel] == revision {
				targets++
			}
		}
		if len(members.Items) != 3 || ready != 2 || targets != 1 {
			t.Fatalf("capacity reclaim must preserve Ready members: total=%d ready=%d targets=%d", len(members.Items), ready, targets)
		}
	})

	t.Run("Ready old surplus at surge limit", func(t *testing.T) {
		parent, c, r, _ := rollingFixture(t, 2)
		*parent.Spec.Replicas = 1
		surge := intstr.FromInt32(1)
		parent.Spec.UpdateStrategy.MaxSurge = &surge
		if err := c.Update(t.Context(), parent); err != nil {
			t.Fatal(err)
		}
		for range 6 {
			reconcileRolling(t, r, parent)
		}
		var members agents.SandboxList
		if err := c.List(t.Context(), &members); err != nil {
			t.Fatal(err)
		}
		ready, target := 0, 0
		for _, sbx := range members.Items {
			if readyMember(&sbx) {
				ready++
			}
			if sbx.Labels[sessionbinding.SessionServiceRevisionLabel] == parent.Status.UpdateRevision {
				target++
			}
		}
		if len(members.Items) != 2 || ready != 1 || target != 1 {
			t.Fatalf("Ready surplus blocked new revision: total=%d ready=%d target=%d", len(members.Items), ready, target)
		}
	})

	t.Run("over-cap old members", func(t *testing.T) {
		parent, c, r, _ := rollingFixture(t, 3)
		reconcileRolling(t, r, parent)
		*parent.Spec.Replicas = 1
		if err := c.Update(context.Background(), parent); err != nil {
			t.Fatal(err)
		}
		// Name ordering can reclaim the Pending target first; allow recreation and observation.
		for range 8 {
			reconcileRolling(t, r, parent)
		}
		var members agents.SandboxList
		if err := c.List(context.Background(), &members); err != nil {
			t.Fatal(err)
		}

		if len(members.Items) != 2 {
			t.Fatalf("reduced rollout did not converge to one old and one pending target: %d", len(members.Items))
		}
		ready, target := 0, 0
		for _, sbx := range members.Items {
			if readyMember(&sbx) {
				ready++
			}
			if sbx.Labels[sessionbinding.SessionServiceRevisionLabel] == parent.Status.UpdateRevision {
				target++
			}
		}
		if ready != 1 || target != 1 {
			t.Fatalf("binding-independent shrink lost rollout readiness budget: ready=%d target=%d status=%+v", ready, target, parent.Status)
		}

	})
	for _, tt := range []struct {
		name       string
		bindings   string
		oldPresent bool
	}{
		{name: "empty target surplus", bindings: "{}"},
		{name: "bound target surplus", bindings: `{"000000000000000000000s":{"createdAt":"2026-11-14T00:00:00Z"}}`},
		{name: "invalid target bindings", bindings: "{"},
		{name: "bound pending targets with Ready old", bindings: `{"000000000000000000000s":{"createdAt":"2026-11-14T00:00:00Z"}}`, oldPresent: true},
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
				if sbx.Labels[sessionbinding.SessionServiceRevisionLabel] != parent.Status.UpdateRevision {
					continue
				}
				sbx.Status.Phase = agents.SandboxRunning
				sbx.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
				sbx.Annotations[sessionbinding.BindingsAnnotation] = strings.ReplaceAll(tt.bindings, "000000000000000000000s", fmt.Sprintf("%022d", i))
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
				restored.Annotations[sessionbinding.BindingsAnnotation] = "{}"
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

			want := 1
			if tt.oldPresent {
				want = 2
			}
			if len(members.Items) != want {
				t.Fatalf("surplus must shrink regardless of bindings: members=%d status=%+v", len(members.Items), parent.Status)
			}
			if tt.oldPresent {
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(&old[0]), &agents.Sandbox{}); err != nil {
					t.Fatalf("pending targets authorized deleting Ready old: %v", err)
				}
			} else if parent.Status.CurrentRevision != parent.Status.UpdateRevision {
				t.Fatalf("Ready target revision did not converge: %+v", parent.Status)
			}

		})
	}
}

func TestRollingRejectedCreateUsesCorrectedTemplate(t *testing.T) {
	for _, reason := range []metav1.StatusReason{metav1.StatusReasonInvalid, metav1.StatusReasonForbidden, metav1.StatusReasonBadRequest, metav1.StatusReasonUnauthorized} {
		t.Run(string(reason), func(t *testing.T) {
			parent := serving(1)
			var attempts []*agents.Sandbox
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
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
			if len(attempts) != 2 || attempts[1].Spec.Template.Spec.Containers[0].Image != "app:corrected" || attempts[0].Labels[sessionbinding.SessionServiceRevisionLabel] == attempts[1].Labels[sessionbinding.SessionServiceRevisionLabel] {
				t.Fatal("explicit rejection pinned the old rejected template")
			}
		})
	}
}

func TestRollingRollbackReusesRevisionSnapshot(t *testing.T) {
	parent, c, r, old := rollingFixture(t, 1)
	originalRevision := old[0].Labels[sessionbinding.SessionServiceRevisionLabel]
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
	if len(members.Items) != 1 || members.Items[0].Labels[sessionbinding.SessionServiceRevisionLabel] != originalRevision || parent.Status.CurrentRevision != originalRevision || parent.Status.UpdateRevision != originalRevision {
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

func TestInitialStatusUpdateIncludesRequiredCounts(t *testing.T) {
	for _, tt := range []struct {
		name        string
		unsupported bool
		legacy      metav1.ConditionStatus
	}{
		{name: "supported"},
		{name: "unsupported", unsupported: true},
		{name: "legacy blocked", legacy: metav1.ConditionTrue},
		{name: "legacy not blocked", legacy: metav1.ConditionFalse},
		{name: "unsupported with legacy condition", unsupported: true, legacy: metav1.ConditionTrue},
	} {
		t.Run(tt.name, func(t *testing.T) {
			parent := serving(0)
			if tt.unsupported {
				parent.Spec.SandboxTemplateSpec.BuildOptions = &agents.SandboxTemplateBuildOptions{}
			}
			other := metav1.Condition{Type: "External", Status: metav1.ConditionTrue, Reason: "Unchanged", LastTransitionTime: metav1.NewTime(time.Now().UTC().Truncate(time.Second))}
			parent.Status.Conditions = []metav1.Condition{other}
			if tt.legacy != "" {
				parent.Status.Conditions = append(parent.Status.Conditions, metav1.Condition{Type: "ScaleDownBlocked", Status: tt.legacy, Reason: "Legacy", LastTransitionTime: metav1.Now()})
			}
			updates := 0
			store := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent).Build()
			c := interceptor.NewClient(store, interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, name string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				data, err := json.Marshal(obj)
				if err != nil {
					return err
				}
				var payload map[string]interface{}
				if err := json.Unmarshal(data, &payload); err != nil {
					return err
				}
				status := payload["status"].(map[string]interface{})
				for _, key := range []string{"replicas", "readyReplicas"} {
					if value, exists := status[key]; !exists || value != float64(0) {
						t.Fatalf("required initial status %s missing or nonzero: %v", key, value)
					}
				}
				conditions := obj.(*agents.SessionService).Status.Conditions
				if meta.FindStatusCondition(conditions, "ScaleDownBlocked") != nil {
					t.Fatal("obsolete ScaleDownBlocked condition survived reconciliation")
				}
				if got := meta.FindStatusCondition(conditions, other.Type); got == nil || !apiequality.Semantic.DeepEqual(*got, other) {
					t.Fatalf("unrelated condition changed: %+v", got)
				}
				updates++
				return c.SubResource(name).Update(ctx, obj, opts...)
			}})
			r := NewReconciler(c)
			reconcile(t, r, parent)
			reconcile(t, r, parent)
			if updates != 1 {
				t.Fatalf("unchanged initial status updated %d times", updates)
			}
		})
	}
}

func TestCalculateStatusSnapshot(t *testing.T) {
	for _, tt := range []struct {
		name         string
		phase        agents.SandboxPhase
		ready        bool
		owned        bool
		wantReplicas int32
		wantReady    int32
	}{
		{name: "ready", phase: agents.SandboxRunning, ready: true, owned: true, wantReplicas: 1, wantReady: 1},
		{name: "pending", phase: agents.SandboxPending, owned: true, wantReplicas: 1},
		{name: "unready", phase: agents.SandboxRunning, owned: true, wantReplicas: 1},
		{name: "failed", phase: agents.SandboxFailed, owned: true},
		{name: "foreign", phase: agents.SandboxRunning, ready: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			parent := serving(1)
			sbx := member(parent, "member")
			sbx.Status.Phase = tt.phase
			if tt.ready {
				sbx.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
			}
			if !tt.owned {
				sbx.OwnerReferences[0].UID = "foreign"
			}
			snapshot := []agents.Sandbox{*sbx}
			before := sbx.DeepCopy()
			parentBefore := parent.DeepCopy()
			status, active := calculateStatus(parent, snapshot)
			if status.Replicas != tt.wantReplicas || status.ReadyReplicas != tt.wantReady || len(active) != int(tt.wantReplicas) {
				t.Fatalf("status=%+v active=%d", status, len(active))
			}
			if status.Selector != sessionbinding.SessionServiceUIDLabel+"="+string(parent.UID) {
				t.Fatalf("unexpected scale selector: %q", status.Selector)
			}
			status.Conditions = append(status.Conditions, metav1.Condition{Type: "Test"})
			if !reflect.DeepEqual(&snapshot[0], before) || !reflect.DeepEqual(parent, parentBefore) {
				t.Fatal("status computation mutated cached input")
			}
		})
	}
}

func TestStatusUpdatePreservesRVConflictAndInputs(t *testing.T) {
	parent := serving(0)
	s := testScheme(t)
	store := fake.NewClientBuilder().WithScheme(s).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent).Build()
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(parent), parent); err != nil {
		t.Fatal(err)
	}
	previous := agents.SessionServiceStatus{CurrentRevision: "gone", UpdateRevision: "gone", Selector: "old=value"}
	desired := &agents.SessionServiceStatus{}
	parent.Status = previous
	if err := store.Status().Update(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	stale := &agents.SessionService{}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(parent), stale); err != nil {
		t.Fatal(err)
	}
	staleBefore := stale.DeepCopy()
	parent.Spec.Ports = []int32{8081}
	if err := store.Update(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	r := NewReconciler(store)
	err := r.updateStatus(context.Background(), stale, desired)
	if !apierrors.IsConflict(err) {
		t.Fatalf("stale status RV was not rejected as Conflict: %v", err)
	}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(parent), parent); err != nil {
		t.Fatal(err)
	}
	before := previous.DeepCopy()
	desiredBefore := desired.DeepCopy()
	fresh := &agents.SessionService{}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(parent), fresh); err != nil {
		t.Fatal(err)
	}
	freshBefore := fresh.DeepCopy()
	if err := r.updateStatus(context.Background(), fresh, desired); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stale, staleBefore) || !reflect.DeepEqual(fresh, freshBefore) {
		t.Fatal("status update mutated cached snapshots")
	}
	if previous.Selector != before.Selector || previous.CurrentRevision != before.CurrentRevision || desired.CurrentRevision != desiredBefore.CurrentRevision {
		t.Fatal("status update mutated its input snapshots")
	}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(parent), parent); err != nil {
		t.Fatal(err)
	}
	if parent.Status.CurrentRevision != "" || parent.Status.UpdateRevision != "" || parent.Status.Selector != "" {
		t.Fatalf("omitted status fields survived: %+v", parent.Status)
	}
}

func TestReconcileUsesReadOnlyMemberSnapshot(t *testing.T) {
	parent := serving(0)
	sbx := member(parent, "member")
	snapshot := []agents.Sandbox{*sbx}
	before := sbx.DeepCopy()
	unsafeRead := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent, sbx).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if members, ok := list.(*agents.SandboxList); ok {
				options := &client.ListOptions{}
				options.ApplyOptions(opts)
				unsafeRead = options.UnsafeDisableDeepCopy != nil && *options.UnsafeDisableDeepCopy
				members.Items = snapshot
				return nil
			}
			return c.List(ctx, list, opts...)
		},
	}).Build()
	reconcile(t, NewReconciler(c), parent)
	if !unsafeRead {
		t.Fatal("member list did not disable DeepCopy")
	}
	if !reflect.DeepEqual(&snapshot[0], before) {
		t.Fatal("reconciliation mutated cached member")
	}
}

func TestMemberNamePrefixAndRevisionIdentity(t *testing.T) {
	for _, name := range []string{"service", strings.Repeat("a", 39) + ".long-name"} {
		parent := serving(1)
		parent.Name = name
		prefix := memberPrefix(parent)
		expected := strings.TrimRight(name[:min(len(name), 40)], "-.") + "-"
		if prefix != expected {
			t.Fatalf("prefix=%q want=%q", prefix, expected)
		}
		first := provisionRevisionName(parent, []byte("template"))
		parent.UID = "replacement-uid"
		if memberPrefix(parent) != prefix {
			t.Fatal("member name prefix depends on parent UID")
		}
		if provisionRevisionName(parent, []byte("template")) == first {
			t.Fatal("revision identity lost parent UID isolation")
		}
	}
}

func TestDeletionPrefersEmptyBindingsWithinBudgets(t *testing.T) {
	for _, tt := range []struct {
		name, annotation       string
		emptyReady, boundReady bool
		emptyUpdated           bool
		want                   string
	}{
		{name: "bound before empty by name", annotation: `{"000000000000000000000s":{"createdAt":"2026-11-03T10:00:00Z"}}`, emptyReady: true, boundReady: true, want: "z-empty"},
		{name: "malformed is not empty", annotation: `{`, emptyReady: true, boundReady: true, want: "z-empty"},
		{name: "ready budget overrides preference", annotation: `{`, emptyReady: true, boundReady: false, want: "a-bound"},
		{name: "target revision protected", annotation: `{`, emptyReady: true, boundReady: true, emptyUpdated: true, want: "a-bound"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			parent := serving(1)
			bound, empty := member(parent, "a-bound"), member(parent, "z-empty")
			bound.Annotations = map[string]string{sessionbinding.BindingsAnnotation: tt.annotation}
			ready := int32(0)
			for sbx, yes := range map[*agents.Sandbox]bool{bound: tt.boundReady, empty: tt.emptyReady} {
				sbx.Status.Phase = agents.SandboxPending
				if yes {
					sbx.Status.Phase = agents.SandboxRunning
					sbx.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
					ready++
				}
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithObjects(parent, bound, empty).Build()
			r := NewReconciler(c)
			status := &agents.SessionServiceStatus{Replicas: 2, ReadyReplicas: ready}
			members := memberRevisions{old: []*agents.Sandbox{bound, empty}, total: 2}
			if tt.emptyUpdated {
				empty.Labels[sessionbinding.SessionServiceRevisionLabel] = "new-revision"
				members.old = []*agents.Sandbox{bound}
				members.updated = []*agents.Sandbox{empty}
			}
			err := r.scaleDown(t.Context(), parent, "new-revision", members, 1, 0, status, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, sbx := range []*agents.Sandbox{bound, empty} {
				var actual agents.Sandbox
				err := c.Get(t.Context(), client.ObjectKeyFromObject(sbx), &actual)
				if sbx.Name == tt.want {
					if !apierrors.IsNotFound(err) {
						t.Fatalf("expected %s deleted, err=%v", sbx.Name, err)
					}
				} else if err != nil {
					t.Fatalf("kept member missing: %v", err)
				}
			}
		})
	}
}

func TestRevisionUIDCacheIndex(t *testing.T) {
	objects := []client.Object{}
	for _, tt := range []struct{ name, namespace, uid string }{
		{"matching", "ns", "parent"}, {"other-parent", "ns", "other"}, {"unlabeled", "ns", ""}, {"other-namespace", "other", "parent"},
	} {
		objects = append(objects, &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{Name: tt.name, Namespace: tt.namespace, Labels: map[string]string{sessionbinding.SessionServiceUIDLabel: tt.uid}}, Revision: 1})
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithObjects(objects...).Build()
	var revisions appsv1.ControllerRevisionList
	if err := c.List(t.Context(), &revisions, client.InNamespace("ns"), client.MatchingFields{revisionUIDIndex: "parent"}); err != nil {
		t.Fatal(err)
	}
	if len(revisions.Items) != 1 || revisions.Items[0].Name != "matching" {
		t.Fatalf("unexpected revisions: %+v", revisions.Items)
	}
	if keys := revisionUID(&appsv1.ControllerRevision{}); len(keys) != 0 {
		t.Fatalf("unlabeled indexed: %v", keys)
	}
}

func TestMemberWritesRelyOnWatchReconciliation(t *testing.T) {
	parent := serving(1)
	hide := true
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if members, ok := list.(*agents.SandboxList); ok && hide {
				members.Items = nil
			}
			return nil
		},
	}).Build()
	r := NewReconciler(c)
	if result := reconcile(t, r, parent); result.RequeueAfter != 0 {
		t.Fatalf("member write added trailing timer: %+v", result)
	}
	if satisfied, _, _ := r.Expectations.SatisfiedExpectations(expectationKey(parent)); satisfied {
		t.Fatal("creation did not register expectation")
	}
	if result := reconcile(t, r, parent); result.RequeueAfter != observationDelay {
		t.Fatalf("pending expectation lost initial wait: %+v", result)
	}
	hide = false
	reconcile(t, r, parent)
	if satisfied, _, _ := r.Expectations.SatisfiedExpectations(expectationKey(parent)); !satisfied {
		t.Fatal("watch-triggered snapshot did not satisfy expectation")
	}
}

// The normal path completes without waiting for another informer observation.
func TestRebindingCompletesInSingleReconcile(t *testing.T) {
	for _, mode := range []string{"normal", "terminating source"} {
		t.Run(mode, func(t *testing.T) {
			const sid = "000000000000000session"
			parent := serving(2)
			parent.Spec.SessionPolicy.Affinity = "BestEffort"
			source, target := member(parent, "source"), member(parent, "target")
			if mode == "terminating source" {
				now := metav1.Now()
				source.DeletionTimestamp = &now
				source.Finalizers = []string{"test/retain"}
			}
			record := sessionbinding.Record{CreatedAt: "2026-11-18T10:00:00Z", Phase: sessionbinding.Moving}
			var err error
			source.Annotations, err = sessionbinding.Encode(map[string]string{"example.com/keep": "yes"}, sessionbinding.Bindings{sid: record, "00000000000000000other": {CreatedAt: "2026-11-18T09:00:00Z"}})
			if err != nil {
				t.Fatal(err)
			}
			target.Status.Phase = agents.SandboxRunning
			target.Status.PodInfo.PodIP = "10.0.0.2"
			target.Status.Conditions = []metav1.Condition{{Type: string(agents.SandboxConditionReady), Status: metav1.ConditionTrue}}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(parent, source, target).Build()
			var list agents.SandboxList
			if err := c.List(t.Context(), &list); err != nil {
				t.Fatal(err)
			}
			changed, pending, err := NewReconciler(c).reconcileRebindings(t.Context(), parent, list.Items)
			if err != nil || !changed || !pending {
				t.Fatalf("changed=%t pending=%t err=%v", changed, pending, err)
			}
			var current agents.Sandbox
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(source), &current); err != nil {
				t.Fatal(err)
			}
			records, err := sessionbinding.Decode(current.Annotations)
			if err != nil || len(records) != 1 || records["00000000000000000other"].CreatedAt != "2026-11-18T09:00:00Z" || current.Annotations["example.com/keep"] != "yes" {
				t.Fatalf("source=%+v err=%v", current.Annotations, err)
			}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(target), &current); err != nil {
				t.Fatal(err)
			}
			records, err = sessionbinding.Decode(current.Annotations)
			if err != nil || records[sid].Phase != "" || records[sid].CreatedAt != record.CreatedAt {
				t.Fatalf("target=%+v err=%v", records, err)
			}
		})
	}
}

func TestRebindingUnknownWriteRecovery(t *testing.T) {
	for _, loseAt := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("lost patch %d", loseAt), func(t *testing.T) {
			const sid = "000000000000000session"
			parent := serving(2)
			source, target := member(parent, "source"), member(parent, "target")
			record := sessionbinding.Record{CreatedAt: "2026-11-18T10:00:00Z", Phase: sessionbinding.Moving}
			var err error
			source.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{sid: record})
			if err != nil {
				t.Fatal(err)
			}
			target.Status.Phase = agents.SandboxRunning
			target.Status.PodInfo.PodIP = "10.0.0.2"
			target.Status.Conditions = []metav1.Condition{{Type: string(agents.SandboxConditionReady), Status: metav1.ConditionTrue}}
			patches := 0
			lost := false
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(parent, source, target).WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches++
				if err := c.Patch(ctx, obj, patch, opts...); err != nil {
					return err
				}
				if patches == loseAt {
					lost = true
					return context.DeadlineExceeded
				}
				return nil
			}}).Build()
			for range 8 {
				var list agents.SandboxList
				if err := c.List(t.Context(), &list); err != nil {
					t.Fatal(err)
				}
				_, _, err := NewReconciler(c).reconcileRebindings(t.Context(), parent, list.Items)
				if err != nil && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			}
			if !lost {
				t.Fatal("write result loss was not injected")
			}
			var current agents.Sandbox
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(source), &current); err != nil {
				t.Fatal(err)
			}
			records, err := sessionbinding.Decode(current.Annotations)
			if err != nil || len(records) != 0 {
				t.Fatalf("source=%+v %v", records, err)
			}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(target), &current); err != nil {
				t.Fatal(err)
			}
			records, err = sessionbinding.Decode(current.Annotations)
			if err != nil || records[sid].CreatedAt != record.CreatedAt || records[sid].Phase != "" {
				t.Fatalf("target=%+v %v", records, err)
			}
		})
	}
}

func TestRebindingTargetFailure(t *testing.T) {
	for _, mode := range []string{"full", "storage full", "stale full", "stale available", "parent limit lowered", "missing", "recreated", "foreign target", "unready", "paused", "expired", "already bound full", "already bound unready", "already bound paused", "already bound deleting", "already bound starts deleting", "already bound terminal", "already bound deleting no alternative", "deleting", "unrelated duplicate", "no alternative", "preferred available"} {
		t.Run(mode, func(t *testing.T) {
			const sid = "000000000000000session"
			parent := serving(3)
			limit := int32(1)
			parent.Spec.SessionPolicy.MaxSessionsPerSandbox = &limit
			source, target, next := member(parent, "source"), member(parent, "target"), member(parent, "next")
			for _, box := range []*agents.Sandbox{target, next} {
				box.Status.Phase = agents.SandboxRunning
				box.Status.PodInfo.PodIP = "10.0.0.2"
				box.Status.Conditions = []metav1.Condition{{Type: string(agents.SandboxConditionReady), Status: metav1.ConditionTrue}}
			}
			record := sessionbinding.Record{CreatedAt: "2026-11-18T10:00:00Z", Phase: sessionbinding.Moving, TargetSandboxUID: string(target.UID)}
			if mode == "recreated" {
				target.UID = "replacement-uid"
			}
			if mode == "foreign target" {
				target.OwnerReferences[0].UID = "another-parent"
			}
			if mode == "paused" {
				target.Spec.Paused = true
			}
			if mode == "expired" {
				timestamp := metav1.NewTime(time.Now().Add(-time.Hour))
				target.Spec.ShutdownTime = &timestamp
			}
			if mode == "storage full" {
				limit = 2
			}
			var err error
			source.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{sid: record})
			if err != nil {
				t.Fatal(err)
			}
			targetRecords := sessionbinding.Bindings{"00000000000000000other": {CreatedAt: record.CreatedAt}}
			if strings.HasPrefix(mode, "already bound") {
				targetRecords = sessionbinding.Bindings{sid: {CreatedAt: record.CreatedAt}}
			}
			if mode == "unready" || mode == "already bound unready" {
				target.Status.Conditions = nil
			}
			if mode == "already bound paused" {
				target.Spec.Paused = true
			}
			if mode == "already bound terminal" {
				target.Status.Phase = agents.SandboxFailed
			}
			if mode == "already bound deleting" || mode == "already bound deleting no alternative" || mode == "deleting" {
				timestamp := metav1.Now()
				target.DeletionTimestamp = &timestamp
				target.Finalizers = []string{"test/retain"}
			}
			if mode == "unready" || mode == "stale available" || mode == "preferred available" || mode == "deleting" {
				targetRecords = sessionbinding.Bindings{}
			}
			target.Annotations, err = sessionbinding.Encode(nil, targetRecords)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "storage full" {
				size := 0
				for key, value := range target.Annotations {
					size += len(key) + len(value)
				}
				const paddingKey = "example.com/padding"
				target.Annotations[paddingKey] = strings.Repeat("x", sessionbinding.MaxAnnotationBytes-size-len(paddingKey)-40)
			}
			if mode == "unrelated duplicate" {
				next.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{sid: {CreatedAt: record.CreatedAt}})
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "no alternative" || mode == "already bound deleting no alternative" {
				next.Status.Conditions = nil
			}
			objects := []client.Object{parent, source, next}
			if mode != "missing" {
				objects = append(objects, target)
			}
			if mode == "already bound starts deleting" {
				target.Finalizers = []string{"test/retain"}
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if err := c.Patch(ctx, obj, patch, opts...); err != nil {
						return err
					}
					if mode == "already bound starts deleting" && obj.GetName() == target.Name {
						if err := c.Delete(ctx, obj); err != nil {
							return err
						}
						return c.Get(ctx, client.ObjectKeyFromObject(obj), obj)
					}
					return nil
				},
			}).Build()
			var list agents.SandboxList
			if err := c.List(t.Context(), &list); err != nil {
				t.Fatal(err)
			}
			if mode == "stale full" || mode == "stale available" {
				var current agents.Sandbox
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(target), &current); err != nil {
					t.Fatal(err)
				}
				records := sessionbinding.Bindings{}
				if mode == "stale available" {
					records[sid] = sessionbinding.Record{CreatedAt: record.CreatedAt}
				}
				current.Annotations, err = sessionbinding.Encode(nil, records)
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Update(t.Context(), &current); err != nil {
					t.Fatal(err)
				}
			}
			changed, pending, err := NewReconciler(c).reconcileRebindings(t.Context(), parent, list.Items)
			if !pending {
				t.Fatal("lost migration task")
			}
			if err != nil {
				t.Fatal(err)
			}
			var current agents.Sandbox
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(source), &current); err != nil {
				t.Fatal(err)
			}
			records, err := sessionbinding.Decode(current.Annotations)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "stale available", "already bound starts deleting":
				if !changed || records[sid] != record {
					t.Fatalf("conflicting write changed migration target: %+v", records)
				}
			case "no alternative", "already bound deleting no alternative":
				if changed || records[sid] != record {
					t.Fatalf("no alternative lost hint: %+v", records)
				}
			default:
				if !changed || len(records) != 0 {
					t.Fatalf("source not cleaned after recovery: %+v", records)
				}
				chosen := next
				if strings.HasPrefix(mode, "already bound") && mode != "already bound deleting" && mode != "already bound terminal" || mode == "preferred available" {
					chosen = target
				}
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(chosen), &current); err != nil {
					t.Fatal(err)
				}
				active, err := sessionbinding.Decode(current.Annotations)
				if err != nil || active[sid].CreatedAt != record.CreatedAt || active[sid].Phase != "" {
					t.Fatalf("recovery target=%+v err=%v", active, err)
				}
				if mode == "deleting" {
					if err := c.Get(t.Context(), client.ObjectKeyFromObject(target), &current); err != nil {
						t.Fatal(err)
					}
					records, err := sessionbinding.Decode(current.Annotations)
					if err != nil || len(records) != 0 {
						t.Fatalf("terminating target accepted binding: %+v err=%v", records, err)
					}
				}
			}
		})
	}
}

func TestRebindingBatchProgress(t *testing.T) {
	for _, mode := range []string{"success", "conflict", "error"} {
		t.Run(mode, func(t *testing.T) {
			parent := serving(4)
			var objects []client.Object
			for i := 0; i < 2; i++ {
				source := member(parent, fmt.Sprintf("source-%d", i))
				target := member(parent, fmt.Sprintf("target-%d", i))
				// Distinct sessions avoid a duplicate binding across independent tasks.
				id := fmt.Sprintf("%021d%d", 0, i)
				record := sessionbinding.Record{CreatedAt: "2026-11-18T10:00:00Z", Phase: sessionbinding.Moving, TargetSandboxUID: string(target.UID)}
				var err error
				source.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{id: record})
				if err != nil {
					t.Fatal(err)
				}
				target.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{id: {CreatedAt: record.CreatedAt}})
				if err != nil {
					t.Fatal(err)
				}
				objects = append(objects, source, target)
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if obj.GetName() == "source-0" {
					if mode == "conflict" {
						return apierrors.NewConflict(agents.GroupVersion.WithResource("sandboxs").GroupResource(), obj.GetName(), errors.New("concurrent write"))
					}
					if mode == "error" {
						return errors.New("patch failed")
					}
				}
				return c.Patch(ctx, obj, patch, opts...)
			}}).Build()
			var snapshot agents.SandboxList
			if err := c.List(t.Context(), &snapshot); err != nil {
				t.Fatal(err)
			}
			changed, pending, err := NewReconciler(c).reconcileRebindings(t.Context(), parent, snapshot.Items)
			if !changed || !pending {
				t.Fatalf("lost batch progress: changed=%v pending=%v err=%v", changed, pending, err)
			}
			if (err != nil) != (mode == "error") {
				t.Fatalf("unexpected batch error: %v", err)
			}
			for i := 0; i < 2; i++ {
				var source agents.Sandbox
				if err := c.Get(t.Context(), client.ObjectKey{Namespace: parent.Namespace, Name: fmt.Sprintf("source-%d", i)}, &source); err != nil {
					t.Fatal(err)
				}
				records, err := sessionbinding.Decode(source.Annotations)
				if err != nil {
					t.Fatal(err)
				}
				want := 0
				if i == 0 && mode != "success" {
					want = 1
				}
				if len(records) != want {
					t.Fatalf("source %d: got %d bindings, want %d", i, len(records), want)
				}
			}
		})
	}
}

func TestExpireBindings(t *testing.T) {
	now := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	for _, mode := range []string{"disabled", "before deadline", "at deadline", "moving", "foreign", "conflict", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			parent := serving(1)
			parent.Spec.SessionPolicy.ExpireAfterCreation = "1h"
			box := member(parent, "source")
			record := sessionbinding.Record{CreatedAt: "2026-11-19T00:00:00Z"}
			if mode == "disabled" {
				parent.Spec.SessionPolicy.ExpireAfterCreation = ""
			}
			if mode == "before deadline" {
				record.CreatedAt = "2026-11-19T00:30:00Z"
			}
			if mode == "moving" {
				record.Phase = sessionbinding.Moving
				record.TargetSandboxUID = "unavailable-target"
			}
			if mode == "foreign" {
				box.OwnerReferences[0].UID = "foreign"
			}
			var err error
			box.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{"000000000000000session": record})
			if err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(parent, box).Build()
			var snapshot agents.SandboxList
			if err := c.List(t.Context(), &snapshot); err != nil {
				t.Fatal(err)
			}
			if mode == "conflict" {
				current := box.DeepCopy()
				current.Annotations["example.com/concurrent"] = "preserved"
				if err := c.Update(t.Context(), current); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			changed, next, err := NewReconciler(c).expireBindings(ctx, parent, snapshot.Items, now)
			if mode == "canceled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("expected cancellation: %v", err)
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
			wantRemoved := mode == "at deadline" || mode == "moving"
			if changed != wantRemoved || (len(records) == 0) != wantRemoved {
				t.Fatalf("changed=%v records=%v", changed, records)
			}
			if mode == "before deadline" && next != 30*time.Minute {
				t.Fatalf("next=%v", next)
			}
			if mode == "conflict" && (next != observationDelay || current.Annotations["example.com/concurrent"] != "preserved") {
				t.Fatalf("stale snapshot was replayed: %+v next=%v", current.Annotations, next)
			}
		})
	}
}

func TestRebindingSkipsInvalidMemberBindings(t *testing.T) {
	for _, mode := range []string{"migration", "duplicate repair"} {
		t.Run(mode, func(t *testing.T) {
			parent := serving(3)
			const id = "000000000000000session"
			source, target, broken := member(parent, "source"), member(parent, "target"), member(parent, "broken")
			for _, box := range []*agents.Sandbox{target, broken} {
				box.Status.Phase = agents.SandboxRunning
				box.Status.PodInfo.PodIP = "10.0.0.2"
				box.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
			}
			record := sessionbinding.Record{CreatedAt: "2026-11-18T10:00:00Z"}
			if mode == "migration" {
				record.Phase, record.TargetSandboxUID = sessionbinding.Moving, string(target.UID)
			} else {
				var err error
				target.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{id: record})
				if err != nil {
					t.Fatal(err)
				}
			}
			var err error
			source.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{id: record})
			if err != nil {
				t.Fatal(err)
			}
			broken.Annotations = map[string]string{sessionbinding.BindingsAnnotation: "invalid"}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(source, target, broken).WithInterceptorFuncs(interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if obj.GetName() == broken.Name {
						t.Fatal("corrupt binding member was used as a target or overwritten")
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			}).Build()
			var list agents.SandboxList
			if err := c.List(t.Context(), &list); err != nil {
				t.Fatal(err)
			}
			changed, pending, err := NewReconciler(c).reconcileRebindings(t.Context(), parent, list.Items)
			if err != nil || !changed || !pending {
				t.Fatalf("healthy bindings blocked: %v/%v %v", changed, pending, err)
			}
			for _, box := range []*agents.Sandbox{source, target, broken} {
				var current agents.Sandbox
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(box), &current); err != nil {
					t.Fatal(err)
				}
				if box == broken {
					if current.Annotations[sessionbinding.BindingsAnnotation] != "invalid" {
						t.Fatal("corrupt annotation was reset")
					}
					continue
				}
				records, err := sessionbinding.Decode(current.Annotations)
				if err != nil || (box == target && (records[id].Phase != "" || records[id].CreatedAt != record.CreatedAt)) || box == source && len(records) != 0 {
					t.Fatalf("%s bindings=%v err=%v", box.Name, records, err)
				}
			}
		})
	}
}

func TestBindingMaintenanceDoesNotBlockMemberDeletion(t *testing.T) {
	for _, mode := range []string{"rebinding success", "unknown source", "unknown target", "unknown committed target", "rejected source", "corrupt source", "expiry success", "expiry unknown", "rollout success", "rollout unknown target"} {
		t.Run(mode, func(t *testing.T) {
			parent := serving(0)
			source, target, spare := member(parent, "a-source"), member(parent, "b-target"), member(parent, "z-spare")
			target.Status.Phase = agents.SandboxRunning
			target.Status.PodInfo.PodIP = "10.0.0.2"
			target.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
			const id = "000000000000000session"
			record := sessionbinding.Record{CreatedAt: time.Now().UTC().Format(time.RFC3339), Phase: sessionbinding.Moving, TargetSandboxUID: string(target.UID)}
			if strings.HasPrefix(mode, "expiry") {
				parent.Spec.SessionPolicy.ExpireAfterCreation = "1h"
				record.CreatedAt = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
			}
			var err error
			source.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{id: record})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "corrupt source" {
				parent.Spec.SessionPolicy.ExpireAfterCreation = "1h"
				source.Annotations[sessionbinding.BindingsAnnotation] = "invalid"
			}
			unknownName := ""
			if mode == "unknown source" || mode == "expiry unknown" {
				unknownName = source.Name
			} else if mode == "unknown target" || mode == "unknown committed target" || mode == "rollout unknown target" {
				unknownName = target.Name
			}
			if strings.HasPrefix(mode, "rollout") {
				*parent.Spec.Replicas = 1
				parent.Spec.SandboxTemplateSpec.Template.Spec.Containers[0].Image = "app:v2"
			}
			lostResponse := errors.New("binding response lost")
			inject := true
			var deletes []string
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent, source, target, spare).WithInterceptorFuncs(interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if inject && obj.GetName() == unknownName {
						if mode == "unknown committed target" {
							if err := c.Patch(ctx, obj, patch, opts...); err != nil {
								return err
							}
						}
						return lostResponse
					}
					if inject && mode == "rejected source" && obj.GetName() == source.Name {
						return apierrors.NewForbidden(agents.Resource("sandboxes"), obj.GetName(), errors.New("denied"))
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if _, ok := obj.(*agents.Sandbox); !ok {
						return c.Delete(ctx, obj, opts...)
					}
					deletes = append(deletes, obj.GetName())
					if inject && obj.GetName() == unknownName {
						t.Fatal("attempted deletion after an unknown binding write")
					}
					options := &client.DeleteOptions{}
					options.ApplyOptions(opts)
					var current agents.Sandbox
					if err := c.Get(ctx, client.ObjectKeyFromObject(obj), &current); err != nil {
						return err
					}
					if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil {
						t.Fatal("missing snapshot DELETE conditions")
					}
					if current.UID != *options.Preconditions.UID || current.ResourceVersion != *options.Preconditions.ResourceVersion {
						return apierrors.NewConflict(agents.Resource("sandboxes"), obj.GetName(), errors.New("binding changed"))
					}
					return c.Delete(ctx, obj, opts...)
				},
			}).Build()
			r := NewReconciler(c)
			_, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(parent)})
			wantErr := unknownName != "" || mode == "rejected source"
			if (err != nil) != wantErr {
				t.Fatalf("reconcile error=%v, wantError=%v", err, wantErr)
			}
			if unknownName != "" && !errors.Is(err, lostResponse) {
				t.Fatalf("lost binding error: %v", err)
			}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(spare), &agents.Sandbox{}); !apierrors.IsNotFound(err) {
				t.Fatalf("independent member deletion blocked: %v, attempts=%v", err, deletes)
			}
			if mode == "rejected source" || mode == "corrupt source" {
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(source), &agents.Sandbox{}); !apierrors.IsNotFound(err) {
					t.Fatalf("definitively rejected/corrupt binding incorrectly protected member: %v", err)
				}
			}
			if unknownName != "" {
				key := client.ObjectKey{Namespace: parent.Namespace, Name: unknownName}
				if err := c.Get(t.Context(), key, &agents.Sandbox{}); err != nil {
					t.Fatalf("unknown member not retained: %v", err)
				}
				if strings.HasPrefix(mode, "rollout") {
					return
				}
				inject = false
				for round := 0; round < 3; round++ {
					reconcile(t, r, parent)
				}
				if err := c.Get(t.Context(), key, &agents.Sandbox{}); !apierrors.IsNotFound(err) {
					t.Fatalf("unknown-member exclusion leaked into future reconciles: %v", err)
				}
			}
		})
	}
}

func TestUnknownBindingMembersStillCountForCapacity(t *testing.T) {
	for _, desired := range []int32{0, 5} {
		t.Run(fmt.Sprint(desired), func(t *testing.T) {
			parent := serving(desired)
			target, spare := member(parent, "target"), member(parent, "spare")
			target.Status.Phase = agents.SandboxRunning
			target.Status.PodInfo.PodIP = "10.0.0.2"
			target.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
			objects := []client.Object{parent, target, spare}
			lost := map[string]error{}
			for i := 0; i < 2; i++ {
				source := member(parent, fmt.Sprintf("source-%d", i))
				var err error
				source.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{fmt.Sprintf("%022d", i): {
					CreatedAt: time.Now().UTC().Format(time.RFC3339), Phase: sessionbinding.Moving, TargetSandboxUID: string(target.UID),
				}})
				if err != nil {
					t.Fatal(err)
				}
				objects = append(objects, source)
				lost[source.Name] = fmt.Errorf("response lost for %s", source.Name)
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if err, unknown := lost[obj.GetName()]; unknown {
						return err
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if _, unknown := lost[obj.GetName()]; unknown {
						t.Fatal("unknown binding member was deleted")
					}
					return c.Delete(ctx, obj, opts...)
				},
			}).Build()
			_, err := NewReconciler(c).Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(parent)})
			for _, cause := range lost {
				if !errors.Is(err, cause) {
					t.Fatalf("independent worker error lost: %v", err)
				}
			}
			var list agents.SandboxList
			if err := c.List(t.Context(), &list); err != nil {
				t.Fatal(err)
			}
			want := int(desired)
			if desired == 0 {
				want = 2 // Only the two unknown members are retained.
			}
			if len(list.Items) != want {
				t.Fatalf("member count=%d want=%d", len(list.Items), want)
			}
			var current agents.SessionService
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(parent), &current); err != nil {
				t.Fatal(err)
			}
			if current.Status.Replicas != 4 {
				t.Fatalf("unknown members were excluded from observed capacity: %+v", current.Status)
			}
		})
	}
}

func TestBindingWriteResultsPreserveIndependentErrors(t *testing.T) {
	for _, firstObservation := range []bool{false, true} {
		t.Run(fmt.Sprint(firstObservation), func(t *testing.T) {
			missing := apierrors.NewNotFound(agents.Resource("sandboxes"), "source-a")
			forbidden := apierrors.NewForbidden(agents.Resource("sandboxes"), "source-b", errors.New("denied"))
			failures := []error{forbidden, missing}
			if firstObservation {
				slices.Reverse(failures)
			}
			unknown, err := bindingWriteResults(errors.Join(failures...))
			if len(unknown) != 0 || !errors.Is(err, forbidden) || errors.Is(err, missing) {
				t.Fatalf("unknown=%v remaining error=%v", unknown, err)
			}
		})
	}
}

func TestExpiryStageInvalidatesMemberSnapshot(t *testing.T) {
	parent := serving(0)
	parent.Spec.SessionPolicy.ExpireAfterCreation = "1h"
	box := member(parent, "source")
	var err error
	box.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{"000000000000000session": {CreatedAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339), Phase: sessionbinding.Moving}})
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent, box).Build()
	result := reconcile(t, NewReconciler(c), parent)
	if result.RequeueAfter != observationDelay {
		t.Fatalf("missing observation requeue: %v", result)
	}
	var current agents.Sandbox
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(box), &current); err != nil {
		t.Fatalf("scaled from pre-cleanup snapshot: %v", err)
	}
	records, err := sessionbinding.Decode(current.Annotations)
	if err != nil || len(records) != 0 {
		t.Fatalf("expired Moving binding retained: %v %v", records, err)
	}
}

func TestExpiredMigrationDoesNotAdvance(t *testing.T) {
	parent := serving(2)
	parent.Spec.SessionPolicy.ExpireAfterCreation = "1h"
	source, target := member(parent, "source"), member(parent, "target")
	target.Status.Phase = agents.SandboxRunning
	target.Status.PodInfo.PodIP = "10.0.0.2"
	target.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
	var err error
	source.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{"000000000000000session": {CreatedAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339), Phase: sessionbinding.Moving, TargetSandboxUID: string(target.UID)}})
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(parent, source, target).Build()
	var snapshot agents.SandboxList
	if err := c.List(t.Context(), &snapshot); err != nil {
		t.Fatal(err)
	}
	changed, _, err := NewReconciler(c).reconcileRebindings(t.Context(), parent, snapshot.Items)
	if err != nil || changed {
		t.Fatalf("expired migration advanced: changed=%v err=%v", changed, err)
	}
	var current agents.Sandbox
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(target), &current); err != nil {
		t.Fatal(err)
	}
	records, err := sessionbinding.Decode(current.Annotations)
	if err != nil || len(records) != 0 {
		t.Fatalf("expired target written: %v %v", records, err)
	}
}

func TestExpiryIndependentMembers(t *testing.T) {
	for _, failure := range []string{"conflict", "patch error", "malformed"} {
		t.Run(failure, func(t *testing.T) {
			parent := serving(2)
			parent.Spec.SessionPolicy.ExpireAfterCreation = "1h"
			failed, healthy := member(parent, "a-failed"), member(parent, "b-healthy")
			for _, box := range []*agents.Sandbox{failed, healthy} {
				var err error
				box.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{"000000000000000session": {CreatedAt: "2026-11-19T00:00:00Z"}})
				if err != nil {
					t.Fatal(err)
				}
			}
			if failure == "malformed" {
				failed.Annotations[sessionbinding.BindingsAnnotation] = "invalid"
			}
			original := failed.Annotations[sessionbinding.BindingsAnnotation]
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(parent, failed, healthy).WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if obj.GetName() == failed.Name {
					if failure == "conflict" {
						return apierrors.NewConflict(agents.GroupVersion.WithResource("sandboxes").GroupResource(), failed.Name, errors.New("concurrent write"))
					}
					return errors.New("write unavailable")
				}
				return c.Patch(ctx, obj, patch, opts...)
			}}).Build()
			var snapshot agents.SandboxList
			if err := c.List(t.Context(), &snapshot); err != nil {
				t.Fatal(err)
			}
			changed, _, err := NewReconciler(c).expireBindings(t.Context(), parent, snapshot.Items, time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC))
			if !changed || (err != nil) != (failure == "patch error") {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			for _, box := range []*agents.Sandbox{failed, healthy} {
				var current agents.Sandbox
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(box), &current); err != nil {
					t.Fatal(err)
				}
				if box == failed {
					if current.Annotations[sessionbinding.BindingsAnnotation] != original {
						t.Fatal("failed member overwritten")
					}
				} else {
					records, err := sessionbinding.Decode(current.Annotations)
					if err != nil || len(records) != 0 {
						t.Fatalf("independent member not cleaned: %v %v", records, err)
					}
				}
			}
		})
	}
}

func TestExpirySchedulesDeadline(t *testing.T) {
	parent := serving(1)
	parent.Spec.SessionPolicy.ExpireAfterCreation = "1h"
	box := member(parent, "source")
	box.Status.Phase = agents.SandboxRunning
	var err error
	box.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{"000000000000000session": {CreatedAt: time.Now().UTC().Format(time.RFC3339)}})
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithIndex(&appsv1.ControllerRevision{}, revisionUIDIndex, revisionUID).WithStatusSubresource(parent).WithObjects(parent, box).Build()
	result := reconcile(t, NewReconciler(c), parent)
	if result.RequeueAfter <= 59*time.Minute || result.RequeueAfter > time.Hour {
		t.Fatalf("expiry not scheduled: %v", result)
	}
}

func TestRebindingSingleReconcile(t *testing.T) {
	for _, mode := range []string{"same source", "same source shared target", "shared target", "independent sources", "partial error", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			parent := serving(4)
			limit := int32(1)
			if mode == "same source shared target" {
				limit = 2
			}
			parent.Spec.SessionPolicy.MaxSessionsPerSandbox = &limit
			var objects []client.Object
			var sources []*agents.Sandbox
			for i := 0; i < 2; i++ {
				source := member(parent, fmt.Sprintf("source-%d", i))
				if strings.HasPrefix(mode, "same source") && i == 1 {
					source = sources[0]
				}
				records, err := sessionbinding.Decode(source.Annotations)
				if err != nil {
					t.Fatal(err)
				}
				if records == nil {
					records = sessionbinding.Bindings{}
				}
				record := sessionbinding.Record{CreatedAt: "2026-11-18T10:00:00Z", Phase: sessionbinding.Moving}
				if mode == "independent sources" {
					record.TargetSandboxUID = fmt.Sprintf("target-%d-uid", i)
				}
				records[fmt.Sprintf("%022d", i)] = record
				source.Annotations, err = sessionbinding.Encode(source.Annotations, records)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(mode, "same source") || i == 0 {
					sources = append(sources, source)
				}
				target := member(parent, fmt.Sprintf("target-%d", i))
				target.Status.Phase = agents.SandboxRunning
				target.Status.PodInfo.PodIP = "10.0.0.2"
				target.Status.Conditions = []metav1.Condition{{Type: string(agents.SandboxConditionReady), Status: metav1.ConditionTrue}}
				if (mode != "shared target" && mode != "same source shared target") || i == 0 {
					objects = append(objects, target)
				}
			}
			for _, source := range sources {
				objects = append(objects, source)
			}
			entered := make(chan struct{}, 2)
			release := make(chan struct{})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if strings.HasPrefix(obj.GetName(), "target-") && mode == "partial error" {
					return errors.New("target write rejected")
				}
				if strings.HasPrefix(obj.GetName(), "source-") && (mode == "shared target" || mode == "independent sources") {
					records, err := sessionbinding.Decode(obj.(*agents.Sandbox).Annotations)
					if err != nil {
						return err
					}
					for _, record := range records {
						if record.TargetSandboxUID == "" || mode == "independent sources" {
							entered <- struct{}{}
							select {
							case <-release:
							case <-ctx.Done():
								return ctx.Err()
							}
							break
						}
					}
				}
				if err := c.Patch(ctx, obj, patch, opts...); err != nil {
					return err
				}
				// Model admission changing more than the RV in the successful response.
				if strings.HasPrefix(mode, "same source") && strings.HasPrefix(obj.GetName(), "source-") {
					obj.SetAnnotations(maps.Clone(obj.GetAnnotations()))
					obj.GetAnnotations()["example.com/admitted"] = "retained"
					return c.Update(ctx, obj)
				}
				return nil
			}}).Build()
			if mode == "shared target" || mode == "independent sources" {
				go func() {
					for range 2 {
						select {
						case <-entered:
						case <-ctx.Done():
							return
						}
					}
					close(release)
				}()
			}
			var list agents.SandboxList
			if err := c.List(t.Context(), &list); err != nil {
				t.Fatal(err)
			}
			changed, pending, err := NewReconciler(c).reconcileRebindings(ctx, parent, list.Items)
			if !pending || changed != (mode != "canceled") || (err != nil) != (mode == "partial error" || mode == "canceled") {
				t.Fatalf("changed=%v pending=%v err=%v", changed, pending, err)
			}
			if err := c.List(t.Context(), &list); err != nil {
				t.Fatal(err)
			}
			active, moving := 0, 0
			for _, box := range list.Items {
				if strings.HasPrefix(mode, "same source") && strings.HasPrefix(box.Name, "source-") && box.Annotations["example.com/admitted"] != "retained" {
					t.Fatal("lost complete patch response")
				}
				records, err := sessionbinding.Decode(box.Annotations)
				if err != nil {
					t.Fatal(err)
				}
				for _, record := range records {
					if record.Phase == sessionbinding.Moving {
						moving++
					} else {
						active++
					}
				}
			}
			wantActive, wantMoving := 2, 0
			if mode == "shared target" {
				wantActive, wantMoving = 1, 1
			}
			if mode == "partial error" || mode == "canceled" {
				wantActive, wantMoving = 0, 2
			}
			if active != wantActive || moving != wantMoving {
				t.Fatalf("active=%d moving=%d, want %d/%d", active, moving, wantActive, wantMoving)
			}
		})
	}
}

func TestRebindingConcurrencyBoundAndCancellation(t *testing.T) {
	parent := serving(24)
	var objects []client.Object
	for i := range 12 {
		source, target := member(parent, fmt.Sprintf("source-%d", i)), member(parent, fmt.Sprintf("target-%d", i))
		id := fmt.Sprintf("%022d", i)
		var err error
		source.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{id: {CreatedAt: "2026-11-18T10:00:00Z", Phase: sessionbinding.Moving, TargetSandboxUID: string(target.UID)}})
		if err != nil {
			t.Fatal(err)
		}
		target.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{id: {CreatedAt: "2026-11-18T10:00:00Z"}})
		if err != nil {
			t.Fatal(err)
		}
		objects = append(objects, source, target)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	active, peak, calls := 0, 0, 0
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		mu.Lock()
		active++
		calls++
		if active > peak {
			peak = active
		}
		if active == 10 {
			cancel()
		}
		mu.Unlock()
		<-ctx.Done()
		mu.Lock()
		active--
		mu.Unlock()
		return ctx.Err()
	}}).Build()
	var list agents.SandboxList
	if err := c.List(t.Context(), &list); err != nil {
		t.Fatal(err)
	}
	changed, pending, err := NewReconciler(c).reconcileRebindings(ctx, parent, list.Items)
	if changed || !pending || !errors.Is(err, context.Canceled) || peak != 10 || calls != 10 || active != 0 {
		t.Fatalf("changed=%v pending=%v err=%v peak=%d calls=%d active=%d", changed, pending, err, peak, calls, active)
	}
}

func TestRebindingRepairsLateActiveWrite(t *testing.T) {
	for _, mode := range []string{"late write", "reverse snapshot", "stale loser", "stale winner", "lost cleanup response", "deleting lower UID", "terminal lower UID", "unready lower UID", "winner deleting in response"} {
		t.Run(mode, func(t *testing.T) {
			parent := serving(3)
			const id = "000000000000000session"
			winner, loser := member(parent, "a-winner"), member(parent, "z-loser")
			for i, box := range []*agents.Sandbox{winner, loser} {
				var err error
				box.Annotations, err = sessionbinding.Encode(map[string]string{"example.com/keep": "yes"}, sessionbinding.Bindings{id: {CreatedAt: "2026-11-18T10:00:00Z"}, fmt.Sprintf("%022d", i+1): {CreatedAt: "2026-11-18T10:00:00Z"}})
				if err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasSuffix(mode, "lower UID") {
				loser.Status.Phase = agents.SandboxRunning
				loser.Status.PodInfo.PodIP = "10.0.0.2"
				loser.Status.Conditions = []metav1.Condition{{Type: string(agents.SandboxConditionReady), Status: metav1.ConditionTrue}}
				switch mode {
				case "deleting lower UID":
					now := metav1.Now()
					winner.DeletionTimestamp = &now
					winner.Finalizers = []string{"test/retain"}
				case "terminal lower UID":
					winner.Status.Phase = agents.SandboxFailed
				}
				winner, loser = loser, winner
			}
			if mode == "winner deleting in response" {
				winner.Finalizers = []string{"test/retain"}
			}
			// The source has already been cleaned. A late old-target write now
			// leaves two Active copies, so repair cannot depend on Moving records.
			lost := false
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(winner, loser).WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if err := c.Patch(ctx, obj, patch, opts...); err != nil {
					return err
				}
				if mode == "winner deleting in response" && obj.GetName() == winner.Name {
					if err := c.Delete(ctx, obj); err != nil {
						return err
					}
					return c.Get(ctx, client.ObjectKeyFromObject(obj), obj)
				}
				if mode == "lost cleanup response" && obj.GetName() == loser.Name && !lost {
					lost = true
					return context.DeadlineExceeded
				}
				return nil
			}}).Build()
			var list agents.SandboxList
			if err := c.List(t.Context(), &list); err != nil {
				t.Fatal(err)
			}
			if mode == "reverse snapshot" {
				slices.Reverse(list.Items)
			}
			if mode == "stale loser" || mode == "stale winner" {
				box := loser.DeepCopy()
				if mode == "stale winner" {
					box = winner.DeepCopy()
				}
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(box), box); err != nil {
					t.Fatal(err)
				}
				box.Annotations["example.com/concurrent"] = "keep"
				if err := c.Update(t.Context(), box); err != nil {
					t.Fatal(err)
				}
			}
			changed, pending, err := NewReconciler(c).reconcileRebindings(t.Context(), parent, list.Items)
			if !pending || (err != nil) != (mode == "lost cleanup response" || mode == "stale loser" || mode == "stale winner") {
				t.Fatalf("changed=%v pending=%v err=%v", changed, pending, err)
			}
			if mode == "late write" || mode == "lost cleanup response" {
				if !changed {
					t.Fatal("repair lost progress")
				}
			}
			var current agents.Sandbox
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(winner), &current); err != nil {
				t.Fatal(err)
			}
			records, err := sessionbinding.Decode(current.Annotations)
			if err != nil || len(records) != 2 {
				t.Fatalf("winner changed: %v %v", records, err)
			}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(loser), &current); err != nil {
				t.Fatal(err)
			}
			records, err = sessionbinding.Decode(current.Annotations)
			if err != nil || current.Annotations["example.com/keep"] != "yes" {
				t.Fatal("loser unrelated state lost")
			}
			_, exists := records[id]
			if exists != (mode == "stale loser" || mode == "stale winner" || mode == "winner deleting in response") {
				t.Fatalf("loser=%v", records)
			}
		})
	}
}

func TestRebindingRetargetLateWriteAfterSourceCleanup(t *testing.T) {
	parent := serving(3)
	const id = "000000000000000session"
	source, oldTarget, newTarget := member(parent, "source"), member(parent, "a-old"), member(parent, "z-new")
	record := sessionbinding.Record{CreatedAt: "2026-11-18T10:00:00Z", Phase: sessionbinding.Moving, TargetSandboxUID: string(oldTarget.UID)}
	var err error
	source.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{id: record})
	if err != nil {
		t.Fatal(err)
	}
	newTarget.Status.Phase = agents.SandboxRunning
	newTarget.Status.PodInfo.PodIP = "10.0.0.2"
	newTarget.Status.Conditions = []metav1.Condition{{Type: string(agents.SandboxConditionReady), Status: metav1.ConditionTrue}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(source, oldTarget, newTarget).Build()
	var list agents.SandboxList
	if err := c.List(t.Context(), &list); err != nil {
		t.Fatal(err)
	}
	if changed, pending, err := NewReconciler(c).reconcileRebindings(t.Context(), parent, list.Items); err != nil || !changed || !pending {
		t.Fatalf("migration=%v/%v %v", changed, pending, err)
	}
	var current agents.Sandbox
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(source), &current); err != nil {
		t.Fatal(err)
	}
	records, err := sessionbinding.Decode(current.Annotations)
	if err != nil || len(records) != 0 {
		t.Fatalf("source not cleaned before late write: %v %v", records, err)
	}
	// A delayed write from an earlier decision appears only after source cleanup.
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(oldTarget), &current); err != nil {
		t.Fatal(err)
	}
	current.Annotations, err = sessionbinding.Encode(current.Annotations, sessionbinding.Bindings{id: {CreatedAt: record.CreatedAt}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Update(t.Context(), &current); err != nil {
		t.Fatal(err)
	}
	if err := c.List(t.Context(), &list); err != nil {
		t.Fatal(err)
	}
	if changed, pending, err := NewReconciler(c).reconcileRebindings(t.Context(), parent, list.Items); err != nil || !changed || !pending {
		t.Fatalf("repair=%v/%v %v", changed, pending, err)
	}
	for _, box := range []*agents.Sandbox{source, oldTarget, newTarget} {
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(box), &current); err != nil {
			t.Fatal(err)
		}
		records, err := sessionbinding.Decode(current.Annotations)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if box == newTarget {
			want = 1
		}
		if len(records) != want {
			t.Fatalf("%s bindings=%v", box.Name, records)
		}
	}
}

func TestRebindingIneffectiveDuplicatesKeepOneBinding(t *testing.T) {
	for _, affinity := range []string{"BestEffort", "Strict"} {
		t.Run(affinity, func(t *testing.T) {
			parent := serving(2)
			parent.Spec.SessionPolicy.Affinity = affinity
			const id = "000000000000000session"
			boxes := []*agents.Sandbox{member(parent, "a-old"), member(parent, "b-old")}
			var objects []client.Object
			for _, box := range boxes {
				box.Status.Phase = agents.SandboxFailed
				var err error
				box.Annotations, err = sessionbinding.Encode(map[string]string{"example.com/keep": "yes"}, sessionbinding.Bindings{id: {CreatedAt: "2026-11-18T10:00:00Z"}})
				if err != nil {
					t.Fatal(err)
				}
				objects = append(objects, box)
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
			for round := 0; round < 2; round++ {
				var list agents.SandboxList
				if err := c.List(t.Context(), &list); err != nil {
					t.Fatal(err)
				}
				changed, pending, err := NewReconciler(c).reconcileRebindings(t.Context(), parent, list.Items)
				if err != nil || changed != (round == 0) || pending != (round == 0) {
					t.Fatalf("repair=%v/%v %v", changed, pending, err)
				}
			}
			for i, box := range boxes {
				var current agents.Sandbox
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(box), &current); err != nil {
					t.Fatal(err)
				}
				records, err := sessionbinding.Decode(current.Annotations)
				if err != nil || len(records) != 1-i || current.Annotations["example.com/keep"] != "yes" {
					t.Fatalf("%s bindings=%v err=%v", box.Name, records, err)
				}
				if i == 0 && records[id].Phase != "" {
					t.Fatal("duplicate repair must not initiate migration")
				}
			}
		})
	}
}

func TestRebindingDuplicateSessionsShareMembers(t *testing.T) {
	parent := serving(2)
	winner, loser := member(parent, "a-winner"), member(parent, "z-loser")
	records := sessionbinding.Bindings{
		"0000000000000000000001": {CreatedAt: "2026-11-18T10:00:00Z"},
		"0000000000000000000002": {CreatedAt: "2026-11-18T10:00:00Z"},
	}
	var err error
	winner.Annotations, err = sessionbinding.Encode(nil, records)
	if err != nil {
		t.Fatal(err)
	}
	records["0000000000000000000003"] = sessionbinding.Record{CreatedAt: "2026-11-18T10:00:00Z"}
	loser.Annotations, err = sessionbinding.Encode(map[string]string{"example.com/keep": "yes"}, records)
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(winner, loser).Build()
	var list agents.SandboxList
	if err := c.List(t.Context(), &list); err != nil {
		t.Fatal(err)
	}
	if changed, pending, err := NewReconciler(c).reconcileRebindings(t.Context(), parent, list.Items); err != nil || !changed || !pending {
		t.Fatalf("repair=%v/%v %v", changed, pending, err)
	}
	var current agents.Sandbox
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(loser), &current); err != nil {
		t.Fatal(err)
	}
	records, err = sessionbinding.Decode(current.Annotations)
	if err != nil || len(records) != 1 || records["0000000000000000000003"].CreatedAt == "" || current.Annotations["example.com/keep"] != "yes" {
		t.Fatalf("loser=%v err=%v", records, err)
	}
}

func TestRebindingMultipleMovingSourcesConverge(t *testing.T) {
	for _, mode := range []string{"different hints", "missing hints", "no alternative", "late active"} {
		t.Run(mode, func(t *testing.T) {
			parent := serving(4)
			const id = "000000000000000session"
			a, b, target := member(parent, "a-source"), member(parent, "b-source"), member(parent, "target")
			for i, source := range []*agents.Sandbox{a, b} {
				// Both journals are otherwise eligible members: neither may be
				// selected as a destination or overwritten by the other source.
				source.Status.Phase = agents.SandboxRunning
				source.Status.PodInfo.PodIP = "10.0.0.3"
				source.Status.Conditions = []metav1.Condition{{Type: string(agents.SandboxConditionReady), Status: metav1.ConditionTrue}}
				record := sessionbinding.Record{CreatedAt: "2026-11-18T10:00:00Z", Phase: sessionbinding.Moving}
				if mode == "different hints" {
					record.TargetSandboxUID = []string{string(b.UID), "missing-target"}[i]
				}
				var err error
				source.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{id: record})
				if err != nil {
					t.Fatal(err)
				}
			}
			target.Status.Phase = agents.SandboxRunning
			target.Status.PodInfo.PodIP = "10.0.0.2"
			target.Status.Conditions = []metav1.Condition{{Type: string(agents.SandboxConditionReady), Status: metav1.ConditionTrue}}
			if mode == "no alternative" {
				target.Status.Conditions = nil
			}
			if mode == "late active" {
				var err error
				target.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{id: {CreatedAt: "2026-11-18T10:00:00Z"}})
				if err != nil {
					t.Fatal(err)
				}
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(a, b, target).Build()
			for round := range 5 {
				var snapshot agents.SandboxList
				if err := c.List(t.Context(), &snapshot); err != nil {
					t.Fatal(err)
				}
				if _, _, err := NewReconciler(c).reconcileRebindings(t.Context(), parent, snapshot.Items); err != nil {
					t.Fatal(err)
				}
				if round == 0 && mode != "late active" {
					var current agents.Sandbox
					if err := c.Get(t.Context(), client.ObjectKeyFromObject(b), &current); err != nil {
						t.Fatal(err)
					}
					records, err := sessionbinding.Decode(current.Annotations)
					if err != nil || records[id].Phase != sessionbinding.Moving {
						t.Fatalf("noncanonical journal overwritten: %v %v", records, err)
					}
				}
			}
			var snapshot agents.SandboxList
			if err := c.List(t.Context(), &snapshot); err != nil {
				t.Fatal(err)
			}
			active, moving := 0, 0
			for _, box := range snapshot.Items {
				records, err := sessionbinding.Decode(box.Annotations)
				if err != nil {
					t.Fatal(err)
				}
				if record, ok := records[id]; ok {
					if record.Phase == "" {
						active++
					} else {
						moving++
					}
				}
			}
			wantActive, wantMoving := 1, 0
			if mode == "no alternative" {
				wantActive, wantMoving = 0, 2
			}
			if active != wantActive || moving != wantMoving {
				t.Fatalf("active=%d moving=%d, want %d/%d", active, moving, wantActive, wantMoving)
			}
		})
	}
}

func TestRebindingCleanupResult(t *testing.T) {
	for _, mode := range []string{"source gone", "cleanup conflict", "cleanup unknown", "target not found"} {
		t.Run(mode, func(t *testing.T) {
			parent := serving(2)
			source, target := member(parent, "source"), member(parent, "target")
			const first = "0000000000000000000001"
			const second = "0000000000000000000002"
			record := sessionbinding.Record{CreatedAt: "2026-11-18T10:00:00Z", Phase: sessionbinding.Moving}
			var err error
			source.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{first: record, second: record})
			if err != nil {
				t.Fatal(err)
			}
			target.Status.Phase = agents.SandboxRunning
			target.Status.PodInfo.PodIP = "10.0.0.2"
			target.Status.Conditions = []metav1.Condition{{Type: string(agents.SandboxConditionReady), Status: metav1.ConditionTrue}}
			targetWritten, targetCalls := false, 0
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(source, target).WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if obj.GetName() == target.Name {
					targetCalls++
					if mode == "target not found" {
						return apierrors.NewNotFound(agents.GroupVersion.WithResource("sandboxes").GroupResource(), target.Name)
					}
					if err := c.Patch(ctx, obj, patch, opts...); err != nil {
						return err
					}
					targetWritten = true
					if mode == "source gone" {
						return c.Delete(ctx, source)
					}
					return nil
				}
				if targetWritten {
					if mode == "cleanup conflict" {
						return apierrors.NewConflict(agents.GroupVersion.WithResource("sandboxes").GroupResource(), source.Name, errors.New("concurrent write"))
					}
					if mode == "cleanup unknown" {
						return context.DeadlineExceeded
					}
				}
				return c.Patch(ctx, obj, patch, opts...)
			}}).Build()
			var snapshot agents.SandboxList
			if err := c.List(t.Context(), &snapshot); err != nil {
				t.Fatal(err)
			}
			bindings, sources := collectBindingMigrations(t.Context(), parent, snapshot.Items)
			if len(sources) != 1 {
				t.Fatalf("sources=%v", sources)
			}
			changed, err := NewReconciler(c).reconcileSourceBindings(t.Context(), parent, sources[0], bindings)
			if !changed || targetCalls != 1 {
				t.Fatalf("changed=%v target calls=%d err=%v", changed, targetCalls, err)
			}
			var current agents.Sandbox
			if getErr := c.Get(t.Context(), client.ObjectKeyFromObject(target), &current); getErr != nil {
				t.Fatal(getErr)
			}
			records, decodeErr := sessionbinding.Decode(current.Annotations)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if mode == "target not found" {
				if len(records) != 0 {
					t.Fatalf("failed target unexpectedly bound: %v", records)
				}
			} else if len(records) != 1 || records[first].CreatedAt != record.CreatedAt || records[first].Phase != "" {
				t.Fatalf("confirmed target binding lost or extra session processed: %v", records)
			}
			switch mode {
			case "source gone":
				if err != nil {
					t.Fatal(err)
				}
			case "cleanup conflict":
				if !apierrors.IsConflict(err) {
					t.Fatalf("lost conflict: %v", err)
				}
			case "cleanup unknown":
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("lost unknown outcome: %v", err)
				}
			case "target not found":
				if !apierrors.IsNotFound(err) {
					t.Fatalf("target failure hidden: %v", err)
				}
			}
		})
	}
}
