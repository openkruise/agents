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
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

func TestBindingSafeScaleDown(t *testing.T) {
	for _, tt := range []struct {
		name    string
		raw     *string
		blocked bool
	}{
		{name: "absent"}, {name: "empty", raw: ptr("{}")},
		{name: "bound", raw: ptr(`{"000000000000000000000s":{"lastRequestAt":"2026-11-03T10:00:00Z"}}`), blocked: true},
		{name: "malformed", raw: ptr(`{`), blocked: true},
		{name: "invalid activity", raw: ptr(`{"000000000000000000000s":{"lastRequestAt":"invalid"}}`), blocked: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			parent := serving(0)
			sbx := member(parent, "member")
			if tt.raw != nil {
				sbx.Annotations = map[string]string{sessionbinding.BindingsAnnotation: *tt.raw}
			}
			s := testScheme(t)
			deleted := 0
			c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(parent).WithObjects(parent, sbx).WithInterceptorFuncs(interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deleted++
				del := &client.DeleteOptions{}
				del.ApplyOptions(opts)
				if del.Preconditions == nil || del.Preconditions.UID == nil || *del.Preconditions.UID != sbx.UID || del.Preconditions.ResourceVersion == nil || *del.Preconditions.ResourceVersion != sbx.ResourceVersion {
					t.Fatalf("missing snapshot preconditions: %+v", del.Preconditions)
				}
				return c.Delete(ctx, obj, opts...)
			}}).Build()
			r := NewReconciler(c)
			reconcile(t, r, parent)
			var got agents.SessionService
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(parent), &got); err != nil {
				t.Fatal(err)
			}
			if meta.IsStatusConditionTrue(got.Status.Conditions, "ScaleDownBlocked") != tt.blocked {
				t.Fatalf("conditions: %+v", got.Status.Conditions)
			}
			if (deleted == 0) != tt.blocked {
				t.Fatalf("deleted=%d blocked=%v", deleted, tt.blocked)
			}
		})
	}
}

func TestConflictRejudgesFreshBindings(t *testing.T) {
	parent := serving(0)
	sbx := member(parent, "member")
	s := testScheme(t)
	attempts := 0
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(parent).WithObjects(parent, sbx).WithInterceptorFuncs(interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		attempts++
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
	if attempts != 1 {
		t.Fatalf("retried delete without fresh binding judgment: %d", attempts)
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
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(parent).WithObjects(parent).Build()
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
		if sbx.Spec.Template.Labels[sessionbinding.SessionServiceUIDLabel] != string(parent.UID) || sbx.Labels[sessionbinding.SessionServiceRevisionLabel] == "" || sbx.Spec.PauseStrategy.Type != agents.PauseStrategyStop || len(sbx.Spec.Runtimes) != 1 || len(sbx.Spec.PersistentContents) != 1 {
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
		{name: "backend", mutate: func(p *agents.SessionService) { p.Spec.SandboxTemplateSpec.Backend = "other" }},
		{name: "empty build options", mutate: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.BuildOptions = &agents.SandboxTemplateBuildOptions{}
		}},
		{name: "build options", mutate: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.BuildOptions = &agents.SandboxTemplateBuildOptions{FromTemplate: "base"}
		}},
		{name: "duplicate probes", mutate: func(p *agents.SessionService) {
			p.Spec.Probes = append(p.Spec.Probes, p.Spec.Probes[0])
		}},
		{name: "unsupported probe handler", mutate: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.Probes[0].ProbeHandler = corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/"}}
		}},
		{name: "empty auto pause policy", mutate: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.AutoPausePolicy = &agents.AutoPausePolicy{}
		}},
		{name: "missing policy probe", mutate: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.AutoPausePolicy.Pause.WhenProbedIdleState.Probe = "missing"
		}},
		{name: "invalid policy regex", mutate: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.AutoPausePolicy.Pause.WhenProbedIdleState.MessageRegex = "["
		}},
		{name: "missing PVC name", mutate: func(p *agents.SessionService) { p.Spec.SandboxTemplateSpec.VolumeClaimTemplates[0].Name = "" }},
		{name: "invalid PVC volume name", mutate: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.VolumeClaimTemplates[0].Name = "bad_name"
		}},
		{name: "duplicate PVC names", mutate: func(p *agents.SessionService) {
			p.Spec.VolumeClaimTemplates = append(p.Spec.VolumeClaimTemplates, *p.Spec.VolumeClaimTemplates[0].DeepCopy())
		}},
		{name: "unmounted PVC", mutate: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.Template.Spec.Containers[0].VolumeMounts = nil
		}},
		{name: "pre upgrade lifecycle", mutate: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.Lifecycle.PreUpgrade = &agents.LifecycleAction{}
		}},
		{name: "post upgrade lifecycle", mutate: func(p *agents.SessionService) {
			p.Spec.SandboxTemplateSpec.Lifecycle.PostUpgrade = &agents.LifecycleAction{}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			parent := serving(0)
			parent.Spec.SandboxTemplateSpec = *populatedTemplate()
			tt.mutate(parent)
			sbx := member(parent, "member")
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(parent).WithObjects(parent, sbx).Build()
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
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(parent).WithObjects(parent).WithInterceptorFuncs(interceptor.Funcs{
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
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(parent).WithObjects(objects...).Build()
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
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(parent).WithObjects(parent, first, second).WithInterceptorFuncs(interceptor.Funcs{
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
			c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(parent).WithObjects(parent, sbx).Build()
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
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(parent).WithObjects(parent).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
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
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(parent).WithObjects(parent).WithInterceptorFuncs(interceptor.Funcs{
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
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&agents.SessionService{}).WithObjects(objects...).Build()
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
				c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(parent).WithObjects(parent).WithInterceptorFuncs(interceptor.Funcs{
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
				want := 1
				if bindings != "{}" {
					want = 2
				}
				if len(members.Items) != want {
					t.Fatalf("members=%d want=%d", len(members.Items), want)
				}
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(parent), parent); err != nil {
					t.Fatal(err)
				}
				if meta.IsStatusConditionTrue(parent.Status.Conditions, "ScaleDownBlocked") != (bindings != "{}") {
					t.Fatalf("unexpected shrink protection: %+v", parent.Status)
				}
				if bindings != "{}" {
					for i := range members.Items {
						members.Items[i].Annotations[sessionbinding.BindingsAnnotation] = "{}"
						if err := c.Update(t.Context(), &members.Items[i]); err != nil {
							t.Fatal(err)
						}
					}
					reconcile(t, r, parent)
					if err := c.List(t.Context(), &members); err != nil {
						t.Fatal(err)
					}
					if len(members.Items) != 1 {
						t.Fatal("released surplus did not converge")
					}
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
			store := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(parent).WithObjects(parent).Build()
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
			store := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(parent).WithObjects(parent).Build()
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
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(parent).WithObjects(parent).WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
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
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(parent).WithObjects(parent, old, other).Build()
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
		t.Fatalf("expected a new deletion wait only for the unbound member, got %v", pending)
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
		t.Fatalf("same-name new UID kept old delete expectation stuck or was deleted: %+v", members.Items)
	}
}

func TestDeleteExpectationTimeoutRejudgesCachedSnapshot(t *testing.T) {
	immediateExpectationTimeout(t)
	parent := serving(0)
	sbx := member(parent, "old")
	s := testScheme(t)
	stale := true
	deletes := 0
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(parent).WithObjects(parent, sbx).WithInterceptorFuncs(interceptor.Funcs{
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
			c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(parent).WithObjects(parent, sbx).Build()
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
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(parent).WithObjects(parent).Build()
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
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(parent, box).WithInterceptorFuncs(interceptor.Funcs{
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
