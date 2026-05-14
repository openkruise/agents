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

package sandboxclaim

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/events"
	"github.com/openkruise/agents/pkg/controller/sandboxclaim/core"
)

func TestSandboxClaimLifecycleEvents(t *testing.T) {
	tests := []struct {
		name         string
		claim        *agentsv1alpha1.SandboxClaim
		sandboxSet   *agentsv1alpha1.SandboxSet
		expectEvents []string
	}{
		{
			name: "completed claim with TTL expired emits SandboxClaimTTLDelete event",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "ttl-claim",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName:       "test-sandboxset",
					Replicas:           int32Ptr(1),
					TTLAfterCompleted:  &metav1.Duration{Duration: 1 * time.Second},
				},
				Status: agentsv1alpha1.SandboxClaimStatus{
					Phase:           agentsv1alpha1.SandboxClaimPhaseCompleted,
					ClaimedReplicas: 1,
					CompletionTime: &metav1.Time{
						Time: time.Now().Add(-5 * time.Second), // Completed 5s ago
					},
					ClaimStartTime: &metav1.Time{
						Time: time.Now().Add(-10 * time.Second),
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandboxset",
					Namespace: "default",
				},
			},
			expectEvents: []string{events.SandboxClaimTTLDelete},
		},
		{
			name: "claim with unknown phase emits UnknownPhase event",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "unknown-phase-claim",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "test-sandboxset",
					Replicas:     int32Ptr(1),
				},
				Status: agentsv1alpha1.SandboxClaimStatus{
					Phase: "SomeInvalidPhase",
					ClaimStartTime: &metav1.Time{
						Time: time.Now(),
					},
				},
			},
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandboxset",
					Namespace: "default",
				},
			},
			expectEvents: []string{events.UnknownPhase},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = agentsv1alpha1.AddToScheme(scheme)

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.claim, tt.sandboxSet).
				WithStatusSubresource(&agentsv1alpha1.SandboxClaim{}).
				Build()

			fakeRecorder := record.NewFakeRecorder(100)

			reconciler := &Reconciler{
				Client:   fakeClient,
				Scheme:   scheme,
				controls: core.NewClaimControl(fakeClient, fakeRecorder, nil),
				recorder: fakeRecorder,
			}

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      tt.claim.Name,
					Namespace: tt.claim.Namespace,
				},
			}

			_, _ = reconciler.Reconcile(context.Background(), req)

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
