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

package securitytokenrefresh

import (
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
)

// needsRefreshPredicate filters Sandbox events down to the subset that this controller
// is interested in:
//
//   - The sandbox has been claimed (label LabelSandboxIsClaimed == "true").
//   - The sandbox carries a security-token refresh status annotation produced by the
//     claim flow. Sandboxes without this annotation are not under the
//     SecurityIdentityProvider regime and must be ignored.
//   - The sandbox is not being deleted.
//
// On Update events, the predicate additionally short-circuits unrelated noise:
// only mutations of the token-status annotation itself are treated as relevant,
// because expiration-driven requeues are handled via RequeueAfter rather than
// arbitrary status churn from the sandbox controller.
func needsRefreshPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return isRefreshTarget(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if !isRefreshTarget(e.ObjectNew) {
				return false
			}
			oldRaw := tokenStatusAnnotation(e.ObjectOld)
			newRaw := tokenStatusAnnotation(e.ObjectNew)
			return oldRaw != newRaw
		},
		DeleteFunc: func(_ event.DeleteEvent) bool {
			// Deletion is handled by the sandbox controller / GC; nothing to refresh.
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return isRefreshTarget(e.Object)
		},
	}
}

// isRefreshTarget reports whether the given object is a claimed, alive sandbox
// that carries a meaningful token-status annotation. The annotation must not
// only be present, but also decode into a status that actually has an access
// token expiration to refresh; otherwise the workqueue would be woken up for
// sandboxes that have nothing to do (e.g. a stale `{}` payload).
//
// Malformed annotation payloads are intentionally treated as a refresh target:
// the reconciler will surface a TokenStatusDecodeFailed event so the bad
// payload becomes observable instead of being silently dropped here.
func isRefreshTarget(obj client.Object) bool {
	if obj == nil || !obj.GetDeletionTimestamp().IsZero() {
		return false
	}
	if obj.GetLabels()[agentsv1alpha1.LabelSandboxIsClaimed] != agentsv1alpha1.True {
		return false
	}
	raw := tokenStatusAnnotation(obj)
	if raw == "" {
		return false
	}
	status, err := decodeTokenRefreshStatus(raw)
	if err != nil {
		// Let the reconciler surface the decode failure as an event.
		return true
	}
	return status != nil && status.AccessTokenExpiration != ""
}

// tokenStatusAnnotation returns the raw value of the token-status annotation,
// or an empty string when missing. It centralises annotation access so callers
// stay decoupled from the constant key.
func tokenStatusAnnotation(obj client.Object) string {
	if obj == nil {
		return ""
	}
	return obj.GetAnnotations()[identity.AgentKeyTokenRefreshStatus]
}
