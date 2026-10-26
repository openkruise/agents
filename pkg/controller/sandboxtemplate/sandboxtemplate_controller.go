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

// Package sandboxtemplate converges logical SandboxTemplate builds.
package sandboxtemplate

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/discovery"
)

var controllerKind = agentsv1alpha1.SandboxTemplateControllerKind

const (
	controllerName    = "sandboxtemplate-controller"
	conditionReady    = agentsv1alpha1.SandboxTemplateBuildConditionReady
	reasonReserved    = "BuildReserved"
	reasonReady       = "Ready"
	reasonUnexecuted  = "CommandsNotExecuted"
	reasonInvalid     = "ValidationFailed"
	reasonExpired     = agentsv1alpha1.SandboxTemplateBuildReasonReservationExpired
	reasonUnsupported = "UnsupportedBackend"

	commandsNotExecutedMessage = "build commands were accepted but were not executed in Phase 1"
)

// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxtemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxtemplates/status,verbs=get;update;patch

// Add creates the SandboxTemplate controller and adds it to the manager.
func Add(mgr manager.Manager) error {
	if !discovery.DiscoverGVK(controllerKind) {
		return nil
	}
	r := &Reconciler{
		Client: mgr.GetClient(),
		now:    time.Now,
	}
	return r.SetupWithManager(mgr)
}

// Reconciler converges logical SandboxTemplate builds and owns their status.
type Reconciler struct {
	client.Client
	now func() time.Time
}

// Reconcile implements the Kubernetes reconciliation loop.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	template := &agentsv1alpha1.SandboxTemplate{}
	if err := r.Get(ctx, req.NamespacedName, template); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !isLogicalTemplate(template) || !template.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if template.Status.Phase == agentsv1alpha1.SandboxTemplatePhaseFailed {
		return ctrl.Result{}, nil
	}

	switch template.Annotations[agentsv1alpha1.AnnotationTemplateBuildState] {
	case agentsv1alpha1.TemplateBuildStateReserved:
		if _, exists := template.Annotations[agentsv1alpha1.AnnotationTemplateBuildInputDigest]; exists {
			return r.fail(ctx, template, reasonInvalid, "build-input-digest annotation must not be set before submission")
		}
		return r.reconcileReserved(ctx, template)
	case agentsv1alpha1.TemplateBuildStateSubmitted:
		if strings.TrimSpace(template.Annotations[agentsv1alpha1.AnnotationTemplateBuildInputDigest]) == "" {
			return r.fail(ctx, template, reasonInvalid, "build-input-digest annotation is required after submission")
		}
	default:
		return r.fail(ctx, template, reasonInvalid, "build-state annotation must be reserved or submitted")
	}

	backend := template.Spec.Backend
	if backend == "" {
		backend = agentsv1alpha1.SandboxTemplateBackendKubernetes
	}
	if backend != agentsv1alpha1.SandboxTemplateBackendKubernetes {
		return r.fail(ctx, template, reasonUnsupported, fmt.Sprintf("backend %q is not supported by this controller", backend))
	}

	if err := validateKubernetesTemplate(template); err != nil {
		return r.fail(ctx, template, reasonInvalid, err.Error())
	}

	reason, message := reasonReady, "template build is ready"
	if hasBuildCommands(template.Spec.BuildOptions) {
		reason, message = reasonUnexecuted, commandsNotExecutedMessage
	}
	if err := r.patchStatus(ctx, template, agentsv1alpha1.SandboxTemplatePhaseReady, metav1.ConditionTrue, reason, message); err != nil {
		return ctrl.Result{}, err
	}
	log.V(4).Info("template build is ready")
	return ctrl.Result{}, nil
}

func isLogicalTemplate(template *agentsv1alpha1.SandboxTemplate) bool {
	if len(template.OwnerReferences) != 0 || template.Annotations == nil {
		return false
	}
	_, ok := template.Annotations[agentsv1alpha1.AnnotationTemplateBuildState]
	return ok
}

func (r *Reconciler) reconcileReserved(ctx context.Context, template *agentsv1alpha1.SandboxTemplate) (ctrl.Result, error) {
	expiresAt := template.CreationTimestamp.Add(agentsv1alpha1.SandboxTemplateBuildReservationTTL)
	remaining := expiresAt.Sub(r.now())
	if remaining <= 0 {
		return r.fail(ctx, template, reasonExpired, "reserved template build was not submitted before its reservation expired")
	}
	if err := r.patchStatus(ctx, template, agentsv1alpha1.SandboxTemplatePhasePending, metav1.ConditionUnknown,
		reasonReserved, "template build is reserved and waiting for submission"); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: remaining}, nil
}

func validateKubernetesTemplate(template *agentsv1alpha1.SandboxTemplate) error {
	if template.Spec.Template == nil {
		return fmt.Errorf("spec.template is required")
	}
	containers := template.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		return fmt.Errorf("spec.template.spec.containers must not be empty")
	}
	for i := range containers {
		if strings.TrimSpace(containers[i].Image) == "" {
			return fmt.Errorf("spec.template.spec.containers[%d].image is required", i)
		}
	}
	return nil
}

func hasBuildCommands(options *agentsv1alpha1.SandboxTemplateBuildOptions) bool {
	return options != nil && (len(options.Steps) > 0 || options.StartCommand != nil || options.ReadyCommand != nil)
}

func (r *Reconciler) fail(ctx context.Context, template *agentsv1alpha1.SandboxTemplate, reason, message string) (ctrl.Result, error) {
	if err := r.patchStatus(ctx, template, agentsv1alpha1.SandboxTemplatePhaseFailed, metav1.ConditionFalse, reason, message); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) patchStatus(
	ctx context.Context,
	template *agentsv1alpha1.SandboxTemplate,
	phase agentsv1alpha1.SandboxTemplateBuildPhase,
	conditionStatus metav1.ConditionStatus,
	reason, message string,
) error {
	original := template.DeepCopy()
	status := *template.Status.DeepCopy()
	status.ObservedGeneration = template.Generation
	status.Phase = phase
	apiMeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             conditionStatus,
		ObservedGeneration: template.Generation,
		LastTransitionTime: metav1.NewTime(r.now()),
		Reason:             reason,
		Message:            message,
	})
	if reflect.DeepEqual(template.Status, status) {
		return nil
	}
	template.Status = status
	if err := r.Status().Patch(ctx, template, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("patch SandboxTemplate status: %w", err)
	}
	return nil
}

func logicalTemplatePredicate() predicate.Predicate {
	isLogical := func(obj client.Object) bool {
		template, ok := obj.(*agentsv1alpha1.SandboxTemplate)
		return ok && isLogicalTemplate(template)
	}
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return isLogical(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return isLogical(e.ObjectOld) || isLogical(e.ObjectNew)
		},
		DeleteFunc: func(event.DeleteEvent) bool {
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return isLogical(e.Object)
		},
	}
}

// SetupWithManager sets up the controller with the manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(controllerName).
		For(&agentsv1alpha1.SandboxTemplate{}, builder.WithPredicates(logicalTemplatePredicate())).
		Complete(r)
}
