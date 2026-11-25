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
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/openkruise/agents/api/v1alpha1"
)

func TestTemplateGuardBeforeMaterialization(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, api.AddToScheme(scheme))
	template := &api.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "abcdefghijklmnopqrst", Namespace: "default", UID: types.UID("version-uid"), Generation: 1,
			Labels: map[string]string{api.LabelTemplateName: "python-agent"}},
		Spec:   api.SandboxTemplateSpec{Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runtime", Image: "test/image"}}}}},
		Status: api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseReady, ObservedGeneration: 1},
	}
	pool := &api.SandboxSet{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: template.Namespace}, Spec: api.SandboxSetSpec{EmbeddedSandboxTemplate: api.EmbeddedSandboxTemplate{TemplateRef: &api.SandboxTemplateRef{Name: template.Name}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(template).WithStatusSubresource(&api.SandboxTemplate{}).Build()
	recorder := record.NewFakeRecorder(4)
	r := &Reconciler{Client: c, Scheme: scheme, Recorder: recorder}
	_, err := r.buildSandboxTemplateSpec(context.Background(), pool)
	require.NoError(t, err)
	current := &api.SandboxTemplate{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(template), current))
	current.Status.Phase = api.SandboxTemplatePhaseBuilding
	require.NoError(t, c.Status().Update(context.Background(), current))
	_, err = r.createSandbox(context.Background(), pool, "revision")
	require.ErrorContains(t, err, "not a Ready static Version")
	select {
	case event := <-recorder.Events:
		require.Contains(t, event, EventCreateSandboxFailed)
		require.Contains(t, event, "Failed to validate template ref")
		require.Contains(t, event, "not a Ready static Version")
	default:
		t.Fatal("expected a create-sandbox-failed event for the rejected templateRef")
	}
	boxes := &api.SandboxList{}
	require.NoError(t, c.List(context.Background(), boxes))
	require.Empty(t, boxes.Items)

	legacy := template.DeepCopy()
	legacy.Labels = nil
	legacy.Status = api.SandboxTemplateStatus{}
	require.NoError(t, r.validateTemplateRef(context.Background(), legacy))

	current.Status.Phase = api.SandboxTemplatePhaseReady
	require.NoError(t, c.Status().Update(t.Context(), current))
	createError := errors.New("create unavailable")
	r.Recorder = record.NewFakeRecorder(1)
	r.Client = interceptor.NewClient(c, interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
		return createError
	}})
	_, err = r.createSandbox(t.Context(), pool, "revision")
	require.ErrorIs(t, err, createError)
	require.NoError(t, c.List(t.Context(), boxes))
	require.Empty(t, boxes.Items)
}

func TestManagedTemplateRefMaterialization(t *testing.T) {
	for _, tt := range []struct {
		name         string
		mutate       func(*api.SandboxTemplate)
		errorMessage string
	}{
		{name: "Ready static Version"},
		{name: "unmanaged template", mutate: func(v *api.SandboxTemplate) {
			v.Labels = nil
			v.Status = api.SandboxTemplateStatus{}
		}},
		{name: "ordinary Snapshot template", mutate: func(v *api.SandboxTemplate) {
			v.Labels = nil
			v.OwnerReferences = []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Checkpoint", Name: "capture", UID: "checkpoint-uid", Controller: ptr.To(true)}}
			v.Status = api.SandboxTemplateStatus{}
		}},
		{name: "deleting unmanaged template", errorMessage: "is being deleted", mutate: func(v *api.SandboxTemplate) {
			v.Labels = nil
			v.Status = api.SandboxTemplateStatus{}
			v.DeletionTimestamp = ptr.To(metav1.Now())
			v.Finalizers = []string{"test/retain"}
		}},
		{name: "deleting ordinary Snapshot template", errorMessage: "is being deleted", mutate: func(v *api.SandboxTemplate) {
			v.Labels = nil
			v.OwnerReferences = []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Checkpoint", Name: "capture", UID: "checkpoint-uid", Controller: ptr.To(true)}}
			v.Status = api.SandboxTemplateStatus{}
			v.DeletionTimestamp = ptr.To(metav1.Now())
			v.Finalizers = []string{"test/retain"}
		}},
		{name: "Draft", errorMessage: "not a Ready static Version", mutate: func(v *api.SandboxTemplate) { v.Status.Phase = api.SandboxTemplatePhasePending }},
		{name: "stale Ready", errorMessage: "not a Ready static Version", mutate: func(v *api.SandboxTemplate) { v.Status.ObservedGeneration = 0 }},
		{name: "captured Version", errorMessage: "not a Ready static Version", mutate: func(v *api.SandboxTemplate) {
			v.Status.Artifacts = []api.SandboxTemplateArtifact{{Type: "checkpoint", Name: "capture", Value: "backend-id"}}
		}},
		{name: "deleting Version", errorMessage: "not a Ready static Version", mutate: func(v *api.SandboxTemplate) {
			v.DeletionTimestamp = ptr.To(metav1.Now())
			v.Finalizers = []string{"test/retain"}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, api.AddToScheme(scheme))
			version := &api.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "st-abcdefghijklmnopqrst", Namespace: "default", Generation: 1,
					Labels: map[string]string{api.LabelTemplateName: "python-agent"}},
				Spec:   api.SandboxTemplateSpec{Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runtime", Image: "test/image"}}}}},
				Status: api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseReady, ObservedGeneration: 1},
			}
			if tt.mutate != nil {
				tt.mutate(version)
			}
			r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(version).Build(), Scheme: scheme}
			pool := &api.SandboxSet{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: version.Namespace}, Spec: api.SandboxSetSpec{EmbeddedSandboxTemplate: api.EmbeddedSandboxTemplate{TemplateRef: &api.SandboxTemplateRef{Name: version.Name}}}}
			spec, err := r.buildSandboxTemplateSpec(t.Context(), pool)
			if tt.errorMessage != "" {
				require.ErrorContains(t, err, tt.errorMessage)
				require.Nil(t, spec)
			} else {
				require.NoError(t, err)
				require.Equal(t, version.Spec.Template, spec.Template)
			}
		})
	}
}
