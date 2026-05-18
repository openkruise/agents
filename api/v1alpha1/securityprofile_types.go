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
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PathMatchType enumerates URL path matching strategies.
// +kubebuilder:validation:Enum=Prefix;Exact;Regex
type PathMatchType string

const (
	PathMatchTypePrefix PathMatchType = "Prefix"
	PathMatchTypeExact  PathMatchType = "Exact"
	PathMatchTypeRegex  PathMatchType = "Regex"
)

// StringMatchType enumerates the matching strategy used by header- and
// query-parameter value matchers.
// +kubebuilder:validation:Enum=Exact;Prefix;Regex
type StringMatchType string

const (
	// StringMatchTypeExact requires the value to equal Value verbatim.
	StringMatchTypeExact StringMatchType = "Exact"
	// StringMatchTypePrefix requires the value to start with Value.
	StringMatchTypePrefix StringMatchType = "Prefix"
	// StringMatchTypeRegex evaluates Value as an RE2 regular expression
	// against the request value. An invalid regex fails closed (the rule
	// does not fire).
	StringMatchTypeRegex StringMatchType = "Regex"
)

// FailStrategy controls behaviour when an external service call fails or
// when the action encounters an error.
// +kubebuilder:validation:Enum=Allow;Block;Ignore
type FailStrategy string

const (
	// FailStrategyAllow lets the request proceed when the external call fails.
	FailStrategyAllow FailStrategy = "Allow"
	// FailStrategyBlock aborts the request when the external call fails.
	FailStrategyBlock FailStrategy = "Block"
	// FailStrategyIgnore silently ignores the failure and continues the
	// action chain as if the action was never configured. Unlike Allow,
	// Ignore also suppresses any warning headers or error metrics.
	FailStrategyIgnore FailStrategy = "Ignore"
)

// PathMatch specifies how to match the request URL path.
type PathMatch struct {
	// +kubebuilder:default:=Prefix
	Type PathMatchType `json:"type"`
	// Value is the match pattern. For Regex, it is an RE2 expression and
	// must be <= 256 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Value string `json:"value"`
}

// HeaderMatch filters a request by a single header's value.
// Multiple HeaderMatch entries in one RuleMatch are ANDed.
//
// Type defaults to Exact. Use Prefix to match a leading substring (common
// for "Bearer ..." tokens) or Regex for full RE2 power.
type HeaderMatch struct {
	// Name is the header name (case-insensitive). Restricted to a safe
	// subset of RFC 7230 tchar characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9!#$%&'*+\-.^_|~]+$`
	Name string `json:"name"`
	// Type selects the matching strategy. Defaults to Exact.
	// +kubebuilder:default:=Exact
	Type StringMatchType `json:"type,omitempty"`
	// Value is the match operand. For Exact/Prefix it is compared
	// verbatim; for Regex it is interpreted as an RE2 expression.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Value string `json:"value"`
}

// QueryParamMatch filters a request by a single URL query-parameter value.
// Multiple QueryParamMatch entries in one RuleMatch are ANDed.
//
// When the same key appears multiple times in the URL (e.g.
// "?tag=a&tag=b"), only the FIRST occurrence is matched. Type defaults
// to Exact.
type QueryParamMatch struct {
	// Name is the query parameter key. Comparison is case-sensitive per
	// RFC 3986. Restricted to a safe subset of RFC 3986 unreserved /
	// sub-delims characters; brackets are permitted to support PHP-style
	// array keys (e.g. "filter[type]").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9!$&'()*+,\-./:;=?@_~\[\]]+$`
	Name string `json:"name"`
	// Type selects the matching strategy. Defaults to Exact.
	// +kubebuilder:default:=Exact
	Type StringMatchType `json:"type,omitempty"`
	// Value is the match operand. For Exact/Prefix it is compared
	// verbatim against the percent-decoded query value; for Regex it is
	// interpreted as an RE2 expression.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Value string `json:"value"`
}

// RuleMatch is a conjunctive match condition. Multiple RuleMatch entries
// inside a rule's match list are ORed; fields inside one RuleMatch are ANDed.
//
// Domains is required. Paths / Methods / Ports / Headers / QueryParams
// further restrict the match. Methods MUST appear together with Paths
// (standalone methods filters have unbounded scope).
//
// +kubebuilder:validation:XValidation:rule="!has(self.methods) || size(self.methods) == 0 || (has(self.paths) && size(self.paths) > 0)",message="methods requires paths to be set"
type RuleMatch struct {
	// Domains lists target host names. Supports "*" (any domain) and
	// "*.example.com" wildcard prefixes.
	//
	// CAUTION: wildcard and specific domains can both match the same request
	// under Default Continue semantics, so rule ordering matters. See
	// docs/components/traffix-extension.md.
	// +kubebuilder:validation:MinItems=1
	Domains []string `json:"domains"`
	// Paths lists URL path matches; multiple entries are ORed. The path
	// is compared without any "?query" suffix — write QueryParams matches
	// to constrain query parameters.
	// +optional
	Paths []PathMatch `json:"paths,omitempty"`
	// Methods filters by HTTP method. Only valid when Paths is also set.
	// +optional
	// +kubebuilder:validation:items:Enum=GET;HEAD;POST;PUT;PATCH;DELETE;OPTIONS;CONNECT;TRACE
	Methods []string `json:"methods,omitempty"`
	// Ports filters by the port the client targeted on the upstream
	// authority. Multiple entries are ORed.
	//
	// When the request authority spells out a port (e.g.
	// "api.example.com:8443"), that port is used directly. When the client
	// omits the port — relying on the scheme default — the matcher infers
	// 80 for http and 443 for https from the request's :scheme. Listing
	// `ports: [80]` therefore matches both "host:80" and a plain "host"
	// over HTTP. An unrecognized scheme leaves the inferred port at 0,
	// which never matches a non-empty Ports list.
	// +optional
	// +kubebuilder:validation:items:Minimum=1
	// +kubebuilder:validation:items:Maximum=65535
	Ports []int32 `json:"ports,omitempty"`
	// Headers lists header matches; multiple entries are ANDed.
	// +optional
	Headers []HeaderMatch `json:"headers,omitempty"`
	// QueryParams lists URL query-parameter matches; multiple entries are
	// ANDed. Matched against the percent-decoded value of the FIRST
	// occurrence of each key.
	// +optional
	QueryParams []QueryParamMatch `json:"queryParams,omitempty"`
}

// BlockAction configures the response returned to the client when a
// Block action fires.
type BlockAction struct {
	// StatusCode is the HTTP status returned to the client.
	// +kubebuilder:default:=403
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=599
	StatusCode int32 `json:"statusCode,omitempty"`
	// Body is an optional response body sent verbatim to the client.
	// +optional
	Body *string `json:"body,omitempty"`
}

// ActionCondition is an optional pre-condition that gates action execution.
// The action only fires when the specified header matches the pattern.
type ActionCondition struct {
	// Header is the request header name to inspect.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9!#$%&'*+\-.^_|~]+$`
	Header string `json:"header"`
	// Pattern is an RE2 regex evaluated against the header value.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Pattern string `json:"pattern"`
}

// TokenTransformationAction rewrites credential/authorization headers by
// fetching a real token from an external provider and injecting it into
// the request. Non-terminal.
//
// The token provider endpoint and any provider credentials are configured
// at cluster level (out of band) — this CRD intentionally does not expose
// them, so each profile only declares per-Pod transformation behaviour.
type TokenTransformationAction struct {
	// Disabled temporarily disables this action without removing its
	// configuration. When true the action is skipped during evaluation.
	// +optional
	// +kubebuilder:default:=false
	Disabled bool `json:"disabled,omitempty"`
	// FailStrategy controls what happens when the token provider call
	// fails or times out.
	// +optional
	// +kubebuilder:default:=Block
	FailStrategy FailStrategy `json:"failStrategy,omitempty"`
	// When is an optional condition; the transformation is skipped if the
	// header does not match.
	// +optional
	When *ActionCondition `json:"when,omitempty"`
	// TargetHeader is the request header to overwrite with the new token.
	// +kubebuilder:default:="Authorization"
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9!#$%&'*+\-.^_|~]+$`
	TargetHeader string `json:"targetHeader,omitempty"`
	// ValueTemplate is a Go template string producing the final header
	// value. The template receives the provider response as ".Token".
	// Example: "Bearer {{ .Token }}"
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	ValueTemplate string `json:"valueTemplate"`
	// TokenProviderRef references the token provider object that supplies
	// the token for this transformation.
	// +optional
	TokenProviderRef *corev1.TypedLocalObjectReference `json:"tokenProviderRef,omitempty"`
}

// HeaderValueSource specifies where a header value comes from.
type HeaderValueSource struct {
	// PodField injects a value from the matched Pod's metadata
	// (e.g. "metadata.name", "metadata.namespace").
	// +optional
	PodField string `json:"podField,omitempty"`
}

// IdentityHeader defines a single header to inject.
type IdentityHeader struct {
	// Name is the header name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9!#$%&'*+\-.^_|~]+$`
	Name string `json:"name"`
	// ValueFrom specifies the source of the header value.
	ValueFrom HeaderValueSource `json:"valueFrom"`
}

// IdentityInjectionAction injects sandbox identity headers into the
// outgoing request. Non-terminal.
//
// Unlike TokenTransformationAction, there is no When condition: identity
// injection is unconditional — every request matched by the rule is stamped
// with the sandbox identity. This is intentional to prevent callers from
// bypassing identity tracking.
type IdentityInjectionAction struct {
	// Disabled temporarily disables this action without removing its
	// configuration. When true the action is skipped during evaluation.
	// +optional
	// +kubebuilder:default:=false
	Disabled bool `json:"disabled,omitempty"`
	// FailStrategy controls what happens when identity resolution fails.
	// +optional
	// +kubebuilder:default:=Block
	FailStrategy FailStrategy `json:"failStrategy,omitempty"`
	// Headers lists the headers to inject.
	// +kubebuilder:validation:MinItems=1
	Headers []IdentityHeader `json:"headers"`
}

// SecurityCheckAction calls an external security inspection service
// synchronously. Non-terminal (unless FailStrategy is Block and the check
// fails).
//
// The cloud security service endpoint and credentials are configured at
// cluster level (out of band) — this CRD intentionally does not expose
// them, so each profile only declares per-Pod check behaviour.
type SecurityCheckAction struct {
	// Disabled temporarily disables this action without removing its
	// configuration. When true the action is skipped during evaluation.
	// +optional
	// +kubebuilder:default:=false
	Disabled bool `json:"disabled,omitempty"`
	// FailStrategy controls what happens when the check call fails or
	// returns an error.
	// +kubebuilder:default:=Block
	FailStrategy FailStrategy `json:"failStrategy,omitempty"`
}

// MirroringAction asynchronously mirrors request traffic to a collector
// endpoint. The mirror call is fire-and-forget and does not affect the
// main request latency. Non-terminal.
//
// The collector endpoint is configured at cluster level (out of band) —
// this CRD intentionally does not expose it, so each profile only declares
// whether mirroring is enabled for the matched Pods.
type MirroringAction struct {
	// Disabled temporarily disables this action without removing its
	// configuration. When true the action is skipped during evaluation.
	// +optional
	// +kubebuilder:default:=false
	Disabled bool `json:"disabled,omitempty"`
}

// TokenBucketRate configures a token bucket rate limiter.
//
// The sustained rate is RequestsPerSecond tokens refilled per second.
// Burst controls the bucket capacity (max_tokens in Envoy), allowing
// short spikes above the sustained rate. When omitted, Burst defaults
// to RequestsPerSecond (no extra burst capacity).
type TokenBucketRate struct {
	// RequestsPerSecond is the sustained request rate — the number of
	// tokens refilled per second. This is the long-term average rate.
	// +kubebuilder:validation:Minimum=1
	RequestsPerSecond int32 `json:"requestsPerSecond"`
	// Burst is the maximum number of requests allowed in a single burst
	// above the sustained rate. Maps to Envoy token_bucket.max_tokens.
	// Defaults to RequestsPerSecond (no extra burst capacity).
	// +optional
	// +kubebuilder:validation:Minimum=1
	Burst *int32 `json:"burst,omitempty"`
}

// RateLimitAction enforces request rate limits using a token bucket
// algorithm. Non-terminal.
//
// RequestRate maps to Envoy's local_ratelimit HTTP filter (token bucket).
type RateLimitAction struct {
	// Disabled temporarily disables this action without removing its
	// configuration. When true the action is skipped during evaluation.
	// +optional
	// +kubebuilder:default:=false
	Disabled bool `json:"disabled,omitempty"`
	// RequestRate configures request-level rate limiting using a token
	// bucket algorithm. Omit to skip request rate limiting.
	// +optional
	RequestRate *TokenBucketRate `json:"requestRate,omitempty"`
}

// ForwardingAction is a terminal action that redirects the request to a
// different upstream host (e.g. an internal LLM gateway).
type ForwardingAction struct {
	// TargetHost is the upstream hostname to forward to.
	TargetHost string `json:"targetHost"`
	// TargetPort is the upstream port. Defaults to 80.
	// +optional
	// +kubebuilder:default:=80
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	TargetPort int32 `json:"targetPort,omitempty"`
	// PreserveHost keeps the original Host header instead of rewriting
	// it to TargetHost.
	// +kubebuilder:default:=false
	PreserveHost bool `json:"preserveHost,omitempty"`
}

// SecurityRuleActions is a map-style struct where each field corresponds to
// one action type. All fields are optional. In the Envoy data plane the
// execution order is deterministic and each action runs at most once, so
// there is no need for an ordered array — the controller compiles the
// populated fields into the correct filter-chain position.
//
// Terminal actions (Block, Bypass, Forwarding) short-circuit the
// rule chain; non-terminal actions (the rest) execute and fall through.
type SecurityRuleActions struct {
	// Block is a terminal action that returns a configured HTTP response
	// to the client without forwarding upstream.
	// +optional
	Block *BlockAction `json:"block,omitempty"`
	// Bypass is a terminal action that skips all subsequent rules and
	// forwards the request without further processing. Useful for trusted
	// internal domains.
	// +optional
	Bypass bool `json:"bypass,omitempty"`
	// TokenTransformation rewrites credential headers (e.g. replacing a
	// placeholder Bearer token with a real one from a token service).
	// Non-terminal.
	// +optional
	TokenTransformation *TokenTransformationAction `json:"tokenTransformation,omitempty"`
	// IdentityInjection injects sandbox identity headers into the outgoing
	// request (e.g. pod name, agent UUID). Non-terminal.
	// +optional
	IdentityInjection *IdentityInjectionAction `json:"identityInjection,omitempty"`
	// SecurityCheck calls an external security inspection service
	// synchronously. Non-terminal (unless failStrategy is Block and the
	// check fails).
	// +optional
	SecurityCheck *SecurityCheckAction `json:"securityCheck,omitempty"`
	// Mirroring asynchronously mirrors request traffic to a collector
	// endpoint without affecting the main request path. Non-terminal.
	// +optional
	Mirroring *MirroringAction `json:"mirroring,omitempty"`
	// RateLimit enforces request rate and connection limits using a token
	// bucket algorithm. Non-terminal.
	// +optional
	RateLimit *RateLimitAction `json:"rateLimit,omitempty"`
	// Forwarding is a terminal action that redirects the request to a
	// different upstream host (e.g. an internal LLM gateway).
	// +optional
	Forwarding *ForwardingAction `json:"forwarding,omitempty"`
}

// SecurityRule is one entry in the ordered rule chain.
//
// Rule evaluation is Default Continue: after a rule's actions run, the next
// rule is evaluated unless a terminal action (Block / Bypass / Forwarding)
// fired. The first terminal action wins and short-circuits the
// chain; already-executed non-terminal actions are NOT rolled back.
//
// CAUTION: rule order is significant. A rule with a wildcard domain does
// not mask a later rule with a more specific domain — both may match the
// same request in sequence. Put terminal rules before non-terminal ones
// if you want to skip work for a specific domain.
type SecurityRule struct {
	// Name uniquely identifies the rule within the profile. Used in
	// metrics, events, and generated xDS resource names.
	Name string `json:"name"`
	// Match lists match conditions. Multiple entries are ORed.
	// +kubebuilder:validation:MinItems=1
	Match []RuleMatch `json:"match"`
	// Actions is a map of action types to their configurations. The Envoy
	// data plane executes populated actions in a deterministic order; each
	// action runs at most once. Terminal actions (Block, Bypass, Forwarding)
	// short-circuit the rule chain.
	Actions *SecurityRuleActions `json:"actions"`
}

// SecurityProfileSpec describes an L7 security profile applied to the egress
// traffic of the selected Pods.
type SecurityProfileSpec struct {
	// Selector chooses the Pods this profile applies to. Standard
	// LabelSelector semantics: an EMPTY selector (no matchLabels and no
	// matchExpressions) matches EVERY pod in the same namespace, in line
	// with NetworkPolicy / Istio AuthorizationPolicy. Use a deliberate
	// matchExpression (e.g. `key: __none__, operator: DoesNotExist`) to
	// express "match nothing".
	Selector metav1.LabelSelector `json:"selector"`
	// Rules is the ordered rule chain. Semantics are Default Continue:
	// all matching rules' actions run in order until a terminal action
	// (Block / Bypass / Forwarding) short-circuits the chain. An empty rule chain is
	// equivalent to "forward everything to the original destination".
	// +optional
	// +listType=map
	// +listMapKey=name
	Rules []SecurityRule `json:"rules,omitempty"`
}

// Standard SecurityProfile condition types. Controllers MUST use these
// constants instead of free-form strings so that downstream tooling can
// rely on stable values.
const (
	// SecurityProfileConditionAccepted indicates the spec passed validation
	// and the rule chain compiled successfully.
	SecurityProfileConditionAccepted = "Accepted"
	// SecurityProfileConditionProgrammed indicates the compiled policy is
	// pushed to gateway.
	SecurityProfileConditionProgrammed = "Programmed"
)

// SecurityProfileStatus captures the observed state of a SecurityProfile.
type SecurityProfileStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions summarizes the profile's current state. Standard types are
	// Accepted and Programmed (see SecurityProfileCondition* constants).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sp
//
// SecurityProfile defines the L7 security/compliance profile for Sandbox
// AI Agent egress HTTP/HTTPS traffic.
//
// See docs/components/traffix-extension.md for the full semantic model.
type SecurityProfile struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +optional
	Spec SecurityProfileSpec `json:"spec,omitempty"`
	// +optional
	Status SecurityProfileStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
//
// SecurityProfileList contains a list of SecurityProfile.
type SecurityProfileList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SecurityProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SecurityProfile{}, &SecurityProfileList{})
}
