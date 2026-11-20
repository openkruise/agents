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

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	api "github.com/openkruise/agents/api/v1alpha1"
)

func TestTemplateDefinitionState(t *testing.T) {
	ready := &api.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Spec: api.SandboxTemplateSpec{Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "runtime", Image: "test/image"}},
		}}},
		Status: api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseReady, ObservedGeneration: 2},
	}
	for _, test := range []struct {
		name      string
		change    func(*api.SandboxTemplate)
		submitted bool
		static    bool
	}{
		{name: "Ready without submission metadata", submitted: true, static: true},
		{name: "multiple submitted containers", submitted: true, static: true, change: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers = append(v.Spec.Template.Spec.Containers, corev1.Container{Name: "sidecar", Image: "test/sidecar"})
		}},
		{name: "multiple Draft containers", change: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers[0].Image = ""
			v.Spec.Template.Spec.Containers = append(v.Spec.Template.Spec.Containers, corev1.Container{Name: "sidecar"})
			v.Status.Phase = api.SandboxTemplatePhasePending
		}},
		{name: "image outside first container denotes submission", submitted: true, change: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers[0].Image = ""
			v.Spec.Template.Spec.Containers = append(v.Spec.Template.Spec.Containers, corev1.Container{Name: "sidecar", Image: "test/sidecar"})
			v.Status.Phase = api.SandboxTemplatePhasePending
		}},
		{name: "Draft", change: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers[0].Image = ""
			v.Status.Phase = api.SandboxTemplatePhasePending
		}},
		{name: "submitted before status convergence", submitted: true, change: func(v *api.SandboxTemplate) {
			v.Status.Phase = api.SandboxTemplatePhasePending
			v.Status.ObservedGeneration = 1
		}},
		{name: "building", submitted: true, change: func(v *api.SandboxTemplate) { v.Status.Phase = api.SandboxTemplatePhaseBuilding }},
		{name: "stale Ready", submitted: true, change: func(v *api.SandboxTemplate) { v.Status.ObservedGeneration = 1 }},
		{name: "Ready captured definition", submitted: true, change: func(v *api.SandboxTemplate) {
			v.Status.Artifacts = []api.SandboxTemplateArtifact{{Type: "checkpoint", Name: "checkpoint", Value: "backend-checkpoint"}}
		}},
		{name: "Ready reader trusts observed status", static: true, change: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers[0].Image = ""
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Checkpoint: true, Steps: []api.TemplateBuildStep{{Command: &api.TemplateBuildCommand{Command: "echo build"}}}}
		}},
		{name: "missing Pod template is not submitted", change: func(v *api.SandboxTemplate) {
			v.Spec.Template = nil
			v.Status.Phase = api.SandboxTemplatePhasePending
		}},
		{name: "missing container is not submitted", change: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers = nil
			v.Status.Phase = api.SandboxTemplatePhasePending
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			version := ready.DeepCopy()
			if test.change != nil {
				test.change(version)
			}
			require.Equal(t, test.submitted, IsSubmitted(version))
			require.Equal(t, test.static, IsStaticReady(version))
		})
	}
}

func TestTemplateOwnerReferences(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*api.SandboxTemplate)
		valid  bool
	}{
		{name: "controller owner", valid: true},
		{name: "no owner", mutate: func(v *api.SandboxTemplate) { v.OwnerReferences = nil }},
		{name: "non-controller owner", mutate: func(v *api.SandboxTemplate) { v.OwnerReferences[0].Controller = ptr.To(false) }},
		{name: "unspecified controller", mutate: func(v *api.SandboxTemplate) { v.OwnerReferences[0].Controller = nil }},
		{name: "wrong API version", mutate: func(v *api.SandboxTemplate) { v.OwnerReferences[0].APIVersion = "other.io/v1" }},
		{name: "wrong kind", mutate: func(v *api.SandboxTemplate) { v.OwnerReferences[0].Kind = "Sandbox" }},
		{name: "missing name", mutate: func(v *api.SandboxTemplate) { v.OwnerReferences[0].Name = "" }},
		{name: "missing UID", mutate: func(v *api.SandboxTemplate) { v.OwnerReferences[0].UID = "" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obj := &api.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplate", Name: "owner", UID: "owner-uid", Controller: ptr.To(true),
			}}}}
			if tt.mutate != nil {
				tt.mutate(obj)
			}
			owner := VersionOwnerReference(obj)
			if tt.valid {
				require.Same(t, &obj.OwnerReferences[0], owner)
			} else {
				require.Nil(t, owner)
			}
		})
	}
}

func TestBuildVersionLabel(t *testing.T) {
	require.False(t, IsBuildVersion(nil))
	for _, tt := range []struct {
		name   string
		labels map[string]string
		build  bool
	}{
		{name: "ordinary template"},
		{name: "unrelated label", labels: map[string]string{"app": "test"}},
		{name: "empty template name", labels: map[string]string{api.LabelTemplateName: ""}},
		{name: "Build Version", labels: map[string]string{api.LabelTemplateName: "python-agent"}, build: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.build, IsBuildVersion(&api.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Labels: tt.labels}}))
		})
	}
}
