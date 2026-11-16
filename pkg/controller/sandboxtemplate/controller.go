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
	"fmt"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/discovery"
	"github.com/openkruise/agents/pkg/templateconfig"
	"github.com/openkruise/agents/pkg/templatecreation"
	"github.com/openkruise/agents/pkg/templateindex"
	"github.com/openkruise/agents/pkg/templatevalidation"
)

// KeyReader limits approved live reads to known object keys; it cannot List.
type KeyReader interface {
	Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error
}

type Reconciler struct {
	client.Client
	Reader   KeyReader
	Scheme   *runtime.Scheme
	Recorder recorder.EventRecorder
}

const retryInterval = 2 * time.Second

// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch;update
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxtemplateclaims,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxtemplates,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxsets;poolautoscalers,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxtemplateclaims/finalizers;sandboxtemplates/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxtemplates/status,verbs=get;update;patch

func Add(m manager.Manager) error {
	if !discovery.DiscoverGVK(api.SandboxTemplateClaimControllerKind) {
		return nil
	}
	if err := m.GetFieldIndexer().IndexField(context.Background(), &api.SandboxTemplate{}, templateindex.ClaimOwner, templateindex.ClaimOwnerUID); err != nil {
		return err
	}
	r := &Reconciler{Client: m.GetClient(), Reader: m.GetAPIReader(), Scheme: m.GetScheme(), Recorder: m.GetEventRecorder("sandbox-template")}
	if err := ctrl.NewControllerManagedBy(m).Named("sandbox-template-version").For(&api.SandboxTemplate{}).
		Owns(&api.SandboxSet{}).Owns(&api.PoolAutoscaler{}).
		Watches(&api.SandboxTemplateClaim{}, handler.EnqueueRequestsFromMapFunc(r.versionsForClaim)).Complete(reconcile.Func(r.ReconcileVersion)); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(m).Named("sandbox-template-claim").For(&api.SandboxTemplateClaim{}).Owns(&api.SandboxTemplate{}).Complete(reconcile.Func(r.ReconcileClaim))
}
func (r *Reconciler) versionsForClaim(ctx context.Context, o client.Object) []reconcile.Request {
	list := &api.SandboxTemplateList{}
	if err := r.List(ctx, list, client.InNamespace(o.GetNamespace()), client.MatchingFields{templateindex.ClaimOwner: string(o.GetUID())}); err != nil {
		log.FromContext(ctx).Error(err, "list template versions for claim event")
		return nil
	}
	out := []reconcile.Request{}
	for _, v := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&v)})
	}
	return out
}
func (r *Reconciler) ReconcileClaim(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	c := &api.SandboxTemplateClaim{}
	if err := r.Get(ctx, req.NamespacedName, c); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if c.DeletionTimestamp == nil || !controllerutil.ContainsFinalizer(c, api.SandboxTemplateClaimCleanupFinalizer) {
		return ctrl.Result{}, nil
	}
	list := &api.SandboxTemplateList{}
	if err := r.List(ctx, list, client.InNamespace(c.Namespace), client.MatchingFields{templateindex.ClaimOwner: string(c.UID)}); err != nil {
		return ctrl.Result{}, err
	}
	versions := map[string]*api.SandboxTemplate{}
	for i := range list.Items {
		v := &list.Items[i]
		o := templateindex.ClaimOwnerReference(v)
		if o != nil && o.Name == c.Name && o.UID == c.UID {
			versions[v.Name] = v
		}
	}
	// Confirm the known allocation key when the cache misses its current owner.
	if id := c.Annotations[templatecreation.AllocationAnnotation]; id != "" {
		v := &api.SandboxTemplate{}
		key := client.ObjectKey{Namespace: c.Namespace, Name: id}
		err := r.Get(ctx, key, v)
		cachedOwner := templateindex.ClaimOwnerReference(v)
		if apierrors.IsNotFound(err) || err == nil && (cachedOwner == nil || cachedOwner.UID != c.UID || cachedOwner.Name != c.Name) {
			err = r.Reader.Get(ctx, key, v)
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err == nil {
			o := templateindex.ClaimOwnerReference(v)
			if o != nil && o.Name == c.Name && o.UID == c.UID {
				versions[id] = v
			}
		}
	}
	for _, v := range versions {
		if v.DeletionTimestamp == nil {
			err := r.Delete(ctx, v, client.Preconditions{UID: &v.UID, ResourceVersion: &v.ResourceVersion}, client.PropagationPolicy(metav1.DeletePropagationForeground))
			if apierrors.IsNotFound(err) {
				continue
			}
			if apierrors.IsConflict(err) {
				return ctrl.Result{RequeueAfter: retryInterval}, nil
			}
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: retryInterval}, nil
	}
	// Keep the name reserved while an ambiguous bounded version Create can finish.
	if c.Annotations[templatecreation.CreationCompleteAnnotation] != "true" && !c.CreationTimestamp.IsZero() {
		remaining := time.Until(c.CreationTimestamp.Add(templatecreation.EmptyCreationGrace))
		if remaining > 0 {
			return ctrl.Result{RequeueAfter: remaining}, nil
		}
	}
	before := c.DeepCopy()
	controllerutil.RemoveFinalizer(c, api.SandboxTemplateClaimCleanupFinalizer)
	return ctrl.Result{}, r.Patch(ctx, c, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
func (r *Reconciler) ReconcileVersion(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	v := &api.SandboxTemplate{}
	if err := r.Get(ctx, req.NamespacedName, v); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	o := templateindex.ClaimOwnerReference(v)
	if o == nil || v.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}
	c := &api.SandboxTemplateClaim{}
	key := client.ObjectKey{Namespace: v.Namespace, Name: o.Name}
	err := r.Get(ctx, key, c)
	if apierrors.IsNotFound(err) || err == nil && c.UID != o.UID {
		err = r.Reader.Get(ctx, key, c)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if apierrors.IsNotFound(err) || c.UID != o.UID || c.DeletionTimestamp != nil {
		err := r.Delete(ctx, v, client.Preconditions{UID: &v.UID, ResourceVersion: &v.ResourceVersion}, client.PropagationPolicy(metav1.DeletePropagationForeground))
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if templatecreation.DefinitionPhase(v) == "" {
		old := v.DeepCopy()
		_, errs := templatevalidation.NormalizeCustomSpec(&v.Spec, field.NewPath("spec"))
		phase, condition, reason, message := api.SandboxTemplatePhaseReady, metav1.ConditionTrue, templatecreation.DefinitionValidReason, "Template definition validation passed."
		if len(errs) > 0 {
			phase, condition, reason, message = api.SandboxTemplatePhaseFailed, metav1.ConditionFalse, templatecreation.DefinitionInvalidReason, "Template definition validation failed."
		}
		v.Status.Phase = phase
		v.Status.ObservedGeneration = v.Generation
		meta.SetStatusCondition(&v.Status.Conditions, metav1.Condition{Type: templatecreation.DefinitionCondition, Status: condition, ObservedGeneration: v.Generation, Reason: reason, Message: message})
		if err := r.Status().Patch(ctx, v, client.MergeFromWithOptions(old, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	if v.Status.Phase != api.SandboxTemplatePhaseReady {
		return ctrl.Result{}, nil
	}
	if err := r.ensureCompanions(ctx, v); err != nil {
		if r.Recorder != nil {
			r.Recorder.Eventf(v, nil, corev1.EventTypeWarning, "CompanionReconcileFailed", "Reconcile", "Companion reconciliation failed; inspect controller logs.")
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}
func ownedByVersion(o client.Object, v *api.SandboxTemplate) bool {
	ref := templateindex.VersionOwnerReference(o)
	return o.GetNamespace() == v.Namespace && ref != nil && ref.Name == v.Name && ref.UID == v.UID
}
func (r *Reconciler) ensurePool(ctx context.Context, v *api.SandboxTemplate, warm *api.SandboxTemplateWarmPoolOptions) error {
	expected := &api.SandboxTemplateRef{Name: v.Name, Kind: ptr.To("SandboxTemplate"), APIVersion: ptr.To(api.GroupVersion.String())}
	p := &api.SandboxSet{}
	err := r.Get(ctx, client.ObjectKey{Namespace: v.Namespace, Name: v.Name}, p)
	if apierrors.IsNotFound(err) {
		metadata := templateconfig.UserMetadata(v.Labels, v.Annotations)
		p = &api.SandboxSet{ObjectMeta: metav1.ObjectMeta{Namespace: v.Namespace, Name: v.Name, Labels: metadata.Labels, Annotations: metadata.Annotations}, Spec: api.SandboxSetSpec{Replicas: ptr.Deref(warm.Size, 0)}}
		p.Spec.TemplateRef = expected
		if err := controllerutil.SetControllerReference(v, p, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, p)
	}
	if err != nil {
		return err
	}
	if !ownedByVersion(p, v) {
		return fmt.Errorf("companion name occupied by an external sandboxset")
	}
	if p.DeletionTimestamp != nil {
		return fmt.Errorf("companion pool is being deleted")
	}
	if p.Spec.Template != nil || !equality.Semantic.DeepEqual(p.Spec.TemplateRef, expected) {
		return fmt.Errorf("managed pool definition reference differs from its owner")
	}
	before := p.DeepCopy()
	metadata := templateconfig.UserMetadata(v.Labels, v.Annotations)
	// Restore declared business values without deleting unrelated admission or
	// controller keys. Immutable API definitions have no user key removals.
	if len(metadata.Labels) > 0 {
		if p.Labels == nil {
			p.Labels = map[string]string{}
		}
		maps.Copy(p.Labels, metadata.Labels)
	}
	if len(metadata.Annotations) > 0 {
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		maps.Copy(p.Annotations, metadata.Annotations)
	}
	if warm.Autoscaler == nil && p.Spec.Replicas != ptr.Deref(warm.Size, 0) {
		p.Spec.Replicas = ptr.Deref(warm.Size, 0)
	}
	if !equality.Semantic.DeepEqual(p.Labels, before.Labels) || !equality.Semantic.DeepEqual(p.Annotations, before.Annotations) || p.Spec.Replicas != before.Spec.Replicas {
		return r.Patch(ctx, p, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	return nil
}
func (r *Reconciler) ensureCompanions(ctx context.Context, v *api.SandboxTemplate) error {
	if v.Spec.BuildOptions == nil || v.Spec.BuildOptions.WarmPool == nil {
		return nil
	}
	warm := v.Spec.BuildOptions.WarmPool
	if !ptr.Deref(warm.Enabled, true) {
		return nil
	}
	if err := r.ensurePool(ctx, v, warm); err != nil {
		return err
	}
	if warm.Autoscaler == nil {
		return nil
	}
	opts := warm.Autoscaler.DeepCopy()
	expected := api.PoolAutoscalerSpec{
		ScaleTargetRef: api.CrossVersionObjectReference{APIVersion: api.GroupVersion.String(), Kind: "SandboxSet", Name: v.Name},
		MinReplicas:    opts.MinReplicas, MaxReplicas: opts.MaxReplicas,
		CapacityPolicy: opts.CapacityPolicy, CronPolicies: opts.CronPolicies,
	}
	pa := &api.PoolAutoscaler{}
	err := r.Get(ctx, client.ObjectKey{Namespace: v.Namespace, Name: v.Name}, pa)
	if apierrors.IsNotFound(err) {
		pa = &api.PoolAutoscaler{ObjectMeta: metav1.ObjectMeta{Namespace: v.Namespace, Name: v.Name}, Spec: expected}
		if err := controllerutil.SetControllerReference(v, pa, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, pa)
	}
	if err != nil {
		return err
	}
	if !ownedByVersion(pa, v) {
		return fmt.Errorf("autoscaler name is occupied by a different owner")
	}
	if pa.DeletionTimestamp != nil {
		return fmt.Errorf("companion autoscaler is being deleted")
	}
	if !equality.Semantic.DeepEqual(pa.Spec, expected) {
		before := pa.DeepCopy()
		pa.Spec = expected
		return r.Patch(ctx, pa, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	return nil
}
