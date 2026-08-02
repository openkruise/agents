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

package identity

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// ValidateAgentName reports whether an agent-name value can be mirrored into
// Kubernetes labels. An empty value is valid: it means the sandbox is not
// opted into the identity issuance path. Request parsing layers call this to
// reject an invalid user-supplied agent name up front, before any sandbox is
// picked or created; the sandboxcr infra layer reuses it when mirroring the
// annotation into the sandbox and pod template labels during claim and clone.
func ValidateAgentName(agentName string) error {
	if agentName == "" {
		return nil
	}
	if errs := validation.IsValidLabelValue(agentName); len(errs) > 0 {
		return fmt.Errorf("annotation %s value %q is not a valid label value: %s",
			AnnotationAgentName, agentName, strings.Join(errs, "; "))
	}
	return nil
}
