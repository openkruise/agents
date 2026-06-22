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

package sandboxset

import (
	"context"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/fieldindex"
)

// defaultTemplateHistoryLimit is the max auto-materialised SBTs kept per SandboxSet.
const defaultTemplateHistoryLimit = 10

// ensureTemplateRevision builds the effective SandboxTemplateSpec, computes its
// revision hash, and materialises it as a SandboxTemplate CR (for inline templates).
// Returns (revisionHash, templateName, error).
func (r *Reconciler) ensureTemplateRevision(ctx context.Context, sbs *agentsv1alpha1.SandboxSet) (string, string, error) {
	spec, err := r.buildSandboxTemplateSpec(ctx, sbs)
	if err != nil {
		return "", "", err
	}
	hash, err := computeRevisionHash(spec)
	if err != nil {
		return "", "", err
	}
	name, err := r.ensureSandboxTemplate(ctx, sbs, spec, hash)
	if err != nil {
		return "", "", err
	}
	return hash, name, nil
}

// ensureSandboxTemplate materialises the pre-built SandboxTemplateSpec as a
// SandboxTemplate CR named "{sbs.Name}-{hash}", owned by the SandboxSet.
// When spec.templateRef is set, no object is created and the referenced
// name is returned so it can be reflected into status.currentRevision.
func (r *Reconciler) ensureSandboxTemplate(ctx context.Context, sbs *agentsv1alpha1.SandboxSet, spec *agentsv1alpha1.SandboxTemplateSpec, hash string) (string, error) {
	if sbs.Spec.TemplateRef != nil {
		return sbs.Spec.TemplateRef.Name, nil
	}
	if sbs.Spec.Template == nil {
		return "", nil
	}
	name := fmt.Sprintf("%s-%s", sbs.Name, hash)
	// Shallow copy is safe: the SBT becomes an independent API object; sbs.Spec is never mutated after this point.
	sbt := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: sbs.Namespace,
			Name:      name,
		},
		Spec: *spec,
	}
	if err := ctrl.SetControllerReference(sbs, sbt, r.Scheme); err != nil {
		return "", err
	}
	// Override the controller reference to explicitly opt out of
	// BlockOwnerDeletion so deleting the SandboxSet is never blocked by the
	// SBT finalizers.
	for i := range sbt.OwnerReferences {
		if sbt.OwnerReferences[i].UID == sbs.UID {
			sbt.OwnerReferences[i].BlockOwnerDeletion = ptr.To(false)
		}
	}
	if err := r.Create(ctx, sbt); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Verify the existing SBT is actually owned by this SandboxSet.
			existing := &agentsv1alpha1.SandboxTemplate{}
			if getErr := r.Get(ctx, client.ObjectKey{Namespace: sbs.Namespace, Name: name}, existing); getErr != nil {
				return "", fmt.Errorf("failed to verify existing SandboxTemplate %s/%s: %w", sbs.Namespace, name, getErr)
			}
			ref := metav1.GetControllerOf(existing)
			if ref == nil || ref.UID != sbs.UID {
				return "", fmt.Errorf("SandboxTemplate %s/%s already exists but is not owned by this SandboxSet", sbs.Namespace, name)
			}
			return name, nil
		}
		return "", err
	}
	logf.FromContext(ctx).Info("created auto-materialised SandboxTemplate", "name", name, "revision", hash)
	return name, nil
}

// cleanupOldSandboxTemplates annotates auto-materialised SandboxTemplates that
// exceed defaultTemplateHistoryLimit with AnnotationCleanupCandidate so that a
// future GC controller can safely reclaim them after verifying no Sandbox or
// Checkpoint still references them. Errors are logged and swallowed so that
// cleanup never blocks the main reconcile.
func (r *Reconciler) cleanupOldSandboxTemplates(ctx context.Context, sbs *agentsv1alpha1.SandboxSet) {
	log := logf.FromContext(ctx)
	sbtList := &agentsv1alpha1.SandboxTemplateList{}
	if err := r.List(ctx, sbtList,
		client.InNamespace(sbs.Namespace),
		client.MatchingFields{fieldindex.IndexNameForOwnerRefUID: string(sbs.UID)},
	); err != nil {
		log.Error(err, "failed to list sandbox templates for cleanup")
		return
	}
	if len(sbtList.Items) <= defaultTemplateHistoryLimit {
		return
	}
	// Sort oldest first by CreationTimestamp.
	sort.SliceStable(sbtList.Items, func(i, j int) bool {
		return sbtList.Items[i].CreationTimestamp.Before(&sbtList.Items[j].CreationTimestamp)
	})
	toMark := sbtList.Items[:len(sbtList.Items)-defaultTemplateHistoryLimit]
	for i := range toMark {
		sbt := &toMark[i]
		if sbt.Annotations != nil && sbt.Annotations[agentsv1alpha1.AnnotationCleanupCandidate] != "" {
			continue // already marked
		}
		patch := client.MergeFrom(sbt.DeepCopy())
		if sbt.Annotations == nil {
			sbt.Annotations = map[string]string{}
		}
		sbt.Annotations[agentsv1alpha1.AnnotationCleanupCandidate] = agentsv1alpha1.True
		if err := r.Patch(ctx, sbt, patch); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "failed to annotate stale sandbox template", "template", sbt.Name)
		}
	}
}
