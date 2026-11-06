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
	"cmp"
	"encoding/base64"
	"encoding/json"
	"slices"
	"sort"
	"strconv"
)

// cursorToken is a position in the sort order. An empty ID marks a raw-key
// position, which comes after every object sharing its key.
type cursorToken struct {
	Key string `json:"key"`
	ID  string `json:"id"`
}

// Paginator pages the selected objects.
type Paginator[T any] struct {
	Limit     int
	NextToken string
	// All objects are sorted by the key, and the key of the last object is used as next token. If the key is empty, the object is not included.
	GetKey func(T) string
	// GetUniqueKey is an optional stable tiebreaker for objects with the same sort key.
	GetUniqueKey func(T) string
	// Descending reverses key and tiebreaker order. Cursor comparisons follow the same direction.
	// Callers that omit it keep the historical ascending order.
	Descending bool
	// Return true if the object should be included
	Filter func(T) bool
}

func (p *Paginator[T]) Apply(objs []T) ([]T, string) {
	sortable := make([]T, 0, len(objs))
	for _, obj := range objs {
		if !p.Filter(obj) || p.GetKey(obj) == "" {
			continue
		}
		if p.hasStableCursor() && p.GetUniqueKey(obj) == "" {
			continue
		}
		sortable = append(sortable, obj)
	}
	sorted := p.sortObjects(sortable)
	return p.paginateResults(sorted)
}

// sortObjects sorts objects by key, then by unique key when stable cursors are enabled.
func (p *Paginator[T]) sortObjects(items []T) []T {
	slices.SortFunc(items, func(a, b T) int {
		return p.compare(p.cursorOf(a), p.cursorOf(b))
	})
	return items
}

// paginateResults applies pagination to a sorted slice of items. The position of the last item is used as next token.
func (p *Paginator[T]) paginateResults(items []T) ([]T, string) {
	// If limit <= 0, return all items without pagination
	if p.Limit <= 0 {
		return items, ""
	}

	// Find the first item after the token position in O(log n)
	startIdx := 0
	if p.NextToken != "" {
		start := p.startAfter()
		startIdx = sort.Search(len(items), func(i int) bool {
			return p.compare(start, p.cursorOf(items[i])) < 0
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
		nextToken = p.tokenOf(paged[len(paged)-1])
	}

	return paged, nextToken
}

// startAfter returns the position encoded in NextToken. Raw-key tokens,
// including tokens generated before stable cursors, have an empty ID.
func (p *Paginator[T]) startAfter() cursorToken {
	if p.hasStableCursor() {
		if cursor, ok := decodeCursorToken(p.NextToken); ok {
			return cursor
		}
	}
	return cursorToken{Key: p.NextToken}
}

func (p *Paginator[T]) cursorOf(obj T) cursorToken {
	if !p.hasStableCursor() {
		return cursorToken{Key: p.GetKey(obj)}
	}
	return cursorToken{Key: p.GetKey(obj), ID: p.GetUniqueKey(obj)}
}

func (p *Paginator[T]) tokenOf(obj T) string {
	cursor := p.cursorOf(obj)
	if cursor.ID == "" {
		return cursor.Key
	}
	return encodeCursorToken(cursor.Key, cursor.ID)
}

func (p *Paginator[T]) hasStableCursor() bool {
	return p.GetUniqueKey != nil
}

// compare orders cursors in the active sort direction. A raw-key position
// follows every object sharing its key in that direction.
func (p *Paginator[T]) compare(a, b cursorToken) int {
	c := cmp.Compare(a.Key, b.Key)
	if c == 0 {
		switch {
		case a.ID == b.ID:
			return 0
		case a.ID == "":
			return 1
		case b.ID == "":
			return -1
		}
		c = cmp.Compare(a.ID, b.ID)
	}
	if p.Descending {
		return -c
	}
	return c
}

func encodeCursorToken(key, id string) string {
	raw := `{"key":` + strconv.Quote(key) + `,"id":` + strconv.Quote(id) + `}`
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursorToken(token string) (cursorToken, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return cursorToken{}, false
	}

	var decoded cursorToken
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return cursorToken{}, false
	}
	if decoded.Key == "" || decoded.ID == "" {
		return cursorToken{}, false
	}
	return decoded, true
}
