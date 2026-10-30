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
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
)

// CreateTemplateOptions is the protocol-neutral input for atomic logical
// Template creation. Namespace is resolved by the API layer from the caller.
type CreateTemplateOptions struct {
	Namespace string
	Name      string
	Spec      agentsv1alpha1.SandboxTemplateSpec
}

// ReserveTemplateOptions holds a logical name and resource envelope for a
// later two-phase submission.
type ReserveTemplateOptions struct {
	Namespace string
	Name      string
	Backend   string
	CPUCount  int32
	MemoryMB  int32
}

// SubmitTemplateOptions completes one reserved draft in place.
type SubmitTemplateOptions struct {
	Namespace string
	Name      string
	BuildID   string
	Spec      agentsv1alpha1.SandboxTemplateSpec
}

// TemplateDetails is the Manager projection shared by custom and compatible
// API surfaces.
type TemplateDetails struct {
	Name              string
	BuildID           string
	CreationTimestamp metav1.Time
	Spec              agentsv1alpha1.SandboxTemplateSpec
	Status            agentsv1alpha1.SandboxTemplateStatus
}

// CreateTemplate atomically creates a submitted logical Template. A name
// that already holds any SandboxTemplate conflicts.
func (m *SandboxManager) CreateTemplate(ctx context.Context, opts CreateTemplateOptions) (*TemplateDetails, error) {
	if err := validateTemplateIdentity(opts.Namespace, opts.Name); err != nil {
		return nil, err
	}
	store, err := m.templateStoreOrErr()
	if err != nil {
		return nil, err
	}
	existing, err := m.resolveTemplateNameForWrite(ctx, store, opts.Namespace, opts.Name)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, managererrors.NewError(managererrors.ErrorConflict, "SandboxTemplate %q already exists", existing.Name)
	}
	template := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   opts.Namespace,
			Name:        opts.Name,
			Annotations: map[string]string{agentsv1alpha1.AnnotationTemplateBuildState: agentsv1alpha1.TemplateBuildStateSubmitted},
		},
		Spec: *opts.Spec.DeepCopy(),
	}
	if err := store.Create(ctx, template); err != nil {
		return nil, templateStorageError("create SandboxTemplate", err)
	}
	if template.UID == "" {
		return nil, managererrors.NewError(managererrors.ErrorInternal, "created SandboxTemplate has no uid")
	}
	return templateToDetails(template), nil
}

// ReserveTemplate creates one reserved draft. The SandboxTemplate UID is
// the Build identity.
func (m *SandboxManager) ReserveTemplate(ctx context.Context, opts ReserveTemplateOptions) (*TemplateDetails, error) {
	if err := validateTemplateIdentity(opts.Namespace, opts.Name); err != nil {
		return nil, err
	}
	backend := opts.Backend
	if backend == "" {
		backend = agentsv1alpha1.SandboxTemplateBackendKubernetes
	}
	if backend != agentsv1alpha1.SandboxTemplateBackendKubernetes {
		return nil, managererrors.NewError(managererrors.ErrorNotImplemented, "backend %q is not configured", backend)
	}
	if opts.CPUCount <= 0 || opts.MemoryMB <= 0 {
		return nil, managererrors.NewError(managererrors.ErrorBadRequest, "cpuCount and memoryMB must be greater than zero")
	}
	store, err := m.templateStoreOrErr()
	if err != nil {
		return nil, err
	}
	existing, err := m.resolveTemplateNameForWrite(ctx, store, opts.Namespace, opts.Name)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, managererrors.NewError(managererrors.ErrorConflict, "template name %q is already bound", existing.Name)
	}
	template := reservedTemplate(opts, backend)
	if err := store.Create(ctx, template); err != nil {
		return nil, templateStorageError("reserve SandboxTemplate", err)
	}
	if template.UID == "" {
		return nil, managererrors.NewError(managererrors.ErrorInternal, "reserved SandboxTemplate has no uid")
	}
	return templateToDetails(template), nil
}

// SubmitTemplate completes one reserved draft in place.
func (m *SandboxManager) SubmitTemplate(ctx context.Context, opts SubmitTemplateOptions) (*TemplateDetails, error) {
	if err := validateTemplateIdentity(opts.Namespace, opts.Name); err != nil {
		return nil, err
	}
	if opts.BuildID == "" {
		return nil, managererrors.NewError(managererrors.ErrorBadRequest, "buildID is required")
	}
	store, err := m.templateStoreOrErr()
	if err != nil {
		return nil, err
	}
	template, err := readTemplateAfterWrite(ctx, store, opts.Namespace, opts.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, managererrors.NewError(managererrors.ErrorNotFound, "template reservation %q not found", opts.Name)
		}
		return nil, err
	}
	if string(template.UID) != opts.BuildID {
		return nil, managererrors.NewError(managererrors.ErrorConflict, "template %q is bound to build %q", opts.Name, template.UID)
	}
	if template.Annotations[agentsv1alpha1.AnnotationTemplateBuildState] != agentsv1alpha1.TemplateBuildStateReserved {
		return nil, managererrors.NewError(managererrors.ErrorConflict, "template %q is not accepting a submission", opts.Name)
	}
	if reservationExpired(template, time.Now()) {
		return nil, managererrors.NewError(managererrors.ErrorNotFound, "template reservation %q has expired", opts.Name)
	}
	if template.Status.Phase == agentsv1alpha1.SandboxTemplatePhaseFailed {
		return nil, managererrors.NewError(managererrors.ErrorConflict, "template reservation %q has failed", opts.Name)
	}

	updated := template.DeepCopy()
	updated.Spec = *opts.Spec.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[agentsv1alpha1.AnnotationTemplateBuildState] = agentsv1alpha1.TemplateBuildStateSubmitted
	if err := store.Update(ctx, updated); err != nil {
		if !apierrors.IsConflict(err) {
			return nil, templateStorageError("submit SandboxTemplate", err)
		}
		latest, getErr := store.GetFresh(ctx, opts.Namespace, opts.Name)
		if getErr != nil {
			if apierrors.IsNotFound(getErr) {
				return nil, managererrors.NewError(managererrors.ErrorNotFound, "template reservation %q not found", opts.Name)
			}
			return nil, templateStorageError("resolve concurrent Template submission", getErr)
		}
		if string(latest.UID) != opts.BuildID {
			return nil, managererrors.NewError(managererrors.ErrorConflict, "template %q is bound to build %q", opts.Name, latest.UID)
		}
		return nil, templateStorageError("submit SandboxTemplate", err)
	}
	return templateToDetails(updated), nil
}

// GetTemplate returns one logical Template by name.
func (m *SandboxManager) GetTemplate(ctx context.Context, namespace, name string) (*TemplateDetails, error) {
	if err := validateTemplateIdentity(namespace, name); err != nil {
		return nil, err
	}
	store, err := m.templateStoreOrErr()
	if err != nil {
		return nil, err
	}
	template, err := readTemplateAfterWrite(ctx, store, namespace, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, managererrors.NewError(managererrors.ErrorNotFound, "template %q not found", name)
		}
		return nil, err
	}
	state, logical := templateBuildState(template)
	if !logical || (state == agentsv1alpha1.TemplateBuildStateReserved && reservationExpired(template, time.Now())) {
		return nil, managererrors.NewError(managererrors.ErrorNotFound, "template %q not found", name)
	}
	return templateToDetails(template), nil
}

func (m *SandboxManager) templateStoreOrErr() (TemplateStore, error) {
	if m.templateStore == nil {
		return nil, managererrors.NewError(managererrors.ErrorUnavailable, "logical Template storage is unavailable")
	}
	return m.templateStore, nil
}

func (m *SandboxManager) resolveTemplateNameForWrite(
	ctx context.Context,
	store TemplateStore,
	namespace, name string,
) (*agentsv1alpha1.SandboxTemplate, error) {
	existing, err := store.Get(ctx, namespace, name)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, templateStorageError("get SandboxTemplate", err)
	}
	latest, reclaimed, err := m.reclaimExpiredReservation(ctx, store, existing)
	if err != nil {
		return nil, err
	}
	if reclaimed {
		return nil, nil
	}
	return latest, nil
}

// readTemplateAfterWrite resolves a read that may follow this process's own
// write. The informer cache may briefly miss a reservation the caller just
// created, or still show it as reserved after a completed submission, so a
// cached read that reports the template as missing or as an expired
// reservation is confirmed against the API server before the caller acts.
func readTemplateAfterWrite(ctx context.Context, store TemplateStore, namespace, name string) (*agentsv1alpha1.SandboxTemplate, error) {
	template, err := store.Get(ctx, namespace, name)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, templateStorageError("get SandboxTemplate", err)
	}
	if err == nil && !reclaimableReservation(template, time.Now()) {
		return template, nil
	}
	fresh, err := store.GetFresh(ctx, namespace, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, err
		}
		return nil, templateStorageError("resolve SandboxTemplate", err)
	}
	return fresh, nil
}

func validateTemplateIdentity(namespace, name string) error {
	if namespace == "" {
		return managererrors.NewError(managererrors.ErrorBadRequest, "namespace is required")
	}
	if name == "" {
		return managererrors.NewError(managererrors.ErrorBadRequest, "template name is required")
	}
	if name == "tags" {
		return managererrors.NewError(managererrors.ErrorBadRequest, "template name %q is reserved", name)
	}
	return nil
}

// reclaimExpiredReservation deletes an expired reservation so its name can be
// taken over. The cached snapshot only nominates the object for reclaim; the
// delete runs against a fresh read so the UID and resourceVersion
// preconditions target the current object, and a fresh state that is not
// reclaimable replaces the stale view the caller evaluates.
func (m *SandboxManager) reclaimExpiredReservation(ctx context.Context, store TemplateStore, existing *agentsv1alpha1.SandboxTemplate) (*agentsv1alpha1.SandboxTemplate, bool, error) {
	if !reclaimableReservation(existing, time.Now()) {
		return existing, false, nil
	}
	latest, err := store.GetFresh(ctx, existing.Namespace, existing.Name)
	if apierrors.IsNotFound(err) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, templateStorageError("reclaim expired Template reservation", err)
	}
	if latest.UID != existing.UID || !reclaimableReservation(latest, time.Now()) {
		return latest, false, nil
	}
	if err := store.Delete(ctx, latest.Namespace, latest.Name, latest.UID, latest.ResourceVersion); err != nil && !apierrors.IsNotFound(err) {
		return nil, false, templateStorageError("reclaim expired Template reservation", err)
	}
	confirmed, err := store.GetFresh(ctx, latest.Namespace, latest.Name)
	if apierrors.IsNotFound(err) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, templateStorageError("confirm expired Template reservation deletion", err)
	}
	if confirmed.UID != latest.UID {
		return nil, false, managererrors.NewError(managererrors.ErrorUnavailable,
			"template name %q was rebound while reclaiming an expired reservation; retry", existing.Name)
	}
	return nil, false, managererrors.NewError(managererrors.ErrorUnavailable,
		"expired template reservation %q is still terminating; retry", existing.Name)
}

func templateBuildState(template *agentsv1alpha1.SandboxTemplate) (string, bool) {
	if len(template.OwnerReferences) != 0 || template.Annotations == nil {
		return "", false
	}
	state, ok := template.Annotations[agentsv1alpha1.AnnotationTemplateBuildState]
	return state, ok
}

func reclaimableReservation(template *agentsv1alpha1.SandboxTemplate, now time.Time) bool {
	state, logical := templateBuildState(template)
	return logical && state == agentsv1alpha1.TemplateBuildStateReserved && reservationExpired(template, now)
}

func reservationExpired(template *agentsv1alpha1.SandboxTemplate, now time.Time) bool {
	if !now.Before(template.CreationTimestamp.Add(agentsv1alpha1.SandboxTemplateBuildReservationTTL)) {
		return true
	}
	condition := apiMeta.FindStatusCondition(template.Status.Conditions, agentsv1alpha1.SandboxTemplateBuildConditionReady)
	return template.Status.Phase == agentsv1alpha1.SandboxTemplatePhaseFailed && condition != nil &&
		condition.Reason == agentsv1alpha1.SandboxTemplateBuildReasonReservationExpired
}

func reservedTemplate(opts ReserveTemplateOptions, backend string) *agentsv1alpha1.SandboxTemplate {
	cpu := resource.MustParse(strconv.FormatInt(int64(opts.CPUCount), 10))
	memory := resource.MustParse(fmt.Sprintf("%dMi", opts.MemoryMB))
	resources := corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: memory}
	annotations := map[string]string{agentsv1alpha1.AnnotationTemplateBuildState: agentsv1alpha1.TemplateBuildStateReserved}
	return &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: opts.Namespace, Name: opts.Name, Annotations: annotations},
		Spec: agentsv1alpha1.SandboxTemplateSpec{
			Backend: backend,
			Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:      "runtime",
				Resources: corev1.ResourceRequirements{Requests: resources.DeepCopy(), Limits: resources.DeepCopy()},
			}}}},
		},
	}
}

func templateToDetails(template *agentsv1alpha1.SandboxTemplate) *TemplateDetails {
	status := *template.Status.DeepCopy()
	if status.Phase == "" {
		status.Phase = agentsv1alpha1.SandboxTemplatePhasePending
	}
	return &TemplateDetails{
		Name:              template.Name,
		BuildID:           string(template.UID),
		CreationTimestamp: template.CreationTimestamp,
		Spec:              *template.Spec.DeepCopy(),
		Status:            status,
	}
}

func templateStorageError(operation string, err error) error {
	switch {
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err):
		return managererrors.WrapError(managererrors.ErrorConflict, err, "%s: %v", operation, err)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return managererrors.WrapError(managererrors.ErrorBadRequest, err, "%s: %v", operation, err)
	case apierrors.IsNotFound(err):
		return managererrors.WrapError(managererrors.ErrorNotFound, err, "%s: %v", operation, err)
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), apierrors.IsTooManyRequests(err), apierrors.IsServiceUnavailable(err):
		return managererrors.WrapError(managererrors.ErrorUnavailable, err, "%s: %v", operation, err)
	default:
		return managererrors.WrapError(managererrors.ErrorInternal, err, "%s: %v", operation, err)
	}
}
