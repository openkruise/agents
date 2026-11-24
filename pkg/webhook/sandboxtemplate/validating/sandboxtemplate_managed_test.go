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
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/defaults"
)

func TestManagedTemplateAdmission(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, api.AddToScheme(scheme))
	draft := &api.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "st-abcdefghijklmnopqrst", Namespace: "default", UID: types.UID("version-uid"), Generation: 1,
			Labels: map[string]string{api.LabelTemplateName: "python-agent"}},
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
	unmanaged := submitted.DeepCopy()
	unmanaged.Labels = nil
	snapshot := unmanaged.DeepCopy()
	snapshot.OwnerReferences = []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Checkpoint", Name: "capture", UID: "checkpoint-uid", Controller: ptr.To(true)}}
	deletingDraft := draft.DeepCopy()
	deletingDraft.DeletionTimestamp = ptr.To(metav1.Now())
	deletingDraft.Finalizers = []string{"test/retain"}
	deletingSubmitted := submitted.DeepCopy()
	deletingSubmitted.DeletionTimestamp = ptr.To(metav1.Now())
	deletingSubmitted.Finalizers = []string{"test/retain"}

	tests := []struct {
		name    string
		old     *api.SandboxTemplate
		oldRaw  []byte
		object  *api.SandboxTemplate
		mutate  func(*api.SandboxTemplate)
		status  bool
		allowed bool
	}{
		{name: "reserve Draft with template name label", object: draft, allowed: true},
		{name: "unmanaged updates retain existing behavior", old: unmanaged, object: unmanaged, allowed: true, mutate: func(v *api.SandboxTemplate) { v.Spec.Template.Spec.Containers[0].Image = "another/image" }},
		{name: "ordinary Snapshot template remains unmanaged", object: snapshot, allowed: true},
		{name: "ordinary Snapshot updates retain existing behavior", old: snapshot, object: snapshot, allowed: true, mutate: func(v *api.SandboxTemplate) { v.Spec.Template.Spec.Containers[0].Image = "another/image" }},
		{name: "existing templates cannot be adopted", old: unmanaged, object: submitted},
		{name: "malformed old object is rejected", old: draft, oldRaw: []byte("{"), object: submitted},
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
		{name: "empty template name rejected", object: draft, mutate: func(v *api.SandboxTemplate) { v.Labels[api.LabelTemplateName] = "" }},
		{name: "reserved template name rejected", object: draft, mutate: func(v *api.SandboxTemplate) { v.Labels[api.LabelTemplateName] = "tags" }},
		{name: "invalid template name rejected", object: draft, mutate: func(v *api.SandboxTemplate) { v.Labels[api.LabelTemplateName] = "INVALID" }},
		{name: "template name cannot change", old: draft, object: submitted, mutate: func(v *api.SandboxTemplate) { v.Labels[api.LabelTemplateName] = "another-template" }},
		{name: "template name cannot become empty", old: submitted, object: submitted, mutate: func(v *api.SandboxTemplate) { v.Labels[api.LabelTemplateName] = "" }},
		{name: "default Immediate startup is allowed in Draft", object: draft, allowed: true, mutate: func(v *api.SandboxTemplate) { v.Spec.StartupMode = api.StartupModeImmediate }},
		{name: "Deferred startup is build input", object: draft, mutate: func(v *api.SandboxTemplate) { v.Spec.StartupMode = api.StartupModeDeferred }},
		{name: "create finalized definition", object: submitted, allowed: true},
		{name: "finalize Draft once", old: draft, object: submitted, allowed: true},
		{name: "image completes Draft without a marker", old: draft, object: draft, allowed: true, mutate: func(v *api.SandboxTemplate) { v.Spec.Template.Spec.Containers[0].Image = "test/image" }},
		{name: "deleting Draft cannot be finalized", old: deletingDraft, object: deletingDraft, mutate: func(v *api.SandboxTemplate) { v.Spec.Template.Spec.Containers[0].Image = "test/image" }},
		{name: "deleting Draft permits finalizer removal", old: deletingDraft, object: deletingDraft, allowed: true, mutate: func(v *api.SandboxTemplate) { v.Finalizers = nil }},
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
		{name: "template name cannot be removed", old: submitted, object: submitted, mutate: func(v *api.SandboxTemplate) { delete(v.Labels, api.LabelTemplateName) }},
		{name: "status writes remain allowed", old: submitted, object: submitted, status: true, allowed: true, mutate: func(v *api.SandboxTemplate) { v.Status.Phase = api.SandboxTemplatePhaseReady }},
		{name: "first artifact status publication is allowed", old: submitted, object: published, status: true, allowed: true},
		{name: "published artifact status is immutable", old: published, object: published, status: true, mutate: func(v *api.SandboxTemplate) { v.Status.Artifacts[0].Value = "another-checkpoint" }},
		{name: "metadata writes remain allowed", old: submitted, object: submitted, allowed: true, mutate: func(v *api.SandboxTemplate) { v.Labels["app"] = "test" }},
		{name: "cleanup annotation and finalizer writes remain allowed on submitted", old: submitted, object: submitted, allowed: true, mutate: func(v *api.SandboxTemplate) {
			v.Annotations = map[string]string{api.E2BPrefix + "template-cleanup": "op-1"}
			v.Finalizers = append(v.Finalizers, "agents.kruise.io/template-cleanup")
		}},
		{name: "tag annotation writes remain allowed while deleting", old: deletingSubmitted, object: deletingSubmitted, allowed: true, mutate: func(v *api.SandboxTemplate) {
			v.Annotations = map[string]string{api.E2BPrefix + "template-tags": `{"v1.1.0":"2026-11-24T00:00:00Z"}`}
		}},
		{name: "steps need capture", object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Steps: []api.TemplateBuildStep{{Command: &api.TemplateBuildCommand{Command: "echo hi"}}}}
		}},
		{name: "capture requires a scope", object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Checkpoint: &api.CheckpointStrategy{Enable: true}}
		}},
		{name: "start command is deferred", object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{StartCommand: &api.TemplateBuildCommand{Command: "serve"}}
		}},
		{name: "fromTemplate is unsupported", object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{FromTemplate: "base"}
		}},
		{name: "fromTemplate remains unsupported with capture", old: draft, object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.PersistentContents = []string{api.PersistentContentFilesystem, api.PersistentContentMemory}
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{FromTemplate: "base", Checkpoint: &api.CheckpointStrategy{Enable: true}}
		}},
		{name: "ready command is deferred", object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{ReadyCommand: &api.TemplateBuildCommand{Command: "ready"}}
		}},
		{name: "capture rejects unsupported scope", object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.PersistentContents = []string{"unsupported"}
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Checkpoint: &api.CheckpointStrategy{Enable: true}}
		}},
		{name: "step requires an operation", object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.PersistentContents = []string{api.PersistentContentFilesystem}
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Checkpoint: &api.CheckpointStrategy{Enable: true}, Steps: []api.TemplateBuildStep{{}}}
		}},
		{name: "step cannot combine command and COPY", object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.PersistentContents = []string{api.PersistentContentFilesystem}
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Checkpoint: &api.CheckpointStrategy{Enable: true}, Steps: []api.TemplateBuildStep{{Command: &api.TemplateBuildCommand{Command: "echo build"}, Copy: &api.TemplateBuildCopy{Src: "app", Dest: "/app", FilesHash: "hash"}}}}
		}},
		{name: "command cannot be whitespace", object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.PersistentContents = []string{api.PersistentContentFilesystem}
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Checkpoint: &api.CheckpointStrategy{Enable: true}, Steps: []api.TemplateBuildStep{{Command: &api.TemplateBuildCommand{Command: " \t "}}}}
		}},
		{name: "command working directory must be absolute", object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.PersistentContents = []string{api.PersistentContentFilesystem}
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Checkpoint: &api.CheckpointStrategy{Enable: true}, Steps: []api.TemplateBuildStep{{Command: &api.TemplateBuildCommand{Command: "echo build", Cwd: "app"}}}}
		}},
		{name: "COPY source must stay in context", object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.PersistentContents = []string{api.PersistentContentFilesystem}
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Checkpoint: &api.CheckpointStrategy{Enable: true}, Steps: []api.TemplateBuildStep{{Copy: &api.TemplateBuildCopy{Src: "../app", Dest: "/app", FilesHash: "hash"}}}}
		}},
		{name: "COPY destination must be absolute", object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.PersistentContents = []string{api.PersistentContentFilesystem}
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Checkpoint: &api.CheckpointStrategy{Enable: true}, Steps: []api.TemplateBuildStep{{Copy: &api.TemplateBuildCopy{Src: "app", Dest: "app", FilesHash: "hash"}}}}
		}},
		{name: "valid COPY is accepted", object: submitted, allowed: true, mutate: func(v *api.SandboxTemplate) {
			v.Spec.PersistentContents = []string{api.PersistentContentFilesystem}
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Checkpoint: &api.CheckpointStrategy{Enable: true}, Steps: []api.TemplateBuildStep{{Copy: &api.TemplateBuildCopy{Src: "app", Dest: "/app", FilesHash: "hash"}}}}
		}},
		{name: "copy hash is required", object: submitted, mutate: func(v *api.SandboxTemplate) {
			v.Spec.PersistentContents = []string{api.PersistentContentFilesystem}
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Checkpoint: &api.CheckpointStrategy{Enable: true}, Steps: []api.TemplateBuildStep{{Copy: &api.TemplateBuildCopy{Src: "app.py", Dest: "/app/app.py"}}}}
		}},
		{name: "valid command build accepted", object: submitted, allowed: true, mutate: func(v *api.SandboxTemplate) {
			v.Spec.PersistentContents = []string{api.PersistentContentFilesystem, api.PersistentContentMemory}
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Checkpoint: &api.CheckpointStrategy{Enable: true}, Steps: []api.TemplateBuildStep{{Command: &api.TemplateBuildCommand{Command: "echo hi", Cwd: "/app"}}}}
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
				if tt.oldRaw != nil {
					req.OldObject.Raw = tt.oldRaw
				}
			}
			if tt.status {
				req.SubResource = "status"
			}
			h := &ValidatingHandler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Decoder: admission.NewDecoder(scheme)}
			response := h.Handle(context.Background(), req)
			require.Equal(t, tt.allowed, response.Allowed, "%v", response.Result)
		})
	}
}

func TestTemplateNameGroupsIndependentVersions(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, api.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := &ValidatingHandler{Client: c, Decoder: admission.NewDecoder(scheme)}
	names := []string{"abcdefghijklmnopqrst", "st-abcdefghijklmnopqrst", "st-bcdefghijklmnopqrstu0"}
	for i, name := range names {
		t.Run(name, func(t *testing.T) {
			draft := &api.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, Labels: map[string]string{api.LabelTemplateName: "python-agent"}},
				Spec:       api.SandboxTemplateSpec{Backend: api.SandboxTemplateBackendKubernetes, Template: minimalPodTemplate(), Runtimes: []api.RuntimeConfig{{Name: "agent-runtime"}}},
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
			require.True(t, apierrors.IsNotFound(c.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: names[0]}, &api.SandboxTemplate{})))
		})
	}
	versions := &api.SandboxTemplateList{}
	require.NoError(t, c.List(t.Context(), versions, client.InNamespace("default"), client.MatchingLabels{api.LabelTemplateName: "python-agent"}))
	require.Len(t, versions.Items, 2)
}

func TestConcurrentBuildsWithSameTemplateName(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, api.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := &ValidatingHandler{Client: c, Decoder: admission.NewDecoder(scheme)}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, name := range []string{"st-abcdefghijklmnopqrst", "st-bcdefghijklmnopqrstu0"} {
		version := &api.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, Labels: map[string]string{api.LabelTemplateName: "python-agent"}},
			Spec:       api.SandboxTemplateSpec{Backend: api.SandboxTemplateBackendKubernetes, Template: minimalPodTemplate()},
		}
		data, err := json.Marshal(version)
		require.NoError(t, err)
		go func() {
			<-start
			response := handler.Handle(t.Context(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: data}}})
			if !response.Allowed {
				results <- fmt.Errorf("admission rejected Build: %s", response.Result.Message)
				return
			}
			results <- c.Create(t.Context(), version)
		}()
	}
	close(start)
	for range 2 {
		require.NoError(t, <-results)
	}
	versions := &api.SandboxTemplateList{}
	require.NoError(t, c.List(t.Context(), versions, client.InNamespace("default"), client.MatchingLabels{api.LabelTemplateName: "python-agent"}))
	require.Len(t, versions.Items, 2)
	require.NotEqual(t, versions.Items[0].Name, versions.Items[1].Name)
	require.Empty(t, versions.Items[0].OwnerReferences)
	require.Empty(t, versions.Items[1].OwnerReferences)
	duplicate := versions.Items[0].DeepCopy()
	duplicate.ResourceVersion, duplicate.UID = "", ""
	require.True(t, apierrors.IsAlreadyExists(c.Create(t.Context(), duplicate)))
}

func TestTemplateNameAdmission(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, api.AddToScheme(scheme))
	for _, tt := range []struct {
		name         string
		templateName *string
		objects      []runtime.Object
		getError     error
		update       bool
		allowed      bool
		code         int32
	}{
		{name: "available", allowed: true},
		{name: "empty name", templateName: ptr.To(""), code: 422},
		{name: "reserved name", templateName: ptr.To("tags"), code: 422},
		{name: "invalid name", templateName: ptr.To("INVALID"), code: 422},
		{name: "DNS name cannot contain dots", templateName: ptr.To("has.dot"), code: 422},
		{name: "maximum name length", templateName: ptr.To(strings.Repeat("a", 63)), allowed: true},
		{name: "name too long", templateName: ptr.To(strings.Repeat("a", 64)), code: 422},
		{name: "name lookup unavailable", getError: apierrors.NewServiceUnavailable("cache unavailable"), code: 503},
		{name: "unsupported resource kind", getError: &meta.NoKindMatchError{GroupKind: api.GroupVersion.WithKind("SandboxSet").GroupKind()}, allowed: true},
		{name: "metadata update", update: true, allowed: true},
		{name: "metadata update does not recheck occupancy", update: true, allowed: true, objects: []runtime.Object{&api.SandboxSet{ObjectMeta: metav1.ObjectMeta{Name: "python-agent", Namespace: "default"}}}},
		{name: "SandboxSet occupancy", code: 409, objects: []runtime.Object{&api.SandboxSet{ObjectMeta: metav1.ObjectMeta{Name: "python-agent", Namespace: "default"}}}},
		{name: "Checkpoint occupancy", code: 409, objects: []runtime.Object{&api.Checkpoint{ObjectMeta: metav1.ObjectMeta{Name: "python-agent", Namespace: "default"}}}},
		{name: "other namespace does not collide", allowed: true, objects: []runtime.Object{&api.SandboxSet{ObjectMeta: metav1.ObjectMeta{Name: "python-agent", Namespace: "other"}}}},
		{name: "other Build with same name does not occupy group", allowed: true, objects: []runtime.Object{&api.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: "st-existing", Namespace: "default", Labels: map[string]string{api.LabelTemplateName: "python-agent"}}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			version := &api.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: "st-abcdefghijklmnopqrst", Namespace: "default", Labels: map[string]string{api.LabelTemplateName: "python-agent"}}, Spec: api.SandboxTemplateSpec{Backend: api.SandboxTemplateBackendKubernetes, Template: minimalPodTemplate()}}
			if tt.templateName != nil {
				version.Labels[api.LabelTemplateName] = *tt.templateName
			}
			data, err := json.Marshal(version)
			require.NoError(t, err)
			request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: data}}}
			if tt.update {
				request.Operation = admissionv1.Update
				request.OldObject.Raw = data
				version.Labels["app"] = "test"
				request.Object.Raw, err = json.Marshal(version)
				require.NoError(t, err)
			}
			builder := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tt.objects...)
			if tt.getError != nil {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return tt.getError
				}})
			}
			h := &ValidatingHandler{Client: builder.Build(), Decoder: admission.NewDecoder(scheme)}
			response := h.Handle(context.Background(), request)
			require.Equal(t, tt.allowed, response.Allowed, "%v", response.Result)
			if tt.code != 0 {
				require.Equal(t, tt.code, response.Result.Code)
			}
		})
	}
}
