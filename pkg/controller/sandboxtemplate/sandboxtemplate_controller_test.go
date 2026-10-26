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
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

var testNow = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func newTestReconciler(t *testing.T, objects ...client.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&agentsv1alpha1.SandboxTemplate{}).
		WithObjects(objects...).
		Build()
	return &Reconciler{
		Client: fakeClient,
		now:    func() time.Time { return testNow },
	}
}

func submittedTemplate(name string) *agentsv1alpha1.SandboxTemplate {
	return &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			UID:               types.UID(name + "-uid"),
			Generation:        1,
			CreationTimestamp: metav1.NewTime(testNow.Add(-time.Minute)),
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationTemplateBuildState:       agentsv1alpha1.TemplateBuildStateSubmitted,
				agentsv1alpha1.AnnotationTemplateBuildInputDigest: "digest-" + name,
			},
		},
		Spec: agentsv1alpha1.SandboxTemplateSpec{
			Backend: agentsv1alpha1.SandboxTemplateBackendKubernetes,
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "runtime", Image: "python:3.12"}},
				},
			},
		},
	}
}

func reconcileRequest(template *agentsv1alpha1.SandboxTemplate) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: template.Namespace, Name: template.Name}}
}

func getTemplate(t *testing.T, r *Reconciler, name string) *agentsv1alpha1.SandboxTemplate {
	t.Helper()
	template := &agentsv1alpha1.SandboxTemplate{}
	require.NoError(t, r.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: name}, template))
	return template
}

func TestReconcileIgnoresNonLogicalTemplates(t *testing.T) {
	controller := true
	tests := []struct {
		name   string
		mutate func(*agentsv1alpha1.SandboxTemplate)
	}{
		{
			name: "template without build state",
			mutate: func(template *agentsv1alpha1.SandboxTemplate) {
				delete(template.Annotations, agentsv1alpha1.AnnotationTemplateBuildState)
			},
		},
		{
			name: "owned template",
			mutate: func(template *agentsv1alpha1.SandboxTemplate) {
				template.OwnerReferences = []metav1.OwnerReference{{
					APIVersion: agentsv1alpha1.GroupVersion.String(),
					Kind:       "SandboxSet",
					Name:       "owner",
					UID:        "owner-uid",
					Controller: &controller,
				}}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			template := submittedTemplate("ignored")
			tt.mutate(template)
			r := newTestReconciler(t, template)

			result, err := r.Reconcile(t.Context(), reconcileRequest(template))
			require.NoError(t, err)
			assert.Zero(t, result)
			assert.Empty(t, getTemplate(t, r, template.Name).Status.Phase)
		})
	}
}

func TestReconcileReservedDraft(t *testing.T) {
	tests := []struct {
		name            string
		createdAt       time.Time
		expectedPhase   agentsv1alpha1.SandboxTemplateBuildPhase
		expectedStatus  metav1.ConditionStatus
		expectedReason  string
		expectedRequeue time.Duration
	}{
		{
			name:            "fresh reservation remains pending",
			createdAt:       testNow.Add(-20 * time.Minute),
			expectedPhase:   agentsv1alpha1.SandboxTemplatePhasePending,
			expectedStatus:  metav1.ConditionUnknown,
			expectedReason:  reasonReserved,
			expectedRequeue: 40 * time.Minute,
		},
		{
			name:           "expired reservation fails",
			createdAt:      testNow.Add(-agentsv1alpha1.SandboxTemplateBuildReservationTTL),
			expectedPhase:  agentsv1alpha1.SandboxTemplatePhaseFailed,
			expectedStatus: metav1.ConditionFalse,
			expectedReason: reasonExpired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			template := submittedTemplate("reserved")
			template.Annotations[agentsv1alpha1.AnnotationTemplateBuildState] = agentsv1alpha1.TemplateBuildStateReserved
			delete(template.Annotations, agentsv1alpha1.AnnotationTemplateBuildInputDigest)
			template.CreationTimestamp = metav1.NewTime(tt.createdAt)
			template.Spec.Template = nil
			r := newTestReconciler(t, template)

			result, err := r.Reconcile(t.Context(), reconcileRequest(template))
			require.NoError(t, err)
			assert.Equal(t, tt.expectedRequeue, result.RequeueAfter)

			updated := getTemplate(t, r, template.Name)
			assert.Equal(t, tt.expectedPhase, updated.Status.Phase)
			assert.Equal(t, updated.Generation, updated.Status.ObservedGeneration)
			condition := apiMeta.FindStatusCondition(updated.Status.Conditions, conditionReady)
			require.NotNil(t, condition)
			assert.Equal(t, tt.expectedStatus, condition.Status)
			assert.Equal(t, tt.expectedReason, condition.Reason)
		})
	}
}

func TestReconcileReservedDraftRejectsInputDigest(t *testing.T) {
	template := submittedTemplate("reserved-with-digest")
	template.Annotations[agentsv1alpha1.AnnotationTemplateBuildState] = agentsv1alpha1.TemplateBuildStateReserved
	template.Spec.Template = nil
	r := newTestReconciler(t, template)

	_, err := r.Reconcile(t.Context(), reconcileRequest(template))
	require.NoError(t, err)

	updated := getTemplate(t, r, template.Name)
	assert.Equal(t, agentsv1alpha1.SandboxTemplatePhaseFailed, updated.Status.Phase)
	condition := apiMeta.FindStatusCondition(updated.Status.Conditions, conditionReady)
	require.NotNil(t, condition)
	assert.Equal(t, reasonInvalid, condition.Reason)
	assert.Equal(t, "build-input-digest annotation must not be set before submission", condition.Message)
}

func TestReconcileSubmittedKubernetesTemplate(t *testing.T) {
	tests := []struct {
		name            string
		mutate          func(*agentsv1alpha1.SandboxTemplate)
		expectedPhase   agentsv1alpha1.SandboxTemplateBuildPhase
		expectedStatus  metav1.ConditionStatus
		expectedReason  string
		expectedMessage string
	}{
		{
			name:           "valid template becomes ready",
			expectedPhase:  agentsv1alpha1.SandboxTemplatePhaseReady,
			expectedStatus: metav1.ConditionTrue,
			expectedReason: reasonReady,
		},
		{
			name: "empty backend uses the kubernetes default",
			mutate: func(template *agentsv1alpha1.SandboxTemplate) {
				template.Spec.Backend = ""
			},
			expectedPhase:  agentsv1alpha1.SandboxTemplatePhaseReady,
			expectedStatus: metav1.ConditionTrue,
			expectedReason: reasonReady,
		},
		{
			name: "accepted commands are reported as unexecuted",
			mutate: func(template *agentsv1alpha1.SandboxTemplate) {
				template.Spec.BuildOptions = &agentsv1alpha1.SandboxTemplateBuildOptions{
					Steps: []agentsv1alpha1.TemplateBuildCommand{{Command: "pip install numpy"}},
				}
			},
			expectedPhase:   agentsv1alpha1.SandboxTemplatePhaseReady,
			expectedStatus:  metav1.ConditionTrue,
			expectedReason:  reasonUnexecuted,
			expectedMessage: commandsNotExecutedMessage,
		},
		{
			name: "missing input digest fails",
			mutate: func(template *agentsv1alpha1.SandboxTemplate) {
				delete(template.Annotations, agentsv1alpha1.AnnotationTemplateBuildInputDigest)
			},
			expectedPhase:   agentsv1alpha1.SandboxTemplatePhaseFailed,
			expectedStatus:  metav1.ConditionFalse,
			expectedReason:  reasonInvalid,
			expectedMessage: "build-input-digest annotation is required after submission",
		},
		{
			name: "empty input digest fails",
			mutate: func(template *agentsv1alpha1.SandboxTemplate) {
				template.Annotations[agentsv1alpha1.AnnotationTemplateBuildInputDigest] = " "
			},
			expectedPhase:   agentsv1alpha1.SandboxTemplatePhaseFailed,
			expectedStatus:  metav1.ConditionFalse,
			expectedReason:  reasonInvalid,
			expectedMessage: "build-input-digest annotation is required after submission",
		},
		{
			name: "missing pod template fails",
			mutate: func(template *agentsv1alpha1.SandboxTemplate) {
				template.Spec.Template = nil
			},
			expectedPhase:   agentsv1alpha1.SandboxTemplatePhaseFailed,
			expectedStatus:  metav1.ConditionFalse,
			expectedReason:  reasonInvalid,
			expectedMessage: "spec.template is required",
		},
		{
			name: "empty container list fails",
			mutate: func(template *agentsv1alpha1.SandboxTemplate) {
				template.Spec.Template.Spec.Containers = nil
			},
			expectedPhase:   agentsv1alpha1.SandboxTemplatePhaseFailed,
			expectedStatus:  metav1.ConditionFalse,
			expectedReason:  reasonInvalid,
			expectedMessage: "spec.template.spec.containers must not be empty",
		},
		{
			name: "container without image fails",
			mutate: func(template *agentsv1alpha1.SandboxTemplate) {
				template.Spec.Template.Spec.Containers[0].Image = " "
			},
			expectedPhase:   agentsv1alpha1.SandboxTemplatePhaseFailed,
			expectedStatus:  metav1.ConditionFalse,
			expectedReason:  reasonInvalid,
			expectedMessage: "spec.template.spec.containers[0].image is required",
		},
		{
			name: "unsupported backend fails",
			mutate: func(template *agentsv1alpha1.SandboxTemplate) {
				template.Spec.Backend = agentsv1alpha1.SandboxTemplateBackendSubstrate
			},
			expectedPhase:  agentsv1alpha1.SandboxTemplatePhaseFailed,
			expectedStatus: metav1.ConditionFalse,
			expectedReason: reasonUnsupported,
		},
		{
			name: "invalid build state fails",
			mutate: func(template *agentsv1alpha1.SandboxTemplate) {
				template.Annotations[agentsv1alpha1.AnnotationTemplateBuildState] = "unknown"
			},
			expectedPhase:  agentsv1alpha1.SandboxTemplatePhaseFailed,
			expectedStatus: metav1.ConditionFalse,
			expectedReason: reasonInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			template := submittedTemplate("template")
			if tt.mutate != nil {
				tt.mutate(template)
			}
			r := newTestReconciler(t, template)

			_, err := r.Reconcile(t.Context(), reconcileRequest(template))
			require.NoError(t, err)

			updated := getTemplate(t, r, template.Name)
			assert.Equal(t, tt.expectedPhase, updated.Status.Phase)
			condition := apiMeta.FindStatusCondition(updated.Status.Conditions, conditionReady)
			require.NotNil(t, condition)
			assert.Equal(t, tt.expectedStatus, condition.Status)
			assert.Equal(t, tt.expectedReason, condition.Reason)
			if tt.expectedMessage != "" {
				assert.Equal(t, tt.expectedMessage, condition.Message)
			}
		})
	}
}

func TestReconcilePreservesArtifacts(t *testing.T) {
	template := submittedTemplate("with-artifact")
	template.Status.Artifacts = []agentsv1alpha1.SandboxTemplateArtifact{{Type: "checkpoint", Name: "checkpoint-1"}}
	r := newTestReconciler(t, template)

	_, err := r.Reconcile(t.Context(), reconcileRequest(template))
	require.NoError(t, err)

	updated := getTemplate(t, r, template.Name)
	assert.Equal(t, template.Status.Artifacts, updated.Status.Artifacts)
	assert.Equal(t, agentsv1alpha1.SandboxTemplatePhaseReady, updated.Status.Phase)
}

func TestLogicalTemplatePredicate(t *testing.T) {
	p := logicalTemplatePredicate()
	logical := submittedTemplate("logical")
	ordinary := logical.DeepCopy()
	delete(ordinary.Annotations, agentsv1alpha1.AnnotationTemplateBuildState)
	delete(ordinary.Annotations, agentsv1alpha1.AnnotationTemplateBuildInputDigest)
	subordinate := logical.DeepCopy()
	controller := true
	subordinate.OwnerReferences = []metav1.OwnerReference{{Controller: &controller}}

	assert.True(t, p.Create(event.CreateEvent{Object: logical}))
	assert.False(t, p.Create(event.CreateEvent{Object: ordinary}))
	assert.False(t, p.Create(event.CreateEvent{Object: subordinate}))
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: ordinary, ObjectNew: ordinary.DeepCopy()}))
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: ordinary, ObjectNew: logical}))
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: logical, ObjectNew: ordinary}))
	assert.False(t, p.Delete(event.DeleteEvent{Object: logical}))
	assert.True(t, p.Generic(event.GenericEvent{Object: logical}))
	assert.False(t, p.Generic(event.GenericEvent{Object: ordinary}))
}

func TestReconcileSkipsFailedTemplate(t *testing.T) {
	template := submittedTemplate("already-failed")
	template.Status.Phase = agentsv1alpha1.SandboxTemplatePhaseFailed
	template.Status.Conditions = []metav1.Condition{{
		Type:    conditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  reasonUnsupported,
		Message: "kept",
	}}
	r := newTestReconciler(t, template)

	result, err := r.Reconcile(t.Context(), reconcileRequest(template))
	require.NoError(t, err)
	assert.Zero(t, result)

	updated := getTemplate(t, r, template.Name)
	assert.Equal(t, agentsv1alpha1.SandboxTemplatePhaseFailed, updated.Status.Phase)
	condition := apiMeta.FindStatusCondition(updated.Status.Conditions, conditionReady)
	require.NotNil(t, condition)
	assert.Equal(t, "kept", condition.Message)
}

func TestReconcileReturnsReadFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errors.New("read boom")
			},
		}).
		Build()
	r := &Reconciler{Client: fakeClient, now: func() time.Time { return testNow }}

	_, err := r.Reconcile(t.Context(), reconcileRequest(submittedTemplate("missing")))
	require.Error(t, err)
}

func newStubManager(t *testing.T) ctrl.Manager {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:0"}, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	require.NoError(t, err)
	return mgr
}

// TestReconciler_SetupWithManager registers the SandboxTemplate controller with
// a real controller-runtime manager backed by a stub REST config. The manager
// is never started, so no apiserver is required.
func TestReconciler_SetupWithManager(t *testing.T) {
	mgr := newStubManager(t)
	r := &Reconciler{Client: mgr.GetClient(), now: time.Now}
	require.NoError(t, r.SetupWithManager(mgr))
}
