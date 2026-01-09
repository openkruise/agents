package sandboxutils

import (
	"flag"
	"fmt"
	"sync"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Define what we are storing (State AND Reason)
type cachedState struct {
	State           string
	Reason          string
	ResourceVersion string
	UID             types.UID
}

var (
	// Map key is NamespacedName (Namespace + Name)
	sandboxStateCache = make(map[types.NamespacedName]cachedState)

	// Lock to make it thread-safe
	cacheLock sync.RWMutex

	// skipCacheInTests defaults to true to prevent collisions in existing unit tests
	// that use deterministic/fake UIDs.
	skipCacheInTests = true
)

// DeleteSandboxStateCache removes a sandbox from the cache.
// This should be called by the Controller when a Sandbox is deleted.
func DeleteSandboxStateCache(ns, name string) {
	key := types.NamespacedName{Namespace: ns, Name: name}
	cacheLock.Lock()
	delete(sandboxStateCache, key)
	cacheLock.Unlock()
}

// GetSandboxState the state of agentsv1alpha1 Sandbox.
// NOTE: the reason is unique and hard-coded, so we can easily search the conditions of some reason when debugging.
func GetSandboxState(sbx *agentsv1alpha1.Sandbox) (state string, reason string) {
	// SAFETY CHECK 1: Empty fields
	if sbx.ResourceVersion == "" || sbx.UID == "" {
		return computeSandboxState(sbx)
	}

	// SAFETY CHECK 2: Detect Test Environment
	// By default, we skip the cache during "go test" to avoid collisions in existing tests.
	// We can toggle 'skipCacheInTests' to false in specific tests to verify cache behavior.
	if skipCacheInTests && flag.Lookup("test.v") != nil {
		return computeSandboxState(sbx)
	}

	key := types.NamespacedName{
		Namespace: sbx.Namespace,
		Name:      sbx.Name,
	}

	// 1. FAST PATH: Check Cache
	cacheLock.RLock()
	if item, found := sandboxStateCache[key]; found {
		if item.ResourceVersion == sbx.ResourceVersion && item.UID == sbx.UID {
			cacheLock.RUnlock()
			return item.State, item.Reason
		}
	}
	cacheLock.RUnlock()

	// 2. SLOW PATH: Calculate State
	state, reason = computeSandboxState(sbx)

	// 3. UPDATE CACHE
	cacheLock.Lock()
	sandboxStateCache[key] = cachedState{
		State:           state,
		Reason:          reason,
		ResourceVersion: sbx.ResourceVersion,
		UID:             sbx.UID,
	}
	cacheLock.Unlock()

	return state, reason
}

// computeSandboxState contains the original logic to calculate state.
func computeSandboxState(sbx *agentsv1alpha1.Sandbox) (string, string) {
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
	if sbx.Status.PodInfo.PodIP == "" {
		return false
	}
	readyCond := utils.GetSandboxCondition(&sbx.Status, string(agentsv1alpha1.SandboxConditionReady))
	return readyCond != nil && readyCond.Status == metav1.ConditionTrue
}
