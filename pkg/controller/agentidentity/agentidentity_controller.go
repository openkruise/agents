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

// Package agentidentity reconciles AgentIdentity readiness.
//
// An AgentIdentity is a declaration, not a workload: there is nothing to create
// or scale for it. The only question this controller answers is whether the
// identity is usable, which is what the token issuance path checks before it
// mints a token for a sandbox that selected the identity by annotation. Nothing
// here issues or touches a token.
package agentidentity

import (
	"context"
	"fmt"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
	"github.com/openkruise/agents/pkg/discovery"
)

var controllerKind = securityv1alpha1.GroupVersion.WithKind("AgentIdentity")

// Reasons reported on the Ready condition. Reason values are part of what an
// operator reads off the resource, so they name the specific thing that is
// wrong rather than collapsing every failure into one string.
const (
	// reasonValidated means the identity is usable.
	reasonValidated = "Validated"
	// reasonAuthenticationConfigNotFound means a referenced
	// AgentAuthenticationConfig does not exist in the identity's namespace.
	// References are same-namespace by design, so a config that exists
	// elsewhere is still absent as far as this identity is concerned.
	reasonAuthenticationConfigNotFound = "AuthenticationConfigNotFound"
	// reasonAuthenticationConfigNotReady means a referenced
	// AgentAuthenticationConfig exists but is not itself Ready, so its issuer
	// has not been resolved and cannot be trusted yet.
	reasonAuthenticationConfigNotReady = "AuthenticationConfigNotReady"
)

// Reconciler reconciles AgentIdentity objects.
type Reconciler struct {
	client.Client
}

// Add registers the controller with the manager.
//
// The capability is opt-in at the cluster level: when the AgentIdentity CRD is
// not installed, Add is a no-op and the controller never starts. That keeps the
// identity path out of a deployment that has not asked for it without
// introducing a process-global feature switch.
func Add(mgr ctrl.Manager) error {
	if !discovery.DiscoverGVK(controllerKind) {
		return nil
	}
	return (&Reconciler{Client: mgr.GetClient()}).SetupWithManager(mgr)
}

// +kubebuilder:rbac:groups=security.agents.kruise.io,resources=agentidentities,verbs=get;list;watch
// +kubebuilder:rbac:groups=security.agents.kruise.io,resources=agentidentities/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=security.agents.kruise.io,resources=agentauthenticationconfigs,verbs=get;list;watch

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	identity := &securityv1alpha1.AgentIdentity{}
	if err := r.Get(ctx, req.NamespacedName, identity); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A deleting identity is not worth a status write: the object is on its way
	// out and nothing may assume it again. Referring resources learn about it
	// from their own reconcile, not from this one.
	if !identity.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	ready, err := r.evaluateReady(ctx, identity)
	if err != nil {
		return ctrl.Result{}, err
	}

	original := identity.DeepCopy()
	identity.Status.ObservedGeneration = identity.Generation
	apiMeta.SetStatusCondition(&identity.Status.Conditions, ready)
	if reflect.DeepEqual(original.Status, identity.Status) {
		return ctrl.Result{}, nil
	}

	if err := client.IgnoreNotFound(r.Status().Patch(ctx, identity, client.MergeFrom(original))); err != nil {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).Info("Updated AgentIdentity readiness", "agentIdentity", klog.KObj(identity),
		"ready", ready.Status, "reason", ready.Reason)
	return ctrl.Result{}, nil
}

// evaluateReady builds the Ready condition for the identity.
//
// An identity with no authenticationRefs is Ready. Those references are what an
// end user is authenticated against when a principal token is exchanged, and an
// identity that only ever proves the workload has no end user to authenticate,
// so requiring one would make the common case unusable.
//
// An error return means the answer is unknown rather than negative, so the
// condition is left alone and the reconcile is retried. Writing Ready=False on
// a transient API failure would make an identity look broken when it is not.
func (r *Reconciler) evaluateReady(ctx context.Context, identity *securityv1alpha1.AgentIdentity) (metav1.Condition, error) {
	condition := metav1.Condition{
		Type:               securityv1alpha1.ConditionReady,
		ObservedGeneration: identity.Generation,
	}

	for _, ref := range identity.Spec.AuthenticationRefs {
		config := &securityv1alpha1.AgentAuthenticationConfig{}
		key := client.ObjectKey{Namespace: identity.Namespace, Name: ref.Name}
		if err := r.Get(ctx, key, config); err != nil {
			if !apierrors.IsNotFound(err) {
				return metav1.Condition{}, err
			}
			condition.Status = metav1.ConditionFalse
			condition.Reason = reasonAuthenticationConfigNotFound
			condition.Message = fmt.Sprintf("AgentAuthenticationConfig %q not found in namespace %q",
				ref.Name, identity.Namespace)
			return condition, nil
		}

		if !apiMeta.IsStatusConditionTrue(config.Status.Conditions, securityv1alpha1.ConditionReady) {
			condition.Status = metav1.ConditionFalse
			condition.Reason = reasonAuthenticationConfigNotReady
			condition.Message = fmt.Sprintf("AgentAuthenticationConfig %q is not Ready", ref.Name)
			return condition, nil
		}
	}

	condition.Status = metav1.ConditionTrue
	condition.Reason = reasonValidated
	condition.Message = fmt.Sprintf("identity is valid; %d referenced authentication sources are Ready",
		len(identity.Spec.AuthenticationRefs))
	return condition, nil
}

// identitiesForAuthConfig maps an AgentAuthenticationConfig to the identities
// that reference it, so an identity re-evaluates when the config it depends on
// becomes Ready or stops being Ready. Without this the identity would keep a
// stale verdict until something else happened to touch it.
//
// Deleting a config does not cascade: the referring identities are requeued and
// settle on Ready=False rather than being deleted with it.
func (r *Reconciler) identitiesForAuthConfig(ctx context.Context, obj client.Object) []reconcile.Request {
	identities := &securityv1alpha1.AgentIdentityList{}
	if err := r.List(ctx, identities, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list AgentIdentities for AgentAuthenticationConfig",
			"agentAuthenticationConfig", klog.KObj(obj))
		return nil
	}

	var requests []reconcile.Request
	for i := range identities.Items {
		identity := &identities.Items[i]
		for _, ref := range identity.Spec.AuthenticationRefs {
			if ref.Name == obj.GetName() {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(identity),
				})
				break
			}
		}
	}
	return requests
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&securityv1alpha1.AgentIdentity{}).
		Watches(&securityv1alpha1.AgentAuthenticationConfig{},
			handler.EnqueueRequestsFromMapFunc(r.identitiesForAuthConfig)).
		Complete(r)
}
