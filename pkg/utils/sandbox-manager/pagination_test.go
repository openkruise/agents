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

package utils

import (
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

// sandboxWithKey builds a sandbox whose name is the unique tiebreaker and whose
// "key" annotation is the (possibly shared) sort key, mirroring how the e2b list
// API sorts by a coarse claim/creation timestamp while paging by a unique ID.
func sandboxWithKey(name, key string) *v1alpha1.Sandbox {
	return &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: map[string]string{"key": key},
		},
	}
}

// TestPaginator_Apply_DuplicateSortKeys is a regression test for silent item
// loss: when more than `limit` items share the same sort key at a page boundary,
// a key-only cursor (resume at first key strictly greater than the last key)
// skips every shared-key item after the first. With GetUniqueKey set, paging
// over the whole set must return every item exactly once.
func TestPaginator_Apply_DuplicateSortKeys(t *testing.T) {
	getKey := func(sbx *v1alpha1.Sandbox) string { return sbx.GetAnnotations()["key"] }
	getUnique := func(sbx *v1alpha1.Sandbox) string { return sbx.GetName() }
	filter := func(*v1alpha1.Sandbox) bool { return true }

	// Five sandboxes, three of them sharing the same one-second timestamp "t1".
	input := []*v1alpha1.Sandbox{
		sandboxWithKey("a", "t0"),
		sandboxWithKey("b", "t1"),
		sandboxWithKey("c", "t1"),
		sandboxWithKey("d", "t1"),
		sandboxWithKey("e", "t2"),
	}

	var got []string
	token := ""
	for i := 0; i < 10; i++ { // bounded to avoid an infinite loop on regression
		p := &Paginator[*v1alpha1.Sandbox]{
			Limit:        2,
			NextToken:    token,
			GetKey:       getKey,
			GetUniqueKey: getUnique,
			Filter:       filter,
		}
		page, next := p.Apply(input)
		for _, sbx := range page {
			got = append(got, sbx.GetName())
		}
		if next == "" {
			break
		}
		token = next
	}

	// Every item is returned exactly once, in (key, name) order — none skipped.
	assert.Equal(t, []string{"a", "b", "c", "d", "e"}, got)
}

// TestPaginator_Apply_UniqueKeyTokenRoundTrips verifies the composite token is a
// header-safe opaque string that the next page decodes back to the same cursor.
func TestPaginator_Apply_UniqueKeyTokenRoundTrips(t *testing.T) {
	getKey := func(sbx *v1alpha1.Sandbox) string { return sbx.GetAnnotations()["key"] }
	getUnique := func(sbx *v1alpha1.Sandbox) string { return sbx.GetName() }
	filter := func(*v1alpha1.Sandbox) bool { return true }

	input := []*v1alpha1.Sandbox{
		sandboxWithKey("b", "t1"),
		sandboxWithKey("c", "t1"),
		sandboxWithKey("d", "t2"),
	}

	p := &Paginator[*v1alpha1.Sandbox]{
		Limit:        2,
		GetKey:       getKey,
		GetUniqueKey: getUnique,
		Filter:       filter,
	}
	page, token := p.Apply(input)
	assert.Equal(t, []string{"b", "c"}, []string{page[0].GetName(), page[1].GetName()})
	assert.NotEmpty(t, token)
	assert.NotContains(t, token, "\x00", "token must be header-safe")

	p2 := &Paginator[*v1alpha1.Sandbox]{
		Limit:        2,
		NextToken:    token,
		GetKey:       getKey,
		GetUniqueKey: getUnique,
		Filter:       filter,
	}
	page2, token2 := p2.Apply(input)
	assert.Equal(t, []string{"d"}, []string{page2[0].GetName()})
	assert.Empty(t, token2)
}
