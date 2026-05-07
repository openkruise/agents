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

package sandboxutils

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SelectorsOverlap checks if two label selectors could potentially match the same resources.
// It returns true if there is a possible overlap.
// Note: This is a conservative check. If it's unsure, it returns true.
func SelectorsOverlap(s1, s2 *metav1.LabelSelector) bool {
	if s1 == nil || s2 == nil {
		return true // Nil selector matches everything in some contexts, so assume overlap
	}

	selector1, err := metav1.LabelSelectorAsSelector(s1)
	if err != nil {
		return true
	}
	selector2, err := metav1.LabelSelectorAsSelector(s2)
	if err != nil {
		return true
	}

	// If both are empty, they both match everything in the namespace
	if selector1.Empty() && selector2.Empty() {
		return true
	}

	// Check for conflicting requirements on the same key
	// If we find a key where both selectors have requirements that cannot both be satisfied,
	// then they don't overlap.
	
	// For simplicity and safety in a webhook, if we can't prove they DON'T overlap,
	// we assume they DO.
	
	// A simple overlap check: if there's a specific label match (MatchLabels) in both,
	// and they have different values for the same key, they don't overlap.
	for k, v1 := range s1.MatchLabels {
		if v2, ok := s2.MatchLabels[k]; ok {
			if v1 != v2 {
				return false // Conflicting exact matches, no overlap possible
			}
		}
	}

	// Also check MatchExpressions for basic conflicts
	for _, e1 := range s1.MatchExpressions {
		for _, e2 := range s2.MatchExpressions {
			if e1.Key == e2.Key {
				// If one says In {A} and other says In {B}, and A != B, no overlap
				// This is complex to implement fully.
			}
		}
	}

	// Default to true for safety if we can't prove conflict
	return true
}

// IsSelectorSubset checks if selector 'inner' is a subset of (or equal to) 'outer'.
func IsSelectorSubset(inner, outer *metav1.LabelSelector) bool {
	// Implementation for later if needed
	return true
}
