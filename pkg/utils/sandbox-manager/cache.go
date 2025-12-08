package utils

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

var LabelSelectorIndexName = "labelSelector"

// AddLabelSelectorIndexerToInformer add label selector indexer to informer
func AddLabelSelectorIndexerToInformer[T metav1.Object](informer cache.SharedIndexInformer) error {
	return informer.AddIndexers(cache.Indexers{
		LabelSelectorIndexName: func(obj interface{}) ([]string, error) {
			result, ok := obj.(T)
			if !ok {
				return []string{}, nil
			}
			var indices []string
			for key, value := range result.GetLabels() {
				indices = append(indices, key+"="+value)
			}
			return indices, nil
		},
	})
}

// SelectObjectFromInformerWithLabelSelector selects a group of objects from informer according to labelSelector index
func SelectObjectFromInformerWithLabelSelector[T metav1.Object](informer cache.SharedIndexInformer, keysAndValues ...string) ([]T, error) {
	if len(keysAndValues)%2 != 0 {
		keysAndValues = keysAndValues[:len(keysAndValues)-1]
	}

	if len(keysAndValues) == 0 {
		return []T{}, nil
	}

	// If there is only one key-value pair, use optimized logic
	if len(keysAndValues) == 2 {
		selector := fmt.Sprintf("%s=%s", keysAndValues[0], keysAndValues[1])
		objs, err := informer.GetIndexer().ByIndex(LabelSelectorIndexName, selector)
		if err != nil {
			return nil, err
		}

		results := make([]T, 0, len(objs))
		for _, obj := range objs {
			if got, ok := obj.(T); ok {
				results = append(results, got)
			}
		}
		return results, nil
	}

	// For multiple key-value pairs, need to query separately and then calculate intersection
	resultSets := make([]map[string]T, 0, len(keysAndValues)/2)

	// Query each label condition separately
	for i := 0; i < len(keysAndValues); i += 2 {
		selector := fmt.Sprintf("%s=%s", keysAndValues[i], keysAndValues[i+1])
		objs, err := informer.GetIndexer().ByIndex("labelSelector", selector)
		if err != nil {
			return nil, err
		}

		// Store results in map for subsequent intersection calculation
		resultSet := make(map[string]T)
		for _, obj := range objs {
			if got, ok := obj.(T); ok {
				resultSet[got.GetName()] = got
			}
		}
		resultSets = append(resultSets, resultSet)
	}

	// Calculate intersection
	if len(resultSets) == 0 {
		return []T{}, nil
	}

	// Use the first set as baseline
	result := make([]T, 0)
	for key, got := range resultSets[0] {
		// Check if the object exists in all other sets
		foundInAll := true
		for j := 1; j < len(resultSets); j++ {
			if _, exists := resultSets[j][key]; !exists {
				foundInAll = false
				break
			}
		}

		if foundInAll {
			result = append(result, got)
		}
	}

	return result, nil
}
