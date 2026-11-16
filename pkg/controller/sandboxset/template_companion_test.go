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

package sandboxset

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/lifecycle/pool"
)

func TestCompanionRevisionAndMaterializationShareConfiguration(t *testing.T) {
	name := "a7f2k9m4p1q8r3s6t0v5"
	version := &api.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", UID: "version-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplateClaim", Name: "python-agent", UID: "claim-uid", Controller: ptr.To(true)}}}, Spec: api.SandboxTemplateSpec{Backend: "k8s", BuildOptions: &api.SandboxTemplateBuildOptions{WarmPool: &api.SandboxTemplateWarmPoolOptions{Size: ptr.To(int32(2))}}, Template: samplePodTemplate("python:3.12", nil), Runtimes: []api.RuntimeConfig{{Name: "agent-runtime"}}, PersistentContents: []string{"filesystem"}, Lifecycle: &api.SandboxLifecycle{PrePause: &api.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"save"}}}}}}
	sbs := &api.SandboxSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: version.Namespace, OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplate", Name: name, UID: version.UID, Controller: ptr.To(true)}}}, Spec: api.SandboxSetSpec{Runtimes: []api.RuntimeConfig{{Name: "csi"}}, EmbeddedSandboxTemplate: api.EmbeddedSandboxTemplate{TemplateRef: &api.SandboxTemplateRef{Name: name, Kind: ptr.To("SandboxTemplate"), APIVersion: ptr.To(api.GroupVersion.String())}}}}
	reconciler := newRevisionTestReconciler(version)
	effective, err := reconciler.buildSandboxTemplateSpec(context.Background(), sbs)
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := pool.NewSandboxFromSandboxSet(sbs, version, version)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(effective.Runtimes, sandbox.Spec.Runtimes) || !reflect.DeepEqual(effective.Lifecycle, sandbox.Spec.Lifecycle) || !reflect.DeepEqual(effective.PersistentContents, sandbox.Spec.PersistentContents) {
		t.Fatal("revision and materialization use different policies")
	}
	hash, err := computeRevisionHash(effective)
	if err != nil {
		t.Fatal(err)
	}
	// Pure hash input variation, not a public mutable-version operation.
	changed := effective.DeepCopy()
	changed.Runtimes = []api.RuntimeConfig{{Name: "csi"}}
	changedHash, err := computeRevisionHash(changed)
	if err != nil {
		t.Fatal(err)
	}
	if hash == changedHash {
		t.Fatal("inherited policy omitted from hash")
	}
	legacy := sbs.DeepCopy()
	legacy.OwnerReferences = nil
	legacySpec, err := reconciler.buildSandboxTemplateSpec(context.Background(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacySpec.Runtimes, legacy.Spec.Runtimes) {
		t.Fatal("external reference changed policy precedence")
	}
}
