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
	"net"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/proxy"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

func TestSandboxManager_WakeSandbox(t *testing.T) {
	tests := []struct {
		name                  string
		state                 string
		phase                 v1alpha1.SandboxPhase
		annotations           map[string]string
		initialTimeout        timeout.Options
		deleting              bool
		pausedCondition       metav1.ConditionStatus
		readyCondition        metav1.ConditionStatus
		infraErr              error
		syncPeerIP            string
		expectAction          proxy.WakeAction
		expectSaveCalls       int
		expectResumeCalls     int
		expectPauseTime       bool
		expectShutdownTime    bool
		expectClearedTimeouts bool
		expectDuration        time.Duration
		expectAnnotation      string
	}{
		{
			name:               "paused duration manual timeout writes shutdown",
			state:              v1alpha1.SandboxStatePaused,
			phase:              v1alpha1.SandboxPaused,
			annotations:        map[string]string{v1alpha1.AnnotationWakeOnTraffic: "timeout:10m"},
			initialTimeout:     timeout.Options{ShutdownTime: time.Now().Add(time.Hour)},
			pausedCondition:    metav1.ConditionTrue,
			expectAction:       proxy.WakeActionResumed,
			expectSaveCalls:    1,
			expectResumeCalls:  1,
			expectShutdownTime: true,
			expectDuration:     10 * time.Minute,
			expectAnnotation:   "timeout:10m",
		},
		{
			name:        "paused duration auto pause writes pause and max shutdown",
			state:       v1alpha1.SandboxStatePaused,
			phase:       v1alpha1.SandboxPaused,
			annotations: map[string]string{v1alpha1.AnnotationWakeOnTraffic: "timeout:10m"},
			initialTimeout: timeout.Options{
				PauseTime:    time.Now().Add(time.Hour),
				ShutdownTime: time.Now().Add(24 * time.Hour),
			},
			pausedCondition:    metav1.ConditionTrue,
			expectAction:       proxy.WakeActionResumed,
			expectSaveCalls:    1,
			expectResumeCalls:  1,
			expectPauseTime:    true,
			expectShutdownTime: true,
			expectDuration:     10 * time.Minute,
			expectAnnotation:   "timeout:10m",
		},
		{
			name:                  "paused never policy clears timeouts",
			state:                 v1alpha1.SandboxStatePaused,
			phase:                 v1alpha1.SandboxPaused,
			annotations:           map[string]string{v1alpha1.AnnotationWakeOnTraffic: "timeout:never"},
			initialTimeout:        timeout.Options{ShutdownTime: time.Now().Add(time.Hour)},
			pausedCondition:       metav1.ConditionTrue,
			expectAction:          proxy.WakeActionResumed,
			expectSaveCalls:       1,
			expectResumeCalls:     1,
			expectClearedTimeouts: true,
			expectAnnotation:      "timeout:never",
		},
		{
			name:               "duration below minimum clamps to five minutes",
			state:              v1alpha1.SandboxStatePaused,
			phase:              v1alpha1.SandboxPaused,
			annotations:        map[string]string{v1alpha1.AnnotationWakeOnTraffic: "timeout:30s"},
			initialTimeout:     timeout.Options{ShutdownTime: time.Now().Add(time.Hour)},
			pausedCondition:    metav1.ConditionTrue,
			expectAction:       proxy.WakeActionResumed,
			expectSaveCalls:    1,
			expectResumeCalls:  1,
			expectShutdownTime: true,
			expectDuration:     5 * time.Minute,
			expectAnnotation:   "timeout:30s",
		},
		{
			name:             "running returns already running",
			state:            v1alpha1.SandboxStateRunning,
			phase:            v1alpha1.SandboxRunning,
			annotations:      map[string]string{v1alpha1.AnnotationWakeOnTraffic: "timeout:10m"},
			expectAction:     proxy.WakeActionAlreadyRunning,
			expectAnnotation: "timeout:10m",
		},
		{
			name:            "absent annotation disables auto resume",
			state:           v1alpha1.SandboxStatePaused,
			phase:           v1alpha1.SandboxPaused,
			pausedCondition: metav1.ConditionTrue,
			expectAction:    proxy.WakeActionAutoResumeDisabled,
		},
		{
			name:             "invalid annotation returns invalid policy",
			state:            v1alpha1.SandboxStatePaused,
			phase:            v1alpha1.SandboxPaused,
			annotations:      map[string]string{v1alpha1.AnnotationWakeOnTraffic: "true"},
			pausedCondition:  metav1.ConditionTrue,
			expectAction:     proxy.WakeActionInvalidAutoResumePolicy,
			expectAnnotation: "true",
		},
		{
			name:             "deleting sandbox is gone",
			state:            v1alpha1.SandboxStateDead,
			phase:            v1alpha1.SandboxPaused,
			annotations:      map[string]string{v1alpha1.AnnotationWakeOnTraffic: "timeout:10m"},
			deleting:         true,
			expectAction:     proxy.WakeActionGone,
			expectAnnotation: "timeout:10m",
		},
		{
			name:             "paused phase with false paused condition is pausing",
			state:            v1alpha1.SandboxStatePaused,
			phase:            v1alpha1.SandboxPaused,
			annotations:      map[string]string{v1alpha1.AnnotationWakeOnTraffic: "timeout:10m"},
			pausedCondition:  metav1.ConditionFalse,
			expectAction:     proxy.WakeActionPausing,
			expectAnnotation: "timeout:10m",
		},
		{
			name:             "creating state is bad state",
			state:            v1alpha1.SandboxStateCreating,
			phase:            v1alpha1.SandboxPending,
			annotations:      map[string]string{v1alpha1.AnnotationWakeOnTraffic: "timeout:10m"},
			expectAction:     proxy.WakeActionBadState,
			expectAnnotation: "timeout:10m",
		},
		{
			name:             "resuming phase is bad state",
			state:            v1alpha1.SandboxStatePaused,
			phase:            v1alpha1.SandboxResuming,
			annotations:      map[string]string{v1alpha1.AnnotationWakeOnTraffic: "timeout:10m"},
			expectAction:     proxy.WakeActionBadState,
			expectAnnotation: "timeout:10m",
		},
		{
			name:         "not found maps to not found action",
			infraErr:     managererrors.NewError(managererrors.ErrorNotFound, "missing"),
			expectAction: proxy.WakeActionNotFound,
		},
		{
			name:         "cache not found maps to not found action",
			infraErr:     fmt.Errorf("sandbox sandbox not found in cache"),
			expectAction: proxy.WakeActionNotFound,
		},
		{
			name:               "route sync failure still reports resumed",
			state:              v1alpha1.SandboxStatePaused,
			phase:              v1alpha1.SandboxPaused,
			annotations:        map[string]string{v1alpha1.AnnotationWakeOnTraffic: "timeout:10m"},
			initialTimeout:     timeout.Options{ShutdownTime: time.Now().Add(time.Hour)},
			pausedCondition:    metav1.ConditionTrue,
			syncPeerIP:         "%",
			expectAction:       proxy.WakeActionResumed,
			expectSaveCalls:    1,
			expectResumeCalls:  1,
			expectShutdownTime: true,
			expectDuration:     10 * time.Minute,
			expectAnnotation:   "timeout:10m",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := newWakeFakeSandbox(tt)
			manager := &SandboxManager{
				infra:      &fakeInfrastructure{sandbox: sbx, err: tt.infraErr},
				proxy:      proxy.NewServer(testManagerOptions()),
				maxTimeout: 30 * time.Minute,
			}
			if tt.syncPeerIP != "" {
				manager.proxy.SetPeersManager(&wakeTestPeers{members: []peers.Peer{{IP: tt.syncPeerIP}}})
			}

			start := time.Now()
			result, err := manager.WakeSandbox(t.Context(), "sandbox")
			require.NoError(t, err)

			assert.Equal(t, tt.expectAction, result.Action)
			if sbx != nil {
				assert.Equal(t, tt.expectResumeCalls, sbx.resumeCalls)
				assert.Equal(t, tt.expectSaveCalls, sbx.saveCalls)
				assert.Equal(t, tt.expectAnnotation, sbx.GetAnnotations()[v1alpha1.AnnotationWakeOnTraffic])
			}
			if tt.expectSaveCalls == 0 {
				return
			}
			if tt.expectClearedTimeouts {
				assert.True(t, sbx.lastOptions.PauseTime.IsZero())
				assert.True(t, sbx.lastOptions.ShutdownTime.IsZero())
				return
			}
			if tt.expectPauseTime {
				assert.WithinDuration(t, start.Add(tt.expectDuration), sbx.lastOptions.PauseTime, 5*time.Second)
				assert.WithinDuration(t, start.Add(30*time.Minute), sbx.lastOptions.ShutdownTime, 5*time.Second)
				return
			}
			if tt.expectShutdownTime {
				assert.True(t, sbx.lastOptions.PauseTime.IsZero())
				assert.WithinDuration(t, start.Add(tt.expectDuration), sbx.lastOptions.ShutdownTime, 5*time.Second)
			}
		})
	}
}

func TestSandboxManager_WakeSandboxMetrics(t *testing.T) {
	tests := []struct {
		name         string
		setup        func() *fakeSandbox
		expectAction proxy.WakeAction
	}{
		{
			name: "records resumed action and duration",
			setup: func() *fakeSandbox {
				return newWakeMetricsSandbox(v1alpha1.SandboxStatePaused, v1alpha1.SandboxPaused,
					map[string]string{v1alpha1.AnnotationWakeOnTraffic: "timeout:10m"})
			},
			expectAction: proxy.WakeActionResumed,
		},
		{
			name: "records disabled action and duration",
			setup: func() *fakeSandbox {
				return newWakeMetricsSandbox(v1alpha1.SandboxStatePaused, v1alpha1.SandboxPaused, nil)
			},
			expectAction: proxy.WakeActionAutoResumeDisabled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := tt.setup()
			manager := &SandboxManager{
				infra:      &fakeInfrastructure{sandbox: sbx},
				proxy:      proxy.NewServer(testManagerOptions()),
				maxTimeout: 30 * time.Minute,
			}
			beforeResponses := testutil.ToFloat64(sandboxWakeResponses.WithLabelValues(string(tt.expectAction)))
			beforeDurationCount := wakeDurationSampleCount(t)

			result, err := manager.WakeSandbox(t.Context(), "sandbox")

			require.NoError(t, err)
			assert.Equal(t, tt.expectAction, result.Action)
			assert.Equal(t, float64(1), testutil.ToFloat64(sandboxWakeResponses.WithLabelValues(string(tt.expectAction)))-beforeResponses)
			assert.Equal(t, uint64(1), wakeDurationSampleCount(t)-beforeDurationCount)
		})
	}
}

func wakeDurationSampleCount(t *testing.T) uint64 {
	t.Helper()
	metric := &dto.Metric{}
	require.NoError(t, sandboxWakeDuration.Write(metric))
	require.NotNil(t, metric.Histogram)
	return metric.GetHistogram().GetSampleCount()
}

func newWakeMetricsSandbox(state string, phase v1alpha1.SandboxPhase, annotations map[string]string) *fakeSandbox {
	sbx := newFakeSandbox("sandbox")
	sbx.state = state
	sbx.timeout = timeout.Options{ShutdownTime: time.Now().Add(time.Hour)}
	sbx.raw.Annotations = map[string]string{}
	for key, value := range annotations {
		sbx.raw.Annotations[key] = value
	}
	sbx.SetAnnotations(sbx.raw.Annotations)
	sbx.raw.Status.Phase = phase
	sbx.raw.Status.Conditions = append(sbx.raw.Status.Conditions, metav1.Condition{
		Type:   string(v1alpha1.SandboxConditionPaused),
		Status: metav1.ConditionTrue,
	})
	return sbx
}

func TestSandboxManager_WakeSandboxRunningPhasePausedSpecResumes(t *testing.T) {
	tests := []struct {
		name string
	}{
		{
			name: "running phase with paused spec resumes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			initial := timeout.Options{ShutdownTime: time.Now().Add(time.Hour)}
			sharedTimeout := newPolicyTimeoutState(initial)
			sbx := newPolicyWakeFakeSandbox("sandbox", "timeout:10m", sharedTimeout)
			sbx.raw.Status.Phase = v1alpha1.SandboxRunning
			sbx.raw.Spec.Paused = true
			manager := &SandboxManager{
				infra:      &fakeInfrastructure{sandbox: sbx},
				proxy:      proxy.NewServer(testManagerOptions()),
				maxTimeout: 30 * time.Minute,
			}

			start := time.Now()
			result, err := manager.WakeSandbox(t.Context(), "sandbox")
			require.NoError(t, err)

			assert.Equal(t, proxy.WakeActionResumed, result.Action)
			assert.Equal(t, 1, sbx.resumeCalls)
			assert.Equal(t, 1, sbx.saveCalls)
			finalTimeout := sharedTimeout.get()
			assert.True(t, finalTimeout.PauseTime.IsZero())
			assert.WithinDuration(t, start.Add(10*time.Minute), finalTimeout.ShutdownTime, 5*time.Second)
		})
	}
}

func TestWakeActionForConnectErrorReclassifiesSandbox(t *testing.T) {
	tests := []struct {
		name         string
		refreshState string
		phase        v1alpha1.SandboxPhase
		pausedStatus metav1.ConditionStatus
		deleting     bool
		expectAction proxy.WakeAction
		expectError  string
	}{
		{
			name:         "conflict with refreshed running state is already running",
			refreshState: v1alpha1.SandboxStateRunning,
			phase:        v1alpha1.SandboxRunning,
			expectAction: proxy.WakeActionAlreadyRunning,
		},
		{
			name:         "conflict with refreshed dead state is gone",
			refreshState: v1alpha1.SandboxStateDead,
			phase:        v1alpha1.SandboxFailed,
			expectAction: proxy.WakeActionGone,
		},
		{
			name:         "conflict with paused phase and false paused condition is pausing",
			refreshState: v1alpha1.SandboxStatePaused,
			phase:        v1alpha1.SandboxPaused,
			pausedStatus: metav1.ConditionFalse,
			expectAction: proxy.WakeActionPausing,
		},
		{
			name:         "conflict with resuming phase is bad state",
			refreshState: v1alpha1.SandboxStatePaused,
			phase:        v1alpha1.SandboxResuming,
			expectAction: proxy.WakeActionBadState,
		},
		{
			name:         "conflict with deleted sandbox is gone",
			refreshState: v1alpha1.SandboxStatePaused,
			phase:        v1alpha1.SandboxPaused,
			pausedStatus: metav1.ConditionTrue,
			deleting:     true,
			expectAction: proxy.WakeActionGone,
		},
		{
			name:        "non-conflict bubbles error",
			expectError: "resume failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := newPolicyWakeFakeSandbox("sandbox", "timeout:10m", newPolicyTimeoutState(timeout.Options{}))
			sbx.refreshFn = func(context.Context, bool) error {
				sbx.state = tt.refreshState
				sbx.raw.Status.Phase = tt.phase
				sbx.raw.Status.Conditions = nil
				if tt.pausedStatus != "" {
					sbx.raw.Status.Conditions = append(sbx.raw.Status.Conditions, metav1.Condition{
						Type:   string(v1alpha1.SandboxConditionPaused),
						Status: tt.pausedStatus,
					})
				}
				if tt.deleting {
					deletionTimestamp := metav1.Now()
					sbx.raw.DeletionTimestamp = &deletionTimestamp
				}
				return nil
			}
			var err error = managererrors.NewError(managererrors.ErrorConflict, "conflict without internal reason string")
			if tt.expectError != "" {
				err = errors.New(tt.expectError)
			}

			action, actionErr := wakeActionForConnectError(t.Context(), err, sbx)

			if tt.expectError != "" {
				require.Error(t, actionErr)
				assert.Contains(t, actionErr.Error(), tt.expectError)
				assert.Empty(t, action)
				return
			}
			require.NoError(t, actionErr)
			assert.Equal(t, tt.expectAction, action)
			assert.Equal(t, 1, sbx.refreshCalls)
		})
	}
}

func TestSandboxManager_WakeSandboxResumeConflictReclassifiesCurrentSandbox(t *testing.T) {
	tests := []struct {
		name         string
		currentPhase v1alpha1.SandboxPhase
		pausedStatus metav1.ConditionStatus
		expectAction proxy.WakeAction
	}{
		{
			name:         "current sandbox is still pausing",
			currentPhase: v1alpha1.SandboxPaused,
			pausedStatus: metav1.ConditionFalse,
			expectAction: proxy.WakeActionPausing,
		},
		{
			name:         "current sandbox is not pausing",
			currentPhase: v1alpha1.SandboxResuming,
			expectAction: proxy.WakeActionBadState,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			initial := timeout.Options{ShutdownTime: time.Now().Add(time.Hour)}
			sbx := newPolicyWakeFakeSandbox("sandbox", "timeout:10m", newPolicyTimeoutState(initial))
			sbx.resumeFn = func(context.Context) error {
				sbx.raw.Status.Phase = tt.currentPhase
				sbx.raw.Status.Conditions = nil
				if tt.pausedStatus != "" {
					sbx.raw.Status.Conditions = append(sbx.raw.Status.Conditions, metav1.Condition{
						Type:   string(v1alpha1.SandboxConditionPaused),
						Status: tt.pausedStatus,
					})
				}
				return managererrors.NewError(managererrors.ErrorConflict, "conflict without internal reason string")
			}
			manager := &SandboxManager{
				infra:      &fakeInfrastructure{sandbox: sbx},
				proxy:      proxy.NewServer(testManagerOptions()),
				maxTimeout: 30 * time.Minute,
			}

			result, err := manager.WakeSandbox(t.Context(), "sandbox")

			require.NoError(t, err)
			assert.Equal(t, tt.expectAction, result.Action)
			assert.Equal(t, 1, sbx.resumeCalls)
			assert.Equal(t, 1, sbx.refreshCalls)
			assert.Equal(t, 0, sbx.saveCalls)
		})
	}
}

func TestSandboxManager_WakeSandboxConcurrentWakeWake(t *testing.T) {
	tests := []struct {
		name                string
		annotations         []string
		expectLongestOffset time.Duration
	}{
		{
			name:                "two paused wake callers both succeed and longest timeout survives",
			annotations:         []string{"timeout:5m", "timeout:15m"},
			expectLongestOffset: 15 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			initial := timeout.Options{ShutdownTime: time.Now().Add(time.Hour)}
			sharedTimeout := newPolicyTimeoutState(initial)
			timeoutReads := make(chan struct{}, len(tt.annotations))
			releaseTimeoutReads := make(chan struct{})
			resumeTracker := &firstWriterResumeTracker{}

			sandboxes := make([]infra.Sandbox, 0, len(tt.annotations))
			for i, annotation := range tt.annotations {
				sbx := newPolicyWakeFakeSandbox("sandbox", annotation, sharedTimeout)
				sbx.getTimeoutFn = func() timeout.Options {
					snapshot := sharedTimeout.get()
					timeoutReads <- struct{}{}
					<-releaseTimeoutReads
					return snapshot
				}
				sbx.resumeFn = resumeTracker.resume
				sandboxes = append(sandboxes, sbx)
				_ = i
			}
			manager := &SandboxManager{
				infra:      &sequenceInfrastructure{sandboxes: sandboxes},
				proxy:      proxy.NewServer(testManagerOptions()),
				maxTimeout: 30 * time.Minute,
			}

			results := make(chan proxy.WakeResult, len(tt.annotations))
			errs := make(chan error, len(tt.annotations))
			var wg sync.WaitGroup
			start := time.Now()
			for range tt.annotations {
				wg.Add(1)
				go func() {
					defer wg.Done()
					result, err := manager.WakeSandbox(t.Context(), "sandbox")
					results <- result
					errs <- err
				}()
			}
			for range tt.annotations {
				select {
				case <-timeoutReads:
				case <-time.After(5 * time.Second):
					t.Fatal("timed out waiting for concurrent wake timeout snapshots")
				}
			}
			close(releaseTimeoutReads)
			wg.Wait()
			close(results)
			close(errs)

			for err := range errs {
				require.NoError(t, err)
			}
			for result := range results {
				assert.Equal(t, proxy.WakeActionResumed, result.Action)
			}
			assert.Equal(t, len(tt.annotations), resumeTracker.attempts())
			assert.Equal(t, 1, resumeTracker.updates())
			finalTimeout := sharedTimeout.get()
			assert.True(t, finalTimeout.PauseTime.IsZero())
			assert.WithinDuration(t, start.Add(tt.expectLongestOffset), finalTimeout.ShutdownTime, 5*time.Second)
		})
	}
}

func TestSandboxManager_WakeSandboxCrossFormZeroTimeRaces(t *testing.T) {
	tests := []struct {
		name              string
		wakeFirst         bool
		expectFinalZero   bool
		expectLoserResult int
	}{
		{
			name:              "connect finite wins before stale wake never and deadline remains",
			wakeFirst:         false,
			expectFinalZero:   false,
			expectLoserResult: 1,
		},
		{
			name:              "wake never wins before stale connect finite and timeout remains cleared",
			wakeFirst:         true,
			expectFinalZero:   true,
			expectLoserResult: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			initial := timeout.Options{ShutdownTime: time.Now().Add(time.Hour)}
			sharedTimeout := newPolicyTimeoutState(initial)
			manager := &SandboxManager{
				proxy:      proxy.NewServer(testManagerOptions()),
				maxTimeout: 30 * time.Minute,
			}
			connectSbx := newPolicyWakeFakeSandbox("sandbox", "timeout:300s", sharedTimeout)
			connectEndAt := time.Now().Add(300 * time.Second)

			if tt.wakeFirst {
				wakeSbx := newPolicyWakeFakeSandbox("sandbox", "timeout:never", sharedTimeout)
				manager.infra = &fakeInfrastructure{sandbox: wakeSbx}
				result, err := manager.WakeSandbox(t.Context(), "sandbox")
				require.NoError(t, err)
				assert.Equal(t, proxy.WakeActionResumed, result.Action)

				err = manager.ConnectOrWake(t.Context(), connectSbx, ConnectOrWakeInput{
					PreState: v1alpha1.SandboxStatePaused,
					PreEndAt: initial.ShutdownTime,
					Baseline: initial,
					NewEndAt: connectEndAt,
				})
				require.NoError(t, err)
			} else {
				wakeObserved := make(chan struct{})
				releaseWakeResume := make(chan struct{})
				var closeObserved sync.Once
				wakeSbx := newPolicyWakeFakeSandbox("sandbox", "timeout:never", sharedTimeout)
				wakeSbx.getTimeoutFn = func() timeout.Options {
					closeObserved.Do(func() {
						close(wakeObserved)
					})
					return sharedTimeout.get()
				}
				wakeSbx.resumeFn = func(ctx context.Context) error {
					select {
					case <-releaseWakeResume:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				manager.infra = &fakeInfrastructure{sandbox: wakeSbx}
				wakeResult := make(chan proxy.WakeResult, 1)
				wakeErr := make(chan error, 1)
				go func() {
					result, err := manager.WakeSandbox(t.Context(), "sandbox")
					wakeResult <- result
					wakeErr <- err
				}()
				select {
				case <-wakeObserved:
				case <-time.After(5 * time.Second):
					t.Fatal("timed out waiting for wake timeout snapshot")
				}

				err := manager.ConnectOrWake(t.Context(), connectSbx, ConnectOrWakeInput{
					PreState: v1alpha1.SandboxStatePaused,
					PreEndAt: initial.ShutdownTime,
					Baseline: initial,
					NewEndAt: connectEndAt,
				})
				require.NoError(t, err)
				close(releaseWakeResume)
				require.NoError(t, <-wakeErr)
				assert.Equal(t, proxy.WakeActionResumed, (<-wakeResult).Action)
			}

			results := sharedTimeout.results()
			require.Len(t, results, 2)
			assert.True(t, results[0].Updated)
			assert.False(t, results[tt.expectLoserResult].Updated)
			finalTimeout := sharedTimeout.get()
			if tt.expectFinalZero {
				assert.True(t, finalTimeout.ShutdownTime.IsZero())
				assert.True(t, finalTimeout.PauseTime.IsZero())
			} else {
				assert.True(t, finalTimeout.PauseTime.IsZero())
				assert.WithinDuration(t, connectEndAt, finalTimeout.ShutdownTime, 5*time.Second)
			}
		})
	}
}

func newWakeFakeSandbox(tt struct {
	name                  string
	state                 string
	phase                 v1alpha1.SandboxPhase
	annotations           map[string]string
	initialTimeout        timeout.Options
	deleting              bool
	pausedCondition       metav1.ConditionStatus
	readyCondition        metav1.ConditionStatus
	infraErr              error
	syncPeerIP            string
	expectAction          proxy.WakeAction
	expectSaveCalls       int
	expectResumeCalls     int
	expectPauseTime       bool
	expectShutdownTime    bool
	expectClearedTimeouts bool
	expectDuration        time.Duration
	expectAnnotation      string
}) *fakeSandbox {
	if tt.infraErr != nil {
		return nil
	}
	sbx := newFakeSandbox("sandbox")
	sbx.state = tt.state
	sbx.timeout = tt.initialTimeout
	sbx.raw.Annotations = map[string]string{}
	for key, value := range tt.annotations {
		sbx.raw.Annotations[key] = value
	}
	sbx.SetAnnotations(sbx.raw.Annotations)
	sbx.raw.Status.Phase = tt.phase
	if tt.pausedCondition != "" {
		sbx.raw.Status.Conditions = append(sbx.raw.Status.Conditions, metav1.Condition{
			Type:   string(v1alpha1.SandboxConditionPaused),
			Status: tt.pausedCondition,
		})
	}
	if tt.readyCondition != "" {
		sbx.raw.Status.Conditions = append(sbx.raw.Status.Conditions, metav1.Condition{
			Type:   string(v1alpha1.SandboxConditionReady),
			Status: tt.readyCondition,
		})
	}
	if tt.deleting {
		deletionTimestamp := metav1.Now()
		sbx.raw.DeletionTimestamp = &deletionTimestamp
		sbx.SetDeletionTimestamp(&deletionTimestamp)
	}
	return sbx
}

func newPolicyWakeFakeSandbox(id string, annotation string, state *policyTimeoutState) *fakeSandbox {
	sbx := newFakeSandbox(id)
	sbx.state = v1alpha1.SandboxStatePaused
	sbx.timeout = state.get()
	sbx.raw.Status.Phase = v1alpha1.SandboxPaused
	sbx.raw.Status.Conditions = []metav1.Condition{
		{
			Type:   string(v1alpha1.SandboxConditionPaused),
			Status: metav1.ConditionTrue,
		},
	}
	sbx.raw.Annotations = map[string]string{
		v1alpha1.AnnotationWakeOnTraffic: annotation,
	}
	sbx.SetAnnotations(map[string]string{
		v1alpha1.AnnotationWakeOnTraffic: annotation,
	})
	sbx.saveFn = state.save
	sbx.getTimeoutFn = state.get
	return sbx
}

type firstWriterResumeTracker struct {
	mu             sync.Mutex
	resumeAttempts int
	resumeUpdates  int
	resumed        bool
}

func (t *firstWriterResumeTracker) resume(context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resumeAttempts++
	if !t.resumed {
		t.resumed = true
		t.resumeUpdates++
	}
	return nil
}

func (t *firstWriterResumeTracker) attempts() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.resumeAttempts
}

func (t *firstWriterResumeTracker) updates() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.resumeUpdates
}

type policyTimeoutState struct {
	mu      sync.Mutex
	current timeout.Options
	updates []infra.TimeoutUpdateResult
}

func newPolicyTimeoutState(initial timeout.Options) *policyTimeoutState {
	return &policyTimeoutState{current: timeout.Options{
		ShutdownTime: initial.ShutdownTime,
		PauseTime:    initial.PauseTime,
	}}
}

func (s *policyTimeoutState) get() timeout.Options {
	s.mu.Lock()
	defer s.mu.Unlock()
	return timeout.Options{
		ShutdownTime: s.current.ShutdownTime,
		PauseTime:    s.current.PauseTime,
	}
}

func (s *policyTimeoutState) results() []infra.TimeoutUpdateResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	results := make([]infra.TimeoutUpdateResult, len(s.updates))
	copy(results, s.updates)
	return results
}

func (s *policyTimeoutState) save(_ context.Context, opts timeout.Options, policy timeout.UpdatePolicy) (infra.TimeoutUpdateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	shouldUpdate := false
	switch policy {
	case timeout.UpdatePolicyAlways:
		shouldUpdate = !timeout.Equal(s.current, opts)
	case timeout.UpdatePolicyExtendOnly:
		shouldUpdate = timeout.ShouldExtendTimeout(s.current, opts)
	case timeout.UpdatePolicyBaselineAware:
		if opts.Baseline == nil {
			return infra.TimeoutUpdateResult{}, fmt.Errorf("BaselineAware policy requires opts.Baseline to be set")
		}
		if timeout.Equal(s.current, *opts.Baseline) {
			shouldUpdate = !timeout.Equal(s.current, opts)
		} else {
			shouldUpdate = timeout.ShouldExtendTimeout(s.current, opts)
		}
	default:
		return infra.TimeoutUpdateResult{}, fmt.Errorf("unsupported timeout update policy %q", policy)
	}
	result := infra.TimeoutUpdateResult{Updated: shouldUpdate}
	if shouldUpdate {
		s.current = timeout.Options{
			ShutdownTime: opts.ShutdownTime,
			PauseTime:    opts.PauseTime,
		}
	}
	s.updates = append(s.updates, result)
	return result, nil
}

type sequenceInfrastructure struct {
	fakeInfrastructure
	mu        sync.Mutex
	sandboxes []infra.Sandbox
	next      int
}

func (f *sequenceInfrastructure) GetClaimedSandbox(context.Context, infra.GetClaimedSandboxOptions) (infra.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.next >= len(f.sandboxes) {
		return nil, managererrors.NewError(managererrors.ErrorNotFound, "sandbox not found")
	}
	sbx := f.sandboxes[f.next]
	f.next++
	return sbx, nil
}

type wakeTestPeers struct {
	members []peers.Peer
}

func (p *wakeTestPeers) Start(context.Context, int) error { return nil }
func (p *wakeTestPeers) Stop() error                      { return nil }
func (p *wakeTestPeers) GetPeers() []peers.Peer           { return p.members }
func (p *wakeTestPeers) GetAllMembers() []peers.Peer      { return p.members }
func (p *wakeTestPeers) WaitForPeers(context.Context, int) error {
	return nil
}
func (p *wakeTestPeers) LocalAddr() net.IP { return net.ParseIP("127.0.0.1") }
func (p *wakeTestPeers) LocalPort() int    { return 0 }
