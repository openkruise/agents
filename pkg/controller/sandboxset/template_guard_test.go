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
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/openkruise/agents/api/v1alpha1"
)

func TestTemplateGuardBeforeMaterialization(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, api.AddToScheme(scheme))
	claim := &api.SandboxTemplateClaim{ObjectMeta: metav1.ObjectMeta{Name: "python-agent", Namespace: "default", UID: types.UID("claim-uid"), Finalizers: []string{api.SandboxTemplateClaimCleanupFinalizer}}}
	template := &api.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "abcdefghijklmnopqrst", Namespace: claim.Namespace, UID: types.UID("version-uid"), Generation: 1,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplateClaim", Name: claim.Name, UID: claim.UID, Controller: ptr.To(true)}}},
		Spec:   api.SandboxTemplateSpec{Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runtime", Image: "test/image"}}}}},
		Status: api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseReady, ObservedGeneration: 1},
	}
	pool := &api.SandboxSet{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: claim.Namespace}, Spec: api.SandboxSetSpec{EmbeddedSandboxTemplate: api.EmbeddedSandboxTemplate{TemplateRef: &api.SandboxTemplateRef{Name: template.Name}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim, template).Build()
	r := &Reconciler{Client: c, Scheme: scheme}
	_, err := r.buildSandboxTemplateSpec(context.Background(), pool)
	require.NoError(t, err)
	current := &api.SandboxTemplateClaim{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(claim), current))
	require.NoError(t, c.Delete(context.Background(), current))
	_, err = r.createSandbox(context.Background(), pool, "revision")
	require.ErrorContains(t, err, "being deleted")
	boxes := &api.SandboxList{}
	require.NoError(t, c.List(context.Background(), boxes))
	require.Empty(t, boxes.Items)

	legacy := template.DeepCopy()
	legacy.OwnerReferences = nil
	legacy.Status = api.SandboxTemplateStatus{}
	require.NoError(t, r.validateTemplateRef(context.Background(), legacy))
}
