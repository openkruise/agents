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

package pool

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/templateconfig"
)

func companionFixture() (*api.SandboxSet, *api.SandboxTemplate) {
	name := "a7f2k9m4p1q8r3s6t0v5"
	version := &api.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", UID: "version-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplateClaim", Name: "python-agent", UID: "claim-uid", Controller: ptr.To(true)}}}, Spec: api.SandboxTemplateSpec{
		Backend: "k8s", BuildOptions: &api.SandboxTemplateBuildOptions{WarmPool: &api.SandboxTemplateWarmPoolOptions{Size: ptr.To(int32(2))}},
		Template:             &corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"source": "version"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "python:3.12"}}}},
		VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "data"}, Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}},
		PersistentContents:   []string{"filesystem"}, Runtimes: []api.RuntimeConfig{{Name: "agent-runtime"}}, PauseStrategy: &api.PauseStrategy{Type: api.PauseStrategyHibernate},
		Probes:          []api.Probe{{Name: "work", Probe: corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"check"}}}}}},
		AutoPausePolicy: &api.AutoPausePolicy{}, Lifecycle: &api.SandboxLifecycle{PrePause: &api.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"save"}}}},
	}}
	pool := &api.SandboxSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplate", Name: name, UID: version.UID, Controller: ptr.To(true)}}}, Spec: api.SandboxSetSpec{Replicas: 2, Runtimes: []api.RuntimeConfig{{Name: "csi"}}, EmbeddedSandboxTemplate: api.EmbeddedSandboxTemplate{TemplateRef: &api.SandboxTemplateRef{Name: name, Kind: ptr.To("SandboxTemplate"), APIVersion: ptr.To(api.GroupVersion.String())}}}}
	return pool, version
}
func TestResolveCompanionTemplate(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*api.SandboxSet, *api.SandboxTemplate)
		valid     bool
		companion bool
	}{
		{name: "complete inheritance", valid: true, companion: true},
		{name: "wrong owner UID", mutate: func(p *api.SandboxSet, _ *api.SandboxTemplate) { p.OwnerReferences[0].UID = "wrong" }},
		{name: "wrong pool name", mutate: func(p *api.SandboxSet, _ *api.SandboxTemplate) { p.Name = "wrong" }},
		{name: "wrong namespace", mutate: func(p *api.SandboxSet, _ *api.SandboxTemplate) { p.Namespace = "other" }},
		{name: "wrong reference", mutate: func(p *api.SandboxSet, _ *api.SandboxTemplate) { p.Spec.TemplateRef.Name = "other" }},
		{name: "missing explicit kind", mutate: func(p *api.SandboxSet, _ *api.SandboxTemplate) { p.Spec.TemplateRef.Kind = nil }},
		{name: "missing explicit version", mutate: func(p *api.SandboxSet, _ *api.SandboxTemplate) { p.Spec.TemplateRef.APIVersion = nil }},
		{name: "inline conflict", mutate: func(p *api.SandboxSet, _ *api.SandboxTemplate) { p.Spec.Template = &corev1.PodTemplateSpec{} }},
		{name: "external reference retains pool policy", mutate: func(p *api.SandboxSet, _ *api.SandboxTemplate) { p.OwnerReferences = nil }, valid: true},
		{name: "legacy template owner retains pool policy", mutate: func(_ *api.SandboxSet, v *api.SandboxTemplate) { v.OwnerReferences = nil }, valid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p, v := companionFixture()
			if test.mutate != nil {
				test.mutate(p, v)
			}
			oldPool, oldVersion := p.DeepCopy(), v.DeepCopy()
			result, err := templateconfig.ResolveTemplateSpec(p, v, v)
			if (err == nil) != test.valid {
				t.Fatalf("error=%v", err)
			}
			if !reflect.DeepEqual(p, oldPool) || !reflect.DeepEqual(v, oldVersion) {
				t.Fatal("resolver changed source")
			}
			if err != nil {
				return
			}
			if result.Backend != "" || result.BuildOptions != nil {
				t.Fatal("build-only fields entered workload/hash")
			}
			if test.companion {
				expected := v.Spec.DeepCopy()
				expected.Backend = ""
				expected.BuildOptions = nil
				if !reflect.DeepEqual(expected, result) {
					t.Fatalf("incomplete inherited configuration: %+v", result)
				}
			} else if !reflect.DeepEqual(result.Runtimes, p.Spec.Runtimes) {
				t.Fatal("noncompanion policy changed")
			}
		})
	}
}
func TestCompanionMaterializationOwnsData(t *testing.T) {
	pool, version := companionFixture()
	beforePool, beforeVersion := pool.DeepCopy(), version.DeepCopy()
	sandbox, err := NewSandboxFromSandboxSet(pool, version, version)
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.Spec.TemplateRef != nil || !reflect.DeepEqual(sandbox.Spec.Runtimes, version.Spec.Runtimes) || !reflect.DeepEqual(sandbox.Spec.Probes, version.Spec.Probes) || !reflect.DeepEqual(sandbox.Spec.Lifecycle, version.Spec.Lifecycle) || !reflect.DeepEqual(sandbox.Spec.VolumeClaimTemplates, version.Spec.VolumeClaimTemplates) {
		t.Fatalf("materialization incomplete: %+v", sandbox.Spec)
	}
	sandbox.Spec.Runtimes[0].Name = "changed"
	sandbox.Spec.Template.Labels["source"] = "changed"
	sandbox.Spec.Probes[0].Exec.Command[0] = "changed"
	sandbox.Spec.Lifecycle.PrePause.Exec.Command[0] = "changed"
	sandbox.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("2Gi")
	if !reflect.DeepEqual(beforePool, pool) || !reflect.DeepEqual(beforeVersion, version) {
		t.Fatal("materialization aliases informer data")
	}
	missingPool, _ := companionFixture()
	if _, err := templateconfig.ResolveTemplateSpec(missingPool, nil, nil); err == nil {
		t.Fatal("missing declared controller silently downgraded")
	}
}
