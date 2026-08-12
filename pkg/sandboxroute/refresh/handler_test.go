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

package refresh

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandboxroute"
)

type mutationCall struct {
	operation string
	route     sandboxroute.Route
}

type recordingMutator struct {
	upsertResult sandboxroute.MutationResult
	deleteResult sandboxroute.MutationResult
	calls        []mutationCall
	events       *[]string
}

func (m *recordingMutator) Upsert(route sandboxroute.Route) sandboxroute.MutationResult {
	m.calls = append(m.calls, mutationCall{operation: "upsert", route: route})
	*m.events = append(*m.events, "upsert")
	return m.upsertResult
}

func (m *recordingMutator) Delete(route sandboxroute.Route) sandboxroute.MutationResult {
	m.calls = append(m.calls, mutationCall{operation: "delete", route: route})
	*m.events = append(*m.events, "delete")
	return m.deleteResult
}

func TestHandler(t *testing.T) {
	applied := sandboxroute.MutationResult{Result: sandboxroute.EventResultApplied}
	ignored := sandboxroute.MutationResult{
		Result: sandboxroute.EventResultIgnored,
		Reason: sandboxroute.ReasonStaleResourceVersion,
	}
	invalid := sandboxroute.MutationResult{
		Result: sandboxroute.EventResultInvalid,
		Reason: sandboxroute.ReasonInvalidRoute,
	}

	tests := []struct {
		name             string
		rawBody          string
		route            *sandboxroute.Route
		upsertResult     sandboxroute.MutationResult
		deleteResult     sandboxroute.MutationResult
		nilAfterMutation bool
		expectStatus     int
		expectOperation  string
		expectMutations  int
		expectAfterCalls int
	}{
		{
			name:         "invalid JSON is rejected",
			rawBody:      "not-json",
			expectStatus: http.StatusBadRequest,
		},
		{
			name:         "oversized body is rejected",
			rawBody:      `{"id":"` + strings.Repeat("a", maxRefreshBodyBytes) + `"}`,
			expectStatus: http.StatusRequestEntityTooLarge,
		},
		{
			name: "legacy ID-only route is ignored",
			route: &sandboxroute.Route{
				ID:    "legacy",
				State: v1alpha1.SandboxStateDead,
			},
			expectStatus: http.StatusNoContent,
		},
		{
			name: "dead route without resource version is rejected",
			route: &sandboxroute.Route{
				Namespace: "ns",
				Name:      "sandbox",
				State:     v1alpha1.SandboxStateDead,
			},
			expectStatus: http.StatusBadRequest,
		},
		{
			name: "minimal dead route is deleted",
			route: &sandboxroute.Route{
				Namespace:       "ns",
				Name:            "sandbox",
				State:           v1alpha1.SandboxStateDead,
				ResourceVersion: "2",
			},
			deleteResult:     applied,
			expectStatus:     http.StatusNoContent,
			expectOperation:  "delete",
			expectMutations:  1,
			expectAfterCalls: 1,
		},
		{
			name:             "running route is upserted",
			route:            fullRoute(v1alpha1.SandboxStateRunning, "1"),
			upsertResult:     applied,
			expectStatus:     http.StatusNoContent,
			expectOperation:  "upsert",
			expectMutations:  1,
			expectAfterCalls: 1,
		},
		{
			name:             "available route is upserted and ignored result succeeds",
			route:            fullRoute(v1alpha1.SandboxStateAvailable, "1"),
			upsertResult:     ignored,
			expectStatus:     http.StatusNoContent,
			expectOperation:  "upsert",
			expectMutations:  1,
			expectAfterCalls: 1,
		},
		{
			name:             "paused route is upserted",
			route:            fullRoute(v1alpha1.SandboxStatePaused, "1"),
			upsertResult:     applied,
			expectStatus:     http.StatusNoContent,
			expectOperation:  "upsert",
			expectMutations:  1,
			expectAfterCalls: 1,
		},
		{
			name:             "creating route is upserted with nil callback",
			route:            fullRoute(v1alpha1.SandboxStateCreating, "1"),
			upsertResult:     applied,
			nilAfterMutation: true,
			expectStatus:     http.StatusNoContent,
			expectOperation:  "upsert",
			expectMutations:  1,
		},
		{
			name:             "unknown non-dead state is upserted",
			route:            fullRoute("future-state", "1"),
			upsertResult:     applied,
			expectStatus:     http.StatusNoContent,
			expectOperation:  "upsert",
			expectMutations:  1,
			expectAfterCalls: 1,
		},
		{
			name: "partial route delegates validation",
			route: &sandboxroute.Route{
				ID:              "short",
				Namespace:       "ns",
				UID:             types.UID("uid"),
				State:           v1alpha1.SandboxStateRunning,
				ResourceVersion: "1",
			},
			upsertResult:     invalid,
			expectStatus:     http.StatusBadRequest,
			expectOperation:  "upsert",
			expectMutations:  1,
			expectAfterCalls: 1,
		},
		{
			name:             "malformed resource version delegates validation",
			route:            fullRoute(v1alpha1.SandboxStateRunning, "invalid"),
			upsertResult:     invalid,
			expectStatus:     http.StatusBadRequest,
			expectOperation:  "upsert",
			expectMutations:  1,
			expectAfterCalls: 1,
		},
		{
			name: "invalid delete result is rejected",
			route: &sandboxroute.Route{
				Namespace:       "ns",
				Name:            "sandbox",
				State:           v1alpha1.SandboxStateDead,
				ResourceVersion: "invalid",
			},
			deleteResult:     invalid,
			expectStatus:     http.StatusBadRequest,
			expectOperation:  "delete",
			expectMutations:  1,
			expectAfterCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var events []string
			mutator := &recordingMutator{
				upsertResult: tt.upsertResult,
				deleteResult: tt.deleteResult,
				events:       &events,
			}
			afterCalls := 0
			var afterResults []sandboxroute.MutationResult
			var afterMutation func(sandboxroute.MutationResult)
			if !tt.nilAfterMutation {
				afterMutation = func(result sandboxroute.MutationResult) {
					afterCalls++
					afterResults = append(afterResults, result)
					events = append(events, "after")
				}
			}

			body := []byte(tt.rawBody)
			if tt.route != nil {
				var err error
				body, err = json.Marshal(tt.route)
				require.NoError(t, err)
			}
			request := httptest.NewRequest(http.MethodPost, Path, bytes.NewReader(body))
			response := httptest.NewRecorder()
			NewHandler(mutator, afterMutation).ServeHTTP(response, request)

			assert.Equal(t, tt.expectStatus, response.Code)
			assert.Len(t, mutator.calls, tt.expectMutations)
			if tt.expectMutations > 0 {
				assert.Equal(t, tt.expectOperation, mutator.calls[0].operation)
				assert.Equal(t, *tt.route, mutator.calls[0].route)
			}
			assert.Equal(t, tt.expectAfterCalls, afterCalls)
			if tt.expectAfterCalls > 0 {
				if tt.expectOperation == "delete" {
					assert.Equal(t, tt.deleteResult, afterResults[0])
				} else {
					assert.Equal(t, tt.upsertResult, afterResults[0])
				}
			}
			var expectEvents []string
			if tt.expectMutations > 0 {
				expectEvents = append(expectEvents, tt.expectOperation)
			}
			if tt.expectAfterCalls > 0 {
				expectEvents = append(expectEvents, "after")
			}
			assert.Equal(t, expectEvents, events)
			if tt.expectStatus == http.StatusBadRequest {
				assert.Equal(t, "invalid route refresh payload\n", response.Body.String())
			}
		})
	}
}

func fullRoute(state, resourceVersion string) *sandboxroute.Route {
	return &sandboxroute.Route{
		ID:              "short",
		Namespace:       "ns",
		Name:            "sandbox",
		UID:             types.UID("uid"),
		State:           state,
		ResourceVersion: resourceVersion,
	}
}
