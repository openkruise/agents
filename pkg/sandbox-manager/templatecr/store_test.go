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

package templatecr

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestStoreGetFreshBypassesStaleClient(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	cached := fake.NewClientBuilder().WithScheme(scheme).Build()
	liveTemplate := &agentsv1alpha1.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a", Name: "template", UID: "template-uid",
	}}
	live := fake.NewClientBuilder().WithScheme(scheme).WithObjects(liveTemplate).Build()
	store := NewStore(cached, func() client.Reader { return live })

	_, err := store.Get(t.Context(), "team-a", "template")
	assert.True(t, apierrors.IsNotFound(err))

	got, err := store.GetFresh(t.Context(), "team-a", "template")
	require.NoError(t, err)
	assert.Equal(t, liveTemplate.UID, got.UID)
}

func newTestStore(t *testing.T) (*Store, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	return NewStore(fakeClient, func() client.Reader { return fakeClient }), fakeClient
}

func TestStoreCrud(t *testing.T) {
	store, fakeClient := newTestStore(t)
	template := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "template", UID: "template-uid"},
		Spec: agentsv1alpha1.SandboxTemplateSpec{
			Backend: agentsv1alpha1.SandboxTemplateBackendKubernetes,
		},
	}
	require.NoError(t, store.Create(t.Context(), template))

	duplicate := template.DeepCopy()
	duplicate.ResourceVersion = ""
	err := store.Create(t.Context(), duplicate)
	require.Error(t, err)
	assert.True(t, apierrors.IsAlreadyExists(err))

	template.Annotations = map[string]string{"updated": "true"}
	require.NoError(t, store.Update(t.Context(), template))

	missing := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "missing", UID: "missing-uid"},
	}
	err = store.Update(t.Context(), missing)
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err))

	stored := &agentsv1alpha1.SandboxTemplate{}
	require.NoError(t, fakeClient.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: "template"}, stored))

	err = store.Delete(t.Context(), "team-a", "template", stored.UID, "stale-resource-version")
	require.Error(t, err)
	assert.True(t, apierrors.IsConflict(err))

	require.NoError(t, store.Delete(t.Context(), "team-a", "template", stored.UID, stored.ResourceVersion))
	_, err = store.Get(t.Context(), "team-a", "template")
	assert.True(t, apierrors.IsNotFound(err))

	// A delete without a resource version relies on the uid precondition alone.
	second := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "template", UID: "template-uid-2"},
	}
	require.NoError(t, store.Create(t.Context(), second))
	require.NoError(t, store.Delete(t.Context(), "team-a", "template", second.UID, ""))
	_, err = store.Get(t.Context(), "team-a", "template")
	assert.True(t, apierrors.IsNotFound(err))
}

func TestCreationLabelsFromSelector(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		want     map[string]string
		wantErr  bool
	}{
		{name: "empty selector stamps nothing", selector: ""},
		{name: "single equality", selector: "env=prod", want: map[string]string{"env": "prod"}},
		{name: "multiple equalities", selector: "env=prod,shard=a", want: map[string]string{"env": "prod", "shard": "a"}},
		{name: "double equals", selector: "env==prod", want: map[string]string{"env": "prod"}},
		{name: "in with a single value", selector: "env in (prod)", want: map[string]string{"env": "prod"}},
		{name: "in with multiple values", selector: "env in (prod,staging)", wantErr: true},
		{name: "not equals passes without stamps", selector: "env!=staging", want: map[string]string{}},
		{name: "not in passes without stamps", selector: "env notin (prod,staging)", want: map[string]string{}},
		{name: "negated key passes without stamps", selector: "!env", want: map[string]string{}},
		{name: "negation beside equality", selector: "!debug,env=prod", want: map[string]string{"env": "prod"}},
		{name: "bare key existence", selector: "env", wantErr: true},
		{name: "contradictory selector", selector: "!env,env=prod", wantErr: true},
		{name: "invalid selector", selector: "invalid!!!selector", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CreationLabelsFromSelector(tt.selector)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestStoreCreateStampsCreationLabels(t *testing.T) {
	store, fakeClient := newTestStore(t)
	store.WithCreationLabels(map[string]string{"env": "prod"})
	template := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "template", UID: "template-uid",
			Labels: map[string]string{"app": "x"},
		},
		Spec: agentsv1alpha1.SandboxTemplateSpec{
			Backend: agentsv1alpha1.SandboxTemplateBackendKubernetes,
		},
	}
	require.NoError(t, store.Create(t.Context(), template))

	stored := &agentsv1alpha1.SandboxTemplate{}
	require.NoError(t, fakeClient.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: "template"}, stored))
	assert.Equal(t, map[string]string{"app": "x", "env": "prod"}, stored.Labels)
}
