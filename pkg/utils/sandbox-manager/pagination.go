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
	"encoding/base64"
	"sort"
)

// cursorSep separates the sort key from the unique tiebreaker inside a composite
// cursor. It is the NUL byte so that the tiebreaker always orders after the sort
// key but before any printable continuation of a longer key (e.g. "1\x00z" sorts
// before "10\x00a"), keeping the composite order consistent with (key, unique).
const cursorSep = "\x00"

// Paginator pages the selected objects.
type Paginator[T any] struct {
	Limit     int
	NextToken string
	// All objects are sorted by the key, and the key of the last object is used as next token. If the key is empty, the object is not included.
	GetKey func(T) string
	// GetUniqueKey, when set, returns a stable per-object discriminator (for
	// example the sandbox or checkpoint ID) that is appended to GetKey to form
	// the ordering/cursor. GetKey values are not guaranteed unique — claim and
	// creation timestamps are formatted at RFC3339 (one-second) granularity, so
	// many objects can share a key. Paging on GetKey alone resumes at the first
	// item strictly greater than the previous page's last key, which skips every
	// other item that shares that key, so those objects never appear on any page.
	// With GetUniqueKey set, ordering and the page cursor use the composite
	// (GetKey, GetUniqueKey), which is unique, so no item is dropped at a page
	// boundary. If nil, GetKey alone is used and the caller must guarantee GetKey
	// uniqueness.
	GetUniqueKey func(T) string
	// Return true if the object should be included
	Filter func(T) bool
}

func (p *Paginator[T]) Apply(objs []T) ([]T, string) {
	sortable := make([]T, 0, len(objs))
	for _, obj := range objs {
		if !p.Filter(obj) || p.GetKey(obj) == "" {
			continue
		}
		sortable = append(sortable, obj)
	}
	sorted := p.sortObjects(sortable)
	return p.paginateResults(sorted)
}

// cursor returns the ordering key for an object: the bare sort key when no
// tiebreaker is configured, or the composite (GetKey, GetUniqueKey) otherwise.
func (p *Paginator[T]) cursor(obj T) string {
	if p.GetUniqueKey == nil {
		return p.GetKey(obj)
	}
	return p.GetKey(obj) + cursorSep + p.GetUniqueKey(obj)
}

// encodeToken renders a cursor as an opaque page token. Composite cursors carry
// a NUL separator that is not valid in an HTTP header, so they are base64url
// encoded; bare-key cursors are returned verbatim to preserve the existing token
// format for callers that do not set GetUniqueKey.
func (p *Paginator[T]) encodeToken(cursor string) string {
	if p.GetUniqueKey == nil {
		return cursor
	}
	return base64.RawURLEncoding.EncodeToString([]byte(cursor))
}

// decodeToken reverses encodeToken. A token that does not decode (legacy or
// malformed) is used as-is so the list call degrades to a best-effort resume
// rather than failing.
func (p *Paginator[T]) decodeToken(token string) string {
	if p.GetUniqueKey == nil {
		return token
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return token
	}
	return string(decoded)
}

// sortObjects sorts items by their cursor so that equal sort keys are ordered by
// the unique tiebreaker when one is configured.
func (p *Paginator[T]) sortObjects(items []T) []T {
	// Sort by cursor (string comparison, RFC3339 format is lexicographically sortable)
	sort.Slice(items, func(i, j int) bool {
		return p.cursor(items[i]) < p.cursor(items[j])
	})

	return items
}

// paginateResults applies pagination to a sorted slice of items. The cursor of the last item is used as next token.
func (p *Paginator[T]) paginateResults(items []T) ([]T, string) {
	// If limit <= 0, return all items without pagination
	if p.Limit <= 0 {
		return items, ""
	}

	// Find start index using binary search
	startIdx := 0
	if p.NextToken != "" {
		startCursor := p.decodeToken(p.NextToken)
		// Find the first item with cursor > startCursor(nextToken) in O(log n)
		startIdx = sort.Search(len(items), func(i int) bool {
			return p.cursor(items[i]) > startCursor
		})
	}

	// No more items
	if startIdx >= len(items) {
		return []T{}, ""
	}

	// Calculate end index
	endIdx := startIdx + p.Limit
	if endIdx > len(items) {
		endIdx = len(items)
	}

	paged := items[startIdx:endIdx]

	// Generate next token if there are more items
	var nextToken string
	if endIdx < len(items) && len(paged) > 0 {
		nextToken = p.encodeToken(p.cursor(paged[len(paged)-1]))
	}

	return paged, nextToken
}
