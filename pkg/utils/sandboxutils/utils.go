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

package sandboxutils

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/utils"
)

// GetRuntimeURL resolves the agent-runtime endpoint for a Sandbox.
//
// Lookup order:
//  1. AnnotationRuntimeURL on the Sandbox object.
//  2. AnnotationEnvdURL on the Sandbox object (legacy key, kept for backwards compatibility).
//  3. Pod IP from the cached route plus the well-known consts.RuntimePort, used as a fallback
//     while the controller has not yet stamped the URL annotation.
//
// Returns an empty string when none of the sources is usable (e.g. the pod has not been scheduled
// yet). Callers must treat an empty result as "not ready" and either skip or retry.
func GetRuntimeURL(sbx *agentsv1alpha1.Sandbox) string {
	if sbx == nil {
		return ""
	}
	if u := utils.GetRuntimeURLFromAnnotation(sbx); u != "" {
		return u
	}
	route := GetRouteFromSandbox(sbx)
	if route.IP == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", route.IP, consts.RuntimePort)
}

// GetAccessToken resolves the agent-runtime access token from object annotations, falling back
// to the legacy envd annotation key for backwards compatibility. Accepts metav1.Object so that
// it works for both Sandbox and SandboxClaim objects.
func GetAccessToken(obj metav1.Object) string {
	return utils.GetAccessTokenFromAnnotation(obj)
}

func GetRouteFromSandbox(s *agentsv1alpha1.Sandbox) proxy.Route {
	state, _ := GetSandboxState(s)
	if s.Status.PodInfo.PodIP == "" {
		state = agentsv1alpha1.SandboxStateCreating
	}
	return proxy.Route{
		IP:              s.Status.PodInfo.PodIP,
		ID:              GetSandboxID(s),
		UID:             s.GetUID(),
		Owner:           s.GetAnnotations()[agentsv1alpha1.AnnotationOwner],
		State:           state,
		ResourceVersion: s.GetResourceVersion(),
	}
}

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
	if IsControlledBySandboxSet(sbx) {
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

func IsControlledBySandboxSet(sbx *agentsv1alpha1.Sandbox) bool {
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

// IsSandboxPausable returns true when the pausing operation will not cause any conflict.
func IsSandboxPausable(sbx *agentsv1alpha1.Sandbox) (bool, string) {
	if IsControlledBySandboxSet(sbx) {
		state, _ := GetSandboxState(sbx)
		switch state {
		case agentsv1alpha1.SandboxStateAvailable, agentsv1alpha1.SandboxStateCreating:
			return false, "SandboxStateNotAllowed"
		}
	}
	switch sbx.Status.Phase {
	case agentsv1alpha1.SandboxRunning, agentsv1alpha1.SandboxPaused:
		return true, "SandboxIsRunningOrPaused"
	default:
		return false, "SandboxPhaseNotAllowed"
	}
}

// IsSandboxResumable returns true when the resuming operation will not cause any conflict.
func IsSandboxResumable(sbx *agentsv1alpha1.Sandbox) (bool, string) {
	switch sbx.Status.Phase {
	case agentsv1alpha1.SandboxRunning:
		if sbx.Spec.Paused {
			return false, "SandboxIsPausing"
		}
		return true, "SandboxIsRunning"
	case agentsv1alpha1.SandboxResuming:
		return true, "SandboxIsResuming"
	default:
	}
	if sbx.Status.Phase == agentsv1alpha1.SandboxPaused {
		pauseCond := utils.GetSandboxCondition(&sbx.Status, string(agentsv1alpha1.SandboxConditionPaused))
		paused := pauseCond != nil && pauseCond.Status == metav1.ConditionTrue
		if paused {
			return true, "SandboxIsPaused"
		}
		return false, "SandboxIsPausing"
	}
	return false, "SandboxPhaseNotAllowed"
}
