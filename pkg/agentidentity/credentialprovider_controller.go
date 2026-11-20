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
	"flag"
	"fmt"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

var credentialProviderKind = securityv1alpha1.GroupVersion.WithKind("CredentialProvider")

// credentialProviderWorkers is how many CredentialProviders reconcile at once.
//
// A reconcile here is cheap, a cache read and at most one status patch, so the
// default is modest. It is configurable mainly because a Secret shared by many
// providers fans out to all of them at once.
var credentialProviderWorkers = 3

func init() {
	flag.IntVar(&credentialProviderWorkers, "credentialprovider-workers", credentialProviderWorkers,
		"Max concurrent workers for the CredentialProvider controller.")
}

// Reasons reported on a CredentialProvider's Ready condition.
const (
	// reasonProviderReady means the provider can be used to start an
	// authorization flow.
	reasonProviderReady = "Ready"
	// reasonProviderInvalidSpec means the spec cannot be acted on at all, so
	// nothing but an edit will change the outcome.
	reasonProviderInvalidSpec = "InvalidSpec"
	// reasonClientSecretNotFound means the referenced Secret does not exist in
	// the provider's namespace. References are same-namespace by design, so a
	// Secret of that name elsewhere is still absent as far as this provider is
	// concerned.
	reasonClientSecretNotFound = "ClientSecretNotFound"
	// reasonClientSecretKeyNotFound means the Secret exists but does not carry
	// the named key, or carries it empty. Both are reported the same way
	// because the fix is the same and neither is usable.
	reasonClientSecretKeyNotFound = "ClientSecretKeyNotFound"
)

// CredentialProviderReconciler derives a provider's callback URL and reports
// whether its client secret is present.
//
// The callback URL is status rather than spec because it has to agree with what
// was registered with the third party, and a value an operator could write
// independently of callbackBaseUrl would be a second source of truth for the
// same thing.
type CredentialProviderReconciler struct {
	client.Client
}

// AddCredentialProvider registers the CredentialProvider controller.
//
// Like the other controllers here this is a no-op when the CRD is not
// installed, so a cluster that has not opted into the credential path does not
// run it.
func AddCredentialProvider(mgr ctrl.Manager) error {
	if !discovery.DiscoverGVK(credentialProviderKind) {
		return nil
	}
	return (&CredentialProviderReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr)
}

func (r *CredentialProviderReconciler) Reconcile(ctx context.Context,
	req ctrl.Request) (ctrl.Result, error) {
	provider := &securityv1alpha1.CredentialProvider{}
	if err := r.Get(ctx, req.NamespacedName, provider); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Nothing may start an authorization flow against a provider on its way
	// out, and a flow already in progress will fail its own checks.
	if !provider.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	ready, callbackURL, err := r.evaluate(ctx, provider)
	if err != nil {
		// The decision could not be made. Returning the error retries it and
		// leaves the previous verdict in place, which is right: a read failure
		// is not evidence about the provider either way.
		return ctrl.Result{}, err
	}

	original := provider.DeepCopy()
	provider.Status.ObservedGeneration = provider.Generation
	// The callback URL is cleared whenever the provider is not usable. Leaving
	// the last good value visible would invite someone to register a redirect
	// URI for a provider that cannot complete the exchange behind it.
	provider.Status.CallbackURL = callbackURL
	apiMeta.SetStatusCondition(&provider.Status.Conditions, ready)

	if !reflect.DeepEqual(original.Status, provider.Status) {
		if err := client.IgnoreNotFound(r.Status().Patch(ctx, provider, client.MergeFrom(original))); err != nil {
			return ctrl.Result{}, err
		}
		log.FromContext(ctx).Info("Updated CredentialProvider readiness",
			"credentialProvider", klog.KObj(provider),
			"ready", ready.Status, "reason", ready.Reason, "callbackUrl", callbackURL)
	}
	return ctrl.Result{}, nil
}

// evaluate builds the Ready condition and the callback URL to record with it.
//
// The client secret is read to confirm it is there and not empty, and its value
// is never returned, logged or placed in status. Section 4 forbids a client
// secret from reaching any of those, and a readiness check only needs to know
// whether the operator has supplied one.
func (r *CredentialProviderReconciler) evaluate(ctx context.Context,
	provider *securityv1alpha1.CredentialProvider) (metav1.Condition, string, error) {
	condition := metav1.Condition{
		Type:               securityv1alpha1.ConditionReady,
		ObservedGeneration: provider.Generation,
		Status:             metav1.ConditionFalse,
	}

	// Admission requires the oauth2 block for type oauth2 and the
	// userFederation block for that grant type, but a provider created before
	// those rules existed can still be missing either, and the consumer fails
	// closed for what came before.
	oauth2 := provider.Spec.OAuth2
	if oauth2 == nil {
		condition.Reason = reasonProviderInvalidSpec
		condition.Message = "spec.oauth2 is not set, so there is no client to authorize against"
		return condition, "", nil
	}
	if oauth2.UserFederation == nil {
		condition.Reason = reasonProviderInvalidSpec
		condition.Message = fmt.Sprintf(
			"spec.oauth2.userFederation is not set, which grant type %q requires", oauth2.GrantType)
		return condition, "", nil
	}
	if oauth2.UserFederation.CallbackBaseURL == "" {
		condition.Reason = reasonProviderInvalidSpec
		condition.Message = "spec.oauth2.userFederation.callbackBaseUrl is not set, so no callback URL can be derived"
		return condition, "", nil
	}

	selector := oauth2.Client.ClientSecret.SecretKeyRef
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: provider.Namespace, Name: selector.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		if !apierrors.IsNotFound(err) {
			// A read failure is not a statement about the provider. Reporting
			// it as ClientSecretNotFound would tell an operator the Secret is
			// missing when it may be sitting there untouched, so it is
			// returned to be retried instead.
			return metav1.Condition{}, "", fmt.Errorf(
				"read client secret %s/%s: %w", provider.Namespace, selector.Name, err)
		}
		condition.Reason = reasonClientSecretNotFound
		condition.Message = fmt.Sprintf(
			"Secret %q does not exist in namespace %s", selector.Name, provider.Namespace)
		return condition, "", nil
	}
	// Only the length is looked at. The value itself must not reach a
	// condition message, a log line or status.
	if len(secret.Data[selector.Key]) == 0 {
		condition.Reason = reasonClientSecretKeyNotFound
		condition.Message = fmt.Sprintf(
			"Secret %q has no non-empty key %q", selector.Name, selector.Key)
		return condition, "", nil
	}

	callbackURL := CallbackURLFor(oauth2.UserFederation.CallbackBaseURL)
	condition.Status = metav1.ConditionTrue
	condition.Reason = reasonProviderReady
	condition.Message = fmt.Sprintf("client secret present; callback URL is %s", callbackURL)
	return condition, callbackURL, nil
}

// CallbackURLFor appends the fixed callback path to an operator's base URL.
//
// The trailing slash is removed first. A base written as "https://host/" would
// otherwise produce "https://host//oauth2/callback", and a redirect URI has to
// match the third-party app registration byte for byte, so the doubled slash
// would fail the exchange rather than merely look untidy. Admission permits
// both spellings, which is why normalising is the controller's job.
//
// Nothing else is normalised. Case, percent-encoding and default ports are left
// as the operator wrote them, because the registered redirect URI is whatever
// they typed into the third party and this value has to match that, not a
// canonical form of it.
func CallbackURLFor(callbackBaseURL string) string {
	return strings.TrimSuffix(callbackBaseURL, "/") + securityv1alpha1.CallbackPath
}

func (r *CredentialProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: credentialProviderWorkers}).
		For(&securityv1alpha1.CredentialProvider{}).
		// A provider blocked on a missing Secret has to recover when the Secret
		// is created, and nothing about the provider changes at that moment, so
		// without this watch it would stay NotReady until an unrelated event or
		// an informer resync happened to wake it.
		Watches(&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.providersForSecret)).
		Complete(r)
}

// providersForSecret returns the providers in a Secret's namespace that name
// it as their client secret.
//
// The map function cannot return an error, so a failed list silently drops the
// event. That is tolerable here because the only cost is a provider staying
// NotReady until the next event or resync, and it is why readiness is
// recomputed from scratch on every reconcile rather than trusted from status.
func (r *CredentialProviderReconciler) providersForSecret(ctx context.Context,
	object client.Object) []reconcile.Request {
	providers := &securityv1alpha1.CredentialProviderList{}
	if err := r.List(ctx, providers, client.InNamespace(object.GetNamespace())); err != nil {
		log.FromContext(ctx).V(4).Info("Could not list CredentialProviders for a Secret event",
			"namespace", object.GetNamespace(), "secret", object.GetName(), "error", err)
		return nil
	}

	var requests []reconcile.Request
	for i := range providers.Items {
		provider := &providers.Items[i]
		oauth2 := provider.Spec.OAuth2
		if oauth2 == nil {
			continue
		}
		if oauth2.Client.ClientSecret.SecretKeyRef.Name != object.GetName() {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: provider.Namespace,
				Name:      provider.Name,
			},
		})
	}
	return requests
}
