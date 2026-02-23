package sandboxcr

import (
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
	"k8s.io/client-go/tools/cache"
)

var (
	IndexTemplateAvailable = "templateAvailable"
	IndexSandboxID         = "sandboxID"
	IndexUser              = "user"
)

// AddLabelSelectorIndexerToInformer add label selector indexer to informer
func AddLabelSelectorIndexerToInformer(informer cache.SharedIndexInformer) error {
	return informer.AddIndexers(cache.Indexers{
		IndexTemplateAvailable: func(obj interface{}) ([]string, error) {
			result, ok := obj.(*agentsv1alpha1.Sandbox)
			if !ok {
				return []string{}, nil
			}
			var indices = make([]string, 0, 1)
			state, _ := stateutils.GetSandboxState(result)
			if state == agentsv1alpha1.SandboxStateAvailable {
				tmpl := GetTemplateFromSandbox(result)
				if tmpl != "" {
					indices = append(indices, tmpl)
				}
			}
			return indices, nil
		},
		IndexSandboxID: func(obj interface{}) ([]string, error) {
			result, ok := obj.(*agentsv1alpha1.Sandbox)
			if !ok {
				return []string{}, nil
			}
			return []string{stateutils.GetSandboxID(result)}, nil
		},
		IndexUser: func(obj interface{}) ([]string, error) {
			result, ok := obj.(*agentsv1alpha1.Sandbox)
			if !ok {
				return []string{}, nil
			}
			return []string{result.GetAnnotations()[agentsv1alpha1.AnnotationOwner]}, nil
		},
	})
}
