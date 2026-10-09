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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveEgressPolicy(t *testing.T) {
	for _, tt := range []struct {
		name, body  string
		wantDefault string
		wantRules   int
		wantError   bool
	}{
		{"absent", `null`, "", 0, false},
		{"empty means deny", `{}`, "deny", 0, false},
		{"explicit allow", `{"defaultAction":"allow"}`, "allow", 0, false},
		{"ordered conflict", `{"egress":[{"action":"deny","target":"192.0.2.1"},{"action":"allow","target":"192.0.2.0/24"},{"action":"allow","target":"Api.Example.com."}]}`, "deny", 3, false},
		{"bad default", `{"defaultAction":"drop"}`, "", 0, true},
		{"wildcard rejected", `{"egress":[{"action":"allow","target":"*.example.com"}]}`, "", 0, true},
		{"missing target", `{"egress":[{"action":"allow"}]}`, "", 0, true},
		{"bad action", `{"egress":[{"action":"drop","target":"example.com"}]}`, "", 0, true},
		{"bad shape", `{"egress":[true]}`, "", 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var input *NetworkPolicy
			require.NoError(t, json.Unmarshal([]byte(tt.body), &input))
			got, err := resolveEgressPolicy(input)
			if tt.wantError {
				require.NotNil(t, err)
				assert.Equal(t, 400, err.Code)
				return
			}
			require.Nil(t, err)
			if input == nil {
				assert.Nil(t, got)
				return
			}
			assert.Equal(t, tt.wantDefault, got.DefaultAction)
			require.Len(t, got.Rules, tt.wantRules)
			if tt.wantRules == 3 {
				assert.Equal(t, "deny", got.Rules[0].Action)
				assert.Equal(t, "192.0.2.1/32", got.Rules[0].CIDR)
				assert.Equal(t, "192.0.2.0/24", got.Rules[1].CIDR)
				assert.Equal(t, "api.example.com", got.Rules[2].FQDN)
			}
		})
	}
}
