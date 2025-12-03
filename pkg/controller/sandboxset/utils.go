package sandboxset

import (
	"strings"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	apps "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

var scaleExpectation = expectations.NewScaleExpectations()

// GetControllerKey return key of CloneSet.
func GetControllerKey(sbs *agentsv1alpha1.SandboxSet) string {
	return types.NamespacedName{Namespace: sbs.Namespace, Name: sbs.Name}.String()
}

type GroupedSandboxes struct {
	Creating  []*agentsv1alpha1.Sandbox // Sandboxes being created or initialized
	Available []*agentsv1alpha1.Sandbox // Initialized but not yet claimed Sandboxes
	Used      []*agentsv1alpha1.Sandbox // Sandboxes claimed by client agents
	Failed    []*agentsv1alpha1.Sandbox // Sandboxes should be deleted
}

var (
	GroupCreating  = "creating"
	GroupFailed    = "failed"
	GroupAvailable = "available"
	GroupUsed      = "used"
	GroupUnknown   = "unknown"
)

func (r *Reconciler) initNewStatus(ss *agentsv1alpha1.SandboxSet) (*agentsv1alpha1.SandboxSetStatus, error) {
	newStatus := ss.Status.DeepCopy()
	updateRevision, err := r.newRevision(ss, 0, nil)
	if err != nil {
		return nil, err
	}
	newStatus.UpdateRevision = updateRevision.Labels[ControllerRevisionHashLabel]
	newStatus.ObservedGeneration = ss.Generation
	return newStatus, nil
}

func saveStatusFromGroup(newStatus *agentsv1alpha1.SandboxSetStatus, groups GroupedSandboxes) (actualReplicas int32) {
	newStatus.AvailableReplicas = int32(len(groups.Available))
	newStatus.Replicas = int32(len(groups.Creating)) + int32(len(groups.Available))
	return newStatus.Replicas
}

func findSandboxGroup(sbx *agentsv1alpha1.Sandbox) (group, reason string) {
	if sbx.DeletionTimestamp != nil {
		return GroupFailed, "ResourceDeleted"
	}
	switch sbx.Status.Phase {
	case "":
		return GroupCreating, "ResourcePhaseEmpty"
	case agentsv1alpha1.SandboxPending:
		return GroupCreating, "ResourcePending"
	case agentsv1alpha1.SandboxFailed:
		return GroupFailed, "ResourceFailed"
	case agentsv1alpha1.SandboxSucceeded:
		return GroupFailed, "ResourceSucceeded"
	case agentsv1alpha1.SandboxTerminating:
		return GroupFailed, "ResourceTerminating"
	default: // Running, Paused
		switch sbx.Labels[agentsv1alpha1.LabelSandboxState] {
		case agentsv1alpha1.SandboxStateRunning:
			return GroupUsed, "SandboxStateRunning"
		case agentsv1alpha1.SandboxStatePaused:
			return GroupUsed, "SandboxStatePaused"
		case agentsv1alpha1.SandboxStateAvailable:
			return GroupAvailable, "SandboxStateAvailable"
		case agentsv1alpha1.SandboxStateKilling:
			return GroupFailed, "SandboxStateKilling"
		case "":
			return GroupCreating, "SandboxStateNotPatched"
		default: // impossible, just in case
			return GroupUnknown, "SandboxStateUnknown"
		}
	}
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
		if strings.HasPrefix(k, agentsv1alpha1.InternalPrefix) {
			delete(m, k)
		}
	}
	return m
}

func checkSandboxReady(sbx *agentsv1alpha1.Sandbox) bool {
	cond := utils.GetSandboxCondition(&sbx.Status, string(agentsv1alpha1.SandboxConditionReady))
	return cond != nil && cond.Status == metav1.ConditionTrue
}

// newRevision creates a new ControllerRevision containing a patch that reapplies the target state of set.
// The Revision of the returned ControllerRevision is set to revision. If the returned error is nil, the returned
// ControllerRevision is valid. StatefulSet revisions are stored as patches that re-apply the current state of set
// to a new StatefulSet using a strategic merge patch to replace the saved state of the new StatefulSet.
func (r *Reconciler) newRevision(set *agentsv1alpha1.SandboxSet, revision int64, collisionCount *int32) (*apps.ControllerRevision, error) {
	patch, err := r.getPatch(set)
	if err != nil {
		return nil, err
	}
	cr, err := NewControllerRevision(set,
		sandboxSetControllerKind,
		set.Spec.Template.Labels,
		runtime.RawExtension{Raw: patch},
		revision,
		collisionCount)
	if err != nil {
		return nil, err
	}
	if cr.ObjectMeta.Annotations == nil {
		cr.ObjectMeta.Annotations = make(map[string]string)
	}
	for key, value := range set.Annotations {
		cr.ObjectMeta.Annotations[key] = value
	}
	return cr, nil
}
