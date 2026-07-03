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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

var (
	scaleUpExpectation               = expectations.NewScaleExpectations()
	scaleDownExpectation             = expectations.NewScaleExpectations()
	retryAfterForceDeleteExpectation = 3 * time.Second
)

// GetControllerKey return key of CloneSet.
func GetControllerKey(sbs *agentsv1alpha1.SandboxSet) string {
	return types.NamespacedName{Namespace: sbs.Namespace, Name: sbs.Name}.String()
}

type GroupedSandboxes struct {
	Creating  []*agentsv1alpha1.Sandbox // Sandboxes being created or initialized
	Available []*agentsv1alpha1.Sandbox // Initialized but not yet claimed Sandboxes
	Used      []*agentsv1alpha1.Sandbox // Sandboxes claimed by client agents
	Dead      []*agentsv1alpha1.Sandbox // Sandboxes should be deleted
}

// initStatusResult bundles the output of initNewStatus to avoid excessive return values.
type initStatusResult struct {
	status         *agentsv1alpha1.SandboxSetStatus
	legacyRevision string
}

func (r *Reconciler) initNewStatus(ctx context.Context, ss *agentsv1alpha1.SandboxSet) (*initStatusResult, error) {
	newStatus := ss.Status.DeepCopy()
	hash, name, err := r.ensureTemplateRevision(ctx, ss)
	if err != nil {
		return nil, err
	}
	newStatus.UpdateRevision = hash
	newStatus.ObservedGeneration = ss.Generation
	newStatus.CurrentRevision = name

	// Compute legacy hash for backward compatibility with sandboxes created
	// before the hash algorithm was changed. Errors are non-fatal: if legacy
	// hash cannot be computed, we simply skip the fallback comparison.
	legacyHash, err := r.computeLegacyRevisionHash(ctx, ss)
	if err != nil {
		klog.ErrorS(err, "Failed to compute legacy revision hash")
	}

	return &initStatusResult{
		status:         newStatus,
		legacyRevision: legacyHash,
	}, nil
}

func calculateSandboxSetStatusFromGroup(ctx context.Context, newStatus *agentsv1alpha1.SandboxSetStatus, groups GroupedSandboxes, dirtyScaleUp map[expectations.ScaleAction][]string) {
	log := logf.FromContext(ctx)
	newStatus.AvailableReplicas = int32(len(groups.Available))                                                                      // #nosec G115 -- K8s object count
	newStatus.Replicas = int32(len(groups.Creating)) + int32(len(groups.Available)) + int32(len(dirtyScaleUp[expectations.Create])) // #nosec G115 -- K8s object count
	log.Info("new status calculated", "replicas", newStatus.Replicas, "available", newStatus.AvailableReplicas,
		"creating", len(groups.Creating), "dirtyCreating", len(dirtyScaleUp[expectations.Create]))
}

/* Just Reserved for SandboxAutoScaler
func calculateExpectPoolSize(ctx context.Context, total, unused int32, sbs *agentsv1alpha1.SandboxSet) (int32, error) {
	log := klog.FromContext(ctx).V(utils.DebugLogLevel)
	if sbs.Spec.MaxReplicas == sbs.Spec.MinReplicas {
		return sbs.Spec.MinReplicas, nil // optimize
	}
	actualWaterMark := int(total - unused)
	highWaterMark, err := intstr.GetScaledValueFromIntOrPercent(sbs.Spec.HighWaterMark, int(total), false)
	if err != nil {
		return 0, err
	}
	lowWaterMark, err := intstr.GetScaledValueFromIntOrPercent(sbs.Spec.LowWaterMark, int(total), true)
	if err != nil {
		return 0, err
	}
	expectTotal := total
	if actualWaterMark > highWaterMark {
		// should scale up
		expectScaleUp := int32(actualWaterMark - highWaterMark)
		unusedAfterScaleUp := unused + expectScaleUp
		actualScaleUp := expectScaleUp
		if unusedAfterScaleUp > sbs.Spec.Replicas {
			actualScaleUp = max(0, expectScaleUp-unusedAfterScaleUp-sbs.Spec.Replicas) // just in case
		}
		log.Info("actual scale up calculated", "actualScaleUp", actualScaleUp, "expectScaleUp", expectScaleUp,
			"unusedAfterScaleUp", unusedAfterScaleUp, "maxUnused", sbs.Spec.Replicas, "highWaterMark", highWaterMark, "lowWaterMark", lowWaterMark)
		expectTotal = total + actualScaleUp
	}
	if actualWaterMark < lowWaterMark {
		// should scale down
		expectTotal = total + int32(actualWaterMark-lowWaterMark)
	}
	// limit
	expectTotal = min(expectTotal, sbs.Spec.MaxReplicas)
	expectTotal = max(expectTotal, sbs.Spec.MinReplicas)
	log.Info("expect pool size calculated", "expectTotal", expectTotal, "oldTotal", total,
		"highWaterMark", highWaterMark, "lowWaterMark", lowWaterMark, "actualWaterMark", actualWaterMark)
	return expectTotal, nil
}
*/

func clearAndInitInnerKeys(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	for k := range m {
		if strings.HasPrefix(k, agentsv1alpha1.InternalPrefix) {
			if _, preserve := agentsv1alpha1.InternalKeysPreservedOnCreation[k]; !preserve {
				delete(m, k)
			}
		}
	}
	return m
}

// scaleExpectationSatisfied logic:
// 1. if scaleUpExpectation is not satisfied, both scaling up and scaling down are forbidden
// 2. if scaleDownExpectation is not satisfied, scaling up is allowed and scaling down is forbidden
func scaleExpectationSatisfied(ctx context.Context, scaleExpectation expectations.ScaleExpectations, key string) (
	ok bool, dirty map[expectations.ScaleAction][]string, requeueAfter time.Duration) {
	log := logf.FromContext(ctx)
	satisfied, unsatisfiedDuration, dirty := scaleExpectation.SatisfiedExpectations(key)
	if satisfied {
		return true, dirty, 0
	}

	if unsatisfiedDuration > expectations.ExpectationTimeout {
		scaleExpectation.DeleteExpectations(key)
		log.Error(nil, "expectation unsatisfied overtime, force delete the timeout expectation", "requeueAfter", retryAfterForceDeleteExpectation)
		return false, dirty, retryAfterForceDeleteExpectation
	}

	requeueAfter = expectations.ExpectationTimeout - unsatisfiedDuration
	log.Info("expectations not satisfied",
		"createDirty", len(dirty[expectations.Create]), "deleteDirty", len(dirty[expectations.Delete]), "requeueAfter", requeueAfter)
	return false, dirty, requeueAfter
}

// NewSandboxFromSandboxSet builds a Sandbox object from the SandboxSet. When
// spec.templateRef is used, refTemplate must be the resolved SandboxTemplate
// so that its pod template labels/annotations can be inherited; callers pass
// nil for the inline template case.
func NewSandboxFromSandboxSet(sbs *agentsv1alpha1.SandboxSet, refTemplate *agentsv1alpha1.SandboxTemplate) *agentsv1alpha1.Sandbox {
	generateName := utils.GenerateSandboxName(sbs.Name)
	var template *corev1.PodTemplateSpec
	var inheritedLabels, inheritedAnnotations map[string]string
	// spec.template and spec.templateRef are mutually exclusive. Deep copy the
	// source pod template before reading labels/annotations so subsequent
	// mutations (clearAndInitInnerKeys, internal label writes) never leak
	// back into the SandboxSet spec or the cached SandboxTemplate.
	if sbs.Spec.Template != nil {
		template = sbs.Spec.Template.DeepCopy()
		inheritedLabels = template.Labels
		inheritedAnnotations = template.Annotations
	} else if refTemplate != nil && refTemplate.Spec.Template != nil {
		templateCopy := refTemplate.Spec.Template.DeepCopy()
		inheritedLabels = templateCopy.Labels
		inheritedAnnotations = templateCopy.Annotations
	}
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: generateName,
			Namespace:    sbs.Namespace,
			Labels:       inheritedLabels,
			Annotations:  inheritedAnnotations,
		},
		Spec: agentsv1alpha1.SandboxSpec{
			PersistentContents: sbs.Spec.PersistentContents,
			Runtimes:           sbs.Spec.Runtimes,
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				TemplateRef:          sbs.Spec.TemplateRef,
				Template:             template,
				VolumeClaimTemplates: sbs.Spec.VolumeClaimTemplates,
			},
		},
	}
	sbx.Annotations = clearAndInitInnerKeys(sbx.Annotations)
	sbx.Labels = clearAndInitInnerKeys(sbx.Labels)
	sbx.Labels[agentsv1alpha1.LabelSandboxPool] = sbs.Name
	sbx.Labels[agentsv1alpha1.LabelSandboxTemplate] = sbs.Name
	sbx.Labels[agentsv1alpha1.LabelSandboxIsClaimed] = "false"
	if sbs.Spec.TemplateRef != nil {
		sbx.Labels[agentsv1alpha1.LabelSandboxTemplate] = sbs.Spec.TemplateRef.Name
	} else {
		sbx.Labels[agentsv1alpha1.LabelSandboxTemplate] = sbs.Name
	}
	return sbx
}
