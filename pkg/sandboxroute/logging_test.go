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

package sandboxroute

import (
	"encoding/json"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/utils"
)

func TestLogMutation(t *testing.T) {
	route := Route{
		IP:              "1.2.3.4",
		ID:              "sandbox-id",
		Namespace:       "ns",
		Name:            "one",
		UID:             "uid",
		ResourceVersion: "1",
		AccessToken:     "secret-token",
	}
	tests := []struct {
		name        string
		operation   string
		result      MutationResult
		expectError bool
		expectLevel float64
		expectMsg   string
		expectKV    map[string]any
	}{
		{
			name:        "invalid mutation is reported at Error level",
			operation:   "delete",
			result:      MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute},
			expectError: true,
			expectMsg:   "route mutation rejected",
			expectKV:    map[string]any{"operation": "delete", "route": route.String(), "error": "invalid_route"},
		},
		{
			name:        "ID takeover is reported at Error level",
			operation:   "upsert",
			result:      MutationResult{Result: EventResultApplied, Reason: ReasonIDTakeover},
			expectError: true,
			expectMsg:   "route mutation ID takeover",
			expectKV: map[string]any{
				"operation": "upsert",
				"route":     route.String(),
				"reason":    "id_takeover",
				"error":     "id_takeover",
			},
		},
		{
			name:        "applied mutation logs at debug level",
			operation:   "upsert",
			result:      MutationResult{Result: EventResultApplied},
			expectLevel: float64(utils.DebugLogLevel),
			expectMsg:   "route mutation completed",
			expectKV:    map[string]any{"result": "applied"},
		},
		{
			name:        "applied deletion logs at debug level",
			operation:   "delete",
			result:      MutationResult{Result: EventResultApplied},
			expectLevel: float64(utils.DebugLogLevel),
			expectMsg:   "route mutation completed",
			expectKV:    map[string]any{"result": "applied"},
		},
		{
			name:        "ignored mutation logs at debug level",
			operation:   "delete",
			result:      MutationResult{Result: EventResultIgnored, Reason: ReasonStaleResourceVersion},
			expectLevel: float64(utils.DebugLogLevel),
			expectMsg:   "route mutation completed",
			expectKV:    map[string]any{"result": "ignored", "reason": "stale_resource_version"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var lines []map[string]any
			logger := funcr.NewJSON(func(obj string) {
				entry := map[string]any{}
				require.NoError(t, json.Unmarshal([]byte(obj), &entry))
				lines = append(lines, entry)
			}, funcr.Options{Verbosity: utils.DebugLogLevel})

			LogMutation(logger, tt.operation, route, tt.result)

			require.Len(t, lines, 1)
			line := lines[0]
			assert.Equal(t, tt.expectMsg, line["msg"])
			if tt.expectError {
				assert.NotContains(t, line, "level")
			} else {
				assert.Equal(t, tt.expectLevel, line["level"])
			}
			for k, v := range tt.expectKV {
				assert.Equal(t, v, line[k])
			}
		})
	}
}
