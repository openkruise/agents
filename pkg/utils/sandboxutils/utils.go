package sandboxutils

import (
	"fmt"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GetSandboxState the state of agentsv1alpha1 Sandbox.
// NOTE: the reason is unique and hard-coded, so we can easily search the conditions of some reason when debugging.
func GetSandboxState(sbx *agentsv1alpha1.Sandbox) (state string, reason string) {
	if sbx.DeletionTimestamp != nil {
		return agentsv1alpha1.SandboxStateDead, "ResourceDeleted"
	}
	if sbx.Spec.ShutdownTime != nil && time.Since(sbx.Spec.ShutdownTime.Time) > 0 {
		return agentsv1alpha1.SandboxStateDead, "ShutdownTimeReached"
	}
	if sbx.Status.Phase == agentsv1alpha1.SandboxPending {
		return agentsv1alpha1.SandboxStateCreating, "ResourcePending"
	}
	if sbx.Status.Phase == agentsv1alpha1.SandboxSucceeded {
		return agentsv1alpha1.SandboxStateDead, "ResourceSucceeded"
	}
	if sbx.Status.Phase == agentsv1alpha1.SandboxFailed {
		return agentsv1alpha1.SandboxStateDead, "ResourceFailed"
	}
	if sbx.Status.Phase == agentsv1alpha1.SandboxTerminating {
		return agentsv1alpha1.SandboxStateDead, "ResourceTerminating"
	}

	sandboxReady := IsSandboxReady(sbx)
	if IsControlledBySandboxCR(sbx) {
		if sandboxReady {
			return agentsv1alpha1.SandboxStateAvailable, "ResourceControlledBySbsAndReady"
		} else {
			return agentsv1alpha1.SandboxStateCreating, "ResourceControlledBySbsButNotReady"
		}
	} else {
		if sbx.Status.Phase == agentsv1alpha1.SandboxRunning {
			if sbx.Spec.Paused {
				return agentsv1alpha1.SandboxStatePaused, "RunningResourceClaimedAndPaused"
			} else {
				if sandboxReady {
					return agentsv1alpha1.SandboxStateRunning, "RunningResourceClaimedAndReady"
				} else {
					return agentsv1alpha1.SandboxStateDead, "RunningResourceClaimedButNotReady"
				}
			}
		} else {
			// Paused and Resuming phases are both treated as paused state
			return agentsv1alpha1.SandboxStatePaused, "NotRunningResourceClaimed"
		}
	}
}

func IsControlledBySandboxCR(sbx *agentsv1alpha1.Sandbox) bool {
	controller := metav1.GetControllerOfNoCopy(sbx)
	if controller == nil {
		return false
	}
	return controller.Kind == agentsv1alpha1.SandboxSetControllerKind.Kind &&
		// ** REMEMBER TO MODIFY THIS WHEN A NEW API VERSION(LIKE v1beta1) IS ADDED **
		controller.APIVersion == agentsv1alpha1.SandboxSetControllerKind.GroupVersion().String()
}

func GetSandboxID(sbx *agentsv1alpha1.Sandbox) string {
	return fmt.Sprintf("%s--%s", sbx.Namespace, sbx.Name)
}

func IsSandboxReady(sbx *agentsv1alpha1.Sandbox) bool {
	readyCond := utils.GetSandboxCondition(&sbx.Status, string(agentsv1alpha1.SandboxConditionReady))
	return readyCond != nil && readyCond.Status == metav1.ConditionTrue
}
