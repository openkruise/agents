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

package sandboxset

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	managerutils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
)

// UpdateGroupedSandboxes extends GroupedSandboxes with update-specific groupings.
type UpdateGroupedSandboxes struct {
	// UpdatedCreating are unclaimed sandboxes being created with the update revision.
	UpdatedCreating []*agentsv1alpha1.Sandbox

	// UpdatedAvailable are unclaimed available sandboxes with the update revision.
	UpdatedAvailable []*agentsv1alpha1.Sandbox

	// OldCreating are unclaimed sandboxes being created with an old revision.
	OldCreating []*agentsv1alpha1.Sandbox

	// OldAvailable are unclaimed available sandboxes with an old revision.
	OldAvailable []*agentsv1alpha1.Sandbox
}

// UpdateInfo holds all the computed values for a rolling update.
type UpdateInfo struct {
	// CurrentUpdated is the current number of unclaimed pods with update revision.
	CurrentUpdated int

	// ToUpdate is the number of pods that still need to be updated.
	ToUpdate int

	// AllowedUnavailable is the remaining unavailable budget for this round.
	AllowedUnavailable int
}

// buildUpdateGroups builds update-specific groupings from existing GroupedSandboxes.
// It categorizes unclaimed Creating and Available sandboxes by whether their template hash
// matches the update revision. Claimed sandboxes are excluded from update consideration.
// Returns nil if scale expectations are not satisfied, indicating the caller should wait.
func buildUpdateGroups(groups GroupedSandboxes, updateRevision string) *UpdateGroupedSandboxes {
	updateGroups := &UpdateGroupedSandboxes{}

	// Categorize Creating sandboxes by revision (skip claimed)
	for _, sbx := range groups.Creating {
		if isSandboxClaimed(sbx) {
			continue
		}
		revision := sbx.Labels[agentsv1alpha1.LabelTemplateHash]
		if revision == updateRevision {
			updateGroups.UpdatedCreating = append(updateGroups.UpdatedCreating, sbx)
		} else {
			updateGroups.OldCreating = append(updateGroups.OldCreating, sbx)
		}
	}

	// Categorize Available sandboxes by revision (skip claimed)
	for _, sbx := range groups.Available {
		if isSandboxClaimed(sbx) {
			continue
		}
		revision := sbx.Labels[agentsv1alpha1.LabelTemplateHash]
		if revision == updateRevision {
			updateGroups.UpdatedAvailable = append(updateGroups.UpdatedAvailable, sbx)
		} else {
			updateGroups.OldAvailable = append(updateGroups.OldAvailable, sbx)
		}
	}

	return updateGroups
}

// isSandboxClaimed checks if a sandbox has been claimed.
func isSandboxClaimed(sbx *agentsv1alpha1.Sandbox) bool {
	// Check if the sandbox is marked as claimed
	if sbx.Labels[agentsv1alpha1.LabelSandboxIsClaimed] == agentsv1alpha1.True {
		return true
	}
	// Check if the owner reference is not SandboxSet (claimed sandboxes have ownerRef removed)
	if !sandboxutils.IsControlledBySandboxSet(sbx) {
		return true
	}
	return false
}

// needsUpdate checks if any sandboxes need to be updated.
func needsUpdate(updateGroups *UpdateGroupedSandboxes) bool {
	oldPodsCount := len(updateGroups.OldCreating) + len(updateGroups.OldAvailable)
	return oldPodsCount > 0
}

// isUpdateComplete checks if the update is complete.
func isUpdateComplete(info *UpdateInfo) bool {
	return info.ToUpdate == 0
}

// calculateUpdateInfo calculates the update info based on the current state and update strategy.
func calculateUpdateInfo(sbs *agentsv1alpha1.SandboxSet, updateGroups *UpdateGroupedSandboxes) *UpdateInfo {
	info := &UpdateInfo{}

	// Calculate current updated counts
	info.CurrentUpdated = len(updateGroups.UpdatedCreating) + len(updateGroups.UpdatedAvailable)

	// Calculate how many still need to be updated
	info.ToUpdate = len(updateGroups.OldCreating) + len(updateGroups.OldAvailable)

	// AllowedUnavailable is the remaining unavailable budget after accounting for UpdatedCreating.
	// UpdatedCreating pods are not yet available, so they already consume the unavailable budget.
	info.AllowedUnavailable = max(getMaxUnavailablePods(sbs, int(sbs.Spec.Replicas))-len(updateGroups.UpdatedCreating), 0)

	return info
}

// getMaxUnavailablePods calculates the max unavailable pods allowed.
func getMaxUnavailablePods(sbs *agentsv1alpha1.SandboxSet, replicas int) int {
	// MaxUnavailable should already be set by webhook with default value "20%"
	maxUnavailable := sbs.Spec.UpdateStrategy.MaxUnavailable

	value, err := intstr.GetScaledValueFromIntOrPercent(intstr.ValueOrDefault(maxUnavailable, intstr.FromInt(0)), replicas, false)
	if err != nil {
		// This should not happen after webhook validation
		value = replicas * 20 / 100
	}
	return value
}

// logUpdateInfo logs the update info for debugging.
func logUpdateInfo(ctx context.Context, info *UpdateInfo) {
	log := logf.FromContext(ctx)
	log.Info("update info calculated",
		"currentUpdated", info.CurrentUpdated,
		"toUpdate", info.ToUpdate,
		"allowedUnavailable", info.AllowedUnavailable)
}

// performRollingUpdate performs the rolling update logic.
// Returns the number of sandboxes created and deleted.
func (r *Reconciler) performRollingUpdate(
	ctx context.Context,
	sbs *agentsv1alpha1.SandboxSet,
	updateGroups *UpdateGroupedSandboxes,
	updateInfo *UpdateInfo,
) (int, error) {
	log := logf.FromContext(ctx)
	controllerKey := GetControllerKey(sbs)
	lock := uuid.New().String()
	logUpdateInfo(ctx, updateInfo)

	deleteFunc := func(sbx *agentsv1alpha1.Sandbox) error {
		key := client.ObjectKeyFromObject(sbx)
		scaleDownExpectation.ExpectScale(controllerKey, expectations.Delete, key.Name)
		err := r.deleteSandboxForUpdate(ctx, sbs, sbx, lock)
		if err != nil {
			scaleDownExpectation.ObserveScale(controllerKey, expectations.Delete, key.Name)
		}
		return err
	}

	var totalDeleted int
	// Delete all OldCreating sandboxes freely (they are not available, no budget needed)
	oldCreating := make([]*agentsv1alpha1.Sandbox, len(updateGroups.OldCreating))
	copy(oldCreating, updateGroups.OldCreating)

	if len(oldCreating) > 0 {
		successes, err := utils.DoItSlowlyWithInputs(oldCreating, initialBatchSize, deleteFunc)
		totalDeleted += successes
		if err != nil {
			log.Error(err, "failed to delete some old creating sandboxes", "success", successes, "fails", len(oldCreating)-successes)
			return totalDeleted, err
		}
	}

	if len(updateGroups.OldAvailable) <= 0 {
		log.Info("no old available sandboxes to delete")
		return totalDeleted, nil
	}

	var deleteCount int
	// Delete old available pods based on unavailable budget.
	// New sandboxes will be created by scaleUp in the next reconcile.
	deleteCount = min(updateInfo.AllowedUnavailable, len(updateGroups.OldAvailable))
	log.Info("rolling update plan", "deleteCount", deleteCount)

	// Delete old available sandboxes in batches
	if deleteCount > 0 {
		oldAvailable := updateGroups.OldAvailable
		toDelete := findOldestSandboxes(oldAvailable, deleteCount, oldestFirst)

		successes, err := utils.DoItSlowlyWithInputs(toDelete, initialBatchSize, deleteFunc)
		totalDeleted += successes
		if err != nil {
			log.Error(err, "failed to delete some old available sandboxes", "success", successes, "fails", deleteCount-successes)
			return totalDeleted, err
		}
	}

	return totalDeleted, nil
}

// deleteSandboxForUpdate deletes a sandbox as part of rolling update.
func (r *Reconciler) deleteSandboxForUpdate(ctx context.Context, sbs *agentsv1alpha1.SandboxSet, sbx *agentsv1alpha1.Sandbox, lock string) error {
	log := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
	log.V(consts.DebugLogLevel).Info("deleting sandbox for rolling update")

	if sbx.DeletionTimestamp != nil {
		return nil
	}
	if sbx.Annotations[agentsv1alpha1.AnnotationLock] != "" && sbx.Annotations[agentsv1alpha1.AnnotationOwner] != consts.OwnerManagerScaleDown {
		log.Info("sandbox to be deleted claimed before performed, skip")
		return errors.New("sandbox to be deleted claimed before performed, skip")
	}

	managerutils.LockSandbox(sbx, lock, consts.OwnerManagerScaleDown)
	if err := r.Update(ctx, sbx); err != nil {
		return fmt.Errorf("failed to lock sandbox when delete: %s", err)
	}
	if err := r.Delete(ctx, sbx); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		log.Error(err, "failed to delete sandbox for rolling update")
		return err
	}

	r.Recorder.Eventf(sbs, "Normal", "RollingUpdate", "Deleted sandbox %s for update", klog.KObj(sbx))
	return nil
}
