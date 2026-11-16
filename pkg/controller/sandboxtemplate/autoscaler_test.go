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

package sandboxtemplate

import (
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/openkruise/agents/api/v1alpha1"
)

func TestTemplateAutoscalerLifecycle(t *testing.T) {
	c, v := fixture()
	v.Spec.BuildOptions.WarmPool.Size = ptr.To(int32(2))
	v.Spec.BuildOptions.WarmPool.Autoscaler = &api.SandboxTemplatePoolAutoscalerOptions{
		MinReplicas: 1, MaxReplicas: 10,
		CapacityPolicy: &api.CapacityPolicy{TargetAvailable: intstr.FromInt32(2), Tolerance: ptr.To(intstr.FromString("10%"))},
		CronPolicies:   []api.CronScalingPolicy{{Name: "peak", Schedule: "0 9 * * *", TargetReplicas: 5}},
	}
	backend, scheme := fixtureClient(t, c, v)
	r := &Reconciler{Client: backend, Reader: backend, Scheme: scheme}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(v)}
	run := func() {
		t.Helper()
		if _, err := r.ReconcileVersion(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	run()
	pa := &api.PoolAutoscaler{}
	if err := backend.Get(t.Context(), req.NamespacedName, pa); err != nil {
		t.Fatal(err)
	}
	if !ownedByVersion(pa, v) || pa.Spec.ScaleTargetRef.Kind != "SandboxSet" || pa.Spec.ScaleTargetRef.APIVersion != api.GroupVersion.String() || pa.Spec.ScaleTargetRef.Name != v.Name || pa.Spec.CapacityPolicy.Tolerance == nil || len(pa.Spec.CronPolicies) != 1 || pa.Spec.MinReplicas != 1 || pa.Spec.MaxReplicas != 10 {
		t.Fatalf("autoscaler=%+v", pa)
	}
	p := &api.SandboxSet{}
	if err := backend.Get(t.Context(), req.NamespacedName, p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Replicas = 7
	if err := backend.Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if err := backend.Delete(t.Context(), pa); err != nil {
		t.Fatal(err)
	}
	run()
	if err := backend.Get(t.Context(), req.NamespacedName, p); err != nil || p.Spec.Replicas != 7 {
		t.Fatalf("autoscaler loss reset pool: %+v %v", p, err)
	}
	if err := backend.Get(t.Context(), req.NamespacedName, pa); err != nil {
		t.Fatal(err)
	}
	pa.Spec.MaxReplicas = 99
	if err := backend.Update(t.Context(), pa); err != nil {
		t.Fatal(err)
	}
	run()
	if err := backend.Get(t.Context(), req.NamespacedName, pa); err != nil || pa.Spec.MaxReplicas != 10 {
		t.Fatal("policy drift not corrected", err)
	}
	if err := backend.Delete(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	run()
	if err := backend.Get(t.Context(), req.NamespacedName, p); err != nil || p.Spec.Replicas != 2 {
		t.Fatal("lost pool not initialized", err)
	}
}
func TestTemplateAutoscalerCollisionKeepsReady(t *testing.T) {
	c, v := fixture()
	v.Spec.BuildOptions.WarmPool.Autoscaler = &api.SandboxTemplatePoolAutoscalerOptions{MaxReplicas: 10, CapacityPolicy: &api.CapacityPolicy{TargetAvailable: intstr.FromInt32(2)}}
	external := &api.PoolAutoscaler{ObjectMeta: metav1.ObjectMeta{Namespace: v.Namespace, Name: v.Name, UID: "external"}, Spec: api.PoolAutoscalerSpec{MaxReplicas: 99}}
	backend, scheme := fixtureClient(t, c, v, external)
	r := &Reconciler{Client: backend, Reader: backend, Scheme: scheme}
	if _, err := r.ReconcileVersion(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(v)}); err == nil {
		t.Fatal("collision must fail")
	}
	current := &api.SandboxTemplate{}
	if err := backend.Get(t.Context(), client.ObjectKeyFromObject(v), current); err != nil || current.Status.Phase != api.SandboxTemplatePhaseReady {
		t.Fatal("companion failure changed definition readiness", err)
	}
	pa := &api.PoolAutoscaler{}
	if err := backend.Get(t.Context(), client.ObjectKeyFromObject(external), pa); err != nil || pa.Spec.MaxReplicas != 99 || len(pa.OwnerReferences) != 0 {
		t.Fatal("adopted external autoscaler", err)
	}
}
func TestTemplateNoAutoscalerInFixedOrDisabledMode(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		c, v := fixture()
		if disabled {
			v.Spec.BuildOptions.WarmPool = &api.SandboxTemplateWarmPoolOptions{Enabled: ptr.To(false)}
		}
		backend, scheme := fixtureClient(t, c, v)
		r := &Reconciler{Client: backend, Reader: backend, Scheme: scheme}
		if _, err := r.ReconcileVersion(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(v)}); err != nil {
			t.Fatal(err)
		}
		if err := backend.Get(t.Context(), client.ObjectKeyFromObject(v), &api.PoolAutoscaler{}); !apierrors.IsNotFound(err) {
			t.Fatal("unexpected autoscaler", err)
		}
	}
}
