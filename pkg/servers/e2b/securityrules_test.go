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

package e2b

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandboxid"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
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

func TestResolveSecurityRules_InlineJSON(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		expectError string
	}{
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := &models.NewSandboxRequest{}
			request.Extensions.SecurityRules = parseResolvedRules(t, tt.raw)
			got, err := resolveSecurityRules(request)
			if tt.expectError == "" {
				require.NoError(t, err)
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

// TestResolveSecurityRules_InlineNormalizedShape pins the exact annotation
// structure produced from the metadata entry so the data plane can rely on a
// stable server artifact.
func TestResolveSecurityRules_InlineNormalizedShape(t *testing.T) {
	request := &models.NewSandboxRequest{}
	request.Extensions.SecurityRules = parseResolvedRules(t, validInlineRulesJSON)

	got, err := resolveSecurityRules(request)
	require.NoError(t, err)

	rules := parseResolvedRules(t, got)
	require.Len(t, rules, 1)
	assert.Equal(t, "trace-header", rules[0].Name)
	require.Len(t, rules[0].Match, 1)
	assert.Equal(t, []string{"api.example.com"}, rules[0].Match[0].Domains)
	require.NotNil(t, rules[0].Actions.HeaderManipulation)
	assert.Nil(t, rules[0].Actions.Block)
	require.Len(t, rules[0].Actions.HeaderManipulation.Set, 1)
	assert.Equal(t, agentsv1alpha1.HeaderValue{Name: "x-e2e-trace", Value: "abc123"},
		rules[0].Actions.HeaderManipulation.Set[0])
}

func TestResolveSecurityRules_NetworkRules(t *testing.T) {
	tests := []struct {
		name        string
		allowOut    []string
		rules       map[string][]models.SandboxNetworkRule
		wantRules   int
		expectError string
	}{
		{
			name:     "transform headers become one rule per domain",
			allowOut: []string{"api.example.com", "api.other.com"},
			rules: map[string][]models.SandboxNetworkRule{
				"api.example.com": {{Transform: &models.SandboxNetworkTransform{
					Headers: map[string]string{"X-E2E-Trace": "abc"},
				}}},
				"api.other.com": {{Transform: &models.SandboxNetworkTransform{
					Headers: map[string]string{"Authorization": "Bearer t"},
				}}},
			},
			wantRules: 2,
		},
		{
			name:     "same domain rules merge with later value replacing",
			allowOut: []string{"api.example.com"},
			rules: map[string][]models.SandboxNetworkRule{
				"api.example.com": {
					{Transform: &models.SandboxNetworkTransform{Headers: map[string]string{"X-A": "first", "X-B": "keep"}}},
					{Transform: &models.SandboxNetworkTransform{Headers: map[string]string{"X-A": "second"}}},
				},
			},
			wantRules: 1,
		},
		{
			name: "rule without transform produces no security rule",
			rules: map[string][]models.SandboxNetworkRule{
				"api.example.com": {{}},
			},
			wantRules: 0,
		},
		{
			name: "domains folding to the same sanitized name both accepted",
			rules: map[string][]models.SandboxNetworkRule{
				"a_b.com": {{Transform: &models.SandboxNetworkTransform{Headers: map[string]string{"X-A": "1"}}}},
				"a-b.com": {{Transform: &models.SandboxNetworkTransform{Headers: map[string]string{"X-B": "2"}}}},
			},
			wantRules: 2,
		},
		{
			name: "open-egress mode accepts transforms without allowOut",
			rules: map[string][]models.SandboxNetworkRule{
				"api.example.com": {{Transform: &models.SandboxNetworkTransform{Headers: map[string]string{"X-A": "1"}}}},
			},
			wantRules: 1,
		},
		{
			name:     "transform domain absent from allowOut accepted",
			allowOut: []string{"api.other.com"},
			rules: map[string][]models.SandboxNetworkRule{
				"api.example.com": {{Transform: &models.SandboxNetworkTransform{Headers: map[string]string{"X-A": "1"}}}},
			},
			wantRules: 1,
		},
		{
			name:     "wildcard rule domain accepted alongside concrete allowOut",
			allowOut: []string{"api.example.com"},
			rules: map[string][]models.SandboxNetworkRule{
				"*.example.com": {{Transform: &models.SandboxNetworkTransform{Headers: map[string]string{"X-A": "1"}}}},
			},
			wantRules: 1,
		},
		{
			name: "empty domain key rejected",
			rules: map[string][]models.SandboxNetworkRule{
				"": {{Transform: &models.SandboxNetworkTransform{Headers: map[string]string{"X-A": "1"}}}},
			},
			expectError: "empty domain key",
		},
		{
			name: "case-variant duplicate keys in one map rejected",
			rules: map[string][]models.SandboxNetworkRule{
				"api.example.com": {{Transform: &models.SandboxNetworkTransform{Headers: map[string]string{"X-A": "one", "x-a": "two"}}}},
			},
			expectError: "same header",
		},
		{
			name: "case-variant domain keys rejected with domain-level error",
			rules: map[string][]models.SandboxNetworkRule{
				"Example.com": {{Transform: &models.SandboxNetworkTransform{Headers: map[string]string{"x-one": "1"}}}},
				"example.com": {{Transform: &models.SandboxNetworkTransform{Headers: map[string]string{"x-two": "2"}}}},
			},
			expectError: "same domain",
		},
		{
			name: "Host header in transform rejected",
			rules: map[string][]models.SandboxNetworkRule{
				"api.example.com": {{Transform: &models.SandboxNetworkTransform{Headers: map[string]string{"Host": "evil.com"}}}},
			},
			expectError: "Host cannot be modified",
		},
		{
			name: "invalid header name rejected",
			rules: map[string][]models.SandboxNetworkRule{
				"api.example.com": {{Transform: &models.SandboxNetworkTransform{Headers: map[string]string{"X A": "1"}}}},
			},
			expectError: "header name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := &models.NewSandboxRequest{
				Network: &models.SandboxNetworkConfig{AllowOut: tt.allowOut, Rules: tt.rules},
			}
			got, err := resolveSecurityRules(request)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			if tt.wantRules == 0 {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, tt.wantRules, len(parseResolvedRules(t, got)))
		})
	}
}

// TestResolveSecurityRules_NetworkNormalizedShape pins the translation of the
// native E2B network.rules entry: one headerManipulation rule per domain with
// deterministic sorted output.
func TestResolveSecurityRules_NetworkNormalizedShape(t *testing.T) {
	request := &models.NewSandboxRequest{
		Network: &models.SandboxNetworkConfig{
			AllowOut: []string{"a.example.com", "b.example.com"},
			Rules: map[string][]models.SandboxNetworkRule{
				"b.example.com": {{Transform: &models.SandboxNetworkTransform{
					Headers: map[string]string{"X-B": "2", "X-A": "1"},
				}}},
				"a.example.com": {{Transform: &models.SandboxNetworkTransform{
					Headers: map[string]string{"X-E2E-Trace": "abc"},
				}}},
			},
		},
	}

	got, err := resolveSecurityRules(request)
	require.NoError(t, err)

	// Determinism: identical input always yields an identical annotation.
	got2, err := resolveSecurityRules(request)
	require.NoError(t, err)
	assert.Equal(t, got, got2)

	rules := parseResolvedRules(t, got)
	require.Len(t, rules, 2)
	// Domains are emitted in sorted order; names carry a short hash of the
	// original domain so lossy sanitization cannot collide.
	assert.Equal(t, "e2b-rules-a.example.com-7653e495", rules[0].Name)
	assert.Equal(t, []string{"a.example.com"}, rules[0].Match[0].Domains)
	assert.Equal(t, "e2b-rules-b.example.com-cd59d56f", rules[1].Name)
	// Headers within one rule are sorted by name and normalized to lowercase
	// (HeaderValue.Name is lowercase-only in the CRD schema).
	require.Len(t, rules[1].Actions.HeaderManipulation.Set, 2)
	assert.Equal(t, "x-a", rules[1].Actions.HeaderManipulation.Set[0].Name)
	assert.Equal(t, "1", rules[1].Actions.HeaderManipulation.Set[0].Value)
	assert.Equal(t, "x-b", rules[1].Actions.HeaderManipulation.Set[1].Name)
	assert.Equal(t, "2", rules[1].Actions.HeaderManipulation.Set[1].Value)
}

// TestResolveSecurityRules_MutualExclusion locks the 400 boundary when both
// input entries are used together.
func TestResolveSecurityRules_MutualExclusion(t *testing.T) {
	request := &models.NewSandboxRequest{
		Network: &models.SandboxNetworkConfig{
			Rules: map[string][]models.SandboxNetworkRule{
				"api.example.com": {{Transform: &models.SandboxNetworkTransform{
					Headers: map[string]string{"X-A": "1"},
				}}},
			},
		},
	}
	request.Extensions.SecurityRules = parseResolvedRules(t, validInlineRulesJSON)

	_, err := resolveSecurityRules(request)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

// TestResolveSecurityRules_NoInput verifies requests without either entry keep
// today's behavior: no annotation value is produced.
func TestResolveSecurityRules_NoInput(t *testing.T) {
	got, err := resolveSecurityRules(&models.NewSandboxRequest{})
	require.NoError(t, err)
	assert.Empty(t, got)

	// network present but without rules is still no input
	request := &models.NewSandboxRequest{
		Network: &models.SandboxNetworkConfig{AllowOut: []string{"api.example.com"}},
	}
	got, err = resolveSecurityRules(request)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestParseCreateSandboxRequest_SecurityRules covers the create API boundary:
// the reserved metadata key is consumed (so the blacklist check passes), the
// normalized value lands on the request, and both entries together yield 400.
func TestParseCreateSandboxRequest_SecurityRules(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()
	user := adminTestUser()

	t.Run("metadata entry produces normalized value", func(t *testing.T) {
		req := NewRequest(t, nil, models.NewSandboxRequest{
			TemplateID: "t",
			Metadata: map[string]string{
				models.ExtensionKeySecurityRules: validInlineRulesJSON,
			},
		}, nil, user)
		parsed, apiErr := controller.parseCreateSandboxRequest(req)
		require.Nil(t, apiErr)
		assert.NotEmpty(t, parsed.SecurityRulesJSON)
		_, stillPresent := parsed.Metadata[models.ExtensionKeySecurityRules]
		assert.False(t, stillPresent, "reserved metadata key must be consumed before the blacklist check")
		rules := parseResolvedRules(t, parsed.SecurityRulesJSON)
		require.Len(t, rules, 1)
		assert.Equal(t, "trace-header", rules[0].Name)
	})

	t.Run("network entry produces normalized value", func(t *testing.T) {
		req := NewRequest(t, nil, models.NewSandboxRequest{
			TemplateID: "t",
			Network: &models.SandboxNetworkConfig{
				AllowOut: []string{"api.example.com"},
				Rules: map[string][]models.SandboxNetworkRule{
					"api.example.com": {{Transform: &models.SandboxNetworkTransform{
						Headers: map[string]string{"X-E2E-Trace": "abc"},
					}}},
				},
			},
		}, nil, user)
		parsed, apiErr := controller.parseCreateSandboxRequest(req)
		require.Nil(t, apiErr)
		rules := parseResolvedRules(t, parsed.SecurityRulesJSON)
		require.Len(t, rules, 1)
		assert.Equal(t, []string{"api.example.com"}, rules[0].Match[0].Domains)
	})

	t.Run("both entries rejected with 400", func(t *testing.T) {
		req := NewRequest(t, nil, models.NewSandboxRequest{
			TemplateID: "t",
			Metadata: map[string]string{
				models.ExtensionKeySecurityRules: validInlineRulesJSON,
			},
			Network: &models.SandboxNetworkConfig{
				Rules: map[string][]models.SandboxNetworkRule{
					"api.example.com": {{Transform: &models.SandboxNetworkTransform{
						Headers: map[string]string{"X-A": "1"},
					}}},
				},
			},
		}, nil, user)
		_, apiErr := controller.parseCreateSandboxRequest(req)
		require.NotNil(t, apiErr)
		assert.Equal(t, 400, apiErr.Code)
		assert.Contains(t, apiErr.Message, "mutually exclusive")
	})

	t.Run("unsupported egressProxy shape rejected with 400", func(t *testing.T) {
		req := NewRequest(t, nil, models.NewSandboxRequest{
			TemplateID: "t",
			Network: &models.SandboxNetworkConfig{
				EgressProxy: json.RawMessage(`{"address":"http://proxy:8080"}`),
			},
		}, nil, user)
		_, apiErr := controller.parseCreateSandboxRequest(req)
		require.NotNil(t, apiErr)
		assert.Equal(t, 400, apiErr.Code)
		assert.Contains(t, apiErr.Message, "egressProxy is not supported")
	})

	t.Run("empty metadata value rejected with 400", func(t *testing.T) {
		req := NewRequest(t, nil, models.NewSandboxRequest{
			TemplateID: "t",
			Metadata: map[string]string{
				models.ExtensionKeySecurityRules: "",
			},
		}, nil, user)
		_, apiErr := controller.parseCreateSandboxRequest(req)
		require.NotNil(t, apiErr)
		assert.Equal(t, 400, apiErr.Code)
		assert.Contains(t, apiErr.Message, "must not be empty")
	})

	t.Run("empty metadata value plus network rules fails at parse with 400", func(t *testing.T) {
		req := NewRequest(t, nil, models.NewSandboxRequest{
			TemplateID: "t",
			Metadata: map[string]string{
				models.ExtensionKeySecurityRules: "",
			},
			Network: &models.SandboxNetworkConfig{
				Rules: map[string][]models.SandboxNetworkRule{
					"api.example.com": {{Transform: &models.SandboxNetworkTransform{
						Headers: map[string]string{"X-A": "1"},
					}}},
				},
			},
		}, nil, user)
		_, apiErr := controller.parseCreateSandboxRequest(req)
		require.NotNil(t, apiErr)
		assert.Equal(t, 400, apiErr.Code)
		assert.Contains(t, apiErr.Message, "must not be empty")
	})

	t.Run("trailing second JSON value rejected with 400", func(t *testing.T) {
		req := NewRequest(t, nil, models.NewSandboxRequest{
			TemplateID: "t",
			Metadata: map[string]string{
				models.ExtensionKeySecurityRules: validInlineRulesJSON + validInlineRulesJSON,
			},
		}, nil, user)
		_, apiErr := controller.parseCreateSandboxRequest(req)
		require.NotNil(t, apiErr)
		assert.Equal(t, 400, apiErr.Code)
		assert.Contains(t, apiErr.Message, "exactly one security-rules JSON array value")
	})

	t.Run("open-egress transform without allowOut accepted", func(t *testing.T) {
		req := NewRequest(t, nil, models.NewSandboxRequest{
			TemplateID: "t",
			Network: &models.SandboxNetworkConfig{
				Rules: map[string][]models.SandboxNetworkRule{
					"api.example.com": {{Transform: &models.SandboxNetworkTransform{
						Headers: map[string]string{"X-E2E-Trace": "abc"},
					}}},
				},
			},
		}, nil, user)
		parsed, apiErr := controller.parseCreateSandboxRequest(req)
		require.Nil(t, apiErr)
		rules := parseResolvedRules(t, parsed.SecurityRulesJSON)
		require.Len(t, rules, 1)
		assert.Equal(t, []string{"api.example.com"}, rules[0].Match[0].Domains)
	})

	t.Run("transform domain absent from allowOut accepted", func(t *testing.T) {
		req := NewRequest(t, nil, models.NewSandboxRequest{
			TemplateID: "t",
			Network: &models.SandboxNetworkConfig{
				AllowOut: []string{"api.other.com"},
				Rules: map[string][]models.SandboxNetworkRule{
					"api.example.com": {{Transform: &models.SandboxNetworkTransform{
						Headers: map[string]string{"X-E2E-Trace": "abc"},
					}}},
				},
			},
		}, nil, user)
		parsed, apiErr := controller.parseCreateSandboxRequest(req)
		require.Nil(t, apiErr)
		rules := parseResolvedRules(t, parsed.SecurityRulesJSON)
		require.Len(t, rules, 1)
		assert.Equal(t, []string{"api.example.com"}, rules[0].Match[0].Domains)
	})

	t.Run("case-insensitive duplicate set names rejected with 400", func(t *testing.T) {
		req := NewRequest(t, nil, models.NewSandboxRequest{
			TemplateID: "t",
			Metadata: map[string]string{
				models.ExtensionKeySecurityRules: `[{"name":"r1","match":[{"domains":["api.example.com"]}],` +
					`"actions":{"headerManipulation":{"set":[{"name":"x-dup","value":"a"},{"name":"x-dup","value":"b"}]}}}]`,
			},
		}, nil, user)
		_, apiErr := controller.parseCreateSandboxRequest(req)
		require.NotNil(t, apiErr)
		assert.Equal(t, 400, apiErr.Code)
		assert.Contains(t, apiErr.Message, "appears more than once in set")
	})
}

// TestResolveSecurityRulesUpdate pins the PUT replacement semantics: absent
// keeps, explicit empty clears, and a non-empty map is validated like the
// creation path.
func TestResolveSecurityRulesUpdate(t *testing.T) {
	t.Run("absent rules keep the existing chain", func(t *testing.T) {
		_, present, err := resolveSecurityRulesUpdate(&models.SandboxNetworkUpdateConfig{
			AllowOut: []string{"1.2.3.4"},
		})
		require.NoError(t, err)
		assert.False(t, present)
	})

	t.Run("top-level maskRequestHost rejected", func(t *testing.T) {
		_, _, err := resolveSecurityRulesUpdate(&models.SandboxNetworkUpdateConfig{
			MaskRequestHost: ptrString("masked.example.com"),
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "maskRequestHost is not supported")
	})

	t.Run("top-level egressProxy rejected", func(t *testing.T) {
		_, _, err := resolveSecurityRulesUpdate(&models.SandboxNetworkUpdateConfig{
			EgressProxy: json.RawMessage(`{"address":"http://proxy:8080"}`),
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "egressProxy is not supported")
	})

	t.Run("explicit empty object clears the chain", func(t *testing.T) {
		got, present, err := resolveSecurityRulesUpdate(&models.SandboxNetworkUpdateConfig{
			Rules: map[string][]models.SandboxNetworkRule{},
		})
		require.NoError(t, err)
		assert.True(t, present)
		assert.Empty(t, got)
	})

	t.Run("rules without effective transform clear the chain", func(t *testing.T) {
		got, present, err := resolveSecurityRulesUpdate(&models.SandboxNetworkUpdateConfig{
			Rules: map[string][]models.SandboxNetworkRule{"api.example.com": {{}}},
		})
		require.NoError(t, err)
		assert.True(t, present)
		assert.Empty(t, got)
	})

	t.Run("open-egress replacement without allowOut accepted", func(t *testing.T) {
		got, present, err := resolveSecurityRulesUpdate(&models.SandboxNetworkUpdateConfig{
			Rules: map[string][]models.SandboxNetworkRule{
				"api.example.com": {{Transform: &models.SandboxNetworkTransform{
					Headers: map[string]string{"X-A": "1"},
				}}},
			},
		})
		require.NoError(t, err)
		assert.True(t, present)
		assert.NotEmpty(t, got)
	})

	t.Run("replacement domain absent from the update's allowOut accepted", func(t *testing.T) {
		got, present, err := resolveSecurityRulesUpdate(&models.SandboxNetworkUpdateConfig{
			AllowOut: []string{"api.other.com"},
			Rules: map[string][]models.SandboxNetworkRule{
				"api.example.com": {{Transform: &models.SandboxNetworkTransform{
					Headers: map[string]string{"X-A": "1"},
				}}},
			},
		})
		require.NoError(t, err)
		assert.True(t, present)
		assert.NotEmpty(t, got)
	})

	t.Run("valid replacement produces the normalized chain", func(t *testing.T) {
		got, present, err := resolveSecurityRulesUpdate(&models.SandboxNetworkUpdateConfig{
			AllowOut: []string{"api.example.com"},
			Rules: map[string][]models.SandboxNetworkRule{
				"api.example.com": {{Transform: &models.SandboxNetworkTransform{
					Headers: map[string]string{"X-E2E-Trace": "updated"},
				}}},
			},
		})
		require.NoError(t, err)
		assert.True(t, present)
		rules := parseResolvedRules(t, got)
		require.Len(t, rules, 1)
		assert.Equal(t, "e2b-rules-api.example.com-d0c43d38", rules[0].Name)
	})
}

// TestUpdateSandboxNetwork_RulesReplaced covers the PUT boundary end to end:
// replace writes the annotation, explicit empty clears it, absent keeps it,
// and an invalid replacement returns 400 without touching the sandbox.
func TestUpdateSandboxNetwork_RulesReplaced(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()
	templateName := "test-rules-update-template"
	cleanup := CreateSandboxPool(t, controller, templateName, 10)
	defer cleanup()
	user := adminTestUser()

	createResp, createErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: templateName,
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True,
		},
		Network: &models.SandboxNetworkConfig{
			AllowOut: []string{"api.example.com"},
			Rules: map[string][]models.SandboxNetworkRule{
				"api.example.com": {{Transform: &models.SandboxNetworkTransform{
					Headers: map[string]string{"X-E2E-Trace": "created"},
				}}},
			},
		},
	}, nil, user))
	require.Nil(t, createErr)
	sandboxID := createResp.Body.SandboxID

	readAnnotation := func() (string, bool) {
		sbxList := &agentsv1alpha1.SandboxList{}
		require.NoError(t, getTestCRClient(controller).List(t.Context(), sbxList, ctrlclient.InNamespace(Namespace)))
		for i := range sbxList.Items {
			if sandboxid.Resolve(&sbxList.Items[i]) == sandboxID {
				v, ok := sbxList.Items[i].Annotations[agentsv1alpha1.AnnotationSecurityRules]
				return v, ok
			}
		}
		t.Fatalf("sandbox %s not found", sandboxID)
		return "", false
	}

	v, ok := readAnnotation()
	require.True(t, ok)
	assert.Contains(t, v, "created")

	t.Run("invalid replacement returns 400 and keeps the chain", func(t *testing.T) {
		_, apiErr := controller.UpdateSandboxNetwork(NewRequest(t, nil, models.SandboxNetworkUpdateConfig{
			AllowOut: []string{"api.example.com"},
			Rules: map[string][]models.SandboxNetworkRule{
				"api.example.com": {{Transform: &models.SandboxNetworkTransform{
					Headers: map[string]string{"Host": "evil.com"},
				}}},
			},
		}, map[string]string{"sandboxID": sandboxID}, user))
		require.NotNil(t, apiErr)
		assert.Equal(t, 400, apiErr.Code)
		assert.Contains(t, apiErr.Message, "Host cannot be modified")
		v, _ := readAnnotation()
		assert.Contains(t, v, "created", "failed update must not change the chain")
	})

	t.Run("replacement rewrites the annotation", func(t *testing.T) {
		resp, apiErr := controller.UpdateSandboxNetwork(NewRequest(t, nil, models.SandboxNetworkUpdateConfig{
			AllowOut: []string{"api.example.com"},
			Rules: map[string][]models.SandboxNetworkRule{
				"api.example.com": {{Transform: &models.SandboxNetworkTransform{
					Headers: map[string]string{"X-E2E-Trace": "updated"},
				}}},
			},
		}, map[string]string{"sandboxID": sandboxID}, user))
		require.Nil(t, apiErr)
		assert.Equal(t, http.StatusNoContent, resp.Code)
		v, ok := readAnnotation()
		require.True(t, ok)
		assert.Contains(t, v, "updated")
		assert.NotContains(t, v, "created")
	})

	t.Run("narrowing allowOut with kept transform rules is accepted", func(t *testing.T) {
		resp, apiErr := controller.UpdateSandboxNetwork(NewRequest(t, nil, models.SandboxNetworkUpdateConfig{
			AllowOut: []string{"api.other.com"},
		}, map[string]string{"sandboxID": sandboxID}, user))
		require.Nil(t, apiErr)
		assert.Equal(t, http.StatusNoContent, resp.Code)
		v, ok := readAnnotation()
		require.True(t, ok)
		assert.Contains(t, v, "updated", "absent rules must keep the chain")
	})

	t.Run("absent rules keep the chain", func(t *testing.T) {
		_, apiErr := controller.UpdateSandboxNetwork(NewRequest(t, nil, models.SandboxNetworkUpdateConfig{
			AllowOut: []string{"1.2.3.4"},
		}, map[string]string{"sandboxID": sandboxID}, user))
		require.Nil(t, apiErr)
		v, ok := readAnnotation()
		require.True(t, ok)
		assert.Contains(t, v, "updated")
	})

	t.Run("explicit empty object clears the chain", func(t *testing.T) {
		// omitempty drops an empty map from struct marshalling, so send the
		// raw body to exercise the explicit `"rules": {}` wire shape.
		_, apiErr := controller.UpdateSandboxNetwork(NewRequest(t, nil, json.RawMessage(`{"rules":{}}`),
			map[string]string{"sandboxID": sandboxID}, user))
		require.Nil(t, apiErr)
		_, ok := readAnnotation()
		assert.False(t, ok, "explicit empty rules must remove the annotation")
	})
}

// TestResolveSecurityRules_BlockStatusCodeDefaulted pins the server-side
// default: the annotation bypasses apiserver defaulting, so `{"block":{}}`
// must persist statusCode 403 instead of 0.
func TestResolveSecurityRules_BlockStatusCodeDefaulted(t *testing.T) {
	request := &models.NewSandboxRequest{}
	request.Extensions.SecurityRules = parseResolvedRules(t,
		`[{"name":"b","match":[{"domains":["a.example.com"]}],"actions":{"block":{}}}]`)

	got, err := resolveSecurityRules(request)
	require.NoError(t, err)
	rules := parseResolvedRules(t, got)
	require.Len(t, rules, 1)
	require.NotNil(t, rules[0].Actions.Block)
	assert.Equal(t, int32(403), rules[0].Actions.Block.StatusCode)
}

func ptrString(s string) *string { return &s }
