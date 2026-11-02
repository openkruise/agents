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
	"flag"
	"fmt"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
	"github.com/openkruise/agents/pkg/discovery"
)

var controllerKind = securityv1alpha1.GroupVersion.WithKind("AgentIdentity")

// agentIdentityWorkers is how many AgentIdentity objects reconcile at once.
//
// A reconcile here is cheap, all cache reads and at most one status patch, so
// the default is modest. It is configurable mainly because a namespace with
// many identities would otherwise queue them behind one worker whenever a
// referenced AgentAuthenticationConfig changes and fans out.
var agentIdentityWorkers = 3

func init() {
	flag.IntVar(&agentIdentityWorkers, "agentidentity-workers", agentIdentityWorkers,
		"Max concurrent workers for the AgentIdentity controller.")
}

// resyncInterval bounds how long a stale verdict can survive.
//
// The AgentAuthenticationConfig watch is a promptness optimisation, not the
// correctness mechanism: handler.EnqueueRequestsFromMapFunc cannot return an
// error, so a failed list inside the map function silently drops that event. If
// the dropped event was a config deletion or a transition out of Ready, a
// referring identity would otherwise hold Ready=True until an unrelated event
// or an informer resync happened to wake it. Re-reconciling on this interval
// gives convergence a floor that does not depend on any event being delivered.
const resyncInterval = 10 * time.Minute

// Reasons reported on the Ready condition. Reason values are part of what an
// operator reads off the resource, so they name the specific thing that is
// wrong rather than collapsing every failure into one string.
const (
	// reasonValidated means the identity is usable.
	reasonValidated = "Validated"
	// reasonNoAuthenticationRefs means the identity names no issuer. Admission
	// rejects this now, but an object created before that constraint existed can
	// still carry an empty list, and such an identity cannot take part in a
	// principal token exchange.
	reasonNoAuthenticationRefs = "NoAuthenticationRefs"
	// reasonAuthenticationConfigNotFound means a referenced
	// AgentAuthenticationConfig does not exist in the identity's namespace.
	// References are same-namespace by design, so a config that exists
	// elsewhere is still absent as far as this identity is concerned.
	reasonAuthenticationConfigNotFound = "AuthenticationConfigNotFound"
	// reasonAuthenticationConfigDeleting means a referenced
	// AgentAuthenticationConfig is being deleted. A finalizer can hold the object
	// in the API for an arbitrary time with its last Ready=True still visible, so
	// deletion is treated as unusable the moment it is requested rather than when
	// the object finally disappears.
	reasonAuthenticationConfigDeleting = "AuthenticationConfigDeleting"
	// reasonAuthenticationConfigStale means a referenced
	// AgentAuthenticationConfig has been edited and its controller has not yet
	// observed the change. Its Ready condition still describes the previous
	// generation, so it says nothing about whether the current spec is usable.
	reasonAuthenticationConfigStale = "AuthenticationConfigStale"
	// reasonAuthenticationConfigNotReady means a referenced
	// AgentAuthenticationConfig exists and is current but is not itself Ready, so
	// its issuer has not been resolved and cannot be trusted yet.
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

// This controller runs in agent-identity-provider, not in the shared
// agent-sandbox-controller, so its permissions are not part of controller-role.
// They are declared with the component in config/agent-identity-provider/rbac.yaml,
// the same way sandbox-manager declares its own.

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

	// Every success path requeues on resyncInterval, so a verdict is re-derived
	// on a bounded schedule whether or not the config watch delivered an event.
	original := identity.DeepCopy()
	identity.Status.ObservedGeneration = identity.Generation
	apiMeta.SetStatusCondition(&identity.Status.Conditions, ready)
	if reflect.DeepEqual(original.Status, identity.Status) {
		return ctrl.Result{RequeueAfter: resyncInterval}, nil
	}

	if err := client.IgnoreNotFound(r.Status().Patch(ctx, identity, client.MergeFrom(original))); err != nil {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).Info("Updated AgentIdentity readiness", "agentIdentity", klog.KObj(identity),
		"ready", ready.Status, "reason", ready.Reason)
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

// evaluateReady builds the Ready condition for the identity.
//
// An error return means the answer is unknown rather than negative, so the
// condition is left alone and the reconcile is retried. Writing Ready=False on
// a transient API failure would make an identity look broken when it is not.
func (r *Reconciler) evaluateReady(ctx context.Context, identity *securityv1alpha1.AgentIdentity) (metav1.Condition, error) {
	condition := metav1.Condition{
		Type:               securityv1alpha1.ConditionReady,
		ObservedGeneration: identity.Generation,
		Status:             metav1.ConditionFalse,
	}

	// Admission requires at least one reference, so an empty list here belongs to
	// an object that predates the constraint. It fails closed rather than being
	// read as "nothing to check, therefore fine".
	if len(identity.Spec.AuthenticationRefs) == 0 {
		condition.Reason = reasonNoAuthenticationRefs
		condition.Message = "authenticationRefs is empty, so no issuer can authenticate an end user for this identity"
		return condition, nil
	}

	for _, ref := range identity.Spec.AuthenticationRefs {
		config := &securityv1alpha1.AgentAuthenticationConfig{}
		key := client.ObjectKey{Namespace: identity.Namespace, Name: ref.Name}
		if err := r.Get(ctx, key, config); err != nil {
			if !apierrors.IsNotFound(err) {
				return metav1.Condition{}, err
			}
			condition.Reason = reasonAuthenticationConfigNotFound
			condition.Message = fmt.Sprintf("AgentAuthenticationConfig %q not found in namespace %q",
				ref.Name, identity.Namespace)
			return condition, nil
		}

		if reason, message := configUsability(config); reason != "" {
			condition.Reason = reason
			condition.Message = message
			return condition, nil
		}
	}

	condition.Status = metav1.ConditionTrue
	condition.Reason = reasonValidated
	condition.Message = fmt.Sprintf("identity is valid; %d referenced authentication sources are Ready",
		len(identity.Spec.AuthenticationRefs))
	return condition, nil
}

// configUsability reports why a referenced AgentAuthenticationConfig cannot be
// relied on, or empty strings when it can.
//
// A bare Ready=True is not enough. A config whose spec has changed keeps its
// previous Ready=True until its own controller catches up, and a config being
// deleted keeps its last status for as long as a finalizer holds it in the API.
// Ready is defined as a verdict on the current generation, so this checks that
// the verdict is current and that the object is not on its way out, rather than
// trusting the condition's status alone.
func configUsability(config *securityv1alpha1.AgentAuthenticationConfig) (reason, message string) {
	if !config.DeletionTimestamp.IsZero() {
		return reasonAuthenticationConfigDeleting,
			fmt.Sprintf("AgentAuthenticationConfig %q is being deleted", config.Name)
	}

	ready := apiMeta.FindStatusCondition(config.Status.Conditions, securityv1alpha1.ConditionReady)
	if ready == nil {
		return reasonAuthenticationConfigNotReady,
			fmt.Sprintf("AgentAuthenticationConfig %q has not reported readiness yet", config.Name)
	}

	// Both the condition and the status as a whole must describe the generation
	// in the spec; either one lagging means the verdict predates the current spec.
	if ready.ObservedGeneration != config.Generation || config.Status.ObservedGeneration != config.Generation {
		return reasonAuthenticationConfigStale,
			fmt.Sprintf("AgentAuthenticationConfig %q reports generation %d but its spec is at generation %d",
				config.Name, ready.ObservedGeneration, config.Generation)
	}

	if ready.Status != metav1.ConditionTrue {
		return reasonAuthenticationConfigNotReady,
			fmt.Sprintf("AgentAuthenticationConfig %q is not Ready", config.Name)
	}

	return "", ""
}

// identitiesForAuthConfig maps an AgentAuthenticationConfig to the identities
// that reference it, so an identity re-evaluates promptly when the config it
// depends on changes rather than waiting out resyncInterval.
//
// Deleting a config does not cascade: the referring identities are requeued and
// settle on Ready=False rather than being deleted with it.
//
// This is best-effort by construction. The signature cannot return an error, so
// a failed list drops the event with nowhere to report it; resyncInterval is
// what guarantees the identity converges anyway. Returning nil here therefore
// delays convergence rather than losing it.
func (r *Reconciler) identitiesForAuthConfig(ctx context.Context, obj client.Object) []reconcile.Request {
	identities := &securityv1alpha1.AgentIdentityList{}
	if err := r.List(ctx, identities, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list AgentIdentities for AgentAuthenticationConfig; "+
			"referring identities will converge on the resync instead",
			"agentAuthenticationConfig", klog.KObj(obj), "resyncInterval", resyncInterval)
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
		WithOptions(controller.Options{MaxConcurrentReconciles: agentIdentityWorkers}).
		For(&securityv1alpha1.AgentIdentity{}).
		Watches(&securityv1alpha1.AgentAuthenticationConfig{},
			handler.EnqueueRequestsFromMapFunc(r.identitiesForAuthConfig)).
		Complete(r)
}
