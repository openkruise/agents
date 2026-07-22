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

	"github.com/openkruise/agents/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// Index name constants (consistent with sandboxcr/index.go values)
var (
	IndexSandboxPool      = "sandboxPool"
	IndexClaimedSandboxID = "sandboxID"
	IndexUser             = "user"
	IndexTemplateID       = "templateID"
	IndexCheckpointID     = "checkpointID"
	IndexVolumeName       = "volumeName"
)

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
				state, _ := utils.GetSandboxState(sbx)
				if state == agentsv1alpha1.SandboxStateAvailable ||
					(state == agentsv1alpha1.SandboxStateCreating && utils.IsControlledBySandboxSet(sbx)) {
					tmpl := utils.GetTemplateFromSandbox(sbx)
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
					return []string{utils.GetSandboxID(sbx)}
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
		{
			Obj:       &corev1.PersistentVolumeClaim{},
			FieldName: IndexUser,
			Extract: func(obj client.Object) []string {
				pvc, ok := obj.(*corev1.PersistentVolumeClaim)
				if !ok {
					return nil
				}
				if user := pvc.GetAnnotations()[agentsv1alpha1.AnnotationOwner]; user != "" {
					return []string{user}
				}
				return nil
			},
		},
		{
			Obj:       &corev1.PersistentVolumeClaim{},
			FieldName: IndexVolumeName,
			Extract: func(obj client.Object) []string {
				pvc, ok := obj.(*corev1.PersistentVolumeClaim)
				if !ok {
					return nil
				}
				if pvc.Spec.VolumeName != "" {
					return []string{pvc.Spec.VolumeName}
				}
				return nil
			},
		},
	}
}

// AddIndexesToCache registers required field indexes on the controller-runtime cache.
// When sandboxOnly is true, only indexes for the Sandbox CR are registered;
// SandboxSet, Checkpoint, and PersistentVolumeClaim indexes are skipped.
func AddIndexesToCache(c ctrlcache.Cache, sandboxOnly bool) error {
	if c == nil {
		return nil
	}
	for _, idx := range GetIndexFuncs() {
		if sandboxOnly {
			if _, ok := idx.Obj.(*agentsv1alpha1.Sandbox); !ok {
				continue
			}
		}
		if err := c.IndexField(context.Background(), idx.Obj, idx.FieldName, idx.Extract); err != nil {
			return err
		}
	}
	return nil
}
