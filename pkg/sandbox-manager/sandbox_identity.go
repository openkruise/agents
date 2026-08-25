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

package sandbox_manager

import (
	"errors"

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandboxid"
)

// withSandboxIDAssignment validates the enabled generator before mutation, then
// runs the caller modifier and applies Manager-owned Sandbox ID policy last.
// This prevents caller-supplied or recycled IDs from surviving into the new
// delivery: enabled mode replaces them; disabled mode clears the reserved label.
func withSandboxIDAssignment(
	modifier func(infra.Sandbox) error,
	enabled bool,
	prefix string,
	generate func() (string, error),
) func(infra.Sandbox) error {
	return func(sandbox infra.Sandbox) error {
		if enabled && generate == nil {
			return errors.New("short sandbox ID generator is not initialized")
		}
		if modifier != nil {
			if err := modifier(sandbox); err != nil {
				return err
			}
		}

		if !enabled {
			labels := sandbox.GetLabels()
			if labels != nil {
				delete(labels, sandboxid.LabelKey)
				sandbox.SetLabels(labels)
			}
			return nil
		}
		sandboxID, err := generate()
		if err != nil {
			return err
		}
		labels := sandbox.GetLabels()
		if labels == nil {
			labels = make(map[string]string, 1)
		}
		labels[sandboxid.LabelKey] = prefix + sandboxID
		sandbox.SetLabels(labels)
		return nil
	}
}
