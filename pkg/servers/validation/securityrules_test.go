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

package validation

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// validInlineRulesJSON is the minimal accepted inline security-rules payload.
const validInlineRulesJSON = `[{"name":"trace-header","match":[{"domains":["api.example.com"]}],` +
	`"actions":{"headerManipulation":{"set":[{"name":"x-e2e-trace","value":"abc123"}]}}}]`

// parseResolvedRules decodes the annotation value back into the API types so
// tests can pin the normalized structure, not the raw byte layout.
func parseResolvedRules(t *testing.T, raw string) []agentsv1alpha1.SecurityRule {
	t.Helper()
	var rules []agentsv1alpha1.SecurityRule
	require.NoError(t, json.Unmarshal([]byte(raw), &rules))
	return rules
}

func TestNormalizeSecurityRules(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		expectError string
		expectEmpty bool
	}{
		{
			name:        "nil rules produce no annotation",
			raw:         "null",
			expectEmpty: true,
		},
		{
			name:        "empty rules produce no annotation",
			raw:         "[]",
			expectEmpty: true,
		},
		{
			name: "valid headerManipulation rule",
			raw:  validInlineRulesJSON,
		},
		{
			name: "valid block rule",
			raw:  `[{"name":"block-evil","match":[{"domains":["evil.com"]}],"actions":{"block":{"statusCode":403}}}]`,
		},
		{
			name: "valid set and remove on distinct headers",
			raw: `[{"name":"r1","match":[{"domains":["api.example.com"]}],` +
				`"actions":{"headerManipulation":{"set":[{"name":"x-a","value":"1"}],"remove":["x-b"]}}}]`,
		},
		{
			name: "duplicate rule names rejected",
			raw: `[{"name":"r1","match":[{"domains":["a.example.com"]}],"actions":{"block":{}}},` +
				`{"name":"r1","match":[{"domains":["b.example.com"]}],"actions":{"block":{}}}]`,
			expectError: "duplicates",
		},
		{
			name:        "bypass rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"]}],"actions":{"bypass":true,"headerManipulation":{"set":[{"name":"X-A","value":"1"}]}}}]`,
			expectError: "bypass is not allowed",
		},
		{
			name:        "tokenTransformation rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"]}],"actions":{"tokenTransformation":{}}}]`,
			expectError: "tokenTransformation is not supported",
		},
		{
			name:        "mcpToolPolicy rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"]}],"actions":{"mcpToolPolicy":{"defaultAction":"deny"}}}]`,
			expectError: "mcpToolPolicy is not supported",
		},
		{
			name:        "audit rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"]}],"actions":{"headerManipulation":{"set":[{"name":"X-A","value":"1"}]},"audit":[{"name":"log"}]}}]`,
			expectError: "audit is not supported",
		},
		{
			name:        "empty actions rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"]}],"actions":{}}]`,
			expectError: "at least one of block or headerManipulation is required",
		},
		{
			name:        "same header in set and remove rejected case-insensitively",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"]}],"actions":{"headerManipulation":{"set":[{"name":"x-a","value":"1"}],"remove":["x-a"]}}}]`,
			expectError: "appears in both",
		},
		{
			name:        "uppercase set name rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"]}],"actions":{"headerManipulation":{"set":[{"name":"X-A","value":"1"}]}}}]`,
			expectError: "must be lowercase",
		},
		{
			name:        "uppercase remove name rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"]}],"actions":{"headerManipulation":{"remove":["X-B"]}}}]`,
			expectError: "must be lowercase",
		},
		{
			name:        "control character in value rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"]}],"actions":{"headerManipulation":{"set":[{"name":"x-a","value":"v\r\nx-evil: 1"}]}}}]`,
			expectError: "control character",
		},
		{
			name:        "block statusCode out of range rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"]}],"actions":{"block":{"statusCode":99}}}]`,
			expectError: "outside 100-599",
		},
		{
			name:        "invalid method rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"],"methods":["FETCH"]}],"actions":{"block":{}}}]`,
			expectError: "not a valid HTTP method",
		},
		{
			name:        "port out of range rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"],"ports":[70000]}],"actions":{"block":{}}}]`,
			expectError: "outside 1-65535",
		},
		{
			name:        "invalid scheme rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"],"schemes":["9bad"]}],"actions":{"block":{}}}]`,
			expectError: "not a valid scheme",
		},
		{
			name:        "Host in set rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"]}],"actions":{"headerManipulation":{"set":[{"name":"Host","value":"evil.com"}]}}}]`,
			expectError: "Host cannot be modified",
		},
		{
			name:        "Host in remove rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"]}],"actions":{"headerManipulation":{"remove":["host"]}}}]`,
			expectError: "Host cannot be modified",
		},
		{
			name:        "invalid header name rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"]}],"actions":{"headerManipulation":{"set":[{"name":"X A","value":"1"}]}}}]`,
			expectError: "header name",
		},
		{
			name: "set over limit rejected",
			raw: `[{"name":"r1","match":[{"domains":["a.example.com"]}],` +
				`"actions":{"headerManipulation":{"set":[` +
				strings.Join(func() []string {
					s := make([]string, maxHeaderManipulationSet+1)
					for i := range s {
						s[i] = fmt.Sprintf(`{"name":"X-H%d","value":"v"}`, i)
					}
					return s
				}(), ",") + `]}}}]`,
			expectError: "exceed the maximum",
		},
		{
			name: "remove over limit rejected",
			raw: `[{"name":"r1","match":[{"domains":["a.example.com"]}],` +
				`"actions":{"headerManipulation":{"remove":[` +
				strings.Join(func() []string {
					s := make([]string, maxHeaderManipulationRemove+1)
					for i := range s {
						s[i] = fmt.Sprintf(`"X-H%d"`, i)
					}
					return s
				}(), ",") + `]}}}]`,
			expectError: "exceed the maximum",
		},
		{
			name: "header value over limit rejected",
			raw: `[{"name":"r1","match":[{"domains":["a.example.com"]}],` +
				`"actions":{"headerManipulation":{"set":[{"name":"X-A","value":"` + strings.Repeat("v", maxHeaderValueLength+1) + `"}]}}}]`,
			expectError: "exceeds",
		},
		{
			name:        "missing match domains rejected",
			raw:         `[{"name":"r1","match":[{"domains":[]}],"actions":{"headerManipulation":{"set":[{"name":"X-A","value":"1"}]}}}]`,
			expectError: "domains is required",
		},
		{
			name:        "missing match rejected",
			raw:         `[{"name":"r1","match":[],"actions":{"headerManipulation":{"set":[{"name":"X-A","value":"1"}]}}}]`,
			expectError: "match must contain at least one entry",
		},
		{
			name: "rules over limit rejected",
			raw: "[" + strings.Join(func() []string {
				s := make([]string, maxSecurityRules+1)
				for i := range s {
					s[i] = fmt.Sprintf(`{"name":"r%d","match":[{"domains":["a.example.com"]}],"actions":{"headerManipulation":{"set":[{"name":"X-A","value":"1"}]}}}`, i)
				}
				return s
			}(), ",") + "]",
			expectError: "exceed the maximum",
		},
		{
			name:        "invalid regex in path match rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"],"paths":[{"type":"Regex","value":"["}]}],"actions":{"headerManipulation":{"set":[{"name":"X-A","value":"1"}]}}}]`,
			expectError: "does not compile",
		},
		{
			name:        "invalid path match type rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"],"paths":[{"type":"Suffix","value":"/x"}]}],"actions":{"block":{}}}]`,
			expectError: "not one of Prefix, Exact, Regex",
		},
		{
			name: "empty path match type accepted as CRD default",
			raw:  `[{"name":"r1","match":[{"domains":["a.example.com"],"paths":[{"value":"/x"}]}],"actions":{"block":{}}}]`,
		},
		{
			name:        "invalid header match name rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"],"headers":[{"name":"x a","value":"1"}]}],"actions":{"block":{}}}]`,
			expectError: "header name",
		},
		{
			name:        "empty header match value rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"],"headers":[{"name":"x-a"}]}],"actions":{"block":{}}}]`,
			expectError: "value is required",
		},
		{
			name:        "invalid header match type rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"],"headers":[{"name":"x-a","value":"1","type":"Contains"}]}],"actions":{"block":{}}}]`,
			expectError: "not one of Exact, Prefix, Regex",
		},
		{
			name:        "invalid regex in header match rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"],"headers":[{"name":"x-a","value":"[","type":"Regex"}]}],"actions":{"block":{}}}]`,
			expectError: "does not compile",
		},
		{
			name: "empty header match type accepted as CRD default",
			raw:  `[{"name":"r1","match":[{"domains":["a.example.com"],"headers":[{"name":"x-a","value":"1"}]}],"actions":{"block":{}}}]`,
		},
		{
			name:        "invalid queryParam match type rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"],"queryParams":[{"name":"q","value":"1","type":"Contains"}]}],"actions":{"block":{}}}]`,
			expectError: "not one of Exact, Prefix, Regex",
		},
		{
			name:        "invalid regex in queryParam match rejected",
			raw:         `[{"name":"r1","match":[{"domains":["a.example.com"],"queryParams":[{"name":"q","value":"[","type":"Regex"}]}],"actions":{"block":{}}}]`,
			expectError: "does not compile",
		},
		{
			name: "serialized rules over annotation size limit rejected",
			raw: "[" + strings.Join(func() []string {
				rules := make([]string, 32)
				for i := range rules {
					rules[i] = fmt.Sprintf(`{"name":"r%d","match":[{"domains":["example.com"]}],"actions":{"headerManipulation":{"set":[{"name":"x-value","value":"%s"}]}}}`, i, strings.Repeat("v", 1024))
				}
				return rules
			}(), ",") + "]",
			expectError: "serialized rules exceed the maximum of 32768 bytes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeSecurityRules(parseResolvedRules(t, tt.raw))
			if tt.expectError == "" {
				require.NoError(t, err)
				if tt.expectEmpty {
					assert.Empty(t, got)
					return
				}
				assert.NotEmpty(t, got)
				rules := parseResolvedRules(t, got)
				assert.NotEmpty(t, rules)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
		})
	}
}
