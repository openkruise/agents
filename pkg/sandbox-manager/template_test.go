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

package sandbox_manager

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/templatecr"
)

type templateStoreFuncs struct {
	get      func(context.Context, string, string) (*agentsv1alpha1.SandboxTemplate, error)
	getFresh func(context.Context, string, string) (*agentsv1alpha1.SandboxTemplate, error)
	create   func(context.Context, *agentsv1alpha1.SandboxTemplate) error
	update   func(context.Context, *agentsv1alpha1.SandboxTemplate) error
	delete   func(context.Context, string, string, types.UID, string) error
}

func (s *templateStoreFuncs) Get(ctx context.Context, namespace, name string) (*agentsv1alpha1.SandboxTemplate, error) {
	return s.get(ctx, namespace, name)
}

func (s *templateStoreFuncs) GetFresh(ctx context.Context, namespace, name string) (*agentsv1alpha1.SandboxTemplate, error) {
	return s.getFresh(ctx, namespace, name)
}

func (s *templateStoreFuncs) Create(ctx context.Context, template *agentsv1alpha1.SandboxTemplate) error {
	return s.create(ctx, template)
}

func (s *templateStoreFuncs) Update(ctx context.Context, template *agentsv1alpha1.SandboxTemplate) error {
	return s.update(ctx, template)
}

func (s *templateStoreFuncs) Delete(ctx context.Context, namespace, name string, uid types.UID, resourceVersion string) error {
	return s.delete(ctx, namespace, name, uid, resourceVersion)
}

func templateNotFound(name string) error {
	return apierrors.NewNotFound(schema.GroupResource{Group: agentsv1alpha1.GroupVersion.Group, Resource: "sandboxtemplates"}, name)
}

// stubRead returns a store read that always yields the given template and error.
func stubRead(template *agentsv1alpha1.SandboxTemplate, err error) func(context.Context, string, string) (*agentsv1alpha1.SandboxTemplate, error) {
	return func(context.Context, string, string) (*agentsv1alpha1.SandboxTemplate, error) {
		return template.DeepCopy(), err
	}
}

// stubStore serves cached and fresh reads from one fixed result; layer
// withCreate, withUpdate, or withDelete to control writes.
func stubStore(template *agentsv1alpha1.SandboxTemplate, err error) *templateStoreFuncs {
	return &templateStoreFuncs{get: stubRead(template, err), getFresh: stubRead(template, err)}
}

func (s *templateStoreFuncs) withCreate(err error) *templateStoreFuncs {
	s.create = func(context.Context, *agentsv1alpha1.SandboxTemplate) error { return err }
	return s
}

func (s *templateStoreFuncs) withUpdate(err error) *templateStoreFuncs {
	s.update = func(context.Context, *agentsv1alpha1.SandboxTemplate) error { return err }
	return s
}

func (s *templateStoreFuncs) withDelete(err error) *templateStoreFuncs {
	s.delete = func(context.Context, string, string, types.UID, string) error { return err }
	return s
}

func newTemplateManager(t *testing.T, objects ...ctrlclient.Object) (*SandboxManager, ctrlclient.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	uid := 0
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&agentsv1alpha1.SandboxTemplate{}).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.CreateOption) error {
				if template, ok := obj.(*agentsv1alpha1.SandboxTemplate); ok {
					uid++
					template.UID = types.UID(fmt.Sprintf("00000000-0000-0000-0000-%012d", uid))
					template.CreationTimestamp = metav1.Now()
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	return &SandboxManager{
		templateStore: templatecr.NewStore(client, func() ctrlclient.Reader { return client }),
	}, client
}

func validLogicalTemplateSpec(image string) agentsv1alpha1.SandboxTemplateSpec {
	return agentsv1alpha1.SandboxTemplateSpec{
		Backend: agentsv1alpha1.SandboxTemplateBackendKubernetes,
		Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "runtime", Image: image}},
		}},
	}
}

// submitEnvelopeSpec matches the envelope of a 1-cpu, 1024MiB reservation.
func submitEnvelopeSpec() agentsv1alpha1.SandboxTemplateSpec {
	cpu := resource.MustParse("1")
	memory := resource.MustParse("1024Mi")
	spec := validLogicalTemplateSpec("image")
	spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: memory},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: memory},
	}
	return spec
}

// reservationDraft builds a reserved draft carrying the controller's optional
// failure verdict, shared by the submit and get availability tests.
func reservationDraft(reserve ReserveTemplateOptions, createdAt time.Time, phase agentsv1alpha1.SandboxTemplateBuildPhase, reason string) *agentsv1alpha1.SandboxTemplate {
	draft := reservedTemplate(reserve, agentsv1alpha1.SandboxTemplateBackendKubernetes)
	draft.UID = "reserved-uid"
	draft.CreationTimestamp = metav1.NewTime(createdAt)
	draft.Status.Phase = phase
	if reason != "" {
		draft.Status.Conditions = []metav1.Condition{{
			Type:   agentsv1alpha1.SandboxTemplateBuildConditionReady,
			Status: metav1.ConditionFalse,
			Reason: reason,
		}}
	}
	return draft
}

func TestCreateTemplate(t *testing.T) {
	manager, client := newTemplateManager(t)
	spec := validLogicalTemplateSpec("python:3.12")
	spec.PersistentContents = []string{agentsv1alpha1.PersistentContentFilesystem}
	spec.BuildOptions = &agentsv1alpha1.SandboxTemplateBuildOptions{
		Steps: []agentsv1alpha1.TemplateBuildCommand{{Command: "pip install numpy"}},
	}
	opts := CreateTemplateOptions{Namespace: "team-a", Name: "python-agent", Spec: spec}

	created, err := manager.CreateTemplate(t.Context(), opts)
	require.NoError(t, err)
	assert.NotEmpty(t, created.BuildID)
	assert.Equal(t, agentsv1alpha1.SandboxTemplatePhasePending, created.Status.Phase)

	_, err = manager.CreateTemplate(t.Context(), CreateTemplateOptions{Namespace: "team-a", Name: "plain-template", Spec: validLogicalTemplateSpec("image")})
	require.NoError(t, err)
	plain := &agentsv1alpha1.SandboxTemplate{}
	require.NoError(t, client.Get(t.Context(), types.NamespacedName{Namespace: "team-a", Name: "plain-template"}, plain))
	assert.Nil(t, plain.Spec.BuildOptions)

	stored := &agentsv1alpha1.SandboxTemplate{}
	require.NoError(t, client.Get(t.Context(), types.NamespacedName{Namespace: opts.Namespace, Name: opts.Name}, stored))
	assert.Equal(t, agentsv1alpha1.TemplateBuildStateSubmitted, stored.Annotations[agentsv1alpha1.AnnotationTemplateBuildState])
	require.NotNil(t, stored.Spec.BuildOptions)

	_, err = manager.CreateTemplate(t.Context(), opts)
	require.Error(t, err)
	assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err))

	changed := opts
	changed.Spec = *opts.Spec.DeepCopy()
	changed.Spec.Template.Spec.Containers[0].Image = "python:3.13"
	_, err = manager.CreateTemplate(t.Context(), changed)
	require.Error(t, err)
	assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err))
}

func TestCreateTemplateReclaimsExpiredReservation(t *testing.T) {
	draft := reservedTemplate(ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}, agentsv1alpha1.SandboxTemplateBackendKubernetes)
	draft.UID = "expired-uid"
	draft.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * agentsv1alpha1.SandboxTemplateBuildReservationTTL))
	manager, client := newTemplateManager(t, draft)

	created, err := manager.CreateTemplate(t.Context(), CreateTemplateOptions{
		Namespace: "team-a", Name: "template", Spec: validLogicalTemplateSpec("python:3.12"),
	})
	require.NoError(t, err)
	assert.NotEqual(t, "expired-uid", created.BuildID)

	stored := &agentsv1alpha1.SandboxTemplate{}
	require.NoError(t, client.Get(t.Context(), types.NamespacedName{Namespace: "team-a", Name: "template"}, stored))
	assert.Equal(t, agentsv1alpha1.TemplateBuildStateSubmitted, stored.Annotations[agentsv1alpha1.AnnotationTemplateBuildState])
}

func TestReserveAndSubmitTemplate(t *testing.T) {
	manager, client := newTemplateManager(t)
	reserve := ReserveTemplateOptions{Namespace: "team-a", Name: "python-agent", CPUCount: 2, MemoryMB: 2048}

	reserved, err := manager.ReserveTemplate(t.Context(), reserve)
	require.NoError(t, err)
	assert.NotEmpty(t, reserved.BuildID)
	assert.Equal(t, agentsv1alpha1.SandboxTemplatePhasePending, reserved.Status.Phase)

	_, err = manager.ReserveTemplate(t.Context(), reserve)
	require.Error(t, err)
	assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err))

	cpu := resource.MustParse("2")
	memory := resource.MustParse("2048Mi")
	resources := corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: memory}
	spec := validLogicalTemplateSpec("python:3.12")
	spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: resources.DeepCopy(), Limits: resources.DeepCopy(),
	}
	spec.PersistentContents = []string{agentsv1alpha1.PersistentContentFilesystem}
	spec.BuildOptions = &agentsv1alpha1.SandboxTemplateBuildOptions{Steps: []agentsv1alpha1.TemplateBuildCommand{{Command: "pip install numpy"}}}
	submit := SubmitTemplateOptions{Namespace: reserve.Namespace, Name: reserve.Name, BuildID: reserved.BuildID, Spec: spec}

	submitted, err := manager.SubmitTemplate(t.Context(), submit)
	require.NoError(t, err)
	assert.Equal(t, reserved.BuildID, submitted.BuildID)
	stored := &agentsv1alpha1.SandboxTemplate{}
	require.NoError(t, client.Get(t.Context(), types.NamespacedName{Namespace: reserve.Namespace, Name: reserve.Name}, stored))
	assert.Equal(t, agentsv1alpha1.TemplateBuildStateSubmitted, stored.Annotations[agentsv1alpha1.AnnotationTemplateBuildState])

	_, err = manager.SubmitTemplate(t.Context(), submit)
	require.Error(t, err)
	assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err))
}

func TestSubmitTemplateRejectsUnavailableReservation(t *testing.T) {
	reserve := ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}
	tests := []struct {
		name      string
		createdAt time.Time
		phase     agentsv1alpha1.SandboxTemplateBuildPhase
		reason    string
		wantCode  managererrors.ErrorCode
	}{
		{name: "ttl elapsed before controller status update", createdAt: time.Now().Add(-2 * agentsv1alpha1.SandboxTemplateBuildReservationTTL), wantCode: managererrors.ErrorNotFound},
		{name: "controller already marked reservation expired", createdAt: time.Now(), phase: agentsv1alpha1.SandboxTemplatePhaseFailed, reason: agentsv1alpha1.SandboxTemplateBuildReasonReservationExpired, wantCode: managererrors.ErrorNotFound},
		{name: "non-expiration failure remains terminal", createdAt: time.Now(), phase: agentsv1alpha1.SandboxTemplatePhaseFailed, reason: "ValidationFailed", wantCode: managererrors.ErrorConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, client := newTemplateManager(t, reservationDraft(reserve, tt.createdAt, tt.phase, tt.reason))
			_, err := manager.SubmitTemplate(t.Context(), SubmitTemplateOptions{
				Namespace: reserve.Namespace, Name: reserve.Name, BuildID: "reserved-uid", Spec: submitEnvelopeSpec(),
			})
			require.Error(t, err)
			assert.Equal(t, tt.wantCode, managererrors.GetErrCode(err))

			stored := &agentsv1alpha1.SandboxTemplate{}
			require.NoError(t, client.Get(t.Context(), types.NamespacedName{Namespace: reserve.Namespace, Name: reserve.Name}, stored))
			assert.Equal(t, agentsv1alpha1.TemplateBuildStateReserved, stored.Annotations[agentsv1alpha1.AnnotationTemplateBuildState])
		})
	}
}

func TestSubmitTemplateRejectsSubmittedBuild(t *testing.T) {
	spec := validLogicalTemplateSpec("python:3.12")
	template := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "team-a",
			Name:              "python-agent",
			UID:               "submitted-uid",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * agentsv1alpha1.SandboxTemplateBuildReservationTTL)),
			Annotations:       map[string]string{agentsv1alpha1.AnnotationTemplateBuildState: agentsv1alpha1.TemplateBuildStateSubmitted},
		},
		Spec: spec,
	}
	manager, _ := newTemplateManager(t, template)

	_, err := manager.SubmitTemplate(t.Context(), SubmitTemplateOptions{
		Namespace: template.Namespace, Name: template.Name, BuildID: string(template.UID), Spec: spec,
	})
	require.Error(t, err)
	assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err))
}

func TestTemplateEntriesConfirmFreshStateAfterWrite(t *testing.T) {
	reserve := ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}
	draft := reservedTemplate(reserve, agentsv1alpha1.SandboxTemplateBackendKubernetes)
	draft.UID = "build-uid"
	draft.CreationTimestamp = metav1.Now()

	expired := draft.DeepCopy()
	expired.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * agentsv1alpha1.SandboxTemplateBuildReservationTTL))

	submitted := expired.DeepCopy()
	submitSpec := submitEnvelopeSpec()
	submitted.Spec = *submitSpec.DeepCopy()
	submitted.Annotations = map[string]string{agentsv1alpha1.AnnotationTemplateBuildState: agentsv1alpha1.TemplateBuildStateSubmitted}

	tests := []struct {
		name      string
		submit    bool
		cached    *agentsv1alpha1.SandboxTemplate
		cachedErr error
		fresh     *agentsv1alpha1.SandboxTemplate
		freshErr  error
		wantCode  managererrors.ErrorCode
	}{
		{
			name:      "cached read misses the just-created reservation on submit",
			submit:    true,
			cachedErr: templateNotFound("template"),
			fresh:     draft,
		},
		{
			name:     "cached expiry hides a completed submission on submit",
			submit:   true,
			cached:   expired,
			fresh:    submitted,
			wantCode: managererrors.ErrorConflict,
		},
		{
			name:      "fresh confirmation read failure",
			submit:    true,
			cachedErr: templateNotFound("template"),
			freshErr:  errors.New("read boom"),
			wantCode:  managererrors.ErrorInternal,
		},
		{
			name:      "cached read misses the just-created template on get",
			cachedErr: templateNotFound("template"),
			fresh:     submitted,
		},
		{
			name:   "cached expiry hides a completed submission on get",
			cached: expired,
			fresh:  submitted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := stubStore(tt.cached, tt.cachedErr)
			store.getFresh = stubRead(tt.fresh, tt.freshErr)
			store.update = func(context.Context, *agentsv1alpha1.SandboxTemplate) error { return nil }
			manager := &SandboxManager{templateStore: store}

			var details *TemplateDetails
			var err error
			if tt.submit {
				details, err = manager.SubmitTemplate(t.Context(), SubmitTemplateOptions{
					Namespace: "team-a", Name: "template", BuildID: "build-uid", Spec: submitSpec,
				})
			} else {
				details, err = manager.GetTemplate(t.Context(), "team-a", "template")
			}
			if tt.wantCode == "" {
				require.NoError(t, err)
				assert.Equal(t, "build-uid", details.BuildID)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tt.wantCode, managererrors.GetErrCode(err))
		})
	}
}

func TestGetTemplateHidesExpiredReservation(t *testing.T) {
	reserve := ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}
	tests := []struct {
		name      string
		createdAt time.Time
		phase     agentsv1alpha1.SandboxTemplateBuildPhase
		reason    string
		wantFound bool
	}{
		{name: "ttl elapsed", createdAt: time.Now().Add(-2 * agentsv1alpha1.SandboxTemplateBuildReservationTTL), wantFound: false},
		{name: "controller marked reservation expired", createdAt: time.Now(), phase: agentsv1alpha1.SandboxTemplatePhaseFailed, reason: agentsv1alpha1.SandboxTemplateBuildReasonReservationExpired, wantFound: false},
		{name: "non-expiration failure remains visible", createdAt: time.Now(), phase: agentsv1alpha1.SandboxTemplatePhaseFailed, reason: "ValidationFailed", wantFound: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, _ := newTemplateManager(t, reservationDraft(reserve, tt.createdAt, tt.phase, tt.reason))

			_, err := manager.GetTemplate(t.Context(), reserve.Namespace, reserve.Name)
			if !tt.wantFound {
				require.Error(t, err)
				assert.Equal(t, managererrors.ErrorNotFound, managererrors.GetErrCode(err))
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestReserveTemplateReclaimsExpiredDraft(t *testing.T) {
	tests := []struct {
		name     string
		memoryMB int32
	}{
		{name: "same reservation parameters", memoryMB: 1024},
		{name: "different reservation parameters", memoryMB: 2048},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			draft := reservedTemplate(ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}, agentsv1alpha1.SandboxTemplateBackendKubernetes)
			draft.UID = "expired-uid"
			draft.ResourceVersion = "7"
			draft.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * agentsv1alpha1.SandboxTemplateBuildReservationTTL))
			manager, client := newTemplateManager(t, draft)
			deleteUsedUID, deleteUsedRV := false, false
			intercepted := interceptor.NewClient(client.(ctrlclient.WithWatch), interceptor.Funcs{
				Delete: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.DeleteOption) error {
					deleteOptions := (&ctrlclient.DeleteOptions{}).ApplyOptions(opts)
					deleteUsedUID = deleteOptions.Preconditions != nil && deleteOptions.Preconditions.UID != nil && *deleteOptions.Preconditions.UID == draft.UID
					deleteUsedRV = deleteOptions.Preconditions != nil && deleteOptions.Preconditions.ResourceVersion != nil && *deleteOptions.Preconditions.ResourceVersion == draft.ResourceVersion
					return c.Delete(ctx, obj, opts...)
				},
			})
			manager.templateStore = templatecr.NewStore(intercepted, func() ctrlclient.Reader { return intercepted })

			created, err := manager.ReserveTemplate(t.Context(), ReserveTemplateOptions{
				Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: tt.memoryMB,
			})
			require.NoError(t, err)
			assert.True(t, deleteUsedUID)
			assert.True(t, deleteUsedRV)
			assert.NotEqual(t, "expired-uid", created.BuildID)
		})
	}
}

func TestCreateTemplateRecoversFromStaleCacheAfterAlreadyExists(t *testing.T) {
	opts := CreateTemplateOptions{Namespace: "team-a", Name: "template", Spec: validLogicalTemplateSpec("image")}
	existing := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   opts.Namespace,
			Name:        opts.Name,
			UID:         "existing-uid",
			Annotations: map[string]string{agentsv1alpha1.AnnotationTemplateBuildState: agentsv1alpha1.TemplateBuildStateSubmitted},
		},
		Spec: opts.Spec,
	}
	store := stubStore(nil, templateNotFound("template"))
	store.getFresh = stubRead(existing, nil)
	store.create = func(context.Context, *agentsv1alpha1.SandboxTemplate) error {
		return apierrors.NewAlreadyExists(schema.GroupResource{Group: agentsv1alpha1.GroupVersion.Group, Resource: "sandboxtemplates"}, opts.Name)
	}
	manager := &SandboxManager{templateStore: store}

	_, err := manager.CreateTemplate(t.Context(), opts)
	require.Error(t, err)
	assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err))
}

func TestReserveTemplateRecoversFromStaleCacheAfterAlreadyExists(t *testing.T) {
	opts := ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}
	existing := reservedTemplate(opts, agentsv1alpha1.SandboxTemplateBackendKubernetes)
	existing.UID = "existing-uid"
	existing.CreationTimestamp = metav1.Now()
	store := stubStore(nil, templateNotFound("template"))
	store.getFresh = stubRead(existing, nil)
	store.create = func(context.Context, *agentsv1alpha1.SandboxTemplate) error {
		return apierrors.NewAlreadyExists(schema.GroupResource{Group: agentsv1alpha1.GroupVersion.Group, Resource: "sandboxtemplates"}, opts.Name)
	}
	manager := &SandboxManager{templateStore: store}

	_, err := manager.ReserveTemplate(t.Context(), opts)
	require.Error(t, err)
	assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err))
}

func TestSubmitTemplateRecoversFromStaleCacheAfterConflict(t *testing.T) {
	reserve := ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}
	stale := reservedTemplate(reserve, agentsv1alpha1.SandboxTemplateBackendKubernetes)
	stale.UID = "build-uid"
	stale.CreationTimestamp = metav1.Now()
	spec := submitEnvelopeSpec()
	latest := stale.DeepCopy()
	latest.Spec = *spec.DeepCopy()
	latest.Annotations = map[string]string{agentsv1alpha1.AnnotationTemplateBuildState: agentsv1alpha1.TemplateBuildStateSubmitted}
	store := stubStore(stale, nil)
	store.getFresh = stubRead(latest, nil)
	store.update = func(context.Context, *agentsv1alpha1.SandboxTemplate) error {
		return apierrors.NewConflict(schema.GroupResource{Group: agentsv1alpha1.GroupVersion.Group, Resource: "sandboxtemplates"}, stale.Name, errors.New("stale resourceVersion"))
	}
	manager := &SandboxManager{templateStore: store}

	_, err := manager.SubmitTemplate(t.Context(), SubmitTemplateOptions{
		Namespace: stale.Namespace, Name: stale.Name, BuildID: string(stale.UID), Spec: spec,
	})
	require.Error(t, err)
	assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err))
}

func TestTemplateStorageErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want managererrors.ErrorCode
	}{
		{name: "conflict", err: apierrors.NewConflict(schema.GroupResource{Group: "agents.kruise.io", Resource: "sandboxtemplates"}, "t", errors.New("conflict")), want: managererrors.ErrorConflict},
		{name: "invalid", err: apierrors.NewInvalid(agentsv1alpha1.GroupVersion.WithKind("SandboxTemplate").GroupKind(), "t", field.ErrorList{field.Invalid(field.NewPath("spec"), nil, "invalid")}), want: managererrors.ErrorBadRequest},
		{name: "not found", err: apierrors.NewNotFound(schema.GroupResource{Group: "agents.kruise.io", Resource: "sandboxtemplates"}, "t"), want: managererrors.ErrorNotFound},
		{name: "too many requests", err: apierrors.NewTooManyRequests("busy", 1), want: managererrors.ErrorUnavailable},
		{name: "unknown", err: errors.New("boom"), want: managererrors.ErrorInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, managererrors.GetErrCode(templateStorageError("operation", tt.err)))
		})
	}
}

func TestReserveTemplateRejectsInvalidInput(t *testing.T) {
	manager := &SandboxManager{}
	tests := []struct {
		name     string
		opts     ReserveTemplateOptions
		wantCode managererrors.ErrorCode
	}{
		{name: "unsupported backend", opts: ReserveTemplateOptions{Namespace: "team-a", Name: "template", Backend: agentsv1alpha1.SandboxTemplateBackendSubstrate, CPUCount: 1, MemoryMB: 1024}, wantCode: managererrors.ErrorNotImplemented},
		{name: "zero cpu", opts: ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 0, MemoryMB: 1024}, wantCode: managererrors.ErrorBadRequest},
		{name: "zero memory", opts: ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 0}, wantCode: managererrors.ErrorBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := manager.ReserveTemplate(t.Context(), tt.opts)
			require.Error(t, err)
			assert.Equal(t, tt.wantCode, managererrors.GetErrCode(err))
		})
	}
}

func TestReserveTemplateRejectsStorageFailures(t *testing.T) {
	opts := ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}
	tests := []struct {
		name     string
		store    *templateStoreFuncs
		wantCode managererrors.ErrorCode
	}{
		{name: "reservation read failure", store: stubStore(nil, errors.New("read boom")), wantCode: managererrors.ErrorInternal},
		{name: "reservation create failure", store: stubStore(nil, templateNotFound("template")).withCreate(errors.New("write boom")), wantCode: managererrors.ErrorInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &SandboxManager{templateStore: tt.store}
			_, err := manager.ReserveTemplate(t.Context(), opts)
			require.Error(t, err)
			assert.Equal(t, tt.wantCode, managererrors.GetErrCode(err))
		})
	}
}

func TestCreateTemplateRejectsStorageFailures(t *testing.T) {
	opts := CreateTemplateOptions{Namespace: "team-a", Name: "template", Spec: validLogicalTemplateSpec("image")}
	existing := reservedTemplate(ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}, agentsv1alpha1.SandboxTemplateBackendKubernetes)
	existing.UID = "existing-uid"
	existing.CreationTimestamp = metav1.Now()
	tests := []struct {
		name  string
		store *templateStoreFuncs
	}{
		{name: "template create failure", store: stubStore(nil, templateNotFound("template")).withCreate(errors.New("write boom"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &SandboxManager{templateStore: tt.store}
			_, err := manager.CreateTemplate(t.Context(), opts)
			require.Error(t, err)
			assert.Equal(t, managererrors.ErrorInternal, managererrors.GetErrCode(err))
		})
	}
}

func TestCreateTemplateRejectsExistingTemplates(t *testing.T) {
	owned := true
	existing := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "template", UID: "revision-uid",
			OwnerReferences: []metav1.OwnerReference{{Controller: &owned, UID: "owner"}},
		},
		Spec: validLogicalTemplateSpec("image"),
	}
	manager, _ := newTemplateManager(t, existing)

	_, err := manager.CreateTemplate(t.Context(), CreateTemplateOptions{
		Namespace: "team-a", Name: "template", Spec: validLogicalTemplateSpec("image"),
	})
	require.Error(t, err)
	assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err))
}

func TestSubmitTemplateRejectsInvalidSubmissions(t *testing.T) {
	reserve := ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}
	draft := reservedTemplate(reserve, agentsv1alpha1.SandboxTemplateBackendKubernetes)
	draft.UID = "build-uid"
	draft.CreationTimestamp = metav1.Now()

	tests := []struct {
		name     string
		template *agentsv1alpha1.SandboxTemplate
		getErr   error
		spec     agentsv1alpha1.SandboxTemplateSpec
		buildID  string
		wantCode managererrors.ErrorCode
	}{
		{name: "missing reservation", getErr: templateNotFound("template"), spec: submitEnvelopeSpec(), wantCode: managererrors.ErrorNotFound},
		{name: "read failure", getErr: errors.New("read boom"), spec: submitEnvelopeSpec(), wantCode: managererrors.ErrorInternal},
		{name: "build id mismatch", template: draft, spec: submitEnvelopeSpec(), buildID: "other-build", wantCode: managererrors.ErrorConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &SandboxManager{templateStore: stubStore(tt.template, tt.getErr)}
			buildID := tt.buildID
			if buildID == "" {
				buildID = "build-uid"
			}
			_, err := manager.SubmitTemplate(t.Context(), SubmitTemplateOptions{
				Namespace: "team-a", Name: "template", BuildID: buildID, Spec: tt.spec,
			})
			require.Error(t, err)
			assert.Equal(t, tt.wantCode, managererrors.GetErrCode(err))
		})
	}
}

func TestSubmitTemplateConflictRecoveryRejectsAmbiguousFreshState(t *testing.T) {
	reserve := ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}
	stale := reservedTemplate(reserve, agentsv1alpha1.SandboxTemplateBackendKubernetes)
	stale.UID = "build-uid"
	stale.CreationTimestamp = metav1.Now()
	submitSpec := submitEnvelopeSpec()

	expired := stale.DeepCopy()
	expired.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * agentsv1alpha1.SandboxTemplateBuildReservationTTL))
	rebound := stale.DeepCopy()
	rebound.UID = "other-uid"

	tests := []struct {
		name     string
		fresh    func(context.Context, string, string) (*agentsv1alpha1.SandboxTemplate, error)
		wantCode managererrors.ErrorCode
	}{
		{name: "fresh object vanished", fresh: stubRead(nil, templateNotFound("template")), wantCode: managererrors.ErrorNotFound},
		{name: "fresh read failure", fresh: stubRead(nil, errors.New("read boom")), wantCode: managererrors.ErrorInternal},
		{name: "fresh object bound to another build", fresh: stubRead(rebound, nil), wantCode: managererrors.ErrorConflict},
		{name: "fresh object expired while submitting", fresh: stubRead(expired, nil), wantCode: managererrors.ErrorConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := stubStore(stale, nil).withUpdate(apierrors.NewConflict(schema.GroupResource{Group: agentsv1alpha1.GroupVersion.Group, Resource: "sandboxtemplates"}, "template", errors.New("stale resourceVersion")))
			store.getFresh = tt.fresh
			manager := &SandboxManager{templateStore: store}
			_, err := manager.SubmitTemplate(t.Context(), SubmitTemplateOptions{
				Namespace: "team-a", Name: "template", BuildID: "build-uid", Spec: submitSpec,
			})
			require.Error(t, err)
			assert.Equal(t, tt.wantCode, managererrors.GetErrCode(err))
		})
	}
}

func TestReclaimExpiredReservationRejectsStorageFailures(t *testing.T) {
	reserve := ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}
	expired := reservedTemplate(reserve, agentsv1alpha1.SandboxTemplateBackendKubernetes)
	expired.UID = "expired-uid"
	expired.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * agentsv1alpha1.SandboxTemplateBuildReservationTTL))
	rebound := expired.DeepCopy()
	rebound.UID = "other-uid"
	rebound.CreationTimestamp = metav1.Now()

	submitted := expired.DeepCopy()
	submitSpec := submitEnvelopeSpec()
	submitted.Spec = *submitSpec.DeepCopy()
	submitted.Annotations = map[string]string{agentsv1alpha1.AnnotationTemplateBuildState: agentsv1alpha1.TemplateBuildStateSubmitted}

	tests := []struct {
		name      string
		deleteErr error
		fresh     func(context.Context, string, string) (*agentsv1alpha1.SandboxTemplate, error)
		wantCode  managererrors.ErrorCode
	}{
		{name: "reclaim delete failure", deleteErr: errors.New("delete boom"), fresh: stubRead(expired, nil), wantCode: managererrors.ErrorInternal},
		{name: "reclaim read failure", fresh: stubRead(nil, errors.New("read boom")), wantCode: managererrors.ErrorInternal},
		{name: "rebound reservation conflicts", fresh: stubRead(rebound, nil), wantCode: managererrors.ErrorConflict},
		{name: "completed submission replaces the expired view", fresh: stubRead(submitted, nil), wantCode: managererrors.ErrorConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := stubStore(expired, nil).withDelete(tt.deleteErr)
			store.getFresh = tt.fresh
			manager := &SandboxManager{templateStore: store}
			_, err := manager.ReserveTemplate(t.Context(), reserve)
			require.Error(t, err)
			assert.Equal(t, tt.wantCode, managererrors.GetErrCode(err))
		})
	}
}
