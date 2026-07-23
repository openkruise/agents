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
	"sync"

	"k8s.io/apimachinery/pkg/types"
)

// EventResult identifies the result of a route mutation event.
type EventResult string

const (
	EventResultApplied EventResult = "applied"
	EventResultIgnored EventResult = "ignored"
	EventResultInvalid EventResult = "invalid"
)

// Reason identifies a fixed explanation for a mutation result.
type Reason string

const (
	ReasonNone                 Reason = ""
	ReasonInvalidRoute         Reason = "invalid_route"
	ReasonStaleResourceVersion Reason = "stale_resource_version"
	ReasonInvalidObjectKey     Reason = "invalid_object_key"
)

// MutationResult describes the outcome of one Store mutation request.
type MutationResult struct {
	Result EventResult
	Reason Reason
}

// Store owns source records, deletion fences, and an active ID-to-ObjectKey index.
// A record and a deletion fence for the same ObjectKey never coexist.
type Store struct {
	mu               sync.RWMutex
	recordByObject   map[types.NamespacedName]Route
	deletionByObject map[types.NamespacedName]string
	activeKeyByID    map[string]types.NamespacedName
}

// NewStore creates an empty Store.
func NewStore() *Store {
	return &Store{
		recordByObject:   make(map[types.NamespacedName]Route),
		deletionByObject: make(map[types.NamespacedName]string),
		activeKeyByID:    make(map[string]types.NamespacedName),
	}
}
