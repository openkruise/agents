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

// FailStrategy determines how an action handles an execution error.
// +kubebuilder:validation:Enum=Allow;Block;Ignore
type FailStrategy string

const (
	// FailStrategyAllow lets the request proceed when the action fails.
	FailStrategyAllow FailStrategy = "Allow"
	// FailStrategyBlock rejects the request when the action fails.
	FailStrategyBlock FailStrategy = "Block"
	// FailStrategyIgnore lets the request proceed when the action fails.
	// It currently behaves the same as FailStrategyAllow.
	FailStrategyIgnore FailStrategy = "Ignore"
)

// PathMatchType enumerates URL path matching strategies.
// +kubebuilder:validation:Enum=Prefix;Exact;Regex
type PathMatchType string

const (
	// PathMatchTypePrefix matches a plain string prefix. It is not
	// path-segment aware; for example, "/api" also matches "/apifoo".
	PathMatchTypePrefix PathMatchType = "Prefix"
	// PathMatchTypeExact requires the request path to equal Value.
	PathMatchTypeExact PathMatchType = "Exact"
	// PathMatchTypeRegex matches the request path against an RE2 regular
	// expression. An invalid expression prevents the profile from compiling.
	PathMatchTypeRegex PathMatchType = "Regex"
)

// DefaultSecurityProfilePriority is used when SecurityProfileSpec.Priority
// is unset. It must match the default marker on that field.
const DefaultSecurityProfilePriority int32 = 1000

// StringMatchType enumerates the matching strategy used by header- and
// query-parameter value matchers.
// +kubebuilder:validation:Enum=Exact;Prefix;Regex
type StringMatchType string

const (
	// StringMatchTypeExact requires the value to equal Value verbatim.
	StringMatchTypeExact StringMatchType = "Exact"
	// StringMatchTypePrefix requires the value to start with Value.
	StringMatchTypePrefix StringMatchType = "Prefix"
	// StringMatchTypeRegex matches the request value against an RE2 regular
	// expression. An invalid expression prevents the profile from compiling.
	StringMatchTypeRegex StringMatchType = "Regex"
)

// PathMatch specifies how to match the request URL path.
type PathMatch struct {
	// Type selects the path matching strategy. Defaults to Prefix.
	// +kubebuilder:default:=Prefix
	Type PathMatchType `json:"type"`
	// Value is the match value. For Regex, it is an RE2 expression.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Value string `json:"value"`
}

// HeaderMatch filters a request by a single header's value.
// Multiple HeaderMatch entries in one RuleMatch are ANDed.
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

// QueryParamMatch filters a request by one URL query parameter.
// Multiple QueryParamMatch entries in one RuleMatch are ANDed.
//
// When a key appears more than once, only its first value is matched.
type QueryParamMatch struct {
	// Name is the case-sensitive query parameter name.
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
// Domains is required. All other fields further restrict the match.
type RuleMatch struct {
	// Domains lists target host names. "*" matches any host, and a leading
	// wildcard such as "*.example.com" matches subdomains.
	// +kubebuilder:validation:MinItems=1
	Domains []string `json:"domains"`
	// Paths lists URL path matches. Multiple entries are ORed. The query
	// string is excluded from path matching.
	// +optional
	Paths []PathMatch `json:"paths,omitempty"`
	// Methods lists HTTP methods. Multiple entries are ORed.
	// +optional
	// +kubebuilder:validation:items:Enum=GET;HEAD;POST;PUT;PATCH;DELETE;OPTIONS;CONNECT;TRACE
	Methods []string `json:"methods,omitempty"`
	// Ports lists destination ports. Multiple entries are ORed.
	//
	// An explicit authority port is used directly. Otherwise, HTTP defaults
	// to 80 and HTTPS defaults to 443. Other schemes have no default port.
	// +optional
	// +kubebuilder:validation:items:Minimum=1
	// +kubebuilder:validation:items:Maximum=65535
	Ports []int32 `json:"ports,omitempty"`
	// Schemes lists request schemes, such as "http" and "https". Multiple
	// entries are ORed, and matching is case-insensitive.
	// +optional
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=32
	// +kubebuilder:validation:items:Pattern=`^[a-zA-Z][a-zA-Z0-9+\-.]*$`
	Schemes []string `json:"schemes,omitempty"`
	// Headers lists header matches; multiple entries are ANDed.
	// +optional
	Headers []HeaderMatch `json:"headers,omitempty"`
	// QueryParams lists URL query parameter matches. Multiple entries are
	// ANDed. Values are percent-decoded before matching.
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

// TokenTransformationType identifies a credential transformation strategy.
// +kubebuilder:validation:Enum=ApiKey;AliyunSTS
type TokenTransformationType string

const (
	// TokenTransformationTypeApiKey writes a credential to one request header.
	TokenTransformationTypeApiKey TokenTransformationType = "ApiKey"
	// TokenTransformationTypeAliyunSTS replaces the credentials in an Aliyun
	// SDK request and recomputes its signature.
	TokenTransformationTypeAliyunSTS TokenTransformationType = "AliyunSTS"
)

// CredentialRefKind identifies a deprecated credential source type.
// +kubebuilder:validation:Enum=Secret;CredentialProvider
type CredentialRefKind string

const (
	// CredentialRefKindSecret identifies a Kubernetes Secret source.
	//
	// Deprecated: use CredentialRef.Secret instead.
	CredentialRefKindSecret CredentialRefKind = "Secret"
	// CredentialRefKindCredentialProvider identifies an external credential
	// provider source.
	//
	// Deprecated: use CredentialRef.CredentialProvider instead.
	CredentialRefKindCredentialProvider CredentialRefKind = "CredentialProvider" // #nosec G101 -- not a credential
)

// CredentialRef identifies the credential source for a token transformation.
// Exactly one typed source, or the deprecated Kind and Name fields, must be
// set. Typed and deprecated fields must not be combined.
type CredentialRef struct {
	// Secret references credentials stored in a Kubernetes Secret.
	// +optional
	Secret *SecretCredentialRef `json:"secret,omitempty"`
	// CredentialProvider fetches credentials from an external provider.
	// +optional
	CredentialProvider *CredentialProviderRef `json:"credentialProvider,omitempty"`

	// Kind identifies the deprecated credential source type.
	// Deprecated: use Secret or CredentialProvider.
	// +optional
	Kind CredentialRefKind `json:"kind,omitempty"`
	// Name identifies the deprecated credential source.
	// Deprecated: use Secret or CredentialProvider.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name,omitempty"`
	// Namespace is used by deprecated Secret references. It is ignored by
	// deprecated CredentialProvider references.
	// Deprecated: use Secret.Namespace instead.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Namespace string `json:"namespace,omitempty"`
}

// SecretCredentialRef references credentials stored in a Kubernetes Secret.
// The expected data keys depend on the transformation type:
//
//	ApiKey mode:    "apiKey"
//	AliyunSTS mode: "accessKeyId", "accessKeySecret", "securityToken"
type SecretCredentialRef struct {
	// Name is the Secret name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Namespace is the Secret namespace. When omitted, a SecurityProfile uses
	// its own namespace and a GlobalSecurityProfile uses the selected Pod's
	// namespace.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Namespace string `json:"namespace,omitempty"`
}

// CredentialProviderRef identifies an external credential provider.
type CredentialProviderRef struct {
	// Name is the provider name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Namespace is reserved for namespace-scoped provider lookup. It is
	// currently ignored.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Namespace string `json:"namespace,omitempty"`
	// Parameters supplies values rendered into the provider request's
	// extraMetadata field.
	// +optional
	Parameters map[string]ValueSource `json:"parameters,omitempty"`
}

// MCPToolPolicyRule defines one allow or deny rule for an MCP tool call.
type MCPToolPolicyRule struct {
	// Method is the JSON-RPC method. Only "tools/call" is currently enforced;
	// other methods pass through without policy evaluation.
	// +kubebuilder:validation:MinLength=1
	Method string `json:"method"`
	// ToolNames lists values matched against params.name. Multiple entries are
	// ORed. An empty list matches any tool name.
	// +optional
	ToolNames []string `json:"toolNames,omitempty"`
	// Action determines whether the matching tool call is allowed or denied.
	// +kubebuilder:validation:Enum=allow;deny
	Action string `json:"action"`
}

// MCPDenyResponse configures the response returned when MCP ACL denies a request.
type MCPDenyResponse struct {
	// StatusCode is the HTTP status returned to the client. Defaults to 403.
	// +kubebuilder:default:=403
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=599
	StatusCode int32 `json:"statusCode,omitempty"`
	// Body is the optional response body sent to the client.
	// +optional
	Body string `json:"body,omitempty"`
}

// MCPToolPolicySpec defines access control for MCP tool calls. Rules are
// evaluated in order, and the first match determines the result.
type MCPToolPolicySpec struct {
	// DefaultAction is the decision applied when no rule matches.
	// Defaults to "deny" (whitelist mode); set "allow" for blacklist mode.
	// +kubebuilder:validation:Enum=allow;deny
	// +kubebuilder:default:=deny
	DefaultAction string `json:"defaultAction"`
	// UnsupportedVersionAction determines how a tools/call request with a
	// missing or unsupported MCP-Protocol-Version header is handled. "deny"
	// rejects the request, while "passthrough" skips policy evaluation.
	// +kubebuilder:validation:Enum=deny;passthrough
	// +kubebuilder:default:=deny
	// +optional
	UnsupportedVersionAction string `json:"unsupportedVersionAction,omitempty"`
	// DenyResponse configures the HTTP response when a tool is denied.
	// +optional
	DenyResponse *MCPDenyResponse `json:"denyResponse,omitempty"`
	// Rules are evaluated in order. First match wins.
	// +kubebuilder:validation:MinItems=1
	Rules []MCPToolPolicyRule `json:"rules"`
}

// ApiKeyConfig configures an ApiKey transformation.
type ApiKeyConfig struct {
	// When limits the transformation to requests with a matching header.
	// +optional
	When *ActionCondition `json:"when,omitempty"`
	// TargetHeader is the request header to overwrite with the new token.
	// +kubebuilder:default:="Authorization"
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9!#$%&'*+\-.^_|~]+$`
	TargetHeader string `json:"targetHeader,omitempty"`
	// ValueTemplate is a Go template that renders the header value.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	ValueTemplate string `json:"valueTemplate"`
}

// TokenTransformationAction rewrites credentials on an outgoing request.
type TokenTransformationAction struct {
	// Disabled skips the action without removing its configuration.
	// +optional
	// +kubebuilder:default:=false
	Disabled bool `json:"disabled,omitempty"`
	// FailStrategy controls behavior when the transformation fails.
	// Defaults to Block (fail closed).
	// +optional
	// +kubebuilder:default:=Block
	FailStrategy FailStrategy `json:"failStrategy,omitempty"`
	// Type discriminates the transformation strategy. Defaults to ApiKey.
	// +optional
	// +kubebuilder:default:=ApiKey
	Type TokenTransformationType `json:"type,omitempty"`
	// CredentialRef identifies the credential source.
	CredentialRef CredentialRef `json:"credentialRef"`

	// ApiKey configures an ApiKey transformation. It is required for ApiKey
	// and ignored for AliyunSTS.
	// +optional
	ApiKey *ApiKeyConfig `json:"apiKey,omitempty"`
}

// AuditBody configures the body sent to an audit webhook. JSON and Text are
// mutually exclusive. When both are omitted, the request has an empty body.
type AuditBody struct {
	// JSON is a structured body. String values are rendered as Go templates;
	// other values are preserved. The content type is application/json.
	//
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	JSON *runtime.RawExtension `json:"json,omitempty"`
	// Text is a Go template rendered as a text/plain body.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=8192
	Text *string `json:"text,omitempty"`
}

// AuditHeader defines one HTTP header on an audit request.
type AuditHeader struct {
	// Name is the header name. Restricted to a safe subset of RFC 7230
	// tchar characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9!#$%&'*+\-.^_|~]+$`
	Name string `json:"name"`
	// Value is a Go template that renders the header value.
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
	// Body configures the request body. When omitted, the request has an empty
	// body.
	// +optional
	Body *AuditBody `json:"body,omitempty"`
}

// AuditWebhook configures an HTTP or HTTPS audit destination. Templates may
// reference Request, Pod, Profile, Rule, Inputs, Result, Response, Matched,
// and MatchedCriteria from the audit context.
type AuditWebhook struct {
	// URL is an absolute HTTP or HTTPS URL rendered as a Go template. The
	// event is dropped if rendering fails or the rendered URL is invalid.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`
	// Request configures the HTTP request. When omitted, a POST request with an
	// empty body is sent.
	// +optional
	Request *AuditRequest `json:"request,omitempty"`
	// Timeout limits each HTTP attempt. Defaults to 2s and must be between
	// 500ms and 30s.
	// +optional
	// +kubebuilder:default:="2s"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('500ms') && duration(self) <= duration('30s')",message="timeout must be between 500ms and 30s"
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// AuditAction defines an asynchronous audit event. Audit processing does not
// change the request result.
//
// Audit actions may be configured at two levels:
//   - SecurityProfileSpec.Audit: profile-wide defaults applied to every
//     matched rule.
//   - SecurityRuleActions.Audit: per-rule overrides. When non-empty, the
//     spec-level list is suppressed for that rule's matches.
//
// Each action is evaluated for every matching rule. An empty When expression
// always matches.
type AuditAction struct {
	// Name uniquely identifies this action within its containing list.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// When is a CEL expression that determines whether the event is emitted.
	// It must evaluate to a boolean. An empty expression evaluates to true.
	//
	// A compilation error prevents the profile from compiling. A runtime
	// evaluation error drops the event.
	//
	// Available variables:
	//   result   string                  one of passthrough/mutated/blocked/bypassed/error
	//   request  map<string, dyn>        host, port, path, method, scheme, headers, queryParams
	//   pod      map<string, dyn>        name, namespace, ip, labels
	//   profile  map<string, string>     name, namespace
	//   rule     map<string, string>     name (the matched rule's name)
	//   inputs   map<string, dyn>        profile-scoped inputs
	//   response map<string, dyn>        status
	//
	// Examples:
	//   result == "blocked"
	//   result in ["blocked", "bypassed"]
	//   pod.labels["team"] == "fraud" && result != "passthrough"
	//   rule.name.startsWith("pii-")
	//
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	When string `json:"when,omitempty"`
	// Webhook is the destination for this action.
	Webhook *AuditWebhook `json:"webhook"`
}

// SecurityRuleActions defines the actions executed by one matching rule.
// Actions run in this order: Bypass, Block, MCPToolPolicy, and
// TokenTransformation. Audit actions are emitted asynchronously after the
// request is resolved.
//
// Bypass, Block, and a denying MCPToolPolicy stop the remaining actions and
// rules. Non-terminal actions continue to the next configured action. Every
// matching rule executes its own actions.
type SecurityRuleActions struct {
	// Block is a terminal action that returns a configured HTTP response
	// to the client without forwarding upstream.
	// +optional
	Block *BlockAction `json:"block,omitempty"`
	// Bypass forwards the request and skips all remaining actions and rules
	// across matching profiles. False is equivalent to omission.
	// +optional
	Bypass bool `json:"bypass,omitempty"`
	// TokenTransformation rewrites request credentials.
	// Non-terminal.
	// +optional
	TokenTransformation *TokenTransformationAction `json:"tokenTransformation,omitempty"`
	// MCPToolPolicy defines inline MCP tool access control rules.
	// Non-terminal when the policy allows; terminal (like Block) when
	// denied.
	// +optional
	MCPToolPolicy *MCPToolPolicySpec `json:"mcpToolPolicy,omitempty"`
	// Audit lists rule-specific audit actions. A non-empty list replaces the
	// profile-level Audit list for this rule. An empty list inherits the
	// profile-level list.
	// +optional
	// +listType=map
	// +listMapKey=name
	Audit []AuditAction `json:"audit,omitempty"`
}

// SecurityRule is one entry in the ordered rule chain.
//
// Every matching rule executes in order until an action terminates the
// request. Mutations from earlier rules are preserved by Bypass and discarded
// by Block. A wildcard match does not prevent later, more specific rules from
// matching.
type SecurityRule struct {
	// Name uniquely identifies the rule within the profile. Used in
	// metrics and events.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Match lists match conditions. Multiple entries are ORed.
	// +kubebuilder:validation:MinItems=1
	Match []RuleMatch `json:"match"`
	// Actions configures the actions executed when this rule matches.
	Actions SecurityRuleActions `json:"actions"`
}

// ValueSource produces one provider metadata value. Exactly one of Value,
// Cel, or Template must be set. Value and Template produce strings; Cel may
// produce any JSON-compatible value.
type ValueSource struct {
	// Value is a static string emitted verbatim.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	Value *string `json:"value,omitempty"`
	// Cel is a CEL expression evaluated against the request, Pod, profile,
	// rule, and inputs context.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Cel *string `json:"cel,omitempty"`
	// Template is a Go template whose rendered output is the value.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Template *string `json:"template,omitempty"`
}

// ConfigMapInputRef references a ConfigMap whose data supplies input values.
type ConfigMapInputRef struct {
	// Name is the ConfigMap name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Namespace is the ConfigMap namespace. When omitted, a SecurityProfile
	// uses its own namespace. A GlobalSecurityProfile must set this field.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`
}

// SecurityProfileInput defines one named input source. Exactly one of
// ConfigMap or Inline must be set.
type SecurityProfileInput struct {
	// Name uniquely identifies this input within the profile.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// ConfigMap sources the input values from a ConfigMap's data.
	// +optional
	ConfigMap *ConfigMapInputRef `json:"configMap,omitempty"`
	// Inline declares the input values directly in the profile.
	// +optional
	Inline map[string]string `json:"inline,omitempty"`
}

// SecurityProfileSpec defines L7 security policy for selected Pods.
type SecurityProfileSpec struct {
	// Selector chooses the Pods to which this profile applies. An empty
	// selector matches every Pod in scope.
	Selector metav1.LabelSelector `json:"selector"`
	// Priority determines evaluation order when multiple profiles match a Pod.
	// Lower values run first. Ties are resolved by creation time, name, and
	// namespace. Defaults to DefaultSecurityProfilePriority.
	// +optional
	// +kubebuilder:default:=1000
	// +kubebuilder:validation:Minimum=0
	Priority *int32 `json:"priority,omitempty"`
	// Inputs defines named values available to CEL expressions and Go
	// templates in this profile.
	// +optional
	// +listType=map
	// +listMapKey=name
	Inputs []SecurityProfileInput `json:"inputs,omitempty"`
	// Rules is the ordered rule chain. Every matching rule executes until an
	// action terminates the request. Rules from matching profiles are combined
	// in profile evaluation order. An empty list forwards all traffic.
	// +optional
	// +listType=map
	// +listMapKey=name
	Rules []SecurityRule `json:"rules,omitempty"`
	// Audit defines the audit actions inherited by rules that do not configure
	// their own Audit list.
	// +optional
	// +listType=map
	// +listMapKey=name
	Audit []AuditAction `json:"audit,omitempty"`
}

// Standard SecurityProfile condition types.
const (
	// SecurityProfileConditionAccepted indicates the spec passed validation
	// and the rule chain compiled successfully.
	SecurityProfileConditionAccepted = "Accepted"
	// SecurityProfileConditionResolvedRefs indicates every referenced object
	// required by the profile was found and is accessible.
	SecurityProfileConditionResolvedRefs = "ResolvedRefs"
	// SecurityProfileConditionProgrammed indicates the profile was accepted
	// and is eligible for distribution to the data plane.
	SecurityProfileConditionProgrammed = "Programmed"
)

// SecurityProfileStatus captures the observed state of a SecurityProfile.
type SecurityProfileStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions summarizes the profile's current state.
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
// SecurityProfile defines namespaced L7 security policy for selected Pods.
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

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=gsp
//
// GlobalSecurityProfile defines cluster-scoped L7 security policy for
// selected Pods in all namespaces. Global and namespaced profiles share the
// same priority ordering.
type GlobalSecurityProfile struct {
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
// GlobalSecurityProfileList contains a list of GlobalSecurityProfile.
type GlobalSecurityProfileList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GlobalSecurityProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SecurityProfile{}, &SecurityProfileList{}, &GlobalSecurityProfile{}, &GlobalSecurityProfileList{})
}
