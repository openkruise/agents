/*
Copyright 2025.

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

package sandboxset

import (
	"sort"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// oldestFirst is the less function for sorting sandboxes by creation time ascending (oldest first).
func oldestFirst(i, j *agentsv1alpha1.Sandbox) bool {
	return i.CreationTimestamp.Before(&j.CreationTimestamp)
}

// findOldestSandboxes finds the k "smallest" sandboxes according to less from the list without
// fully sorting, it creates and returns a new sorted slice. Does not modify the input slice.
// less(i, j) should return true if i should come before j in the result (ascending order).
func findOldestSandboxes(sandboxes []*agentsv1alpha1.Sandbox, k int, less func(i, j *agentsv1alpha1.Sandbox) bool) []*agentsv1alpha1.Sandbox {
	if k <= 0 {
		return []*agentsv1alpha1.Sandbox{}
	}
	n := len(sandboxes)
	if k >= n {
		result := make([]*agentsv1alpha1.Sandbox, n)
		copy(result, sandboxes)
		sort.SliceStable(result, func(i, j int) bool { return less(result[i], result[j]) })
		return result
	}

	// Use a max-heap of size k (largest at root) to efficiently find the k smallest sandboxes.
	result := make([]*agentsv1alpha1.Sandbox, k)
	copy(result, sandboxes[:k])
	// Build max-heap from first k elements.
	for i := k/2 - 1; i >= 0; i-- {
		maxHeapify(result, i, k, less)
	}
	// Replace heap root (largest of k) whenever we find a smaller sandbox.
	for i := k; i < n; i++ {
		if less(sandboxes[i], result[0]) {
			result[0] = sandboxes[i]
			maxHeapify(result, 0, k, less)
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return less(result[i], result[j]) })
	return result
}

// maxHeapify maintains the max-heap property (largest element at root according to less) iteratively.
func maxHeapify(sandboxes []*agentsv1alpha1.Sandbox, i, heapSize int, less func(i, j *agentsv1alpha1.Sandbox) bool) {
	for {
		largest := i
		left := 2*i + 1
		right := 2*i + 2

		if left < heapSize && less(sandboxes[largest], sandboxes[left]) {
			largest = left
		}
		if right < heapSize && less(sandboxes[largest], sandboxes[right]) {
			largest = right
		}
		if largest == i {
			break
		}
		sandboxes[i], sandboxes[largest] = sandboxes[largest], sandboxes[i]
		i = largest
	}
}
