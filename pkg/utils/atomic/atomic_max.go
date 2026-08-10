/*
Copyright 2024 The OpenKruise Authors.

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

// Package atomic provides simple atomic primitives that are not covered by
// the standard library. Aligned with community envd's internal utilities so
// that idempotent gates (e.g. /init Timestamp guard) behave identically.
package atomic

import "sync"

// AtomicMax tracks a monotonically non-decreasing int64 value.
// Concurrent callers compete via SetToGreater; only the writer that observes
// a strictly newer value wins.
type AtomicMax struct {
	val int64
	mu  sync.Mutex
}

// NewAtomicMax returns a zero-initialised AtomicMax.
func NewAtomicMax() *AtomicMax {
	return &AtomicMax{}
}

// SetToGreater sets the stored value to newValue iff newValue is greater than
// or equal to the current value, and returns true on success. A strictly
// smaller newValue is rejected and false is returned.
//
// This is the core primitive used to gate idempotent writes against replayed
// or out-of-order requests: callers should treat false as "skip side effects".
func (a *AtomicMax) SetToGreater(newValue int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.val > newValue {
		return false
	}

	a.val = newValue

	return true
}
