/*
Copyright 2020 The Kruise Authors.

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

// Adapted from openkruise/kruise pkg/util/selector.go.
// `slice.ContainsString(..., nil)` calls have been replaced with std `slices.Contains`
// to avoid pulling in `k8s.io/kubernetes/pkg/util/slice`.

package sandboxutils

import (
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IsSelectorOverlapping indicates whether selector overlaps, the criteria:
// if exist one same key has different value and not overlap, then it is judged non-overlap, for examples:
//   - a=b and a=c
//   - a in [b,c] and a not in [b,c...]
//   - a not in [b] and a not exist
//   - a=b,c=d,e=f and a=x,c=d,e=f
//
// then others is overlap：
//   - a=b and c=d
func IsSelectorOverlapping(selector1, selector2 *metav1.LabelSelector) bool {
	if selector1 == nil || selector2 == nil {
		// Nil selector matches everything in some contexts; assume overlap to stay safe.
		return true
	}
	return !(isDisjoint(selector1, selector2) || isDisjoint(selector2, selector1))
}

func isDisjoint(selector1, selector2 *metav1.LabelSelector) bool {
	// label -> values
	// a=b convert to a -> [b]
	// a in [b,c] convert to a -> [b,c]
	// a exist convert to a -> [ALL]
	matchedLabels1 := make(map[string][]string)
	for key, value := range selector1.MatchLabels {
		matchedLabels1[key] = []string{value}
	}
	for _, req := range selector1.MatchExpressions {
		switch req.Operator {
		case metav1.LabelSelectorOpIn:
			matchedLabels1[req.Key] = append(matchedLabels1[req.Key], req.Values...)
		case metav1.LabelSelectorOpExists:
			matchedLabels1[req.Key] = []string{"ALL"}
		}
	}

	for key, value := range selector2.MatchLabels {
		values, ok := matchedLabels1[key]
		if ok {
			if !slices.Contains(values, "ALL") && !slices.Contains(values, value) {
				return true
			}
		}
	}
	for _, req := range selector2.MatchExpressions {
		values, ok := matchedLabels1[req.Key]

		switch req.Operator {
		case metav1.LabelSelectorOpIn:
			if ok && !slices.Contains(values, "ALL") && !sliceOverlaps(values, req.Values) {
				return true
			}
		case metav1.LabelSelectorOpNotIn:
			if ok && sliceContains(req.Values, values) {
				return true
			}
		case metav1.LabelSelectorOpExists:
			if !ok {
				return true
			}
		case metav1.LabelSelectorOpDoesNotExist:
			if ok {
				return true
			}
		}
	}

	return false
}

func sliceOverlaps(a, b []string) bool {
	keyExist := make(map[string]bool, len(a))
	for _, key := range a {
		keyExist[key] = true
	}
	for _, key := range b {
		if keyExist[key] {
			return true
		}
	}
	return false
}

// a contains b
func sliceContains(a, b []string) bool {
	keyExist := make(map[string]bool, len(a))
	for _, key := range a {
		keyExist[key] = true
	}
	for _, key := range b {
		if !keyExist[key] {
			return false
		}
	}
	return true
}

// IsSelectorLooseOverlap indicates whether selectors overlap (indicates that selector1, selector2 have same key, and there is a certain intersection）
//  1. when selector1、selector2 don't have same key, it is considered non-overlap, e.g. selector1(a=b) and selector2(c=d)
//  2. when selector1、selector2 have same key, and matchLabels & matchExps are intersection, it is considered overlap.
func IsSelectorLooseOverlap(selector1, selector2 *metav1.LabelSelector) bool {
	matchExp1 := convertSelectorToMatchExpressions(selector1)
	matchExp2 := convertSelectorToMatchExpressions(selector2)

	for k, exp1 := range matchExp1 {
		exp2, ok := matchExp2[k]
		if !ok {
			return false
		}

		if !isMatchExpOverlap(exp1, exp2) {
			return false
		}
	}

	for k, exp2 := range matchExp2 {
		exp1, ok := matchExp1[k]
		if !ok {
			return false
		}

		if !isMatchExpOverlap(exp2, exp1) {
			return false
		}
	}

	return true
}

func isMatchExpOverlap(matchExp1, matchExp2 metav1.LabelSelectorRequirement) bool {
	switch matchExp1.Operator {
	case metav1.LabelSelectorOpIn:
		if matchExp2.Operator == metav1.LabelSelectorOpExists {
			return true
		} else if matchExp2.Operator == metav1.LabelSelectorOpIn && sliceOverlaps(matchExp2.Values, matchExp1.Values) {
			return true
		} else if matchExp2.Operator == metav1.LabelSelectorOpNotIn && !sliceContains(matchExp2.Values, matchExp1.Values) {
			return true
		}
	case metav1.LabelSelectorOpExists:
		if matchExp2.Operator == metav1.LabelSelectorOpIn || matchExp2.Operator == metav1.LabelSelectorOpNotIn ||
			matchExp2.Operator == metav1.LabelSelectorOpExists {
			return true
		}
	case metav1.LabelSelectorOpNotIn:
		if matchExp2.Operator == metav1.LabelSelectorOpExists || matchExp2.Operator == metav1.LabelSelectorOpDoesNotExist ||
			matchExp2.Operator == metav1.LabelSelectorOpNotIn {
			return true
		} else if matchExp2.Operator == metav1.LabelSelectorOpIn && !sliceContains(matchExp1.Values, matchExp2.Values) {
			return true
		}
	case metav1.LabelSelectorOpDoesNotExist:
		if matchExp2.Operator == metav1.LabelSelectorOpDoesNotExist || matchExp2.Operator == metav1.LabelSelectorOpNotIn {
			return true
		}
	}

	return false
}

func convertSelectorToMatchExpressions(selector *metav1.LabelSelector) map[string]metav1.LabelSelectorRequirement {
	matchExps := map[string]metav1.LabelSelectorRequirement{}
	for _, exp := range selector.MatchExpressions {
		matchExps[exp.Key] = exp
	}

	for k, v := range selector.MatchLabels {
		matchExps[k] = metav1.LabelSelectorRequirement{
			Operator: metav1.LabelSelectorOpIn,
			Values:   []string{v},
		}
	}

	return matchExps
}
