/*
Copyright 2025.

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
)

// RuleAction controls whether matched traffic is permitted or dropped.
//
// +kubebuilder:validation:Enum=allow;reject
type RuleAction string

const (
	// RuleActionAllow permits the matched traffic.
	RuleActionAllow RuleAction = "allow"
	// RuleActionReject reject the matched traffic.
	RuleActionReject RuleAction = "reject"
)

// TrafficPolicyServiceRef references a Kubernetes Service by name and
// optional namespace. When Namespace is omitted, the Service is looked up
// in the same namespace as the TrafficPolicy.
type TrafficPolicyServiceRef struct {
	// Name is the Service resource name.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Namespace is the namespace of the target Service. When omitted, defaults
	// to the namespace of the enclosing TrafficPolicy.
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`
}

// TrafficPolicyWorkloadRef selects pods by namespace and label selector.
// The IP addresses of all matching pods are collected into the IPSet.
type TrafficPolicyWorkloadRef struct {
	// Namespace of the target pods.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
	// Selector is a label selector that matches the target pods.
	//
	// +kubebuilder:validation:MaxProperties=10
	Selector map[string]string `json:"selector"`
}

// TrafficPolicyPeer identifies a traffic source or destination. Exactly one
// of CIDR, FQDN, Service, or Workload must be set.
//
// +kubebuilder:validation:XValidation:rule="[has(self.cidr), has(self.fqdn), has(self.service), has(self.workload)].filter(x, x).size() == 1",message="exactly one of cidr, fqdn, service, or workload must be set"
type TrafficPolicyPeer struct {
	// CIDR is an IP address range in CIDR notation (e.g. "10.0.0.0/8").
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=43
	CIDR string `json:"cidr,omitempty"`
	// FQDN is a fully qualified domain name to match (e.g. "api.example.com").
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	FQDN string `json:"fqdn,omitempty"`
	// Service references a Kubernetes Service and its selected endpoints.
	//
	// +optional
	Service *TrafficPolicyServiceRef `json:"service,omitempty"`
	// Workload selects pods by namespace and labels; their IP addresses form
	// the peer address set.
	//
	// +optional
	Workload *TrafficPolicyWorkloadRef `json:"workload,omitempty"`
}

// TrafficPolicyPort restricts a rule to specific protocol/port combinations.
// If Protocol is non-empty and Port is nil, matches all ports of that protocol.
//
// +kubebuilder:validation:XValidation:rule="!has(self.endPort) || has(self.port)",message="endPort requires port to be set"
// +kubebuilder:validation:XValidation:rule="!has(self.endPort) || self.endPort >= self.port",message="endPort must be greater than or equal to port"
type TrafficPolicyPort struct {
	// +optional
	// +kubebuilder:validation:Enum=TCP;UDP;ICMP;SCTP
	Protocol string `json:"protocol,omitempty"`
	// Port is the destination port number. When nil, the rule applies to all
	// TCP ports.
	//
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port *int32 `json:"port,omitempty"`
	// EndPort defines the upper bound of a port range (inclusive). When set,
	// the rule matches destination ports from Port to EndPort. Requires Port
	// to be set and must be >= Port.
	//
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	EndPort *int32 `json:"endPort,omitempty"`
}

// TrafficPolicyRule is one entry in the ordered rule list. Each rule
// specifies an action (allow/reject) and optional peer and port constraints.
// When From/To/Ports are all empty, the action applies to all traffic in
// that direction.
type TrafficPolicyRule struct {
	// Action determines whether matched traffic is allowed or denied.
	Action RuleAction `json:"action"`
	// From lists source peers. Multiple entries are ORed.
	//
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=20
	From []TrafficPolicyPeer `json:"from,omitempty"`
	// To lists destination peers. Multiple entries are ORed.
	//
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=20
	To []TrafficPolicyPeer `json:"to,omitempty"`
	// Ports restricts this rule to specific L4 protocol/port combinations.
	// Multiple entries are ORed. If empty, the rule matches all ports.
	//
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=20
	Ports []TrafficPolicyPort `json:"ports,omitempty"`
}

// TrafficPolicyDirection groups the ordered rule list for one traffic
// direction (ingress or egress). Rules are evaluated in order; the first
// matching rule's action is applied.
type TrafficPolicyDirection struct {
	// Rules is the ordered rule list for this direction.
	//
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=50
	Rules []TrafficPolicyRule `json:"rules,omitempty"`
}

// TrafficPolicySpec defines bidirectional policy state on selected pods.
//
// +kubebuilder:validation:XValidation:rule="has(self.ingress) || has(self.egress)",message="at least one of ingress or egress must be specified"
type TrafficPolicySpec struct {
	// Priority determines the evaluation order when multiple TrafficPolicies
	// match the same pod. Higher values are evaluated first. When two
	// policies share the same priority, the result is implementation-defined.
	//
	// +optional
	// +kubebuilder:default:=1000
	// +kubebuilder:validation:Minimum=0
	Priority int32 `json:"priority,omitempty"`

	// Selector chooses the pods this policy applies to. Standard
	// LabelSelector semantics: an EMPTY selector matches EVERY pod within
	// the policy's scope (namespace for TrafficPolicy, cluster-wide for
	// GlobalTrafficPolicy).
	Selector metav1.LabelSelector `json:"selector"`

	// Ingress defines rules applied to inbound traffic of selected pods.
	//
	// +optional
	Ingress *TrafficPolicyDirection `json:"ingress,omitempty"`
	// Egress defines rules applied to outbound traffic of selected pods.
	//
	// +optional
	Egress *TrafficPolicyDirection `json:"egress,omitempty"`
}

// TrafficPolicyConditionType represents a standard condition type for
// TrafficPolicy status reporting.
type TrafficPolicyConditionType string

// Standard TrafficPolicy condition types.
const (
	// TrafficPolicyConditionAccepted indicates the spec passed validation.
	TrafficPolicyConditionAccepted TrafficPolicyConditionType = "Accepted"
)

// TrafficPolicyStatus captures the observed state of TrafficPolicy.
type TrafficPolicyStatus struct {
	// Conditions summarises the policy's current state. Standard types are
	// Accepted and Programmed (see TrafficPolicyCondition* constants).
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=20
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=tp
// +kubebuilder:printcolumn:name="Priority",type="integer",JSONPath=".spec.priority"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
//
// TrafficPolicy defines bidirectional traffic rules for selected pods.
type TrafficPolicy struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec TrafficPolicySpec `json:"spec,omitempty"`
	// +optional
	Status TrafficPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
//
// TrafficPolicyList contains a list of TrafficPolicy.
type TrafficPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TrafficPolicy `json:"items"`
}

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=gtp
// +kubebuilder:printcolumn:name="Priority",type="integer",JSONPath=".spec.priority"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
//
// GlobalTrafficPolicy defines bidirectional traffic rules cluster-wide.
type GlobalTrafficPolicy struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec TrafficPolicySpec `json:"spec,omitempty"`
	// +optional
	Status TrafficPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
//
// GlobalTrafficPolicyList contains a list of GlobalTrafficPolicy.
type GlobalTrafficPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GlobalTrafficPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TrafficPolicy{}, &TrafficPolicyList{}, &GlobalTrafficPolicy{}, &GlobalTrafficPolicyList{})
}
