package sandboxcr

import (
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
	"k8s.io/client-go/tools/cache"
)

var (
	IndexSandboxPool      = "sandboxPool"
	IndexClaimedSandboxID = "sandboxID"
	IndexUser             = "user"
	IndexTemplateID       = "templateID"
)

func AddIndexersToSandboxInformer(informer cache.SharedIndexInformer) error {
	return informer.AddIndexers(cache.Indexers{
		IndexSandboxPool: func(obj interface{}) ([]string, error) {
			sbx, ok := obj.(*agentsv1alpha1.Sandbox)
			if !ok {
				return []string{}, nil
			}
			var indices = make([]string, 0, 1)
			state, _ := stateutils.GetSandboxState(sbx)
			if state == agentsv1alpha1.SandboxStateAvailable ||
				(state == agentsv1alpha1.SandboxStateCreating && stateutils.IsControlledBySandboxSet(sbx)) {
				tmpl := GetTemplateFromSandbox(sbx)
				if tmpl != "" {
					indices = append(indices, tmpl)
				}
			}
			return indices, nil
		},
		IndexClaimedSandboxID: func(obj interface{}) ([]string, error) {
			sbx, ok := obj.(*agentsv1alpha1.Sandbox)
			if !ok {
				return []string{}, nil
			}
			if sbx.Labels[agentsv1alpha1.LabelSandboxIsClaimed] == "true" {
				return []string{stateutils.GetSandboxID(sbx)}, nil
			}
			return []string{}, nil
		},
		IndexUser: func(obj interface{}) ([]string, error) {
			result, ok := obj.(*agentsv1alpha1.Sandbox)
			if !ok {
				return []string{}, nil
			}
			if user := result.GetAnnotations()[agentsv1alpha1.AnnotationOwner]; user != "" {
				return []string{user}, nil
			}
			return []string{}, nil
		},
	})
}

func AddIndexersToSandboxSetInformer(informer cache.SharedIndexInformer) error {
	return informer.AddIndexers(cache.Indexers{
		IndexTemplateID: func(obj interface{}) ([]string, error) {
			result, ok := obj.(*agentsv1alpha1.SandboxSet)
			if !ok {
				return []string{}, nil
			}
			return []string{result.Name}, nil
		},
	})
}
