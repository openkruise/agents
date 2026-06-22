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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	// docs/components/traffic-extension.md.
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
	// Schemes filters by the request's :scheme pseudo-header (e.g. "http",
	// "https", or custom schemes used by gRPC/other protocols). Multiple
	// entries are ORed. Matching is case-insensitive.
	// +optional
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=32
	// +kubebuilder:validation:items:Pattern=`^[a-zA-Z][a-zA-Z0-9+\-.]*$`
	Schemes []string `json:"schemes,omitempty"`
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

// TokenTransformationType discriminates the credential-transformation
// strategy used by a TokenTransformationAction.
// +kubebuilder:validation:Enum=ApiKey;AliyunSTS
type TokenTransformationType string

const (
	// TokenTransformationTypeApiKey rewrites a single header with a token
	// fetched from the credential provider. Backwards-compatible default.
	TokenTransformationTypeApiKey TokenTransformationType = "ApiKey"
	// TokenTransformationTypeAliyunSTS swaps the AK/SK/STS triplet inside
	// an intercepted Aliyun-SDK request and recomputes the signature.
	TokenTransformationTypeAliyunSTS TokenTransformationType = "AliyunSTS"
)

// CredentialRefKind discriminates the credential source type.
// +kubebuilder:validation:Enum=Secret;CredentialProvider
type CredentialRefKind string

const (
	// CredentialRefKindSecret reads credential material from a Kubernetes
	// Secret. The expected data keys follow well-known conventions per
	// transformation type:
	//   ApiKey mode:    "apiKey"
	//   AliyunSTS mode: "accessKeyId", "accessKeySecret", "securityToken"
	CredentialRefKindSecret CredentialRefKind = "Secret"
	// CredentialRefKindCredentialProvider fetches credentials at runtime
	// from an external credential provider (e.g. agent-identity service).
	CredentialRefKindCredentialProvider CredentialRefKind = "CredentialProvider"
)

// CredentialRef identifies the credential source for a TokenTransformation.
// The Kind field selects between built-in Secret and extensible
// CredentialProvider; Name identifies the specific resource.
//
// For Kind=Secret the Secret must reside in the same namespace as the
// SecurityProfile. Cross-namespace references are intentionally disallowed
// to enforce namespace isolation; a future version may support them via a
// ReferenceGrant-style mechanism.
type CredentialRef struct {
	// Kind selects the credential source type.
	Kind CredentialRefKind `json:"kind"`
	// Name is the resource name — Secret name for Kind=Secret, or
	// provider name for Kind=CredentialProvider.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// ApiKeyConfig holds ApiKey-mode specific configuration.
type ApiKeyConfig struct {
	// When is an optional condition; the transformation is skipped if
	// the header does not match.
	// +optional
	When *ActionCondition `json:"when,omitempty"`
	// TargetHeader is the request header to overwrite with the new token.
	// +kubebuilder:default:="Authorization"
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9!#$%&'*+\-.^_|~]+$`
	TargetHeader string `json:"targetHeader,omitempty"`
	// ValueTemplate is a Go text/template for the header value.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	ValueTemplate string `json:"valueTemplate"`
}

// TokenTransformationAction rewrites credential/authorization material on
// outgoing requests. The Type field selects between two implementations:
// ApiKey (default, header rewrite) and AliyunSTS (Aliyun-SDK triplet
// swap + signature recompute). The CredentialRef field selects the
// credential source (Secret or CredentialProvider) independently.
//
// +kubebuilder:validation:XValidation:rule="(!has(self.type) || self.type == 'ApiKey') ? has(self.apiKey) : true",message="apiKey config is required when type is ApiKey"
// +kubebuilder:validation:XValidation:rule="self.type == 'AliyunSTS' ? !has(self.apiKey) : true",message="apiKey must be unset when type is AliyunSTS"
type TokenTransformationAction struct {
	// Disabled temporarily disables this action without removing its
	// configuration. When true the action is skipped during evaluation.
	// +optional
	// +kubebuilder:default:=false
	Disabled bool `json:"disabled,omitempty"`
	// FailStrategy controls behaviour when the transformation fails.
	// +optional
	// +kubebuilder:default:=Block
	FailStrategy FailStrategy `json:"failStrategy,omitempty"`
	// Type discriminates the transformation strategy. Defaults to ApiKey.
	// +optional
	// +kubebuilder:default:=ApiKey
	Type TokenTransformationType `json:"type,omitempty"`
	// CredentialRef identifies the credential source for this
	// transformation. Required.
	CredentialRef CredentialRef `json:"credentialRef"`

	// ApiKey holds ApiKey-mode specific configuration.
	// Required when Type == ApiKey, must be unset when Type == AliyunSTS.
	// +optional
	ApiKey *ApiKeyConfig `json:"apiKey,omitempty"`
}

// AuditBody is the request body sent to the webhook. Exactly one of JSON or
// Text must be set.
//
// +kubebuilder:validation:XValidation:rule="(has(self.json) && !has(self.text)) || (!has(self.json) && has(self.text))",message="exactly one of json or text must be set"
type AuditBody struct {
	// JSON is a structured body. String leaves are rendered through Go
	// text/template against AuditContext; non-string scalars and nested
	// objects/arrays are emitted verbatim. Serialised as application/json
	// by default.
	//
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	JSON *runtime.RawExtension `json:"json,omitempty"`
	// Text is a raw text body. The entire string is rendered through Go
	// text/template against AuditContext. Sent as text/plain by default.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=8192
	Text *string `json:"text,omitempty"`
}

// AuditHeader represents a single HTTP header on the audit request. Value
// may contain text/template expressions.
type AuditHeader struct {
	// Name is the header name. Restricted to a safe subset of RFC 7230
	// tchar characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9!#$%&'*+\-.^_|~]+$`
	Name string `json:"name"`
	// Value is the header value template.
	// +kubebuilder:validation:MaxLength=2048
	Value string `json:"value"`
}

// AuditRequest describes the HTTP request shape.
type AuditRequest struct {
	// Method is the HTTP request method. Defaults to POST.
	// +optional
	// +kubebuilder:default:=POST
	// +kubebuilder:validation:Enum=POST;PUT;PATCH
	Method string `json:"method,omitempty"`
	// Headers are appended to the request after the default Content-Type
	// header.
	// +optional
	Headers []AuditHeader `json:"headers,omitempty"`
	// Body is the request body. Exactly one of Body.JSON or Body.Text must
	// be set. Omitting Body sends an empty request.
	// +optional
	Body *AuditBody `json:"body,omitempty"`
}

// AuditWebhook describes an HTTP(S) webhook target for an audit action.
// It is grouped under AuditAction.Webhook so future audit transports
// (e.g. message bus, structured log sink) can be added as sibling
// fields without breaking the surrounding shape.
type AuditWebhook struct {
	// URL is the absolute HTTP(S) URL of the webhook. Supports Go
	// text/template expressions over AuditContext, allowing per-Pod
	// addressing such as: http://{{ .Pod.IP }}:8080/audit
	//
	// Rendering failures (template error or non-HTTP scheme) cause the
	// event to be dropped and counted under
	// traffic_extension_audit_webhook_dropped_total{reason="render_url"}.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`
	// Request describes the HTTP request shape. Defaults to method=POST
	// and empty body when omitted.
	// +optional
	Request *AuditRequest `json:"request,omitempty"`
	// Timeout caps each HTTP attempt. Defaults to 2s, max 30s.
	// +optional
	// +kubebuilder:default:="2s"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('500ms') && duration(self) <= duration('30s')",message="timeout must be between 500ms and 30s"
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// AuditAction is a named, conditional audit fan-out entry. Audit is
// non-terminal: it does not influence the request response and is
// dispatched asynchronously after the request resolves.
//
// AuditAction lists may appear at two levels:
//   - SecurityProfileSpec.Audit: profile-wide defaults applied to every
//     matched rule.
//   - SecurityRuleActions.Audit: per-rule overrides. When non-empty, the
//     spec-level list is suppressed for that rule's matches.
//
// For each (matched rule, audit entry) pair the data plane evaluates
// `When` against AuditContext and dispatches when the expression is
// true (or when `When` is empty, which defaults to true).
type AuditAction struct {
	// Name uniquely identifies this audit entry within its enclosing
	// list. Used in metrics labels and dispatcher dedup. Restricted to
	// label-safe characters so it can flow into Prometheus labels.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// When is a CEL expression evaluated against AuditContext at
	// resolution time. The expression must evaluate to a bool; the audit
	// fires when the result is true. Empty (default) means "always fire".
	//
	// Available variables:
	//   result   string                  one of passthrough/mutated/blocked/bypassed/error
	//   request  map<string, dyn>        host, port, path, method, scheme, headers, queryParams
	//   pod      map<string, dyn>        name, namespace, ip, labels
	//   profile  map<string, string>     name, namespace
	//   rule     map<string, string>     name (the matched rule's name)
	//
	// Examples:
	//   result == "blocked"
	//   result in ["blocked", "bypassed"]
	//   pod.labels["team"] == "fraud" && result != "passthrough"
	//   rule.name.startsWith("pii-")
	//
	// Map indexing follows CEL's strict semantics: indexing with an
	// absent key raises an eval error (counted as a drop, not a "false"
	// match). Use `in` for safe presence checks, e.g.
	//   "x-priority" in request.headers && request.headers["x-priority"] == "high"
	//
	// Compilation failures (parse, type-check) cause the enclosing
	// SecurityProfile to be rejected at load time. Runtime evaluation
	// errors are counted under
	// traffic_extension_audit_webhook_dropped_total{reason="when_eval"}
	// and the event is dropped.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	When string `json:"when,omitempty"`
	// Webhook is the HTTP webhook target. Required for now (no other
	// transports are implemented).
	Webhook *AuditWebhook `json:"webhook"`
}

// SecurityRuleActions is a map-style struct where each field corresponds to
// one action type. All fields are optional. In the Envoy data plane the
// execution order is deterministic and each action runs at most once, so
// there is no need for an ordered array — the controller compiles the
// populated fields into the correct filter-chain position.
//
// Terminal actions (Block, Bypass) short-circuit the rule chain;
// non-terminal actions (the rest) execute and fall through.
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
	// Audit lists per-rule audit entries. When non-empty, this list
	// REPLACES the profile-level SecurityProfileSpec.Audit list for this
	// rule's matches (override semantics). When empty or omitted, the
	// spec-level list applies. To suppress audit on a specific rule,
	// add a single entry with `when: "false"`.
	//
	// Audit is non-terminal — it never alters the upstream response and
	// does not short-circuit the rule chain.
	// +optional
	// +listType=map
	// +listMapKey=name
	Audit []AuditAction `json:"audit,omitempty"`
}

// SecurityRule is one entry in the ordered rule chain.
//
// Rule evaluation is Default Continue: after a rule's actions run, the next
// rule is evaluated unless a terminal action (Block / Bypass) fired. The
// first terminal action wins and short-circuits the chain; already-executed
// non-terminal actions are NOT rolled back.
//
// CAUTION: rule order is significant. A rule with a wildcard domain does
// not mask a later rule with a more specific domain — both may match the
// same request in sequence. Put terminal rules before non-terminal ones
// if you want to skip work for a specific domain.
type SecurityRule struct {
	// Name uniquely identifies the rule within the profile. Used in
	// metrics, events, and generated xDS resource names.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Match lists match conditions. Multiple entries are ORed.
	// +kubebuilder:validation:MinItems=1
	Match []RuleMatch `json:"match"`
	// Actions is a map of action types to their configurations. The Envoy
	// data plane executes populated actions in a deterministic order; each
	// action runs at most once. Terminal actions (Block, Bypass)
	// short-circuit the rule chain.
	Actions SecurityRuleActions `json:"actions"`
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
	// (Block / Bypass) short-circuits the chain. An empty rule chain is
	// equivalent to "forward everything to the original destination".
	// +optional
	// +listType=map
	// +listMapKey=name
	Rules []SecurityRule `json:"rules,omitempty"`
	// Audit declares profile-wide audit entries. They fire for every
	// matched rule (subject to each entry's `When` CEL expression),
	// providing a default audit configuration for all rules in this
	// profile. A SecurityRule may override this list via
	// SecurityRuleActions.Audit.
	// +optional
	// +listType=map
	// +listMapKey=name
	Audit []AuditAction `json:"audit,omitempty"`
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
