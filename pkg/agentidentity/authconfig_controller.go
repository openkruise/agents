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

package agentidentity

import (
	"context"
	"fmt"
	"reflect"
	"time"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
	"github.com/openkruise/agents/pkg/discovery"
	"github.com/openkruise/agents/pkg/identity/oidc"
)

var authConfigKind = securityv1alpha1.GroupVersion.WithKind("AgentAuthenticationConfig")

// discoveryRetryBackoff is how long to wait before retrying an issuer that
// could not be reached.
//
// An unreachable issuer is usually an outage or a typo, neither of which is
// fixed by retrying hard. Spacing the attempts also keeps a namespace from
// turning a broken discovery URL into sustained outbound traffic.
const discoveryRetryBackoff = time.Minute

// Reasons reported on an AgentAuthenticationConfig's Ready condition.
const (
	// reasonDiscovered means the issuer was resolved and recorded.
	reasonDiscovered = "Discovered"
	// reasonInvalidSpec means the spec cannot be acted on at all, so retrying
	// would change nothing until it is edited.
	reasonInvalidSpec = "InvalidSpec"
	// reasonDiscoveryFailed means the discovery document could not be fetched or
	// did not describe a usable issuer.
	reasonDiscoveryFailed = "DiscoveryFailed"
)

// AuthConfigReconciler resolves an AgentAuthenticationConfig's issuer.
//
// The issuer identifier and key set location are deliberately not spec fields.
// This controller reads them from the discovery document and records them in
// status, so the only thing an operator chooses is the discovery URL. If issuer
// were writable, someone with edit rights in a namespace could keep a trusted
// issuer name while pointing the key set at keys they control, and tokens they
// signed themselves would verify.
type AuthConfigReconciler struct {
	client.Client

	// discover is overridden in tests. Reconcile makes a real outbound request
	// through it, which is why it is the one seam in this controller.
	discover func(ctx context.Context, discoveryURL string, opts oidc.Options) (oidc.Discovered, error)
}

// AddAuthConfig registers the AgentAuthenticationConfig controller.
//
// Like the AgentIdentity controller, this is a no-op when the CRD is not
// installed, so a cluster that has not opted into the identity path does not
// run it.
func AddAuthConfig(mgr ctrl.Manager) error {
	if !discovery.DiscoverGVK(authConfigKind) {
		return nil
	}
	return (&AuthConfigReconciler{
		Client:   mgr.GetClient(),
		discover: oidc.Discover,
	}).SetupWithManager(mgr)
}

func (r *AuthConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	config := &securityv1alpha1.AgentAuthenticationConfig{}
	if err := r.Get(ctx, req.NamespacedName, config); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Nothing may start trusting an issuer that is on its way out, and the
	// identities referring to it fail closed on their own reconcile.
	if !config.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	ready, resolved := r.resolve(ctx, config)

	original := config.DeepCopy()
	config.Status.ObservedGeneration = config.Generation
	// Status is only overwritten on success. Keeping the last good values
	// through a transient outage would be misleading, so they are cleared
	// whenever the current generation is not resolved.
	config.Status.Issuer = resolved.Issuer
	config.Status.JWKSURI = resolved.JWKSURI
	apiMeta.SetStatusCondition(&config.Status.Conditions, ready)

	if !reflect.DeepEqual(original.Status, config.Status) {
		if err := client.IgnoreNotFound(r.Status().Patch(ctx, config, client.MergeFrom(original))); err != nil {
			return ctrl.Result{}, err
		}
		log.FromContext(ctx).Info("Updated AgentAuthenticationConfig readiness",
			"agentAuthenticationConfig", klog.KObj(config),
			"ready", ready.Status, "reason", ready.Reason, "issuer", resolved.Issuer)
	}

	// A resolved issuer is re-checked on the shared resync so a rotated
	// jwks_uri is picked up; a failure is retried sooner but still spaced.
	if ready.Status == metav1.ConditionTrue {
		return ctrl.Result{RequeueAfter: resyncInterval}, nil
	}
	if ready.Reason == reasonInvalidSpec {
		// Retrying cannot help: only an edit can, and that produces an event.
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: discoveryRetryBackoff}, nil
}

// resolve builds the Ready condition and the values to record alongside it.
//
// A failure is reported rather than returned, because an unreachable issuer is
// a fact about the config worth surfacing to whoever wrote it, not a controller
// error to retry silently. The requeue is what retries it.
func (r *AuthConfigReconciler) resolve(ctx context.Context,
	config *securityv1alpha1.AgentAuthenticationConfig) (metav1.Condition, oidc.Discovered) {
	condition := metav1.Condition{
		Type:               securityv1alpha1.ConditionReady,
		ObservedGeneration: config.Generation,
		Status:             metav1.ConditionFalse,
	}

	// Admission constrains type to jwt and requires the jwt block with it, but a
	// config created before those constraints can still be missing it.
	if config.Spec.JWT == nil || config.Spec.JWT.DiscoveryURL == "" {
		condition.Reason = reasonInvalidSpec
		condition.Message = "spec.jwt.discoveryUrl is not set, so there is no issuer to resolve"
		return condition, oidc.Discovered{}
	}

	resolved, err := r.discover(ctx, config.Spec.JWT.DiscoveryURL, oidc.Options{})
	if err != nil {
		condition.Reason = reasonDiscoveryFailed
		// The error carries the operator's own URL and the transport failure,
		// neither of which is sensitive, and it is the only way they can tell a
		// typo from an outage.
		condition.Message = fmt.Sprintf("resolving %s: %v", config.Spec.JWT.DiscoveryURL, err)
		return condition, oidc.Discovered{}
	}

	condition.Status = metav1.ConditionTrue
	condition.Reason = reasonDiscovered
	condition.Message = fmt.Sprintf("issuer %s resolved; %d audience(s) accepted",
		resolved.Issuer, len(config.Spec.JWT.AllowedAudience))
	return condition, resolved
}

func (r *AuthConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&securityv1alpha1.AgentAuthenticationConfig{}).
		Complete(r)
}
