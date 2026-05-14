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

package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/controller/events"
)

// drainEvents collects all events currently buffered in the recorder without blocking.
func drainEvents(rec *record.FakeRecorder) []string {
	var got []string
	for {
		select {
		case e := <-rec.Events:
			got = append(got, e)
		default:
			return got
		}
	}
}

// containsEvent reports whether any of evts contains substr.
func containsEvent(evts []string, substr string) bool {
	for _, e := range evts {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

// TestCommonControl_EnsureClaimClaiming_BindingEventGuard verifies that the
// SandboxClaimBinding event is emitted only on the first transition into the
// Claiming phase and is suppressed on subsequent reconciles while the claim is
// already in Claiming, preventing event spam on retries.
func TestCommonControl_EnsureClaimClaiming_BindingEventGuard(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name              string
		currentPhase      agentsv1alpha1.SandboxClaimPhase
		expectBindingFire bool
	}{
		{
			name:              "first entry into Claiming emits SandboxClaimBinding",
			currentPhase:      "",
			expectBindingFire: true,
		},
		{
			name:              "already in Claiming suppresses SandboxClaimBinding",
			currentPhase:      agentsv1alpha1.SandboxClaimPhaseClaiming,
			expectBindingFire: false,
		},
		{
			name:              "transition from Completed back to Claiming emits SandboxClaimBinding",
			currentPhase:      agentsv1alpha1.SandboxClaimPhaseCompleted,
			expectBindingFire: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claim := &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "guard-claim",
					Namespace: "default",
					UID:       "guard-uid",
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "guard-set",
					Replicas:     int32Ptr(2),
				},
				Status: agentsv1alpha1.SandboxClaimStatus{
					Phase:           tt.currentPhase,
					ClaimedReplicas: 0,
				},
			}
			sbs := &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "guard-set",
					Namespace: "default",
				},
			}

			cache, fakeClient, err := cachetest.NewTestCache(t, claim, sbs)
			require.NoError(t, err)

			rec := record.NewFakeRecorder(20)
			ctrl := NewCommonControl(fakeClient, rec, cache)

			args := ClaimArgs{
				Claim:      claim,
				SandboxSet: sbs,
				NewStatus: &agentsv1alpha1.SandboxClaimStatus{
					Phase:           agentsv1alpha1.SandboxClaimPhaseClaiming,
					ClaimedReplicas: 0,
				},
			}

			_, ensureErr := ctrl.EnsureClaimClaiming(context.Background(), args)
			assert.NoError(t, ensureErr)

			evts := drainEvents(rec)
			fired := containsEvent(evts, events.SandboxClaimBinding)
			assert.Equal(t, tt.expectBindingFire, fired,
				"SandboxClaimBinding fire mismatch, got events: %v", evts)
		})
	}
}

// TestCommonControl_EnsureClaimCompleted_DeleteFailure exercises the error
// branch where the underlying client.Delete fails after TTL expires. The TTL
// delete event must still be recorded and the original delete error must be
// propagated unchanged so the controller-runtime exponential backoff kicks in.
func TestCommonControl_EnsureClaimCompleted_DeleteFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)

	claim := &agentsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ttl-fail-claim",
			Namespace: "default",
		},
		Spec: agentsv1alpha1.SandboxClaimSpec{
			TemplateName:      "fail-set",
			TTLAfterCompleted: &metav1.Duration{Duration: 5 * time.Second},
		},
	}

	pastCompletion := metav1.NewTime(time.Now().Add(-30 * time.Second))
	newStatus := &agentsv1alpha1.SandboxClaimStatus{
		Phase:          agentsv1alpha1.SandboxClaimPhaseCompleted,
		CompletionTime: &pastCompletion,
	}

	wantErr := fmt.Errorf("simulated delete failure")
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim).
		WithStatusSubresource(&agentsv1alpha1.SandboxClaim{}).
		Build()
	wrapped := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			return wantErr
		},
	})

	rec := record.NewFakeRecorder(10)
	ctrl := NewCommonControl(wrapped, rec, nil)

	strategy, err := ctrl.EnsureClaimCompleted(context.Background(), ClaimArgs{
		Claim:     claim,
		NewStatus: newStatus,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "simulated delete failure")
	assert.False(t, strategy.Immediate, "should not requeue immediately when delete fails")
	assert.Equal(t, time.Duration(0), strategy.After, "should rely on caller backoff after delete error")

	evts := drainEvents(rec)
	assert.True(t, containsEvent(evts, events.SandboxClaimTTLDelete),
		"SandboxClaimTTLDelete event expected before delete error, got events: %v", evts)
}

// TestCommonControl_EnsureClaimCompleted_NegativeTTLNoEvent asserts that a
// negative TTL short-circuits without emitting deletion events even when the
// completion time is far in the past. This guards against accidental event
// emission for the "never delete" configuration semantics.
func TestCommonControl_EnsureClaimCompleted_NegativeTTLNoEvent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)

	claim := &agentsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "never-delete-claim",
			Namespace: "default",
		},
		Spec: agentsv1alpha1.SandboxClaimSpec{
			TemplateName:      "neg-set",
			TTLAfterCompleted: &metav1.Duration{Duration: -1 * time.Second},
		},
	}
	completion := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	newStatus := &agentsv1alpha1.SandboxClaimStatus{
		Phase:          agentsv1alpha1.SandboxClaimPhaseCompleted,
		CompletionTime: &completion,
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim).
		WithStatusSubresource(&agentsv1alpha1.SandboxClaim{}).
		Build()
	rec := record.NewFakeRecorder(10)
	ctrl := NewCommonControl(fakeClient, rec, nil)

	strategy, err := ctrl.EnsureClaimCompleted(context.Background(), ClaimArgs{
		Claim:     claim,
		NewStatus: newStatus,
	})
	require.NoError(t, err)
	assert.False(t, strategy.Immediate)
	assert.Equal(t, time.Duration(0), strategy.After)

	evts := drainEvents(rec)
	assert.False(t, containsEvent(evts, events.SandboxClaimTTLDelete),
		"negative TTL must not emit SandboxClaimTTLDelete, got events: %v", evts)
}
