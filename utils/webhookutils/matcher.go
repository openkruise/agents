package webhookutils

import (
	"strings"

	v1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
)

// Matcher determines if the Attr matches the Rule.
type Matcher struct {
	Rule v1.RuleWithOperations
	Attr admission.Attributes
}

// Matches returns if the Attr matches the Rule.
func (r *Matcher) Matches() bool {
	return r.scope() &&
		r.operation() &&
		r.group() &&
		r.version() &&
		r.resource()
}

func exactOrWildcard(items []string, requested string) bool {
	for _, item := range items {
		if item == "*" {
			return true
		}
		if item == requested {
			return true
		}
	}

	return false
}

var namespaceResource = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}

func (r *Matcher) scope() bool {
	if r.Rule.Scope == nil || *r.Rule.Scope == v1.AllScopes {
		return true
	}
	// attr.GetNamespace() is set to the name of the namespace for requests of the namespace object itself.
	switch *r.Rule.Scope {
	case v1.NamespacedScope:
		// first make sure that we are not requesting a namespace object (namespace objects are cluster-scoped)
		return r.Attr.GetResource() != namespaceResource && r.Attr.GetNamespace() != metav1.NamespaceNone
	case v1.ClusterScope:
		// also return true if the request is for a namespace object (namespace objects are cluster-scoped)
		return r.Attr.GetResource() == namespaceResource || r.Attr.GetNamespace() == metav1.NamespaceNone
	default:
		return false
	}
}

func (r *Matcher) group() bool {
	return exactOrWildcard(r.Rule.APIGroups, r.Attr.GetResource().Group)
}

func (r *Matcher) version() bool {
	return exactOrWildcard(r.Rule.APIVersions, r.Attr.GetResource().Version)
}

func (r *Matcher) operation() bool {
	attrOp := r.Attr.GetOperation()
	for _, op := range r.Rule.Operations {
		if op == v1.OperationAll {
			return true
		}
		// The constants are the same such that this is a valid cast (and this
		// is tested).
		if op == v1.OperationType(attrOp) {
			return true
		}
	}
	return false
}

func splitResource(resSub string) (res, sub string) {
	parts := strings.SplitN(resSub, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return parts[0], ""
}

func (r *Matcher) resource() bool {
	opRes, opSub := r.Attr.GetResource().Resource, r.Attr.GetSubresource()
	for _, res := range r.Rule.Resources {
		res, sub := splitResource(res)
		resMatch := res == "*" || res == opRes
		subMatch := sub == "*" || sub == opSub
		if resMatch && subMatch {
			return true
		}
	}
	return false
}
