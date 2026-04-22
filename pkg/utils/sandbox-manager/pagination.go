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
	"sort"
)

// Paginator pages the selected objects.
type Paginator[T any] struct {
	Limit     int
	NextToken string
	// All objects are sorted by the key, and the key of the last object is used as next token. If the key is empty, the object is not included.
	GetKey func(T) string
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

// sortObjects retrieves objects from informer and sorts them by annotation.
func (p *Paginator[T]) sortObjects(items []T) []T {
	// Sort by annotation value (string comparison, RFC3339 format is lexicographically sortable)
	sort.Slice(items, func(i, j int) bool {
		iSortKey := p.GetKey(items[i])
		jSortKey := p.GetKey(items[j])
		return iSortKey < jSortKey
	})

	return items
}

// paginateResults applies pagination to a sorted slice of items. The sortKey of the last item is used as next token.
func (p *Paginator[T]) paginateResults(items []T) ([]T, string) {
	// If limit <= 0, return all items without pagination
	if p.Limit <= 0 {
		return items, ""
	}

	// Find start index using binary search
	startIdx := 0
	if p.NextToken != "" {
		// Find the first item with sortKey > startSortKey(nextToken) in O(log n)
		startIdx = sort.Search(len(items), func(i int) bool {
			return p.GetKey(items[i]) > p.NextToken
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
		nextToken = p.GetKey(paged[len(paged)-1])
	}

	return paged, nextToken
}
