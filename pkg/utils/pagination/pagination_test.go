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

package pagination

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func newSandbox(name string) *v1alpha1.Sandbox {
	return &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
}

func newSandboxWithSortKey(name, sortKey string) *v1alpha1.Sandbox {
	sbx := newSandbox(name)
	sbx.Annotations = map[string]string{"sort-key": sortKey}
	return sbx
}

func TestPaginator_Apply(t *testing.T) {
	// Helper to extract names from sandbox slice
	getNames := func(sbxs []*v1alpha1.Sandbox) []string {
		names := make([]string, len(sbxs))
		for i, sbx := range sbxs {
			names[i] = sbx.GetName()
		}
		return names
	}

	// Default GetKey function
	defaultGetKey := func(sbx *v1alpha1.Sandbox) string {
		return sbx.GetName()
	}

	// Default Filter function (include all)
	defaultFilter := func(sbx *v1alpha1.Sandbox) bool {
		return true
	}

	tests := []struct {
		name          string
		input         []*v1alpha1.Sandbox
		limit         int
		nextToken     string
		getKey        func(*v1alpha1.Sandbox) string
		filter        func(*v1alpha1.Sandbox) bool
		expectedNames []string
		expectedToken string
	}{
		{
			name:          "limit=0, no token: return all objects sorted by name",
			input:         []*v1alpha1.Sandbox{newSandbox("c"), newSandbox("a"), newSandbox("b")},
			limit:         0,
			nextToken:     "",
			getKey:        defaultGetKey,
			filter:        defaultFilter,
			expectedNames: []string{"a", "b", "c"},
			expectedToken: "",
		},
		{
			name:          "limit > total count, no token: return all objects",
			input:         []*v1alpha1.Sandbox{newSandbox("c"), newSandbox("a"), newSandbox("b")},
			limit:         10,
			nextToken:     "",
			getKey:        defaultGetKey,
			filter:        defaultFilter,
			expectedNames: []string{"a", "b", "c"},
			expectedToken: "",
		},
		{
			name:          "limit < total count, no token (first page): return first limit items with nextToken",
			input:         []*v1alpha1.Sandbox{newSandbox("d"), newSandbox("a"), newSandbox("c"), newSandbox("b"), newSandbox("e")},
			limit:         2,
			nextToken:     "",
			getKey:        defaultGetKey,
			filter:        defaultFilter,
			expectedNames: []string{"a", "b"},
			expectedToken: "b",
		},
		{
			name:          "limit < total count, with token (middle page): return limit items starting after token",
			input:         []*v1alpha1.Sandbox{newSandbox("d"), newSandbox("a"), newSandbox("c"), newSandbox("b"), newSandbox("e")},
			limit:         2,
			nextToken:     "b",
			getKey:        defaultGetKey,
			filter:        defaultFilter,
			expectedNames: []string{"c", "d"},
			expectedToken: "d",
		},
		{
			name:          "limit < total count, with token (last page): return remaining items without nextToken",
			input:         []*v1alpha1.Sandbox{newSandbox("d"), newSandbox("a"), newSandbox("c"), newSandbox("b"), newSandbox("e")},
			limit:         2,
			nextToken:     "d",
			getKey:        defaultGetKey,
			filter:        defaultFilter,
			expectedNames: []string{"e"},
			expectedToken: "",
		},
		{
			name:          "token beyond all objects: return empty list",
			input:         []*v1alpha1.Sandbox{newSandbox("a"), newSandbox("b"), newSandbox("c")},
			limit:         2,
			nextToken:     "z",
			getKey:        defaultGetKey,
			filter:        defaultFilter,
			expectedNames: []string{},
			expectedToken: "",
		},
		{
			name:          "empty input list: return empty list",
			input:         []*v1alpha1.Sandbox{},
			limit:         10,
			nextToken:     "",
			getKey:        defaultGetKey,
			filter:        defaultFilter,
			expectedNames: []string{},
			expectedToken: "",
		},
		{
			name:  "filter excludes some objects: pagination on filtered results",
			input: []*v1alpha1.Sandbox{newSandbox("a"), newSandbox("b"), newSandbox("c"), newSandbox("d"), newSandbox("e")},
			limit: 2,
			filter: func(sbx *v1alpha1.Sandbox) bool {
				// Only include a, c, e
				return sbx.GetName() == "a" || sbx.GetName() == "c" || sbx.GetName() == "e"
			},
			getKey:        defaultGetKey,
			expectedNames: []string{"a", "c"},
			expectedToken: "c",
		},
		{
			name:  "filter excludes all objects: return empty list",
			input: []*v1alpha1.Sandbox{newSandbox("a"), newSandbox("b"), newSandbox("c")},
			limit: 10,
			filter: func(sbx *v1alpha1.Sandbox) bool {
				return false
			},
			getKey:        defaultGetKey,
			expectedNames: []string{},
			expectedToken: "",
		},
		{
			name:   "GetKey returns empty string: objects excluded from results",
			input:  []*v1alpha1.Sandbox{newSandbox("a"), newSandbox(""), newSandbox("c"), newSandbox("b"), newSandbox("")},
			limit:  10,
			filter: defaultFilter,
			getKey: func(sbx *v1alpha1.Sandbox) string {
				return sbx.GetName()
			},
			expectedNames: []string{"a", "b", "c"},
			expectedToken: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Paginator[*v1alpha1.Sandbox]{
				Limit:     tt.limit,
				NextToken: tt.nextToken,
				GetKey:    tt.getKey,
				Filter:    tt.filter,
			}

			result, nextToken := p.Apply(tt.input)

			assert.Equal(t, tt.expectedNames, getNames(result), "returned objects mismatch")
			assert.Equal(t, tt.expectedToken, nextToken, "nextToken mismatch")
		})
	}
}

func TestPaginator_ApplyWithUniqueKey(t *testing.T) {
	getNames := func(sbxs []*v1alpha1.Sandbox) []string {
		names := make([]string, len(sbxs))
		for i, sbx := range sbxs {
			names[i] = sbx.GetName()
		}
		return names
	}

	getKey := func(sbx *v1alpha1.Sandbox) string {
		return sbx.GetAnnotations()["sort-key"]
	}
	getUniqueKey := func(sbx *v1alpha1.Sandbox) string {
		return sbx.GetName()
	}
	filter := func(sbx *v1alpha1.Sandbox) bool {
		return true
	}

	tests := []struct {
		name      string
		input     []*v1alpha1.Sandbox
		limit     int
		nextToken string
		pages     [][]string
	}{
		{
			name: "duplicate sort keys do not skip objects across pages",
			input: []*v1alpha1.Sandbox{
				newSandboxWithSortKey("c", "2024-01-01T00:00:01Z"),
				newSandboxWithSortKey("a", "2024-01-01T00:00:01Z"),
				newSandboxWithSortKey("d", "2024-01-01T00:00:02Z"),
				newSandboxWithSortKey("b", "2024-01-01T00:00:01Z"),
			},
			limit: 2,
			pages: [][]string{
				{"a", "b"},
				{"c", "d"},
			},
		},
		{
			name: "legacy raw token keeps previous sort-key semantics",
			input: []*v1alpha1.Sandbox{
				newSandboxWithSortKey("a", "2024-01-01T00:00:01Z"),
				newSandboxWithSortKey("b", "2024-01-01T00:00:01Z"),
				newSandboxWithSortKey("c", "2024-01-01T00:00:01Z"),
				newSandboxWithSortKey("d", "2024-01-01T00:00:02Z"),
				newSandboxWithSortKey("e", "2024-01-01T00:00:02Z"),
			},
			limit:     2,
			nextToken: "2024-01-01T00:00:01Z",
			pages: [][]string{
				{"d", "e"},
			},
		},
		{
			name: "limit less than or equal to zero returns all stable-sorted objects",
			input: []*v1alpha1.Sandbox{
				newSandboxWithSortKey("c", "2024-01-01T00:00:01Z"),
				newSandboxWithSortKey("a", "2024-01-01T00:00:01Z"),
				newSandboxWithSortKey("b", "2024-01-01T00:00:01Z"),
			},
			limit: 0,
			pages: [][]string{
				{"a", "b", "c"},
			},
		},
		{
			name: "empty unique key excludes object from stable cursor results",
			input: []*v1alpha1.Sandbox{
				newSandboxWithSortKey("a", "2024-01-01T00:00:01Z"),
				newSandboxWithSortKey("", "2024-01-01T00:00:01Z"),
				newSandboxWithSortKey("b", "2024-01-01T00:00:02Z"),
			},
			limit: 10,
			pages: [][]string{
				{"a", "b"},
			},
		},
		{
			name: "encoded cursor resumes at next sort key when id is past same-key objects",
			input: []*v1alpha1.Sandbox{
				newSandboxWithSortKey("a", "2024-01-01T00:00:01Z"),
				newSandboxWithSortKey("b", "2024-01-01T00:00:01Z"),
				newSandboxWithSortKey("c", "2024-01-01T00:00:02Z"),
				newSandboxWithSortKey("d", "2024-01-01T00:00:02Z"),
			},
			limit:     2,
			nextToken: encodeCursorToken("2024-01-01T00:00:01Z", "z"),
			pages: [][]string{
				{"c", "d"},
			},
		},
		{
			name: "encoded cursor beyond all objects returns empty page",
			input: []*v1alpha1.Sandbox{
				newSandboxWithSortKey("a", "2024-01-01T00:00:01Z"),
				newSandboxWithSortKey("b", "2024-01-01T00:00:02Z"),
			},
			limit:     2,
			nextToken: encodeCursorToken("2024-01-01T00:00:02Z", "z"),
			pages: [][]string{
				{},
			},
		},
		{
			name: "encoded cursor with fewer remaining objects than limit has no next token",
			input: []*v1alpha1.Sandbox{
				newSandboxWithSortKey("a", "2024-01-01T00:00:01Z"),
				newSandboxWithSortKey("b", "2024-01-01T00:00:01Z"),
				newSandboxWithSortKey("c", "2024-01-01T00:00:02Z"),
			},
			limit:     10,
			nextToken: encodeCursorToken("2024-01-01T00:00:01Z", "a"),
			pages: [][]string{
				{"b", "c"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nextToken := tt.nextToken
			for i, expectedNames := range tt.pages {
				p := &Paginator[*v1alpha1.Sandbox]{
					Limit:        tt.limit,
					NextToken:    nextToken,
					GetKey:       getKey,
					GetUniqueKey: getUniqueKey,
					Filter:       filter,
				}

				result, token := p.Apply(tt.input)

				assert.Equal(t, expectedNames, getNames(result), "page %d returned objects mismatch", i)
				if i < len(tt.pages)-1 {
					assert.NotEmpty(t, token, "page %d should have next token", i)
					decoded, ok := decodeCursorToken(token)
					assert.True(t, ok, "page %d should return an encoded cursor token", i)
					assert.NotEmpty(t, decoded.Key, "page %d token key should not be empty", i)
					assert.NotEmpty(t, decoded.ID, "page %d token id should not be empty", i)
				} else {
					assert.Empty(t, token, "last page should not have next token")
				}
				nextToken = token
			}
		})
	}
}

func TestDecodeCursorToken(t *testing.T) {
	tests := []struct {
		name       string
		token      string
		expected   cursorToken
		expectedOK bool
	}{
		{
			name:       "valid token",
			token:      encodeCursorToken("2024-01-01T00:00:01Z", "sandbox-a"),
			expected:   cursorToken{Key: "2024-01-01T00:00:01Z", ID: "sandbox-a"},
			expectedOK: true,
		},
		{
			name:       "invalid base64 token",
			token:      "not a base64 token",
			expectedOK: false,
		},
		{
			name:       "invalid json token",
			token:      base64.RawURLEncoding.EncodeToString([]byte("{")),
			expectedOK: false,
		},
		{
			name:       "missing key",
			token:      base64.RawURLEncoding.EncodeToString([]byte(`{"id":"sandbox-a"}`)),
			expectedOK: false,
		},
		{
			name:       "missing id",
			token:      base64.RawURLEncoding.EncodeToString([]byte(`{"key":"2024-01-01T00:00:01Z"}`)),
			expectedOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := decodeCursorToken(tt.token)

			assert.Equal(t, tt.expectedOK, ok)
			assert.Equal(t, tt.expected, got)
		})
	}
}
