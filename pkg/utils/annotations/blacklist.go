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

package annotations

import (
	"strings"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// BlackListPrefix lists annotation key prefixes reserved for internal use.
// Keys with these prefixes are managed by the sandbox controller and
// sandbox-manager themselves, so user-supplied metadata must never carry
// them and they must never be propagated onto user-facing resources such
// as the pod template.
var BlackListPrefix = []string{agentsv1alpha1.E2BPrefix, agentsv1alpha1.InternalPrefix}

// IsBlackListed reports whether the given annotation key matches one of the
// internal reserved prefixes in BlackListPrefix.
func IsBlackListed(key string) bool {
	for _, prefix := range BlackListPrefix {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// FilterBlackListed returns the subset of the given annotations whose keys
// do not match any internal reserved prefix. It returns nil when no
// annotation is eligible.
func FilterBlackListed(annotations map[string]string) map[string]string {
	if len(annotations) == 0 {
		return nil
	}
	var filtered map[string]string
	for k, v := range annotations {
		if IsBlackListed(k) {
			continue
		}
		if filtered == nil {
			filtered = make(map[string]string)
		}
		filtered[k] = v
	}
	return filtered
}
