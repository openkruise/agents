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

	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/defaults"
)

// defaultTemplateHistoryLimit is the maximum number of auto-materialised
// SandboxTemplates that we keep per SandboxSet. Orphan templates beyond this
// number are pruned in FIFO order (oldest CreationTimestamp first). Templates
// still referenced by any Sandbox are never deleted regardless of the limit.
const defaultTemplateHistoryLimit = 10

// computeTemplateHash derives a stable hash of the pod template carried by a
// SandboxSet. It matches exactly the hash produced by [Reconciler.newRevision]
// so we can reuse the same value for both the status.updateRevision and the
// {sbs}-{hash} naming of the materialised SandboxTemplate.
//
// Before hashing we apply defaulting equivalent to the SandboxTemplate
// mutating webhook. This is necessary because the hash is also used to look
// up an existing SandboxTemplate by name: without pre-defaulting the caller
// side, a second reconcile would compute a different hash than the one
// produced while the SBT was first persisted (the webhook has already
// mutated the stored object).
func (r *Reconciler) computeTemplateHash(ctx context.Context, sbs *agentsv1alpha1.SandboxSet) (string, error) {
	// Work on a deep copy so we never mutate the cached SandboxSet.
	clone := sbs.DeepCopy()
	applyTemplateDefaulting(clone)
	patch, err := r.getPatch(ctx, clone)
	if err != nil {
		return "", err
	}
	cr := &apps.ControllerRevision{Data: runtime.RawExtension{Raw: patch}}
	return HashControllerRevision(cr, nil), nil
}

// ensureSandboxTemplate materialises the inline spec.template as a
// SandboxTemplate CR named "{sbs.Name}-{hash}", owned by the SandboxSet.
// When spec.templateRef is set, no object is created and the referenced
// name is returned so it can be reflected into status.currentTemplate.
// An AlreadyExists error is treated as success: the concrete bytes are
// deterministic per hash so any previously-created SBT with the same name
// is guaranteed to describe the same template.
func (r *Reconciler) ensureSandboxTemplate(ctx context.Context, sbs *agentsv1alpha1.SandboxSet) (string, error) {
	if sbs.Spec.TemplateRef != nil {
		return sbs.Spec.TemplateRef.Name, nil
	}
	if sbs.Spec.Template == nil {
		return "", nil
	}
	hash, err := r.computeTemplateHash(ctx, sbs)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s", sbs.Name, hash)
	sbt := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: sbs.Namespace,
			Name:      name,
		},
		Spec: agentsv1alpha1.SandboxTemplateSpec{
			Template:             sbs.Spec.Template.DeepCopy(),
			VolumeClaimTemplates: deepCopyVCTs(sbs.Spec.VolumeClaimTemplates),
			PersistentContents:   append([]string(nil), sbs.Spec.PersistentContents...),
			Runtimes:             append([]agentsv1alpha1.RuntimeConfig(nil), sbs.Spec.Runtimes...),
		},
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
			return name, nil
		}
		return "", err
	}
	return name, nil
}

// cleanupOldSandboxTemplates keeps at most defaultTemplateHistoryLimit
// auto-materialised SandboxTemplates per SandboxSet. SBTs still referenced
// by any Sandbox via Labels[LabelTemplateHash] are always preserved; the
// remaining (orphan) templates are sorted by CreationTimestamp and the
// oldest entries are deleted until the limit is honoured. Errors are
// logged and swallowed so that cleanup never blocks the main reconcile.
func (r *Reconciler) cleanupOldSandboxTemplates(ctx context.Context, sbs *agentsv1alpha1.SandboxSet) {
	log := logf.FromContext(ctx)
	sbtList := &agentsv1alpha1.SandboxTemplateList{}
	if err := r.List(ctx, sbtList, client.InNamespace(sbs.Namespace)); err != nil {
		log.Error(err, "failed to list sandbox templates for cleanup")
		return
	}
	owned := make([]*agentsv1alpha1.SandboxTemplate, 0, len(sbtList.Items))
	for i := range sbtList.Items {
		sbt := &sbtList.Items[i]
		if !isControlledBy(sbt, sbs) {
			continue
		}
		owned = append(owned, sbt)
	}
	if len(owned) <= defaultTemplateHistoryLimit {
		return
	}
	inUse, err := r.collectInUseTemplateNames(ctx, sbs)
	if err != nil {
		log.Error(err, "failed to collect in-use template names")
		return
	}
	// Orphan = owned by the sbs but not referenced by any live sandbox.
	// Preserved = the rest.
	var orphans []*agentsv1alpha1.SandboxTemplate
	preservedCount := 0
	for _, sbt := range owned {
		if inUse[sbt.Name] {
			preservedCount++
			continue
		}
		orphans = append(orphans, sbt)
	}
	// Total budget we may keep = limit; subtract in-use slots first.
	budget := defaultTemplateHistoryLimit - preservedCount
	if budget < 0 {
		budget = 0
	}
	if len(orphans) <= budget {
		return
	}
	sort.SliceStable(orphans, func(i, j int) bool {
		return orphans[i].CreationTimestamp.Before(&orphans[j].CreationTimestamp)
	})
	toDelete := orphans[:len(orphans)-budget]
	for _, sbt := range toDelete {
		if err := r.Delete(ctx, sbt); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "failed to delete stale sandbox template", "template", sbt.Name)
		}
	}
}

// collectInUseTemplateNames enumerates all Sandboxes owned by the
// SandboxSet and maps their LabelTemplateHash value to the corresponding
// {sbs}-{hash} SandboxTemplate name so that cleanup can safely skip any SBT
// that is still referenced by at least one live sandbox.
func (r *Reconciler) collectInUseTemplateNames(ctx context.Context, sbs *agentsv1alpha1.SandboxSet) (map[string]bool, error) {
	sandboxList := &agentsv1alpha1.SandboxList{}
	if err := r.List(ctx, sandboxList, client.InNamespace(sbs.Namespace)); err != nil {
		return nil, err
	}
	result := make(map[string]bool)
	for i := range sandboxList.Items {
		sbx := &sandboxList.Items[i]
		if !hasOwnerUID(sbx.OwnerReferences, sbs.UID) {
			continue
		}
		hash := sbx.Labels[agentsv1alpha1.LabelTemplateHash]
		if hash == "" {
			continue
		}
		result[fmt.Sprintf("%s-%s", sbs.Name, hash)] = true
	}
	return result, nil
}

func isControlledBy(sbt *agentsv1alpha1.SandboxTemplate, sbs *agentsv1alpha1.SandboxSet) bool {
	ref := metav1.GetControllerOf(sbt)
	if ref == nil {
		return false
	}
	return ref.UID == sbs.UID
}

func hasOwnerUID(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, ref := range refs {
		if ref.UID == uid {
			return true
		}
	}
	return false
}

// applyTemplateDefaulting applies the same defaulting that the SandboxTemplate
// mutating webhook would perform, so that the hash computed before creating
// the SBT matches the hash we would compute after reading the SBT back.
func applyTemplateDefaulting(sbs *agentsv1alpha1.SandboxSet) {
	if sbs.Spec.Template != nil {
		tpl := sbs.Spec.Template
		if ptr.Deref(tpl.Spec.AutomountServiceAccountToken, true) {
			tpl.Spec.AutomountServiceAccountToken = ptr.To(false)
		}
		defaults.SetDefaultPodSpec(&tpl.Spec)
	}
	for i := range sbs.Spec.VolumeClaimTemplates {
		vct := &sbs.Spec.VolumeClaimTemplates[i]
		if len(vct.Spec.AccessModes) == 0 {
			vct.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
		}
		if vct.Spec.VolumeMode == nil {
			mode := corev1.PersistentVolumeFilesystem
			vct.Spec.VolumeMode = &mode
		}
	}
}

func deepCopyVCTs(in []corev1.PersistentVolumeClaim) []corev1.PersistentVolumeClaim {
	if in == nil {
		return nil
	}
	out := make([]corev1.PersistentVolumeClaim, len(in))
	for i := range in {
		out[i] = *in[i].DeepCopy()
	}
	return out
}
