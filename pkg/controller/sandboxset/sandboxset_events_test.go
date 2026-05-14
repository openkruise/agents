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
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/events"
)

// TestReconcile_RollingUpdateCompletedEventGuard verifies the state-transition guard
// added in PR #352: the SandboxSetRollingUpdateCompleted event must be emitted only
// when transitioning from an in-progress rolling update (UpdatedReplicas < Replicas)
// to fully updated, not on every reconcile after the rollout is already complete.
func TestReconcile_RollingUpdateCompletedEventGuard(t *testing.T) {
	tests := []struct {
		name string
		// pre-existing persisted sbs.Status before the reconcile
		preStatus    v1alpha1.SandboxSetStatus
		expectEvents []string
	}{
		{
			name: "transition emits completion event",
			// Status reflects an in-progress rolling update from a previous reconcile:
			// 2 replicas exist, but none have been counted as updated yet. With all
			// sandboxes already at the latest revision in the cluster, the next
			// reconcile must emit RollingUpdateCompleted exactly once.
			preStatus: v1alpha1.SandboxSetStatus{
				Replicas:          2,
				AvailableReplicas: 2,
				UpdatedReplicas:   0,
			},
			expectEvents: []string{events.SandboxSetRollingUpdateCompleted},
		},
		{
			name: "already up to date suppresses event",
			// Previous reconcile already observed the rollout as complete
			// (UpdatedReplicas == Replicas). The guard must suppress the event
			// to avoid duplicate notifications on every steady-state reconcile.
			preStatus: v1alpha1.SandboxSetStatus{
				Replicas:          2,
				AvailableReplicas: 2,
				UpdatedReplicas:   2,
			},
			expectEvents: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			k8sClient := NewClient()
			eventRecorder := record.NewFakeRecorder(10)
			reconciler := &Reconciler{
				Client:   k8sClient,
				Scheme:   testScheme,
				Recorder: eventRecorder,
				Codec:    codec,
			}

			sbs := getSandboxSet(2)
			assert.NoError(t, k8sClient.Create(ctx, sbs))

			// Compute the proper UpdateRevision so that pre-created sandboxes match it.
			newStatus, err := reconciler.initNewStatus(ctx, sbs)
			assert.NoError(t, err)
			tt.preStatus.UpdateRevision = newStatus.UpdateRevision

			// Persist the desired pre-reconcile status into the client.
			sbs.Status = tt.preStatus
			assert.NoError(t, k8sClient.Status().Update(ctx, sbs))

			// Pre-create 2 available sandboxes already at the latest revision.
			// This ensures: delta == 0, no rolling update needed, the controller
			// flows into the "All sandboxes are up to date" branch where the guard lives.
			CreateSandboxes(t, createSandboxRequest{createAvailableSandboxes: 2}, sbs, k8sClient)

			scaleUpExpectation.DeleteExpectations(GetControllerKey(sbs))
			scaleDownExpectation.DeleteExpectations(GetControllerKey(sbs))

			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: client.ObjectKeyFromObject(sbs),
			})
			assert.NoError(t, err)

			CheckAllEvents(t, eventRecorder, tt.expectEvents)
		})
	}
}

// TestReconcile_RollingUpdateStartEvent verifies that the SandboxSetRollingUpdate
// event added in PR #352 is emitted when a rolling update actually begins (i.e.
// some sandboxes still need to be updated).
func TestReconcile_RollingUpdateStartEvent(t *testing.T) {
	ctx := context.Background()
	k8sClient := NewClient()
	eventRecorder := record.NewFakeRecorder(20)
	reconciler := &Reconciler{
		Client:   k8sClient,
		Scheme:   testScheme,
		Recorder: eventRecorder,
		Codec:    codec,
	}

	sbs := getSandboxSet(3)
	assert.NoError(t, k8sClient.Create(ctx, sbs))

	// Pre-create sandboxes pinned to an old hash; the controller's UpdateRevision
	// will be re-computed from the spec, so these will be classified as "old".
	newStatus, err := reconciler.initNewStatus(ctx, sbs)
	assert.NoError(t, err)
	newStatus.UpdateRevision = "old-hash"
	sbs.Status = *newStatus
	CreateSandboxes(t, createSandboxRequest{createCreatingSandboxes: 3}, sbs, k8sClient)

	// Reset status revision so initNewStatus re-derives the latest one.
	sbs.Status.UpdateRevision = ""
	assert.NoError(t, k8sClient.Status().Update(ctx, sbs))

	scaleUpExpectation.DeleteExpectations(GetControllerKey(sbs))
	scaleDownExpectation.DeleteExpectations(GetControllerKey(sbs))

	_, err = reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(sbs),
	})
	assert.NoError(t, err)

	// Drain events; we expect at least one SandboxSetRollingUpdate event.
	var sawStart bool
	for {
		select {
		case ev := <-eventRecorder.Events:
			if containsEvent(ev, corev1.EventTypeNormal, events.SandboxSetRollingUpdate) {
				sawStart = true
			}
			continue
		default:
		}
		break
	}
	assert.True(t, sawStart, "expected SandboxSetRollingUpdate event when rolling update begins")
}

// containsEvent matches the FakeRecorder format "TYPE REASON MESSAGE" with a prefix check.
func containsEvent(event, tp, reason string) bool {
	prefix := tp + " " + reason
	return len(event) >= len(prefix) && event[:len(prefix)] == prefix
}
