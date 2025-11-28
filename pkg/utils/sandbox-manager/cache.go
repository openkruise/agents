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

// SelectObjectFromInformerWithLabelSelector 从 informer 中按照 labelSelector 索引查询一组对象
func SelectObjectFromInformerWithLabelSelector[T metav1.Object](informer cache.SharedIndexInformer, keysAndValues ...string) ([]T, error) {
	if len(keysAndValues)%2 != 0 {
		keysAndValues = keysAndValues[:len(keysAndValues)-1]
	}

	if len(keysAndValues) == 0 {
		return []T{}, nil
	}

	// 如果只有一个键值对，使用优化逻辑
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

	// 对于多个键值对，需要分别查询然后计算交集
	resultSets := make([]map[string]T, 0, len(keysAndValues)/2)

	// 分别查询每个标签条件
	for i := 0; i < len(keysAndValues); i += 2 {
		selector := fmt.Sprintf("%s=%s", keysAndValues[i], keysAndValues[i+1])
		objs, err := informer.GetIndexer().ByIndex("labelSelector", selector)
		if err != nil {
			return nil, err
		}

		// 将结果存入map便于后续求交集
		resultSet := make(map[string]T)
		for _, obj := range objs {
			if got, ok := obj.(T); ok {
				resultSet[got.GetName()] = got
			}
		}
		resultSets = append(resultSets, resultSet)
	}

	// 计算交集
	if len(resultSets) == 0 {
		return []T{}, nil
	}

	// 以第一个集合为基准
	result := make([]T, 0)
	for key, got := range resultSets[0] {
		// 检查该对象是否在所有其他集合中都存在
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
