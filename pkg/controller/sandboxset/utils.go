package sandboxset

import (
	"context"
	"strings"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/sandbox/core"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
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

func (r *Reconciler) initNewStatus(ss *agentsv1alpha1.SandboxSet) (*agentsv1alpha1.SandboxSetStatus, error) {
	newStatus := ss.Status.DeepCopy()
	hash, _ := core.HashSandbox(&agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			SandboxTemplate: ss.Spec.SandboxTemplate,
		},
	})
	newStatus.UpdateRevision = hash
	newStatus.ObservedGeneration = ss.Generation
	return newStatus, nil
}

func saveStatusFromGroup(newStatus *agentsv1alpha1.SandboxSetStatus, groups GroupedSandboxes) (actualReplicas int32) {
	newStatus.AvailableReplicas = int32(len(groups.Available))
	newStatus.Replicas = int32(len(groups.Creating)) + int32(len(groups.Available))
	return newStatus.Replicas
}

/* Just Reserved for SandboxAutoScaler
func calculateExpectPoolSize(ctx context.Context, total, unused int32, sbs *agentsv1alpha1.SandboxSet) (int32, error) {
	log := klog.FromContext(ctx).V(consts.DebugLogLevel)
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
		if strings.HasPrefix(k, agentsv1alpha1.E2BPrefix) {
			delete(m, k)
		}
	}
	return m
}

// scaleExpectationSatisfied logic:
// 1. if scaleUpExpectation is not satisfied, both scaling up and scaling down are forbidden
// 2. if scaleDownExpectation is not satisfied, scaling up is allowed and scaling down is forbidden
func scaleExpectationSatisfied(ctx context.Context, scaleExpectation expectations.ScaleExpectations, key string) (ok bool, requeueAfter time.Duration) {
	log := logf.FromContext(ctx)
	satisfied, unsatisfiedDuration, dirty := scaleExpectation.SatisfiedExpectations(key)
	if satisfied {
		return true, 0
	}

	if unsatisfiedDuration > expectations.ExpectationTimeout {
		scaleExpectation.DeleteExpectations(key)
		log.Error(nil, "expectation unsatisfied overtime, force delete the timeout expectation", "requeueAfter", retryAfterForceDeleteExpectation)
		return false, retryAfterForceDeleteExpectation
	}

	requeueAfter = expectations.ExpectationTimeout - unsatisfiedDuration
	log.Info("expectations not satisfied", "dirty", dirty, "requeueAfter", requeueAfter)
	return false, requeueAfter
}
