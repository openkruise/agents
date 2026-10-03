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

package models

import "github.com/go-logr/logr"

// redactedLogValue stands in for every secret value when a request is rendered
// in a log line.
const redactedLogValue = "[redacted]"

// NewSandboxRequest is logged when a create request is received. Its env var
// values and the header values of network transform rules are user-supplied
// credentials, so log sinks, which consult logr.Marshaler before falling back to
// encoding/json, render a redacted copy instead. The request body itself is
// still decoded with encoding/json and is unaffected.
var _ logr.Marshaler = NewSandboxRequest{}

// newSandboxRequestLogView has the fields of NewSandboxRequest but none of its
// methods, so a log sink renders it directly instead of calling back into
// MarshalLog.
type newSandboxRequestLogView NewSandboxRequest

// MarshalLog implements logr.Marshaler. Env var names and header names stay
// visible for diagnostics; their values are replaced with a placeholder.
func (r NewSandboxRequest) MarshalLog() any {
	r.EnvVars = redactValues(r.EnvVars)
	if r.Network != nil && r.Network.Rules != nil {
		network := *r.Network
		network.Rules = make(map[string][]SandboxNetworkRule, len(r.Network.Rules))
		for domain, rules := range r.Network.Rules {
			redactedRules := make([]SandboxNetworkRule, len(rules))
			for i, rule := range rules {
				if rule.Transform != nil {
					transform := *rule.Transform
					transform.Headers = redactValues(transform.Headers)
					rule.Transform = &transform
				}
				redactedRules[i] = rule
			}
			network.Rules[domain] = redactedRules
		}
		r.Network = &network
	}
	return newSandboxRequestLogView(r)
}

// redactValues returns a copy of m with every value replaced by
// redactedLogValue. A nil map stays nil.
func redactValues[M ~map[string]string](m M) M {
	if m == nil {
		return nil
	}
	redacted := make(M, len(m))
	for key := range m {
		redacted[key] = redactedLogValue
	}
	return redacted
}
