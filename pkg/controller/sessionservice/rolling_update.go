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

package sessionservice

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agents "github.com/openkruise/agents/api/v1alpha1"
	binding "github.com/openkruise/agents/pkg/sessionservice/binding"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

func desiredReplicas(parent *agents.SessionService) int32 {
	if parent.Spec.Replicas == nil {
		return 1
	}
	return *parent.Spec.Replicas
}

func maxSurge(parent *agents.SessionService) (int32, error) {
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
	return int32((desired*percent + 99) / 100), nil
}

func readyMember(sbx *agents.Sandbox) bool {
	return sbx.Status.Phase == agents.SandboxRunning && meta.IsStatusConditionTrue(sbx.Status.Conditions, string(agents.SandboxConditionReady))
}

// deleteMember is mechanical; callers separately authorize binding-safe shrink
// or Ready-budgeted old-version replacement. Pod grace/preStop remain normal.
func (r *Reconciler) deleteMember(ctx context.Context, parent *agents.SessionService, sbx *agents.Sandbox) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	uid, rv := sbx.UID, sbx.ResourceVersion
	key := expectationKey(parent)
	r.Expectations.ExpectScale(key, expectations.Delete, sbx.Name)
	if err := r.Delete(ctx, sbx, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil {
		r.Expectations.ObserveScale(key, expectations.Delete, sbx.Name)
		return fmt.Errorf("delete SessionService member %s: %w", sbx.Name, err)
	}
	return nil
}

func (r *Reconciler) reconcileMembers(ctx context.Context, parent *agents.SessionService, revision string, snapshot []agents.Sandbox, active []*agents.Sandbox, status *agents.SessionServiceStatus) error {
	desired := desiredReplicas(parent)
	surge, err := maxSurge(parent)
	if err != nil {
		setCondition(status, parent, "Supported", false, "Unsupported", err.Error())
		return nil
	}
	members := observeMemberRevisions(parent, revision, snapshot, active, status.Replicas)
	updated := int32(len(members.updated))
	rolling := len(members.old) > 0 || (status.CurrentRevision != "" && status.CurrentRevision != revision)
	capacityLimit := int64(desired) + int64(surge)

	if desired == 0 && members.total == 0 {
		status.CurrentRevision = revision
		return nil
	}

	// Ordinary shrink always respects bindings, even during a rollout.
	switch {
	case desired == 0:
		return r.scaleDownAndReportBindings(ctx, parent, active, status.Replicas, status)
	case updated > desired:
		// Surplus updated members do not authorize deleting bound members.
		return r.scaleDownAndReportBindings(ctx, parent, members.updated, updated-desired, status)
	case !rolling && status.Replicas > desired:
		return r.scaleDownAndReportBindings(ctx, parent, active, status.Replicas-desired, status)
	case rolling && int64(members.total) > capacityLimit && status.Replicas > desired:
		count := min(status.Replicas-desired, int32(int64(members.total)-capacityLimit))
		return r.scaleDownAndReportBindings(ctx, parent, active, count, status)
	}

	if !rolling {
		if status.Replicas < desired {
			return r.scaleUp(ctx, parent, revision, min(desired-status.Replicas, 16))
		}
		if updated == desired && members.updatedReady == desired {
			status.CurrentRevision = revision
		}
		return nil
	}

	if updated == desired && members.updatedReady == desired && status.Replicas == desired && members.total == desired {
		status.CurrentRevision = revision
		return nil
	}

	// Budget from observed CRs, including terminating members. Unknown creates
	// and cache lag can temporarily exceed this budget.
	space := capacityLimit - int64(members.total)
	if updated < desired && space > 0 {
		return r.scaleUp(ctx, parent, revision, min(desired-updated, int32(min(space, 16))))
	}
	return r.replaceOldMembers(ctx, parent, members, desired, space, status)
}

// memberRevisions summarizes the same observation used for capacity decisions.
// total includes controlled terminating CRs, which are absent from active.
type memberRevisions struct {
	old, updated []*agents.Sandbox
	updatedReady int32
	total        int32
}

func observeMemberRevisions(parent *agents.SessionService, revision string, snapshot []agents.Sandbox, active []*agents.Sandbox, replicas int32) memberRevisions {
	members := memberRevisions{total: replicas}
	for _, sbx := range active {
		if sbx.Labels[binding.SessionServiceRevisionLabel] != revision {
			members.old = append(members.old, sbx)
			continue
		}
		members.updated = append(members.updated, sbx)
		if readyMember(sbx) {
			members.updatedReady++
		}
	}
	for i := range snapshot {
		sbx := &snapshot[i]
		if owned(parent, sbx) && (!sbx.DeletionTimestamp.IsZero() || sbx.Status.Phase == agents.SandboxTerminating) {
			members.total++
		}
	}
	return members
}

func (r *Reconciler) scaleDownAndReportBindings(ctx context.Context, parent *agents.SessionService, candidates []*agents.Sandbox, count int32, status *agents.SessionServiceStatus) error {
	remaining, err := r.scaleDown(ctx, parent, candidates, count)
	if remaining > 0 {
		setCondition(status, parent, "ScaleDownBlocked", true, "BindingsPresentOrInvalid", "Surplus members have occupied or invalid bindings")
	}
	return err
}

// replaceOldMembers separates rollout deletion permission from ordinary shrink.
// Unready, unbound obsolete members can free a slot; otherwise replacement needs
// a Ready updated member and enough remaining replicas, including a Ready budget.
func (r *Reconciler) replaceOldMembers(ctx context.Context, parent *agents.SessionService, members memberRevisions, desired int32, space int64, status *agents.SessionServiceStatus) error {
	sort.Slice(members.old, func(i, j int) bool { return members.old[i].Name < members.old[j].Name })
	// A target churn can leave unready obsolete surge members occupying all slots.
	// Reclaim only unbound unavailable members, without granting rollout permission.
	if int32(len(members.updated)) < desired && space <= 0 && status.Replicas > desired {
		for _, sbx := range members.old {
			if !readyMember(sbx) && hasNoBindings(sbx) {
				return r.deleteMember(ctx, parent, sbx)
			}
		}
	}
	if members.updatedReady == 0 {
		return nil
	}
	removable := status.Replicas - desired
	readyBudget := status.ReadyReplicas - desired
	for _, sbx := range members.old {
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
