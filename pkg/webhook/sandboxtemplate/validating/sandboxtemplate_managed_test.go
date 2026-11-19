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

package validating

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/defaults"
)

func TestManagedTemplateAdmission(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, api.AddToScheme(scheme))
	claim := &api.SandboxTemplateClaim{ObjectMeta: metav1.ObjectMeta{Name: "python-agent", Namespace: "default", UID: types.UID("claim-uid")}}
	draft := &api.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "st-abcdefghijklmnopqrst", Namespace: claim.Namespace, UID: types.UID("version-uid"), Generation: 1,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplateClaim", Name: claim.Name, UID: claim.UID, Controller: ptr.To(true)}}},
		Spec: api.SandboxTemplateSpec{Backend: api.SandboxTemplateBackendKubernetes, Template: minimalPodTemplate()},
	}
	draft.Spec.Template.Spec.Containers[0].Image = ""
	draft.Spec.Template.Spec.Containers[0].Name = "runtime"
	resources := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")}
	draft.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{Requests: resources.DeepCopy(), Limits: resources.DeepCopy()}
	draft.Spec.Runtimes = []api.RuntimeConfig{{Name: "agent-runtime"}}
	submitted := draft.DeepCopy()
	submitted.Generation = 2
	submitted.Spec.Template.Spec.Containers[0].Image = "test/image"
	published := submitted.DeepCopy()
	published.Status.Artifacts = []api.SandboxTemplateArtifact{{Type: "checkpoint", Name: "build-checkpoint", Value: "backend-checkpoint"}}
	multiDraft := draft.DeepCopy()
	sidecar := draft.Spec.Template.Spec.Containers[0].DeepCopy()
	sidecar.Name = "sidecar"
	multiDraft.Spec.Template.Spec.Containers = append(multiDraft.Spec.Template.Spec.Containers, *sidecar)
	multiSubmitted := multiDraft.DeepCopy()
	for i := range multiSubmitted.Spec.Template.Spec.Containers {
		multiSubmitted.Spec.Template.Spec.Containers[i].Image = "test/image"
	}

	tests := []struct {
		name    string
		old     *api.SandboxTemplate
		object  *api.SandboxTemplate
		mutate  func(*api.SandboxTemplate)
		status  bool
		allowed bool
	}{
		{name: "reserve Draft without Claim allocation", object: draft, allowed: true},
		{name: "multiple Draft containers", object: multiDraft, allowed: true},
		{name: "create multiple submitted containers", object: multiSubmitted, allowed: true},
		{name: "finalize multiple Draft containers", old: multiDraft, object: multiSubmitted, allowed: true},
		{name: "second container resources remain reserved", old: multiDraft, object: multiSubmitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers[1].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("2")
		}},
		{name: "second Draft container cannot contain build input", object: multiDraft, mutate: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers[1].Env = []corev1.EnvVar{{Name: "BUILD", Value: "input"}}
		}},
		{name: "second Draft container resources must be positive", object: multiDraft, mutate: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers[1].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("0")
			v.Spec.Template.Spec.Containers[1].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("0")
		}},
		{name: "Pod validation rejects duplicate Draft container names", object: multiDraft, mutate: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers[1].Name = v.Spec.Template.Spec.Containers[0].Name
		}},
		{name: "Pod validation rejects partial images", object: multiDraft, mutate: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers[1].Image = "test/image"
		}},
		{name: "Draft without runtimes", object: draft, allowed: true, mutate: func(v *api.SandboxTemplate) { v.Spec.Runtimes = nil }},
		{name: "Draft reserves its own resources and runtimes", object: draft, allowed: true, mutate: func(v *api.SandboxTemplate) {
			resources := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("2Gi")}
			v.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{Requests: resources.DeepCopy(), Limits: resources.DeepCopy()}
			v.Spec.Runtimes = append(v.Spec.Runtimes, api.RuntimeConfig{Name: "extra-runtime"})
		}},
		{name: "Draft resources must remain positive", object: draft, mutate: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("0")
			v.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("0")
		}},
		{name: "Draft requests and limits must match", object: draft, mutate: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("2Gi")
		}},
		{name: "missing owning Claim", object: draft, mutate: func(v *api.SandboxTemplate) { v.OwnerReferences[0].Name = "missing-claim" }},
		{name: "replaced owning Claim", object: draft, mutate: func(v *api.SandboxTemplate) { v.OwnerReferences[0].UID = "old-claim-uid" }},
		{name: "invalid owning Claim reference", object: draft, mutate: func(v *api.SandboxTemplate) { v.OwnerReferences[0].APIVersion = "other.io/v1" }},
		{name: "owner UID cannot change", old: draft, object: submitted, mutate: func(v *api.SandboxTemplate) { v.OwnerReferences[0].UID = "another-claim-uid" }},
		{name: "default Immediate startup is allowed in Draft", object: draft, allowed: true, mutate: func(v *api.SandboxTemplate) { v.Spec.StartupMode = api.StartupModeImmediate }},
		{name: "Deferred startup is build input", object: draft, mutate: func(v *api.SandboxTemplate) { v.Spec.StartupMode = api.StartupModeDeferred }},
		{name: "create finalized definition", object: submitted, allowed: true},
		{name: "finalize Draft once", old: draft, object: submitted, allowed: true},
		{name: "image completes Draft without a marker", old: draft, object: draft, allowed: true, mutate: func(v *api.SandboxTemplate) { v.Spec.Template.Spec.Containers[0].Image = "test/image" }},
		{name: "Draft input cannot change without submitting", old: draft, object: draft, mutate: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "BUILD", Value: "input"}}
		}},
		{name: "nil template is not a valid Draft", object: draft, mutate: func(v *api.SandboxTemplate) { v.Spec.Template = nil }},
		{name: "missing container is not a valid Draft", object: draft, mutate: func(v *api.SandboxTemplate) { v.Spec.Template.Spec.Containers = nil }},
		{name: "cannot replace reserved envelope", old: draft, object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("2")
		}},
		{name: "cannot replace reserved container name", old: draft, object: submitted, mutate: func(v *api.SandboxTemplate) { v.Spec.Template.Spec.Containers[0].Name = "another" }},
		{name: "cannot replace reserved runtimes", old: draft, object: submitted, mutate: func(v *api.SandboxTemplate) { v.Spec.Runtimes[0].Name = "another-runtime" }},
		{name: "cannot remove reserved runtimes", old: draft, object: submitted, mutate: func(v *api.SandboxTemplate) { v.Spec.Runtimes = nil }},
		{name: "finalized spec is immutable", old: submitted, object: submitted, mutate: func(v *api.SandboxTemplate) { v.Spec.Template.Spec.Containers[0].Image = "another/image" }},
		{name: "finalized startup mode is immutable", old: submitted, object: submitted, mutate: func(v *api.SandboxTemplate) { v.Spec.StartupMode = api.StartupModeDeferred }},
		{name: "submitted definition cannot revert to Draft", old: submitted, object: submitted, mutate: func(v *api.SandboxTemplate) { v.Spec.Template.Spec.Containers[0].Image = "" }},
		{name: "owner cannot be removed", old: submitted, object: submitted, mutate: func(v *api.SandboxTemplate) { v.OwnerReferences = nil }},
		{name: "status writes remain allowed", old: submitted, object: submitted, status: true, allowed: true, mutate: func(v *api.SandboxTemplate) { v.Status.Phase = api.SandboxTemplatePhaseReady }},
		{name: "first artifact status publication is allowed", old: submitted, object: published, status: true, allowed: true},
		{name: "published artifact status is immutable", old: published, object: published, status: true, mutate: func(v *api.SandboxTemplate) { v.Status.Artifacts[0].Value = "another-checkpoint" }},
		{name: "metadata writes remain allowed", old: submitted, object: submitted, allowed: true, mutate: func(v *api.SandboxTemplate) { v.Labels = map[string]string{"app": "test"} }},
		{name: "steps need capture", object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Steps: []api.TemplateBuildStep{{Command: &api.TemplateBuildCommand{Command: "echo hi"}}}}
		}},
		{name: "capture requires a scope", object: submitted, mutate: func(v *api.SandboxTemplate) { v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Checkpoint: true} }},
		{name: "start command is deferred", object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{StartCommand: &api.TemplateBuildCommand{Command: "serve"}}
		}},
		{name: "fromTemplate is unsupported", object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{FromTemplate: "base"}
		}},
		{name: "fromTemplate remains unsupported with capture", old: draft, object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.PersistentContents = []string{api.PersistentContentFilesystem, api.PersistentContentMemory}
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{FromTemplate: "base", Checkpoint: true}
		}},
		{name: "copy hash is required", object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.PersistentContents = []string{api.PersistentContentFilesystem}
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Checkpoint: true, Steps: []api.TemplateBuildStep{{Copy: &api.TemplateBuildCopy{Src: "app.py", Dest: "/app/app.py"}}}}
		}},
		{name: "valid command build accepted", object: submitted, allowed: true, mutate: func(v *api.SandboxTemplate) {
			v.Spec.PersistentContents = []string{api.PersistentContentFilesystem, api.PersistentContentMemory}
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Checkpoint: true, Steps: []api.TemplateBuildStep{{Command: &api.TemplateBuildCommand{Command: "echo hi", Cwd: "/app"}}}}
		}},
		{name: "invalid Version name", object: submitted, mutate: func(v *api.SandboxTemplate) { v.Name = "INVALID" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			object := tt.object.DeepCopy()
			if tt.mutate != nil {
				tt.mutate(object)
			}
			data, err := json.Marshal(object)
			require.NoError(t, err)
			req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: data}}}
			if tt.old != nil {
				req.Operation = admissionv1.Update
				oldData, err := json.Marshal(tt.old)
				require.NoError(t, err)
				req.OldObject.Raw = oldData
			}
			if tt.status {
				req.SubResource = "status"
			}
			h := &ValidatingHandler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim.DeepCopy()).Build(), Decoder: admission.NewDecoder(scheme)}
			response := h.Handle(context.Background(), req)
			require.Equal(t, tt.allowed, response.Allowed, "%v", response.Result)
		})
	}
}

func TestClaimOwnsIndependentVersions(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, api.AddToScheme(scheme))
	claim := &api.SandboxTemplateClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "python-agent", UID: "claim-uid"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim).Build()
	handler := &ValidatingHandler{Client: c, Decoder: admission.NewDecoder(scheme)}
	names := []string{"abcdefghijklmnopqrst", "st-abcdefghijklmnopqrst", "st-bcdefghijklmnopqrstu0"}
	for i, name := range names {
		t.Run(name, func(t *testing.T) {
			draft := &api.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{Namespace: claim.Namespace, Name: name,
					OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplateClaim", Name: claim.Name, UID: claim.UID, Controller: ptr.To(true)}}},
				Spec: api.SandboxTemplateSpec{Backend: api.SandboxTemplateBackendKubernetes, Template: minimalPodTemplate(), Runtimes: []api.RuntimeConfig{{Name: "agent-runtime"}}},
			}
			container := &draft.Spec.Template.Spec.Containers[0]
			container.Name = "runtime"
			container.Image = ""
			resources := corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewQuantity(int64(i+1), resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(int64(i+1)*1024*1024*1024, resource.BinarySI),
			}
			container.Resources = corev1.ResourceRequirements{Requests: resources.DeepCopy(), Limits: resources.DeepCopy()}
			if i > 0 {
				draft.Spec.Runtimes = append(draft.Spec.Runtimes, api.RuntimeConfig{Name: "extra-runtime"})
			}
			defaults.SetDefaultPodSpec(&draft.Spec.Template.Spec)
			finalized := draft.DeepCopy()
			finalized.Spec.Template.Spec.Containers[0].Image = "test/image"
			for _, step := range []struct {
				operation admissionv1.Operation
				old       *api.SandboxTemplate
				object    *api.SandboxTemplate
			}{
				{operation: admissionv1.Create, object: draft},
				{operation: admissionv1.Update, old: draft, object: finalized},
				{operation: admissionv1.Update, old: finalized, object: finalized},
			} {
				data, err := json.Marshal(step.object)
				require.NoError(t, err)
				request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: step.operation, Object: runtime.RawExtension{Raw: data}}}
				if step.old != nil {
					request.OldObject.Raw, err = json.Marshal(step.old)
					require.NoError(t, err)
				}
				response := handler.Handle(t.Context(), request)
				require.True(t, response.Allowed, "%v", response.Result)
			}
			finalized.Status.Phase = api.SandboxTemplatePhaseReady
			require.NoError(t, c.Create(t.Context(), finalized))
			if i == 0 {
				require.NoError(t, c.Delete(t.Context(), finalized))
			}
			require.True(t, apierrors.IsNotFound(c.Get(t.Context(), client.ObjectKey{Namespace: claim.Namespace, Name: names[0]}, &api.SandboxTemplate{})))
		})
	}
	versions := &api.SandboxTemplateList{}
	require.NoError(t, c.List(t.Context(), versions, client.InNamespace(claim.Namespace)))
	require.Len(t, versions.Items, 2)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(claim), &api.SandboxTemplateClaim{}))
}

func TestDeletingClaimRejectsVersionCreationAndSubmission(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, api.AddToScheme(scheme))
	claim := &api.SandboxTemplateClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "python-agent", UID: "claim-uid",
		DeletionTimestamp: ptr.To(metav1.Now()), Finalizers: []string{api.SandboxTemplateClaimCleanupFinalizer}}}
	draft := &api.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: claim.Namespace, Name: "st-abcdefghijklmnopqrst",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplateClaim", Name: claim.Name, UID: claim.UID, Controller: ptr.To(true)}}},
		Spec: api.SandboxTemplateSpec{Backend: api.SandboxTemplateBackendKubernetes, Template: minimalPodTemplate()},
	}
	draft.Spec.Template.Spec.Containers[0].Image = ""
	resources := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")}
	draft.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{Requests: resources.DeepCopy(), Limits: resources.DeepCopy()}
	for _, tt := range []struct {
		name   string
		old    *api.SandboxTemplate
		mutate func(*api.SandboxTemplate)
	}{
		{name: "cannot recover Draft"},
		{name: "cannot create submitted Version", mutate: func(v *api.SandboxTemplate) { v.Spec.Template.Spec.Containers[0].Image = "test/image" }},
		{name: "cannot submit existing Draft", old: draft, mutate: func(v *api.SandboxTemplate) { v.Spec.Template.Spec.Containers[0].Image = "test/image" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			object := draft.DeepCopy()
			if tt.mutate != nil {
				tt.mutate(object)
			}
			data, err := json.Marshal(object)
			require.NoError(t, err)
			request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: data}}}
			if tt.old != nil {
				request.Operation = admissionv1.Update
				request.OldObject.Raw, err = json.Marshal(tt.old)
				require.NoError(t, err)
			}
			h := &ValidatingHandler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim).Build(), Decoder: admission.NewDecoder(scheme)}
			response := h.Handle(t.Context(), request)
			require.False(t, response.Allowed)
			require.EqualValues(t, 409, response.Result.Code)
			require.Contains(t, response.Result.Message, "being deleted")
		})
	}
}

func TestClaimDeletionPropagation(t *testing.T) {
	for _, tt := range []struct {
		name    string
		policy  *metav1.DeletionPropagation
		allowed bool
	}{{"default", nil, true}, {"background", ptr.To(metav1.DeletePropagationBackground), true}, {"foreground", ptr.To(metav1.DeletePropagationForeground), false}, {"orphan", ptr.To(metav1.DeletePropagationOrphan), false}} {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(metav1.DeleteOptions{PropagationPolicy: tt.policy})
			require.NoError(t, err)
			h := &ClaimValidatingHandler{}
			response := h.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Delete, Options: runtime.RawExtension{Raw: data}}})
			require.Equal(t, tt.allowed, response.Allowed)
		})
	}
}

func TestClaimNameAdmission(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(scheme))
	for _, tt := range []struct {
		name    string
		objects []runtime.Object
		update  bool
		allowed bool
	}{
		{name: "available", allowed: true},
		{name: "metadata update", update: true, allowed: true},
		{name: "metadata update does not recheck occupancy", update: true, allowed: true, objects: []runtime.Object{&api.SandboxSet{ObjectMeta: metav1.ObjectMeta{Name: "python-agent", Namespace: "default"}}}},
		{name: "SandboxSet occupancy", objects: []runtime.Object{&api.SandboxSet{ObjectMeta: metav1.ObjectMeta{Name: "python-agent", Namespace: "default"}}}},
		{name: "Checkpoint occupancy", objects: []runtime.Object{&api.Checkpoint{ObjectMeta: metav1.ObjectMeta{Name: "python-agent", Namespace: "default"}}}},
		{name: "other namespace does not collide", allowed: true, objects: []runtime.Object{&api.SandboxSet{ObjectMeta: metav1.ObjectMeta{Name: "python-agent", Namespace: "other"}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			claim := &api.SandboxTemplateClaim{ObjectMeta: metav1.ObjectMeta{Name: "python-agent", Namespace: "default"}}
			data, err := json.Marshal(claim)
			require.NoError(t, err)
			request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: data}}}
			if tt.update {
				request.Operation = admissionv1.Update
				request.OldObject.Raw = data
				claim.Labels = map[string]string{"app": "test"}
				request.Object.Raw, err = json.Marshal(claim)
				require.NoError(t, err)
			}
			h := &ClaimValidatingHandler{Client: fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tt.objects...).Build(), Decoder: admission.NewDecoder(scheme)}
			response := h.Handle(context.Background(), request)
			require.Equal(t, tt.allowed, response.Allowed, "%v", response.Result)
		})
	}
}
