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

package sandboxcr

import (
	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

// resolveCloneAgentName resolves the expected agent name for a clone: the value
// carried by the request wins, and when it is empty the value persisted with
// the checkpoint applies, so a snapshot keeps its identity binding without the
// caller having to repeat it. An empty result means the clone is bound to no
// agent identity, and reconcileAgentNameLabels then strips the label the cloned
// pod template inherited from the source sandbox.
func resolveCloneAgentName(opts infra.CloneSandboxOptions, cp *v1alpha1.Checkpoint) string {
	if opts.AgentName != "" {
		return opts.AgentName
	}
	if cp == nil {
		return ""
	}
	return cp.GetAnnotations()[identity.AnnotationAgentName]
}

// reconcileAgentNameLabels converges the identity.AnnotationAgentName key on a
// sandbox towards agentName, the expected value the caller resolved from its
// own protocol input (E2B request metadata, SandboxClaim spec, checkpoint).
// Labels are the storage and consumption carrier for the agent name; the
// annotation is only an input channel and never survives this function.
//
// The convergence is expressed against the expected value, not against
// whatever the sandbox already carries, because a pooled sandbox may still
// carry the label written for a previous claim. Deriving the value from the
// object could not tell that residue apart from a label the current caller
// supplied on purpose (e.g. SandboxClaim spec.labels), so:
//
//   - a non-empty agentName is written to the sandbox labels and the pod
//     template labels, overwriting any pre-existing value there;
//   - an empty agentName removes the key from both, so a sandbox claimed
//     without an agent name can never inherit the previous claimer's identity;
//   - the annotation is removed in both cases, so the agent name is only ever
//     persisted on labels.
//
// A non-empty value that is not a valid Kubernetes label value (63-char limit,
// restricted character set) returns an error before anything is mutated, so
// callers can reject the request instead of failing later on an apiserver
// write. The E2B create parse path already rejects user-supplied invalid names
// via identity.ValidateAgentName, so this check is the backstop for values
// injected after parsing (deployment-specific modifiers, values restored from
// checkpoints).
//
// The pod template labels are only touched when Spec.Template is present,
// matching the existing pod-label semantics of the claim flow: sandboxes whose
// pod template is resolved through TemplateRef carry no inline template to
// mutate, so only the sandbox label is written there and the pod does not
// receive the label.
func reconcileAgentNameLabels(sbx *v1alpha1.Sandbox, agentName string) error {
	if sbx == nil {
		return nil
	}
	if err := identity.ValidateAgentName(agentName); err != nil {
		return err
	}
	// The annotation is an input channel only: drop it whether or not an agent
	// name was requested, so it never reaches the apiserver.
	delete(sbx.Annotations, identity.AnnotationAgentName)
	if agentName == "" {
		delete(sbx.Labels, identity.AnnotationAgentName)
		if sbx.Spec.Template != nil {
			delete(sbx.Spec.Template.Labels, identity.AnnotationAgentName)
		}
		return nil
	}
	if sbx.Labels == nil {
		sbx.Labels = make(map[string]string, 1)
	}
	sbx.Labels[identity.AnnotationAgentName] = agentName
	if sbx.Spec.Template != nil {
		if sbx.Spec.Template.Labels == nil {
			sbx.Spec.Template.Labels = make(map[string]string, 1)
		}
		sbx.Spec.Template.Labels[identity.AnnotationAgentName] = agentName
	}
	return nil
}
