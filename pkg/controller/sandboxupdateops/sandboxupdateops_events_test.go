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

package sandboxupdateops

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/events"
)

func TestSandboxUpdateOpsLifecycleEvents(t *testing.T) {
	tests := []struct {
		name         string
		ops          *agentsv1alpha1.SandboxUpdateOps
		sandboxes    []*agentsv1alpha1.Sandbox
		expectEvents []string
	}{
		{
			name:      "pending ops with candidates emits UpdateOpsStarted event",
			ops:       newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsPending, false, nil),
			sandboxes: []*agentsv1alpha1.Sandbox{newSandbox("sbx-1", "default", "", agentsv1alpha1.SandboxRunning, nil)},
			expectEvents: []string{
				events.UpdateOpsStarted,
				events.UpdateOpsProgressing,
			},
		},
		{
			name: "all sandboxes completed emits UpdateOpsCompleted event",
			ops:  newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil),
			sandboxes: []*agentsv1alpha1.Sandbox{
				newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxRunning, []metav1.Condition{
					{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonSucceeded, Status: metav1.ConditionTrue},
				}),
			},
			expectEvents: []string{events.UpdateOpsCompleted},
		},
		{
			name: "sandbox upgrade failure emits UpdateOpsFailed event",
			ops:  newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil),
			sandboxes: []*agentsv1alpha1.Sandbox{
				newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxRunning, []metav1.Condition{
					{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed, Status: metav1.ConditionFalse},
				}),
			},
			expectEvents: []string{events.UpdateOpsFailed},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objs []runtime.Object
			objs = append(objs, tt.ops)
			for _, sbx := range tt.sandboxes {
				objs = append(objs, sbx)
			}

			// Build reconciler using the existing helper pattern
			r := newTestReconciler(objs...)

			// Replace recorder with a fresh one so we can inspect events
			fakeRecorder := record.NewFakeRecorder(100)
			r.Recorder = fakeRecorder

			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: tt.ops.Name, Namespace: tt.ops.Namespace},
			})
			assert.NoError(t, err)

			// Collect all emitted events
			close(fakeRecorder.Events)
			var gotEvents []string
			for e := range fakeRecorder.Events {
				gotEvents = append(gotEvents, e)
			}

			// Verify expected events
			for _, expected := range tt.expectEvents {
				found := false
				for _, got := range gotEvents {
					if strings.Contains(got, expected) {
						found = true
						break
					}
				}
				assert.True(t, found, "expected event containing %q not found in %v", expected, gotEvents)
			}
		})
	}
}

