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

package tokeninjection

import (
	"fmt"
	"regexp"
	"strings"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// CheckWhenCondition checks if the existing header value matches the when condition pattern.
// If whenCondition is nil, the condition is considered satisfied (always inject).
// The pattern uses __PLACEHOLDER__ to indicate where the existing token value sits.
// Header lookup is case-insensitive since Envoy normalizes header names to lowercase.
func CheckWhenCondition(whenCondition *v1alpha1.ActionCondition, headers map[string]string) (bool, error) {
	if whenCondition == nil {
		return true, nil
	}

	headerValue, ok := headers[strings.ToLower(whenCondition.Header)]
	if !ok || headerValue == "" {
		return false, nil
	}

	regexPattern := escapeAndBuildPattern(whenCondition.Pattern)
	matched, err := regexp.MatchString(regexPattern, headerValue)
	if err != nil {
		return false, fmt.Errorf("invalid when pattern %q: %w", whenCondition.Pattern, err)
	}

	return matched, nil
}

// escapeAndBuildPattern converts a pattern containing __PLACEHOLDER__ to a valid regex.
// __PLACEHOLDER__ is replaced with a regex that matches a typical token value (non-whitespace).
// The input pattern may contain regex anchors like ^ and $, which are preserved.
// Literal text around anchors is escaped to prevent unintended regex interpretation.
func escapeAndBuildPattern(pattern string) string {
	placeholderToken := "\x00PLACEHOLDER\x00"
	safePattern := strings.ReplaceAll(pattern, "__PLACEHOLDER__", placeholderToken)

	parts := strings.Split(safePattern, placeholderToken)

	for i, part := range parts {
		if part == "" {
			continue
		}

		var escaped string
		hasHead := strings.HasPrefix(part, "^")
		hasTail := strings.HasSuffix(part, "$")
		switch {
		case hasHead && hasTail:
			trimmed := strings.TrimSuffix(strings.TrimPrefix(part, "^"), "$")
			escaped = "^" + regexp.QuoteMeta(trimmed) + "$"
		case hasHead:
			escaped = "^" + regexp.QuoteMeta(strings.TrimPrefix(part, "^"))
		case hasTail:
			escaped = regexp.QuoteMeta(strings.TrimSuffix(part, "$")) + "$"
		default:
			escaped = regexp.QuoteMeta(part)
		}
		parts[i] = escaped
	}

	return strings.Join(parts, `\S+`)
}
