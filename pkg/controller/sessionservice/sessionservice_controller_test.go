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
		{name: "bound", raw: ptr(`{"000000000000000000000s":{"lastRequestAt":"2026-11-03T10:00:00Z"}}`)},
		{name: "malformed", raw: ptr(`{`)},
		{name: "invalid activity", raw: ptr(`{"000000000000000000000s":{"lastRequestAt":"invalid"}}`)},
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
		fresh.Annotations = map[string]string{sessionbinding.BindingsAnnotation: `{"000000000000000000000s":{"lastRequestAt":"2026-11-03T10:00:00Z"}}`}
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
		for _, bindings := range []string{"{}", `{"000000000000000000000s":{"lastRequestAt":"2026-11-03T10:00:00Z"}}`, "invalid"} {
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
					members.Items[i].Annotations[sessionbinding.BindingsAnnotation] = bindings
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
	replacement.Annotations = map[string]string{sessionbinding.BindingsAnnotation: `{"000000000000000000000s":{"lastRequestAt":"2026-11-03T10:00:00Z"}}`}
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
	box.Annotations = map[string]string{sessionbinding.BindingsAnnotation: `{"000000000000000000000s":{"lastRequestAt":"2026-11-03T10:00:00Z"}}`}
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
		sbx.Annotations[sessionbinding.BindingsAnnotation] = `{"000000000000000000000s":{"lastRequestAt":"2026-11-14T00:00:00Z"}}`
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
				if sbx.Labels[sessionbinding.SessionServiceRevisionLabel] != parent.Status.UpdateRevision {
					continue
				}
				sbx.Status.Phase = agents.SandboxRunning
				sbx.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
				sbx.Annotations[sessionbinding.BindingsAnnotation] = tt.bindings
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
		{name: "bound before empty by name", annotation: `{"000000000000000000000s":{"lastRequestAt":"2026-11-03T10:00:00Z"}}`, emptyReady: true, boundReady: true, want: "z-empty"},
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
			err := r.scaleDown(t.Context(), parent, "new-revision", members, 1, 0, status)
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
