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

package sandbox_manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	agentmetrics "github.com/openkruise/agents/pkg/metrics"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/sandboxid"
	"github.com/openkruise/agents/pkg/utils"
)

// ErrReservedSandboxIDMutation reports a caller attempt to mutate the core-owned ID label.
var ErrReservedSandboxIDMutation = errors.New("reserved sandbox ID label was mutated")

type reservedLabelSnapshot struct {
	present bool
	value   string
}

func snapshotReservedLabel(object metav1.Object) reservedLabelSnapshot {
	value, present := object.GetLabels()[sandboxid.LabelKey]
	return reservedLabelSnapshot{present: present, value: value}
}

func ensureReservedLabelUnchanged(object metav1.Object, before reservedLabelSnapshot) error {
	after := snapshotReservedLabel(object)
	if before == after {
		return nil
	}
	return fmt.Errorf("%w: %s is managed by sandbox-manager core", ErrReservedSandboxIDMutation, sandboxid.LabelKey)
}

func guardPreModifier(modifier func(infra.Sandbox) error) func(infra.Sandbox) error {
	if modifier == nil {
		return nil
	}
	return func(sandbox infra.Sandbox) error {
		before := snapshotReservedLabel(sandbox)
		if err := modifier(sandbox); err != nil {
			return err
		}
		return ensureReservedLabelUnchanged(sandbox, before)
	}
}

func trackSandboxIDAssignmentObject(
	state *sandboxIDAssignmentState,
	modifier func(infra.Sandbox) error,
) func(infra.Sandbox) error {
	if state == nil || !state.enabled {
		return modifier
	}
	return func(sandbox infra.Sandbox) error {
		state.namespace = sandbox.GetNamespace()
		state.name = sandbox.GetName()
		if modifier == nil {
			return nil
		}
		return modifier(sandbox)
	}
}

type sandboxIDAssignmentState struct {
	enabled       bool
	callerFailed  bool
	assignmentRan bool
	assigned      bool
	invalidUID    bool
	namespace     string
	name          string
}

func composePostModifier(
	modifier func(metav1.Object) (bool, error),
	enableAssignment bool,
) (func(metav1.Object) (bool, error), *sandboxIDAssignmentState) {
	state := &sandboxIDAssignmentState{enabled: enableAssignment}
	if modifier == nil && !enableAssignment {
		return nil, state
	}

	return func(sandbox metav1.Object) (bool, error) {
		state.namespace = sandbox.GetNamespace()
		state.name = sandbox.GetName()
		changed := false
		if modifier != nil {
			before := snapshotReservedLabel(sandbox)
			callerChanged, err := modifier(sandbox)
			if err != nil {
				state.callerFailed = true
				return false, err
			}
			if err := ensureReservedLabelUnchanged(sandbox, before); err != nil {
				state.callerFailed = true
				return false, err
			}
			changed = callerChanged
		}

		if !enableAssignment {
			return changed, nil
		}
		state.assignmentRan = true
		assigned, err := sandboxid.AssignShort(sandbox)
		if err != nil {
			state.invalidUID = true
			return false, err
		}
		state.assigned = state.assigned || assigned
		return changed || assigned, nil
	}, state
}

func (m *SandboxManager) prepareClaimSandboxIdentity(opts infra.ClaimSandboxOptions) (infra.ClaimSandboxOptions, *sandboxIDAssignmentState) {
	postModifier, state := composePostModifier(opts.PostModifier, m.enableShortID)
	state.namespace = opts.Namespace
	opts.Modifier = trackSandboxIDAssignmentObject(state, guardPreModifier(opts.Modifier))
	opts.PostModifier = postModifier
	return opts, state
}

func (m *SandboxManager) prepareCloneSandboxIdentity(opts infra.CloneSandboxOptions) (infra.CloneSandboxOptions, *sandboxIDAssignmentState) {
	postModifier, state := composePostModifier(opts.PostModifier, m.enableShortID)
	state.namespace = opts.Namespace
	state.name = opts.Name
	opts.Modifier = trackSandboxIDAssignmentObject(state, guardPreModifier(opts.Modifier))
	opts.PostModifier = postModifier
	return opts, state
}

func recordSandboxIDAssignment(ctx context.Context, state *sandboxIDAssignmentState, duration time.Duration, operationErr error) {
	if state == nil || !state.enabled {
		return
	}
	if operationErr == nil {
		if state.assigned {
			agentmetrics.RecordSandboxIDAssignment(agentmetrics.SandboxIDAssignmentResultSuccess)
			klog.FromContext(ctx).V(utils.DebugLogLevel).Info("assigned short Sandbox ID", "namespace", state.namespace, "name", state.name)
		}
		return
	}
	reason := sandboxIDAssignmentErrorReason(state, duration, operationErr)
	if reason == "" {
		return
	}
	agentmetrics.RecordSandboxIDAssignment(agentmetrics.SandboxIDAssignmentResultFailure)
	klog.FromContext(ctx).Error(operationErr, "failed to assign short Sandbox ID", "reason", reason, "namespace", state.namespace, "name", state.name)
}

func sandboxIDAssignmentErrorReason(state *sandboxIDAssignmentState, duration time.Duration, operationErr error) string {
	if state == nil || !state.enabled || operationErr == nil {
		return ""
	}
	// Failures before the final stage (readiness, runtime, token, or CSI) are
	// operation failures, not Sandbox ID assignment failures. applyPostModifier
	// records a non-zero duration even when its first direct Get fails.
	if duration <= 0 {
		return ""
	}
	if state.callerFailed {
		return ""
	}

	reason := "read_failed"
	switch {
	case state.invalidUID:
		reason = "invalid_uid"
	case errors.Is(operationErr, context.Canceled), errors.Is(operationErr, context.DeadlineExceeded):
		reason = "context_done"
	case state.assignmentRan:
		reason = "update_failed"
	}
	return reason
}
