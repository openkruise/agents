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

package cache

import (
	"context"

	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
)

// Index name constants (consistent with sandboxcr/index.go values)
var (
	IndexSandboxPool      = "sandboxPool"
	IndexClaimedSandboxID = "sandboxID"
	IndexUser             = "user"
	IndexTemplateID       = "templateID"
	IndexCheckpointID     = "checkpointID"
)

// getTemplateFromSandbox replicates sandboxcr.GetTemplateFromSandbox to avoid circular imports.
func getTemplateFromSandbox(sbx *agentsv1alpha1.Sandbox) string {
	tmpl := sbx.GetLabels()[agentsv1alpha1.LabelSandboxTemplate]
	if tmpl == "" {
		tmpl = sbx.GetLabels()[agentsv1alpha1.LabelSandboxPool]
	}
	return tmpl
}

// IndexFunc defines a field index function for a specific resource type.
type IndexFunc struct {
	Obj       client.Object
	FieldName string
	Extract   func(obj client.Object) []string
}

// GetIndexFuncs returns all field index functions used by the cache.
// This is the single source of truth for index definitions, shared between
// AddIndexesToCache (production) and NewTestCache (testing).
func GetIndexFuncs() []IndexFunc {
	return []IndexFunc{
		{
			Obj:       &agentsv1alpha1.Sandbox{},
			FieldName: IndexSandboxPool,
			Extract: func(obj client.Object) []string {
				sbx, ok := obj.(*agentsv1alpha1.Sandbox)
				if !ok {
					return nil
				}
				state, _ := stateutils.GetSandboxState(sbx)
				if state == agentsv1alpha1.SandboxStateAvailable ||
					(state == agentsv1alpha1.SandboxStateCreating && stateutils.IsControlledBySandboxSet(sbx)) {
					tmpl := getTemplateFromSandbox(sbx)
					if tmpl != "" {
						return []string{tmpl}
					}
				}
				return nil
			},
		},
		{
			Obj:       &agentsv1alpha1.Sandbox{},
			FieldName: IndexClaimedSandboxID,
			Extract: func(obj client.Object) []string {
				sbx, ok := obj.(*agentsv1alpha1.Sandbox)
				if !ok {
					return nil
				}
				if sbx.Labels[agentsv1alpha1.LabelSandboxIsClaimed] == agentsv1alpha1.True {
					return []string{stateutils.GetSandboxID(sbx)}
				}
				return nil
			},
		},
		{
			Obj:       &agentsv1alpha1.Sandbox{},
			FieldName: IndexUser,
			Extract: func(obj client.Object) []string {
				sbx, ok := obj.(*agentsv1alpha1.Sandbox)
				if !ok {
					return nil
				}
				if user := sbx.GetAnnotations()[agentsv1alpha1.AnnotationOwner]; user != "" {
					return []string{user}
				}
				return nil
			},
		},
		{
			Obj:       &agentsv1alpha1.SandboxSet{},
			FieldName: IndexTemplateID,
			Extract: func(obj client.Object) []string {
				sbs, ok := obj.(*agentsv1alpha1.SandboxSet)
				if !ok {
					return nil
				}
				return []string{sbs.Name}
			},
		},
		{
			Obj:       &agentsv1alpha1.Checkpoint{},
			FieldName: IndexCheckpointID,
			Extract: func(obj client.Object) []string {
				cp, ok := obj.(*agentsv1alpha1.Checkpoint)
				if !ok {
					return nil
				}
				if cp.Status.CheckpointId != "" {
					return []string{cp.Status.CheckpointId}
				}
				return nil
			},
		},
		{
			Obj:       &agentsv1alpha1.Checkpoint{},
			FieldName: IndexUser,
			Extract: func(obj client.Object) []string {
				cp, ok := obj.(*agentsv1alpha1.Checkpoint)
				if !ok {
					return nil
				}
				if user := cp.GetAnnotations()[agentsv1alpha1.AnnotationOwner]; user != "" {
					return []string{user}
				}
				return nil
			},
		},
	}
}

// AddIndexesToCache registers all required field indexes on the controller-runtime cache.
func AddIndexesToCache(c ctrlcache.Cache) error {
	if c == nil {
		return nil
	}
	for _, idx := range GetIndexFuncs() {
		if err := c.IndexField(context.Background(), idx.Obj, idx.FieldName, idx.Extract); err != nil {
			return err
		}
	}
	return nil
}
