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

package opensandbox

import (
	"encoding/json"
	"testing"

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveEgressPolicy(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		want       *infra.SandboxNetworkConfig
		internet   string
		wantError  bool
	}{
		{"absent", `null`, nil, "true", false},
		{"empty means native default", `{}`, nil, "true", false},
		{"explicit allow", `{"defaultAction":"allow"}`, nil, "true", false},
		{"explicit deny uses global policy", `{"defaultAction":"deny"}`, &infra.SandboxNetworkConfig{}, "false", false},
		{"conflict groups use native precedence", `{"egress":[{"action":"deny","target":"192.0.2.1"},{"action":"allow","target":"192.0.2.0/24"},{"action":"allow","target":"Api.Example.com"}]}`, &infra.SandboxNetworkConfig{AllowOut: []string{"192.0.2.0/24", "Api.Example.com"}, DenyOut: []string{"192.0.2.1"}}, "true", false},
		{"bad default", `{"defaultAction":"drop"}`, nil, "", true},
		{"deny domain rejected", `{"egress":[{"action":"deny","target":"example.com"}]}`, nil, "", true},
		{"wildcard rejected", `{"egress":[{"action":"allow","target":"*.example.com"}]}`, nil, "", true},
		{"trailing dot follows native validation", `{"egress":[{"action":"allow","target":"example.com."}]}`, nil, "", true},
		{"missing target", `{"egress":[{"action":"allow"}]}`, nil, "", true},
		{"bad action", `{"egress":[{"action":"drop","target":"example.com"}]}`, nil, "", true},
		{"bad shape", `{"egress":[true]}`, nil, "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var input *NetworkPolicy
			require.NoError(t, json.Unmarshal([]byte(tt.body), &input))
			got, apiErr := resolveEgressPolicy(input)
			if tt.wantError {
				require.NotNil(t, apiErr)
				assert.Equal(t, 400, apiErr.Code)
				return
			}
			require.Nil(t, apiErr)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.internet, allowInternetAccess(input))
		})
	}
	for _, action := range []string{"allow", "deny"} {
		for _, count := range []int{100, 101} {
			input := &NetworkPolicy{}
			for i := 0; i < count; i++ {
				input.Egress = append(input.Egress, json.RawMessage(`{"action":"`+action+`","target":"192.0.2.1"}`))
			}
			_, err := resolveEgressPolicy(input)
			if count == 100 {
				require.Nil(t, err)
			} else {
				require.NotNil(t, err)
				assert.Equal(t, 400, err.Code)
			}
		}
	}
}
