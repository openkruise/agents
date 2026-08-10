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

package atomic

import (
	"math"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AtomicMax exposes no reader, so the stored value can only be observed through
// what a later SetToGreater accepts or rejects. probeAtOrAbove reports whether
// the stored value is at least want, by offering want-1: a rejection means the
// stored value is strictly greater than want-1, i.e. at least want.
//
// On the expected path — the stored value really is at least want — the offer is
// rejected and nothing is written, so probing is side-effect free. An offer that
// gets accepted means the assertion has already failed. want must be above
// MinInt64, since want-1 would otherwise underflow.
func probeAtOrAbove(t *testing.T, m *AtomicMax, want int64) bool {
	t.Helper()

	return !m.SetToGreater(want - 1)
}

func TestAtomicMax_SetToGreater(t *testing.T) {
	tests := []struct {
		name string
		// seed is applied in order before the assertion, so the max under test
		// starts from a known high-water mark.
		seed  []int64
		value int64
		want  bool
	}{
		{
			name:  "first write above the zero value is accepted",
			value: 1,
			want:  true,
		},
		{
			// A fresh max holds 0, and 0 is not smaller than 0, so re-writing the
			// initial value counts as accepted rather than as a stale replay.
			name:  "zero on a fresh max is accepted",
			value: 0,
			want:  true,
		},
		{
			// The zero value doubles as the low-water mark, so a negative value can
			// never be stored — not even as the very first write.
			name:  "negative on a fresh max is rejected",
			value: -1,
			want:  false,
		},
		{
			name:  "greater than the current value is accepted",
			seed:  []int64{5},
			value: 6,
			want:  true,
		},
		{
			// Equal is accepted: the guard rejects only a strictly smaller value.
			// A caller that needs "a replay of the same timestamp must be skipped"
			// does not get it from this primitive.
			name:  "equal to the current value is accepted",
			seed:  []int64{5},
			value: 5,
			want:  true,
		},
		{
			name:  "smaller than the current value is rejected",
			seed:  []int64{5},
			value: 4,
			want:  false,
		},
		{
			name:  "the highest seed wins regardless of the order it arrived in",
			seed:  []int64{1, 9, 3},
			value: 8,
			want:  false,
		},
		{
			name:  "MaxInt64 is accepted",
			seed:  []int64{5},
			value: math.MaxInt64,
			want:  true,
		},
		{
			name:  "MinInt64 is rejected",
			seed:  []int64{5},
			value: math.MinInt64,
			want:  false,
		},
		{
			// Nothing is greater than MaxInt64, so the max saturates: once there,
			// only MaxInt64 itself is still accepted (by the equal-is-accepted rule).
			name:  "MaxInt64 saturates the max",
			seed:  []int64{math.MaxInt64},
			value: math.MaxInt64,
			want:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewAtomicMax()
			for _, seed := range tt.seed {
				_ = m.SetToGreater(seed)
			}

			assert.Equal(t, tt.want, m.SetToGreater(tt.value))
		})
	}
}

// TestAtomicMax_RejectedWriteLeavesValueIntact pins down the property the guard
// exists for: a rejected write must not move the high-water mark, or a stale
// request would lower the bar for the next one.
func TestAtomicMax_RejectedWriteLeavesValueIntact(t *testing.T) {
	m := NewAtomicMax()
	require.True(t, m.SetToGreater(10))

	require.False(t, m.SetToGreater(5), "a stale value must be rejected")

	// Had the rejected 5 been stored, 9 would now be accepted.
	assert.False(t, m.SetToGreater(9), "the rejected write must not have lowered the max")
	assert.True(t, probeAtOrAbove(t, m, 10), "the max must still hold 10")
}

// TestAtomicMax_ZeroValueIsUsable asserts the constructor is a convenience, not
// a requirement: an embedded or declared AtomicMax works without it.
func TestAtomicMax_ZeroValueIsUsable(t *testing.T) {
	var m AtomicMax

	assert.True(t, m.SetToGreater(7))
	assert.False(t, m.SetToGreater(6))
	assert.True(t, probeAtOrAbove(t, &m, 7))
}

// TestAtomicMax_MonotonicSequence walks a mixed sequence and asserts the max
// never decreases: every value at or above the running high-water mark is
// accepted, every value below it is rejected.
func TestAtomicMax_MonotonicSequence(t *testing.T) {
	m := NewAtomicMax()

	sequence := []struct {
		value int64
		want  bool
	}{
		{value: 3, want: true},
		{value: 3, want: true},  // equal, still accepted
		{value: 2, want: false}, // below the mark
		{value: 4, want: true},
		{value: 1, want: false},
		{value: 100, want: true},
		{value: 99, want: false},
	}

	for _, step := range sequence {
		assert.Equal(t, step.want, m.SetToGreater(step.value), "value %d", step.value)
	}

	assert.True(t, probeAtOrAbove(t, m, 100), "the highest accepted value must be the one retained")
}

// TestAtomicMax_ConcurrentSetToGreater is the reason the type carries a mutex:
// competing writers must not corrupt the value or race. Under -race this is what
// surfaces an unsynchronised read-modify-write.
func TestAtomicMax_ConcurrentSetToGreater(t *testing.T) {
	const (
		writers   = 8
		perWriter = 100
	)

	m := NewAtomicMax()

	var wg sync.WaitGroup
	for writer := range writers {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()

			// Interleave ascending and descending offers so both branches of the
			// guard run concurrently.
			for i := range perWriter {
				_ = m.SetToGreater(int64(writer*perWriter + i))
				_ = m.SetToGreater(int64(perWriter - i))
			}
		}(writer)
	}
	wg.Wait()

	// The largest value ever offered is (writers-1)*perWriter + perWriter-1.
	highest := int64((writers-1)*perWriter + perWriter - 1)
	assert.True(t, probeAtOrAbove(t, m, highest), "the max must retain the highest offered value")
	assert.False(t, m.SetToGreater(highest-1), "nothing below the high-water mark may be accepted")
}
