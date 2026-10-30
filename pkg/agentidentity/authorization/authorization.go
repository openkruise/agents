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

// Package authorization decides whether an agent may perform an action.
//
// Permission comes from an AgentRoleBinding attaching an AgentRole to an
// AgentIdentity. Rules are Allow-only and there is no deny form, so a request
// that matches no rule is denied: permission exists because something granted
// it, never because nothing forbade it.
//
// Every decision reads the current bindings rather than anything cached from a
// token or an earlier call. A binding deleted a moment ago must stop granting
// immediately, which is why authorization does not consult the Ready condition
// on a binding or a role: readiness reports what a controller has observed,
// which necessarily lags the spec that authorization has to act on.
package authorization

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
)

// Reasons carried on a Decision. They are short and stable so an audit record
// can say why a request was allowed or refused without restating the request.
const (
	// ReasonAllowed means a rule matched.
	ReasonAllowed = "Allowed"
	// ReasonNoBindings means nothing grants this identity anything at all.
	ReasonNoBindings = "NoBindings"
	// ReasonNoMatchingRule means the identity has roles, but none of their rules
	// cover this action on this resource.
	ReasonNoMatchingRule = "NoMatchingRule"
	// ReasonUnknownAction means the action is not one this release defines.
	ReasonUnknownAction = "UnknownAction"
)

// Request is the question being asked.
//
// AgentIdentity is taken from the caller's verified token, never from a request
// body: a caller able to name its own identity could authorize itself as any
// agent in the namespace.
type Request struct {
	// Namespace the agent and its bindings live in. Authorization never crosses
	// namespaces.
	Namespace string

	// AgentIdentity is the identity the caller is acting as.
	AgentIdentity string

	// Action is one of the actions an AgentRole may permit.
	Action string

	// Resource is what the action applies to, as "<Kind>/<name>".
	Resource string
}

// Decision is the outcome. A denial is a result rather than an error, because
// the caller can neither retry it nor fix it.
type Decision struct {
	Allowed bool
	Reason  string
}

var deniedNoBindings = Decision{Reason: ReasonNoBindings}

// Authorizer evaluates requests against the bindings and roles in the cluster.
type Authorizer struct {
	// reader must be informer-backed. Authorization runs on every request, so a
	// direct API read would put the apiserver in the hot path, and the informer
	// is current enough that a deleted binding stops granting within one event.
	reader client.Reader
}

// New returns an Authorizer reading through reader.
func New(reader client.Reader) *Authorizer {
	return &Authorizer{reader: reader}
}

// Authorize reports whether the request is permitted.
//
// An error means the decision could not be made, which a caller must treat as a
// refusal rather than a denial: it is not a statement about permission, and it
// may succeed on retry. A Decision with Allowed false is the opposite, a settled
// answer that retrying will not change.
func (a *Authorizer) Authorize(ctx context.Context, req Request) (Decision, error) {
	if err := req.validate(); err != nil {
		return Decision{}, err
	}

	// An action this release does not define can never be granted, whatever the
	// rules say, so it is refused before any lookup.
	kind, wildcardAllowed, known := resourceKindFor(req.Action)
	if !known {
		return Decision{Reason: ReasonUnknownAction}, nil
	}

	bindings := &securityv1alpha1.AgentRoleBindingList{}
	if err := a.reader.List(ctx, bindings, client.InNamespace(req.Namespace)); err != nil {
		return Decision{}, fmt.Errorf("list agent role bindings in %s: %w", req.Namespace, err)
	}

	roleNames := a.rolesBoundTo(bindings, req.AgentIdentity)
	if len(roleNames) == 0 {
		return deniedNoBindings, nil
	}

	for _, roleName := range roleNames {
		role := &securityv1alpha1.AgentRole{}
		key := client.ObjectKey{Namespace: req.Namespace, Name: roleName}
		if err := a.reader.Get(ctx, key, role); err != nil {
			if !apierrors.IsNotFound(err) {
				return Decision{}, fmt.Errorf("get agent role %s/%s: %w", req.Namespace, roleName, err)
			}
			// A binding naming a role that does not exist grants nothing. Other
			// bindings may still grant this request, so the search continues
			// rather than refusing outright.
			log.FromContext(ctx).V(4).Info("Agent role bound but absent, skipping",
				"namespace", req.Namespace, "agentRole", roleName, "agentIdentity", req.AgentIdentity)
			continue
		}
		// A role on its way out stops granting the moment deletion is requested,
		// not when a finalizer finally lets it go.
		if !role.DeletionTimestamp.IsZero() {
			continue
		}

		if permits(role.Spec.Rules, req.Action, req.Resource, kind, wildcardAllowed) {
			return Decision{Allowed: true, Reason: ReasonAllowed}, nil
		}
	}

	return Decision{Reason: ReasonNoMatchingRule}, nil
}

// rolesBoundTo returns the roles bound to one identity, in the order the
// bindings were listed, with duplicates kept out.
//
// Several bindings may name the same identity, and §5.5 says their rules
// combine. A binding being deleted is skipped: it must stop granting
// immediately, which is the whole reason authorization reads live state.
func (a *Authorizer) rolesBoundTo(bindings *securityv1alpha1.AgentRoleBindingList,
	agentIdentity string) []string {
	var names []string
	seen := make(map[string]struct{}, len(bindings.Items))
	for i := range bindings.Items {
		binding := &bindings.Items[i]
		if binding.Spec.AgentIdentityRef.Name != agentIdentity {
			continue
		}
		if !binding.DeletionTimestamp.IsZero() {
			continue
		}
		roleName := binding.Spec.AgentRoleRef.Name
		if _, duplicate := seen[roleName]; duplicate {
			continue
		}
		seen[roleName] = struct{}{}
		names = append(names, roleName)
	}
	return names
}

// permits reports whether any rule covers the action on the resource.
func permits(rules []securityv1alpha1.AgentRoleRule,
	action, resource, kind string, wildcardAllowed bool) bool {
	for _, rule := range rules {
		if !contains(rule.Actions, action) {
			continue
		}
		for _, allowed := range rule.Resources {
			if resourceMatches(allowed, resource, kind, wildcardAllowed) {
				return true
			}
		}
	}
	return false
}

// resourceMatches reports whether a rule's resource entry covers the requested
// one, for the action's kind.
//
// The kind and wildcard checks are enforced here as well as at admission, and
// deliberately so. §5.4 pins each action to one kind, and admission now rejects
// a role that pairs them wrongly, but a role created before that constraint
// existed is still in the cluster and still readable. Trusting admission alone
// would mean such a role granting, for example, every CredentialProvider in the
// namespace through a single "*". The same reasoning applies as to an
// AgentIdentity carrying an empty authenticationRefs: admission closes the door
// going forward, and the consumer still fails closed for what came before.
func resourceMatches(allowed, requested, kind string, wildcardAllowed bool) bool {
	// The requested resource must name the kind this action acts on, and this is
	// checked before anything else because "*" is not compared against it. A
	// wildcard entry means every resource of that one kind in the namespace, so
	// without this a rule granting ExchangePrincipalToken on "*" would permit
	// the action against a CredentialProvider, a Secret, a bare name or "*"
	// itself. Admission rejects a role pairing an action with the wrong kind, so
	// that would also make the evaluator broader than any role that can exist.
	if !namesKind(requested, kind) {
		return false
	}
	if allowed == "*" {
		return wildcardAllowed
	}
	// A rule entry naming a different kind than the action acts on cannot match,
	// however closely the name happens to line up.
	if !strings.HasPrefix(allowed, kind+"/") {
		return false
	}
	return allowed == requested
}

// namesKind reports whether resource is "<kind>/<name>" with a single concrete
// name.
//
// The name must not be "*". Admission rejects a wildcard in the name position
// in a role's resources, and a request carrying one is asking about every
// resource at once rather than about a resource, which nothing should answer in
// the affirmative.
func namesKind(resource, kind string) bool {
	name, found := strings.CutPrefix(resource, kind+"/")
	if !found {
		return false
	}
	return name != "" && name != "*" && !strings.Contains(name, "/")
}

// resourceKindFor returns the kind an action's resources must name, and whether
// "*" is a legitimate entry for it.
//
// §5.4: ExchangePrincipalToken acts on an AgentIdentity or "*";
// GetResourceOAuth2Token must name one CredentialProvider exactly, because a
// wildcard would also grant every provider added later.
func resourceKindFor(action string) (kind string, wildcardAllowed bool, known bool) {
	switch action {
	case securityv1alpha1.ActionExchangePrincipalToken:
		return "AgentIdentity", true, true
	case securityv1alpha1.ActionGetResourceOAuth2Token:
		return "CredentialProvider", false, true
	default:
		return "", false, false
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// validate rejects a structurally incomplete request.
//
// This is a programming error in the caller rather than a policy outcome, so it
// is an error and not a denial: reporting "not permitted" would suggest the
// request was understood and refused.
func (r Request) validate() error {
	switch {
	case r.Namespace == "":
		return errors.New("authorization: namespace must not be empty")
	case r.AgentIdentity == "":
		return errors.New("authorization: agent identity must not be empty")
	case r.Action == "":
		return errors.New("authorization: action must not be empty")
	case r.Resource == "":
		return errors.New("authorization: resource must not be empty")
	}
	return nil
}

// String renders a decision for a log line or an audit record.
func (d Decision) String() string {
	if d.Allowed {
		return "allowed(" + d.Reason + ")"
	}
	return "denied(" + d.Reason + ")"
}
