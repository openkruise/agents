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

package sandboxserving

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agents "github.com/openkruise/agents/api/v1alpha1"
	binding "github.com/openkruise/agents/pkg/servingsession/binding"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

func desiredReplicas(parent *agents.SandboxServing) int32 {
	if parent.Spec.Replicas == nil {
		return 1
	}
	return *parent.Spec.Replicas
}

func maxSurge(parent *agents.SandboxServing) (int32, error) {
	value := intstr.FromString("25%")
	if parent.Spec.UpdateStrategy.MaxSurge != nil {
		value = *parent.Spec.UpdateStrategy.MaxSurge
	}
	if value.Type == intstr.Int {
		if value.IntVal <= 0 {
			return 0, fmt.Errorf("maxSurge must be positive")
		}
		return value.IntVal, nil
	}
	if value.Type != intstr.String {
		return 0, fmt.Errorf("invalid maxSurge type")
	}
	text := value.StrVal
	if !strings.HasSuffix(text, "%") {
		return 0, fmt.Errorf("maxSurge must be an integer or percentage")
	}
	digits := strings.TrimSuffix(text, "%")
	for _, ch := range digits {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("invalid maxSurge percentage")
		}
	}
	percent, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || percent <= 0 {
		return 0, fmt.Errorf("maxSurge percentage must be positive")
	}
	desired := int64(desiredReplicas(parent))
	// Divide before multiplication to avoid overflowing large valid percentages.
	if desired > 0 && percent > (int64(math.MaxInt32)*100)/desired {
		return 0, fmt.Errorf("maxSurge exceeds supported replica range")
	}
	surge := (desired*percent + 99) / 100
	if surge < 0 || surge > math.MaxInt32 {
		return 0, fmt.Errorf("maxSurge exceeds supported replica range")
	}
	return int32(surge), nil
}

func readyMember(sbx *agents.Sandbox) bool {
	return sbx.Status.Phase == agents.SandboxRunning && meta.IsStatusConditionTrue(sbx.Status.Conditions, string(agents.SandboxConditionReady))
}

// revisionParent reconstructs creation inputs from the named immutable snapshot.
func (r *Reconciler) revisionParent(ctx context.Context, parent *agents.SandboxServing, name string) (*agents.SandboxServing, error) {
	var revision appsv1.ControllerRevision
	if err := r.Get(ctx, client.ObjectKey{Namespace: parent.Namespace, Name: name}, &revision); err != nil {
		return nil, err
	}
	if !owned(parent, &revision) || provisionRevisionName(parent, revision.Data.Raw) != name {
		return nil, fmt.Errorf("Revision %s has invalid ownership or content", name)
	}
	creation := parent.DeepCopy()
	creation.Spec.Provision = agents.SandboxServingProvision{}
	if err := json.Unmarshal(revision.Data.Raw, &creation.Spec.Provision); err != nil {
		return nil, fmt.Errorf("decode Revision %s: %w", name, err)
	}
	if creation.Spec.Provision.SandboxTemplate == nil {
		return nil, fmt.Errorf("Revision %s has no template", name)
	}
	return creation, nil
}

// deleteMember is mechanical; callers separately authorize binding-safe shrink
// or Ready-budgeted old-version replacement. Pod grace/preStop remain normal.
func (r *Reconciler) deleteMember(ctx context.Context, parent *agents.SandboxServing, sbx *agents.Sandbox) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	uid, rv := sbx.UID, sbx.ResourceVersion
	key := expectationKey(parent)
	r.deleteUIDs.Store(key+"\x00"+sbx.Name, uid)
	r.Expectations.ExpectScale(key, expectations.Delete, sbx.Name)
	if err := r.Delete(ctx, sbx, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil {
		r.deleteUIDs.Delete(key + "\x00" + sbx.Name)
		r.Expectations.ObserveScale(key, expectations.Delete, sbx.Name)
		return fmt.Errorf("delete Serving member %s: %w", sbx.Name, err)
	}
	return nil
}

// revisionMembers groups the version counts and candidates from one observation.
type revisionMembers struct {
	old, targets        []*agents.Sandbox
	target, targetReady int32
	footprint           int32
}

func observeMemberRevisions(parent *agents.SandboxServing, revision string, snapshot []agents.Sandbox, active []*agents.Sandbox, replicas int32) revisionMembers {
	members := revisionMembers{footprint: replicas}
	for _, sbx := range active {
		if sbx.Labels[binding.ServingRevisionLabel] == revision {
			members.targets = append(members.targets, sbx)
			members.target++
			if readyMember(sbx) {
				members.targetReady++
			}
		} else {
			members.old = append(members.old, sbx)
		}
	}
	for i := range snapshot {
		sbx := &snapshot[i]
		if owned(parent, sbx) && (!sbx.DeletionTimestamp.IsZero() || sbx.Status.Phase == agents.SandboxTerminating) {
			members.footprint++
		}
	}
	return members
}

func (r *Reconciler) reconcileMembers(ctx context.Context, parent *agents.SandboxServing, revision string, snapshot []agents.Sandbox, active []*agents.Sandbox, status *agents.SandboxServingStatus) error {
	// A create with a lost response may have released its immediate expectation.
	// Resolve its original name/snapshot before making any new capacity decision.
	var unresolvedName, unresolvedRevision string
	prefix := expectationKey(parent) + "\x00"
	r.createRevisions.Range(func(key, value any) bool {
		if strings.HasPrefix(key.(string), prefix) {
			unresolvedName = strings.TrimPrefix(key.(string), prefix)
			unresolvedRevision = value.(string)
			return false
		}
		return true
	})
	if unresolvedName != "" {
		return r.createMember(ctx, parent, unresolvedName, unresolvedRevision)
	}
	desired := desiredReplicas(parent)
	surge, err := maxSurge(parent)
	if err != nil {
		setCondition(status, parent, "Supported", false, "Unsupported", err.Error())
		return nil
	}
	members := observeMemberRevisions(parent, revision, snapshot, active, status.Replicas)
	target, targetReady, footprint := members.target, members.targetReady, members.footprint
	rolling := len(members.old) > 0 || (status.CurrentRevision != "" && status.CurrentRevision != revision)
	if desired == 0 && footprint == 0 {
		status.CurrentRevision = revision
		return nil
	}
	// A replica reduction is ordinary safe shrink, including while upgrading.
	overCap := int64(footprint) > int64(desired)+int64(surge)
	if desired == 0 || target > desired || (!rolling && status.Replicas > desired) || (rolling && overCap && status.Replicas > desired) {
		candidates := active
		count := status.Replicas - desired
		if desired > 0 && target > desired {
			// Target surplus is ordinary shrink even before CurrentRevision advances.
			// It cannot authorize deleting occupied targets or Ready old members.
			candidates = members.targets
			count = target - desired
		} else if desired > 0 && rolling {
			excess := int64(footprint) - int64(desired) - int64(surge)
			if excess < 0 || excess > math.MaxInt32 {
				return fmt.Errorf("excess replicas exceed supported replica range")
			}
			count = min(count, int32(excess))
		}
		remaining, err := r.scaleDown(ctx, parent, candidates, count)
		if remaining > 0 {
			setCondition(status, parent, "ScaleDownBlocked", true, "BindingsPresentOrInvalid", "Surplus members have occupied or invalid bindings")
		}
		return err
	}
	if !rolling {
		if status.Replicas < desired {
			return r.scaleUp(ctx, parent, revision, snapshot, min(desired-status.Replicas, 16))
		}
		if target == desired && targetReady == desired {
			status.CurrentRevision = revision
		}
		return nil
	}
	if target == desired && targetReady == desired && status.Replicas == desired && footprint == desired {
		status.CurrentRevision = revision
		return nil
	}
	space := int64(desired) + int64(surge) - int64(footprint)
	if target < desired && space > 0 {
		return r.scaleUp(ctx, parent, revision, snapshot, min(desired-target, int32(min(space, 16))))
	}
	return r.replaceOldMembers(ctx, parent, members, desired, space, status)
}

// replaceOldMembers preserves the same-snapshot surge and Ready budgets.
func (r *Reconciler) replaceOldMembers(ctx context.Context, parent *agents.SandboxServing, members revisionMembers, desired int32, space int64, status *agents.SandboxServingStatus) error {
	old := members.old
	sort.Slice(old, func(i, j int) bool { return old[i].Name < old[j].Name })
	// A target churn can leave unready obsolete surge members occupying all slots.
	// Reclaim only unbound unavailable members, without granting rollout permission.
	if members.target < desired && space <= 0 && status.Replicas > desired {
		for _, sbx := range old {
			if !readyMember(sbx) && hasNoBindings(sbx) {
				return r.deleteMember(ctx, parent, sbx)
			}
		}
	}
	if members.targetReady == 0 {
		return nil
	}
	removable := status.Replicas - desired
	readyBudget := status.ReadyReplicas - desired
	for _, sbx := range old {
		if removable <= 0 {
			break
		}
		if readyMember(sbx) {
			if readyBudget <= 0 {
				continue
			}
			readyBudget--
		}
		if err := r.deleteMember(ctx, parent, sbx); err != nil {
			return err
		}
		removable--
	}
	return nil
}
