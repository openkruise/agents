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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/utils"
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
			// Re-enqueue when a serving runtime appears (no runtime -> serving
			// runtime). While there is no serving runtime the reconciler defers
			// refresh, so we must re-evaluate promptly on that edge rather than
			// depend on the resume flow's annotation rewrite to wake us up; this
			// keeps the refresh controller self-sufficient and guarantees an
			// overdue token is refreshed as soon as a serving runtime exists.
			if !hasServingRuntime(e.ObjectOld) && hasServingRuntime(e.ObjectNew) {
				return true
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

// hasServingRuntime reports whether the sandbox currently has a live runtime
// pod that has finished initialization and can therefore receive a propagated
// token. It is deliberately token-INDEPENDENT: it looks only at the sandbox
// Phase and the RuntimeInitialized condition, never at readiness.
//
// This independence is the whole point. The aggregate SandboxConditionReady
// tracks the Pod's PodReady, which is the AND of every container's readiness
// probe including the business container. When the business container consumes
// our security token, its readiness probe can fail once the token expires;
// gating refresh on Ready would then close a deadlock loop:
//
//	expired token -> business container not ready -> Pod not ready ->
//	Sandbox not Ready -> refresh deferred -> token never refreshed.
//
// Neither Phase nor RuntimeInitialized can be pulled down by token expiry, so
// gating on them lets the refresh always fire while a serving runtime exists,
// breaking the loop.
//
// A serving runtime requires Phase == Running: a bound pod whose containers
// have started. This excludes Paused (pod deleted), Resuming, Pending/creating
// and recreate-Upgrading, where there is no reachable runtime and a propagate
// would fail.
//
// The RuntimeInitialized condition is only ever set during a resume /
// recreate-upgrade RE-initialization cycle (EnsureSandboxResumed and
// performRecreateUpgrade); the initial claim flow initializes the runtime
// out-of-band (sandbox-manager InitRuntime) and never writes this condition.
// So its semantics are:
//   - absent: the sandbox never went through a resume/recreate re-init cycle,
//     so a Running pod is already serving (the common freshly-claimed case).
//     Treating absent as "not serving" would wedge every never-paused sandbox,
//     deferring its token refresh forever.
//   - present and True: the runtime finished (re-)initialization and can accept
//     a token.
//   - present and non-True: resume/recreate re-init is still in progress (Resume
//     resets this to False), so the resuming pod is correctly excluded until its
//     runtime is re-inited.
//
// A non-Sandbox object (which never reaches this controller) reports false.
func hasServingRuntime(obj client.Object) bool {
	box, ok := obj.(*agentsv1alpha1.Sandbox)
	if !ok {
		return false
	}
	if box.Status.Phase != agentsv1alpha1.SandboxRunning {
		return false
	}
	cond := utils.GetSandboxCondition(&box.Status, string(agentsv1alpha1.RuntimeInitialized))
	return cond == nil || cond.Status == metav1.ConditionTrue
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
