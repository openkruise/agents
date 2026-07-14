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

package sandbox

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"strconv"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

// newAutoPauseTestScheme creates a runtime scheme with both client-go and agents schemes.
func newAutoPauseTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	return scheme
}

// newAutoPauseReconciler builds a SandboxReconciler backed by a fake client
// pre-populated with the given objects. The recorder is a fake recorder with
// a large buffer to prevent blocking.
func newAutoPauseReconciler(t *testing.T, scheme *runtime.Scheme, objs ...client.Object) (*SandboxReconciler, client.Client) {
	t.Helper()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&agentsv1alpha1.Sandbox{}).
		WithObjects(objs...).
		Build()
	fakeRecorder := record.NewFakeRecorder(100)
	r := &SandboxReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		recorder: fakeRecorder,
	}
	return r, fakeClient
}

// makeProbeSandbox creates a Sandbox with AutoPausePolicy and lifecycle probes
// configured for testing.
func makeProbeSandbox(name string, phase agentsv1alpha1.SandboxPhase, mods ...func(*agentsv1alpha1.Sandbox)) *agentsv1alpha1.Sandbox {
	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
					},
				},
			},
			Probes: []agentsv1alpha1.Probe{
				{
					Name: "activity",
					Probe: corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							Exec: &corev1.ExecAction{
								Command: []string{"/bin/sh", "-c", "echo active"},
							},
						},
					},
				},
			},
			AutoPausePolicy: &agentsv1alpha1.AutoPausePolicy{
				Pause: &agentsv1alpha1.PausePolicy{
					WhenProbedIdleState: &agentsv1alpha1.ProbedIdleStateRule{
						Probe:             "activity",
						MessageRegex:      "^inactive$",
						ThresholdDuration: &metav1.Duration{Duration: 1 * time.Minute},
					},
				},
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: phase,
		},
	}
	for _, mod := range mods {
		mod(box)
	}
	return box
}

// ------------------------------------------------------------------
// Pure function tests
// ------------------------------------------------------------------

func TestHasActiveAutoPausePolicy(t *testing.T) {
	tests := []struct {
		name string
		box  *agentsv1alpha1.Sandbox
		want bool
	}{
		{
			name: "nil policy",
			box:  &agentsv1alpha1.Sandbox{},
			want: false,
		},
		{
			name: "policy with no strategies",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					AutoPausePolicy: &agentsv1alpha1.AutoPausePolicy{},
				},
			},
			want: false,
		},
		{
			name: "policy with only pause rule",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					AutoPausePolicy: &agentsv1alpha1.AutoPausePolicy{
						Pause: &agentsv1alpha1.PausePolicy{
							WhenProbedIdleState: &agentsv1alpha1.ProbedIdleStateRule{Probe: "p"},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "policy with only resume rule",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					AutoPausePolicy: &agentsv1alpha1.AutoPausePolicy{
						Resume: &agentsv1alpha1.ResumePolicy{
							WhenProbedScheduleTime: &agentsv1alpha1.ProbedScheduleTimeRule{Probe: "p"},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "policy with both rules",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					AutoPausePolicy: &agentsv1alpha1.AutoPausePolicy{
						Pause: &agentsv1alpha1.PausePolicy{
							WhenProbedIdleState: &agentsv1alpha1.ProbedIdleStateRule{Probe: "p"},
						},
						Resume: &agentsv1alpha1.ResumePolicy{
							WhenProbedScheduleTime: &agentsv1alpha1.ProbedScheduleTimeRule{Probe: "p"},
						},
					},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hasActiveAutoPausePolicy(tt.box))
		})
	}
}

func TestIsUnclaimedPoolSandbox(t *testing.T) {
	tests := []struct {
		name string
		box  *agentsv1alpha1.Sandbox
		want bool
	}{
		{
			name: "claimed=false",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxIsClaimed: "false",
					},
				},
			},
			want: true,
		},
		{
			name: "claimed=true",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxIsClaimed: "true",
					},
				},
			},
			want: false,
		},
		{
			name: "no label",
			box:  &agentsv1alpha1.Sandbox{},
			want: false,
		},
		{
			name: "nil labels",
			box:  &agentsv1alpha1.Sandbox{},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isUnclaimedPoolSandbox(tt.box))
		})
	}
}

func TestEvaluateResumeSchedule(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	condType := agentsv1alpha1.ProbeConditionPrefix + "resume"
	now := metav1.Now()
	futureTime := now.Time.Add(1 * time.Hour)
	futureTimestamp := futureTime.Unix()

	tests := []struct {
		name              string
		box               *agentsv1alpha1.Sandbox
		newStatus         *agentsv1alpha1.SandboxStatus
		wantNil           bool
		wantUnixSec       int64
		wantResumeCleared bool
	}{
		{
			name: "nil resume policy",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					AutoPausePolicy: &agentsv1alpha1.AutoPausePolicy{},
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{},
			wantNil:   true,
		},
		{
			name: "missing probe field",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					AutoPausePolicy: &agentsv1alpha1.AutoPausePolicy{
						Resume: &agentsv1alpha1.ResumePolicy{
							WhenProbedScheduleTime: &agentsv1alpha1.ProbedScheduleTimeRule{
								Probe: "",
							},
						},
					},
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{},
			wantNil:   true,
		},
		{
			name: "resume probe condition not found",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					AutoPausePolicy: &agentsv1alpha1.AutoPausePolicy{
						Resume: &agentsv1alpha1.ResumePolicy{
							WhenProbedScheduleTime: &agentsv1alpha1.ProbedScheduleTimeRule{
								Probe:      "resume",
								TimeFormat: "unix",
							},
						},
					},
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{},
			wantNil:   true,
		},
		{
			name: "resume probe condition not true",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					AutoPausePolicy: &agentsv1alpha1.AutoPausePolicy{
						Resume: &agentsv1alpha1.ResumePolicy{
							WhenProbedScheduleTime: &agentsv1alpha1.ProbedScheduleTimeRule{
								Probe:      "resume",
								TimeFormat: "unix",
							},
						},
					},
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:   condType,
						Status: metav1.ConditionFalse,
					},
				},
			},
			wantNil: true,
		},
		{
			name: "TimeFormat empty with valid timestamp",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					AutoPausePolicy: &agentsv1alpha1.AutoPausePolicy{
						Resume: &agentsv1alpha1.ResumePolicy{
							WhenProbedScheduleTime: &agentsv1alpha1.ProbedScheduleTimeRule{
								Probe:      "resume",
								TimeFormat: "",
								LeadTime:   &metav1.Duration{Duration: 5 * time.Minute},
							},
						},
					},
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:    condType,
						Status:  metav1.ConditionTrue,
						Message: strconv.FormatInt(futureTimestamp, 10),
					},
				},
			},
			wantNil:     false,
			wantUnixSec: futureTimestamp - int64(5*time.Minute/time.Second),
		},
		{
			name: "invalid unix timestamp in message",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					AutoPausePolicy: &agentsv1alpha1.AutoPausePolicy{
						Resume: &agentsv1alpha1.ResumePolicy{
							WhenProbedScheduleTime: &agentsv1alpha1.ProbedScheduleTimeRule{
								Probe:      "resume",
								TimeFormat: "unix",
							},
						},
					},
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:    condType,
						Status:  metav1.ConditionTrue,
						Message: "not-a-number",
					},
				},
				Schedules: []agentsv1alpha1.Schedule{
					{
						Reason:         agentsv1alpha1.ScheduleReasonProbedSchedule,
						NextResumeTime: &metav1.Time{Time: now.Time.Add(1 * time.Hour)},
					},
				},
			},
			wantNil:           true,
			wantResumeCleared: true,
		},
		{
			name: "valid future timestamp with default lead time",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					AutoPausePolicy: &agentsv1alpha1.AutoPausePolicy{
						Resume: &agentsv1alpha1.ResumePolicy{
							WhenProbedScheduleTime: &agentsv1alpha1.ProbedScheduleTimeRule{
								Probe:      "resume",
								TimeFormat: "unix",
								// LeadTime defaults to 5m via CRD defaulter; set explicitly in tests.
								LeadTime: &metav1.Duration{Duration: 5 * time.Minute},
							},
						},
					},
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:    condType,
						Status:  metav1.ConditionTrue,
						Message: strconv.FormatInt(futureTimestamp, 10),
					},
				},
			},
			wantNil:     false,
			wantUnixSec: futureTimestamp - int64(5*time.Minute/time.Second),
		},
		{
			name: "valid future timestamp with lead time",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					AutoPausePolicy: &agentsv1alpha1.AutoPausePolicy{
						Resume: &agentsv1alpha1.ResumePolicy{
							WhenProbedScheduleTime: &agentsv1alpha1.ProbedScheduleTimeRule{
								Probe:      "resume",
								TimeFormat: "unix",
								LeadTime:   &metav1.Duration{Duration: 5 * time.Minute},
							},
						},
					},
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:    condType,
						Status:  metav1.ConditionTrue,
						Message: strconv.FormatInt(futureTimestamp, 10),
					},
				},
			},
			wantNil:     false,
			wantUnixSec: futureTimestamp - int64(5*time.Minute/time.Second),
		},
		{
			name: "past timestamp returns non-nil",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					AutoPausePolicy: &agentsv1alpha1.AutoPausePolicy{
						Resume: &agentsv1alpha1.ResumePolicy{
							WhenProbedScheduleTime: &agentsv1alpha1.ProbedScheduleTimeRule{
								Probe:      "resume",
								TimeFormat: "unix",
								LeadTime:   &metav1.Duration{Duration: 5 * time.Minute},
							},
						},
					},
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				Conditions: []metav1.Condition{
					{
						Type:    condType,
						Status:  metav1.ConditionTrue,
						Message: "1", // Unix timestamp 1 = Jan 1, 1970
					},
				},
			},
			wantNil:     false,
			wantUnixSec: 1 - int64(5*time.Minute/time.Second),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := newAutoPauseReconciler(t, scheme, tt.box)
			result := r.evaluateResumeSchedule(tt.box, tt.newStatus)
			if tt.wantNil {
				assert.Nil(t, result)
			} else {
				assert.NotNil(t, result)
				if tt.wantUnixSec > 0 {
					assert.Equal(t, tt.wantUnixSec, result.Unix())
				}
			}
			if tt.wantResumeCleared {
				sched := findSchedule(tt.newStatus, agentsv1alpha1.ScheduleReasonProbedSchedule)
				if sched != nil {
					assert.Nil(t, sched.NextResumeTime, "NextResumeTime should be cleared when timestamp is invalid")
				}
			}
		})
	}
}

func TestEvaluatePauseSchedule(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	condType := agentsv1alpha1.ProbeConditionPrefix + "activity"
	lastTransition := metav1.NewTime(time.Now().Add(-30 * time.Second))

	// withCond builds a status carrying the activity probe condition.
	withCond := func(status metav1.ConditionStatus, msg string) *agentsv1alpha1.SandboxStatus {
		return &agentsv1alpha1.SandboxStatus{
			Conditions: []metav1.Condition{
				{
					Type:               condType,
					Status:             status,
					Message:            msg,
					LastTransitionTime: lastTransition,
				},
			},
		}
	}

	tests := []struct {
		name      string
		mod       func(*agentsv1alpha1.Sandbox)
		newStatus *agentsv1alpha1.SandboxStatus
		wantNil   bool
	}{
		{
			name:      "nil pause policy",
			mod:       func(b *agentsv1alpha1.Sandbox) { b.Spec.AutoPausePolicy.Pause = nil },
			newStatus: withCond(metav1.ConditionTrue, "inactive"),
			wantNil:   true,
		},
		{
			name: "missing threshold duration",
			mod: func(b *agentsv1alpha1.Sandbox) {
				b.Spec.AutoPausePolicy.Pause.WhenProbedIdleState.ThresholdDuration = nil
			},
			newStatus: withCond(metav1.ConditionTrue, "inactive"),
			wantNil:   true,
		},
		{
			name:      "invalid message regex",
			mod:       func(b *agentsv1alpha1.Sandbox) { b.Spec.AutoPausePolicy.Pause.WhenProbedIdleState.MessageRegex = "[" },
			newStatus: withCond(metav1.ConditionTrue, "inactive"),
			wantNil:   true,
		},
		{
			name:      "probe condition not found",
			newStatus: &agentsv1alpha1.SandboxStatus{},
			wantNil:   true,
		},
		{
			name:      "probe not succeeded (fail-closed)",
			newStatus: withCond(metav1.ConditionFalse, "inactive"),
			wantNil:   true,
		},
		{
			name:      "message does not match (agent active)",
			newStatus: withCond(metav1.ConditionTrue, "active"),
			wantNil:   true,
		},
		{
			name:      "agent idle returns lastTransition + threshold",
			newStatus: withCond(metav1.ConditionTrue, "inactive"),
			wantNil:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mods := []func(*agentsv1alpha1.Sandbox){}
			if tt.mod != nil {
				mods = append(mods, tt.mod)
			}
			box := makeProbeSandbox("get-pause-time", agentsv1alpha1.SandboxRunning, mods...)
			r, _ := newAutoPauseReconciler(t, scheme, box)

			got := r.evaluatePauseSchedule(box, tt.newStatus)
			if tt.wantNil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			// Default threshold from makeProbeSandbox is 1 minute.
			want := lastTransition.Add(1 * time.Minute)
			assert.Equal(t, want.Unix(), got.Unix())
		})
	}
}

func TestShouldPause(t *testing.T) {
	now := metav1.NewTime(time.Now())
	past := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	future := metav1.NewTime(time.Now().Add(1 * time.Minute))

	tests := []struct {
		name      string
		pauseTime *metav1.Time
		now       metav1.Time
		want      bool
	}{
		{
			name:      "nil pauseTime returns false",
			pauseTime: nil,
			now:       now,
			want:      false,
		},
		{
			name:      "pauseTime in the past returns true",
			pauseTime: &past,
			now:       now,
			want:      true,
		},
		{
			name:      "pauseTime in the future returns false",
			pauseTime: &future,
			now:       now,
			want:      false,
		},
		{
			name:      "pauseTime equals now returns true",
			pauseTime: &now,
			now:       now,
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shouldPause(tt.pauseTime, tt.now))
		})
	}
}

func TestShouldResume(t *testing.T) {
	now := metav1.NewTime(time.Now())
	past := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	future := metav1.NewTime(time.Now().Add(1 * time.Minute))

	tests := []struct {
		name       string
		resumeTime *metav1.Time
		now        metav1.Time
		want       bool
	}{
		{
			name:       "nil resumeTime returns false",
			resumeTime: nil,
			now:        now,
			want:       false,
		},
		{
			name:       "resumeTime in the past returns true",
			resumeTime: &past,
			now:        now,
			want:       true,
		},
		{
			name:       "resumeTime in the future returns false",
			resumeTime: &future,
			now:        now,
			want:       false,
		},
		{
			name:       "resumeTime equals now returns true",
			resumeTime: &now,
			now:        now,
			want:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shouldResume(tt.resumeTime, tt.now))
		})
	}
}

func TestRequeueAfter(t *testing.T) {
	now := metav1.NewTime(time.Now())
	past := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	near := metav1.NewTime(time.Now().Add(5 * time.Minute))
	far := metav1.NewTime(time.Now().Add(30 * time.Minute))

	tests := []struct {
		name  string
		now   metav1.Time
		times []*metav1.Time
		want  time.Duration
	}{
		{
			name:  "all nil returns 0",
			now:   now,
			times: []*metav1.Time{nil, nil},
			want:  0,
		},
		{
			name:  "single future time returns remaining",
			now:   now,
			times: []*metav1.Time{&near},
			want:  near.Sub(now.Time),
		},
		{
			name:  "earliest of two future times",
			now:   now,
			times: []*metav1.Time{&far, &near},
			want:  near.Sub(now.Time),
		},
		{
			name:  "past time is skipped",
			now:   now,
			times: []*metav1.Time{&past, &near},
			want:  near.Sub(now.Time),
		},
		{
			name:  "all past returns 0",
			now:   now,
			times: []*metav1.Time{&past},
			want:  0,
		},
		{
			name:  "no times returns 0",
			now:   now,
			times: nil,
			want:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := requeueAfter(tt.now, tt.times...)
			assert.InDelta(t, tt.want, got, float64(time.Second))
		})
	}
}

// ------------------------------------------------------------------
// handleAutoPause integration tests with fake client
// ------------------------------------------------------------------

func TestHandleAutoPause_SkipDeleting(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	box := makeProbeSandbox("deleting-sandbox", agentsv1alpha1.SandboxRunning, func(b *agentsv1alpha1.Sandbox) {
		b.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		b.Finalizers = []string{"sandbox.kruise.io/finalizer"}
	})
	r, _ := newAutoPauseReconciler(t, scheme, box)

	newStatus := box.Status.DeepCopy()
	requeueAfter, err := r.handleAutoPause(context.Background(), box, newStatus)
	assert.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeueAfter)
}

func TestHandleAutoPause_SkipTerminalPhases(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	phases := []agentsv1alpha1.SandboxPhase{
		agentsv1alpha1.SandboxRecycling,
		agentsv1alpha1.SandboxFailed,
		agentsv1alpha1.SandboxSucceeded,
	}
	for _, phase := range phases {
		t.Run(string(phase), func(t *testing.T) {
			box := makeProbeSandbox("terminal-sandbox", phase)
			r, _ := newAutoPauseReconciler(t, scheme, box)

			newStatus := box.Status.DeepCopy()
			requeueAfter, err := r.handleAutoPause(context.Background(), box, newStatus)
			assert.NoError(t, err)
			assert.Equal(t, time.Duration(0), requeueAfter)
		})
	}
}

func TestHandleAutoPause_SkipUnclaimedPoolSandbox(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	box := makeProbeSandbox("pool-sandbox", agentsv1alpha1.SandboxRunning, func(b *agentsv1alpha1.Sandbox) {
		b.Labels = map[string]string{
			agentsv1alpha1.LabelSandboxIsClaimed: "false",
		}
	})
	r, _ := newAutoPauseReconciler(t, scheme, box)

	newStatus := box.Status.DeepCopy()
	requeueAfter, err := r.handleAutoPause(context.Background(), box, newStatus)
	assert.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeueAfter)
}

func TestHandleAutoPause_RunningNoPauseStrategy(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	box := makeProbeSandbox("no-pause-strategy", agentsv1alpha1.SandboxRunning, func(b *agentsv1alpha1.Sandbox) {
		b.Spec.AutoPausePolicy.Pause = nil
	})
	r, _ := newAutoPauseReconciler(t, scheme, box)

	newStatus := box.Status.DeepCopy()
	requeueAfter, err := r.handleAutoPause(context.Background(), box, newStatus)
	assert.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeueAfter)
	assert.Nil(t, newStatus.Schedules)
}

func TestHandleAutoPause_RunningProbeConditionNotFound(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	box := makeProbeSandbox("no-probe-cond", agentsv1alpha1.SandboxRunning)
	r, _ := newAutoPauseReconciler(t, scheme, box)

	newStatus := box.Status.DeepCopy()
	requeueAfter, err := r.handleAutoPause(context.Background(), box, newStatus)
	assert.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeueAfter)
	assert.Nil(t, newStatus.Schedules)
}

func TestHandleAutoPause_RunningProbeUnhealthy(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	box := makeProbeSandbox("probe-unhealthy", agentsv1alpha1.SandboxRunning)
	condType := agentsv1alpha1.ProbeConditionPrefix + "activity"
	r, _ := newAutoPauseReconciler(t, scheme, box)

	newStatus := box.Status.DeepCopy()
	utils.SetSandboxCondition(newStatus, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionTrue,
		Reason:             agentsv1alpha1.ProbeReasonUnhealthy,
		Message:            "error",
		LastTransitionTime: metav1.Now(),
	})
	requeueAfter, err := r.handleAutoPause(context.Background(), box, newStatus)
	assert.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeueAfter)
	assert.Nil(t, newStatus.Schedules)
}

func TestHandleAutoPause_RunningProbeFailed(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	box := makeProbeSandbox("probe-failed", agentsv1alpha1.SandboxRunning)
	condType := agentsv1alpha1.ProbeConditionPrefix + "activity"
	r, _ := newAutoPauseReconciler(t, scheme, box)

	newStatus := box.Status.DeepCopy()
	utils.SetSandboxCondition(newStatus, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionFalse,
		Reason:             agentsv1alpha1.ProbeReasonError,
		Message:            "exit 1",
		LastTransitionTime: metav1.Now(),
	})
	requeueAfter, err := r.handleAutoPause(context.Background(), box, newStatus)
	assert.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeueAfter)
	assert.Nil(t, newStatus.Schedules)
}

func TestHandleAutoPause_RunningAgentActive(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	box := makeProbeSandbox("agent-active", agentsv1alpha1.SandboxRunning)
	condType := agentsv1alpha1.ProbeConditionPrefix + "activity"
	r, _ := newAutoPauseReconciler(t, scheme, box)

	newStatus := box.Status.DeepCopy()
	utils.SetSandboxCondition(newStatus, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionTrue,
		Reason:             agentsv1alpha1.ProbeReasonSucceeded,
		Message:            "active",
		LastTransitionTime: metav1.Now(),
	})
	requeueAfter, err := r.handleAutoPause(context.Background(), box, newStatus)
	assert.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeueAfter)
	assert.Nil(t, newStatus.Schedules)
}

func TestHandleAutoPause_RunningInactiveThresholdNotReached(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	box := makeProbeSandbox("threshold-not-reached", agentsv1alpha1.SandboxRunning, func(b *agentsv1alpha1.Sandbox) {
		b.Spec.AutoPausePolicy.Pause.WhenProbedIdleState.ThresholdDuration = &metav1.Duration{Duration: 10 * time.Minute}
	})
	condType := agentsv1alpha1.ProbeConditionPrefix + "activity"
	// Last transition time was 1 minute ago, threshold is 10 minutes
	lastTransition := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	r, _ := newAutoPauseReconciler(t, scheme, box)

	newStatus := box.Status.DeepCopy()
	utils.SetSandboxCondition(newStatus, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionTrue,
		Reason:             agentsv1alpha1.ProbeReasonSucceeded,
		Message:            "inactive",
		LastTransitionTime: lastTransition,
	})
	requeueAfter, err := r.handleAutoPause(context.Background(), box, newStatus)
	assert.NoError(t, err)
	assert.True(t, requeueAfter > 0)
	assert.True(t, requeueAfter < 10*time.Minute)
	// NextPauseTime is recorded for observability while waiting for the threshold.
	require.Len(t, newStatus.Schedules, 1)
	assert.NotNil(t, newStatus.Schedules[0].NextPauseTime)
	assert.Equal(t, agentsv1alpha1.ScheduleReasonProbedIdle, newStatus.Schedules[0].Reason)
}

func TestHandleAutoPause_RunningInactiveThresholdReached_Pause(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	box := makeProbeSandbox("pause-now", agentsv1alpha1.SandboxRunning, func(b *agentsv1alpha1.Sandbox) {
		b.Spec.AutoPausePolicy.Pause.WhenProbedIdleState.ThresholdDuration = &metav1.Duration{Duration: 1 * time.Minute}
	})
	condType := agentsv1alpha1.ProbeConditionPrefix + "activity"
	// Last transition time was 5 minutes ago, threshold is 1 minute
	lastTransition := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	r, fakeClient := newAutoPauseReconciler(t, scheme, box)

	newStatus := box.Status.DeepCopy()
	utils.SetSandboxCondition(newStatus, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionTrue,
		Reason:             agentsv1alpha1.ProbeReasonSucceeded,
		Message:            "inactive",
		LastTransitionTime: lastTransition,
	})
	requeueAfter, err := r.handleAutoPause(context.Background(), box, newStatus)
	require.NoError(t, err)

	// Verify sandbox was patched to paused=true
	updated := &agentsv1alpha1.Sandbox{}
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKeyFromObject(box), updated))
	assert.True(t, updated.Spec.Paused)

	// After the pause is triggered, NextPauseTime is cleared; Reason is retained.
	require.Len(t, newStatus.Schedules, 1)
	assert.Nil(t, newStatus.Schedules[0].NextPauseTime)
	assert.Equal(t, agentsv1alpha1.ScheduleReasonProbedIdle, newStatus.Schedules[0].Reason)
	assert.Nil(t, newStatus.Schedules[0].NextResumeTime)

	// Should requeue
	assert.True(t, requeueAfter >= 0)
}

func TestHandleAutoPause_RunningInvalidPauseRule_MissingFields(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	box := makeProbeSandbox("missing-fields", agentsv1alpha1.SandboxRunning, func(b *agentsv1alpha1.Sandbox) {
		b.Spec.AutoPausePolicy.Pause.WhenProbedIdleState.ThresholdDuration = nil
	})
	r, _ := newAutoPauseReconciler(t, scheme, box)

	newStatus := box.Status.DeepCopy()
	requeueAfter, err := r.handleAutoPause(context.Background(), box, newStatus)
	assert.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeueAfter)
}

func TestHandleAutoPause_PausedNoResumeTime(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	box := makeProbeSandbox("paused-no-resume", agentsv1alpha1.SandboxPaused, func(b *agentsv1alpha1.Sandbox) {
		b.Spec.Paused = true
	})
	r, _ := newAutoPauseReconciler(t, scheme, box)

	newStatus := box.Status.DeepCopy()
	requeueAfter, err := r.handleAutoPause(context.Background(), box, newStatus)
	assert.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeueAfter)
	assert.Nil(t, newStatus.Schedules)
}

func TestHandleAutoPause_PausedResumeTimeNotReached(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	condType := agentsv1alpha1.ProbeConditionPrefix + "activity"
	futureTime := time.Now().Add(30 * time.Minute)
	box := makeProbeSandbox("paused-future-resume", agentsv1alpha1.SandboxPaused, func(b *agentsv1alpha1.Sandbox) {
		b.Spec.Paused = true
		b.Spec.AutoPausePolicy.Resume = &agentsv1alpha1.ResumePolicy{
			WhenProbedScheduleTime: &agentsv1alpha1.ProbedScheduleTimeRule{
				Probe:      "activity",
				TimeFormat: "unix",
				LeadTime:   &metav1.Duration{Duration: 5 * time.Minute},
			},
		}
		b.Status.Conditions = []metav1.Condition{
			{
				Type:               condType,
				Status:             metav1.ConditionTrue,
				Reason:             agentsv1alpha1.ProbeReasonSucceeded,
				Message:            strconv.FormatInt(futureTime.Unix(), 10),
				LastTransitionTime: metav1.Now(),
			},
		}
	})
	r, _ := newAutoPauseReconciler(t, scheme, box)

	newStatus := box.Status.DeepCopy()
	requeueAfter, err := r.handleAutoPause(context.Background(), box, newStatus)
	assert.NoError(t, err)
	assert.True(t, requeueAfter > 0)
	assert.True(t, requeueAfter <= 30*time.Minute)

	// Verify Schedules is populated with the resume time and reason
	require.Len(t, newStatus.Schedules, 1)
	assert.NotNil(t, newStatus.Schedules[0].NextResumeTime)
	assert.Equal(t, agentsv1alpha1.ScheduleReasonProbedSchedule, newStatus.Schedules[0].Reason)
}

func TestHandleAutoPause_PausedResumeTimeReached_Resume(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	condType := agentsv1alpha1.ProbeConditionPrefix + "activity"
	pastTime := time.Now().Add(-1 * time.Minute)
	box := makeProbeSandbox("resume-now", agentsv1alpha1.SandboxPaused, func(b *agentsv1alpha1.Sandbox) {
		b.Spec.Paused = true
		b.Spec.AutoPausePolicy.Resume = &agentsv1alpha1.ResumePolicy{
			WhenProbedScheduleTime: &agentsv1alpha1.ProbedScheduleTimeRule{
				Probe:      "activity",
				TimeFormat: "unix",
				LeadTime:   &metav1.Duration{Duration: 5 * time.Minute},
			},
		}
		b.Status.Conditions = []metav1.Condition{
			{
				Type:               string(agentsv1alpha1.SandboxConditionPaused),
				Status:             metav1.ConditionTrue,
				Reason:             "Paused",
				LastTransitionTime: metav1.Now(),
			},
			{
				Type:               condType,
				Status:             metav1.ConditionTrue,
				Reason:             agentsv1alpha1.ProbeReasonSucceeded,
				Message:            strconv.FormatInt(pastTime.Unix(), 10),
				LastTransitionTime: metav1.Now(),
			},
		}
	})
	r, fakeClient := newAutoPauseReconciler(t, scheme, box)

	newStatus := box.Status.DeepCopy()
	requeueAfter, err := r.handleAutoPause(context.Background(), box, newStatus)
	require.NoError(t, err)

	// Verify sandbox was patched to paused=false
	updated := &agentsv1alpha1.Sandbox{}
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKeyFromObject(box), updated))
	assert.False(t, updated.Spec.Paused)

	// After the resume is triggered, NextResumeTime is cleared; NextPauseTime is
	// not set in the Paused phase. Reason is retained.
	require.Len(t, newStatus.Schedules, 1)
	assert.Nil(t, newStatus.Schedules[0].NextResumeTime)
	assert.Nil(t, newStatus.Schedules[0].NextPauseTime)
	assert.Equal(t, agentsv1alpha1.ScheduleReasonProbedSchedule, newStatus.Schedules[0].Reason)
	_ = requeueAfter
}

func TestHandleAutoPause_RunningWithResumeSchedule_UpdatesSchedules(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	pauseCondType := agentsv1alpha1.ProbeConditionPrefix + "activity"
	resumeCondType := agentsv1alpha1.ProbeConditionPrefix + "schedule"
	futureTime := time.Now().Add(30 * time.Minute)
	box := makeProbeSandbox("running-with-resume", agentsv1alpha1.SandboxRunning, func(b *agentsv1alpha1.Sandbox) {
		b.Spec.AutoPausePolicy.Resume = &agentsv1alpha1.ResumePolicy{
			WhenProbedScheduleTime: &agentsv1alpha1.ProbedScheduleTimeRule{
				Probe:      "schedule",
				TimeFormat: "unix",
				LeadTime:   &metav1.Duration{Duration: 5 * time.Minute},
			},
		}
		b.Status.Conditions = []metav1.Condition{
			{
				Type:               pauseCondType,
				Status:             metav1.ConditionTrue,
				Reason:             agentsv1alpha1.ProbeReasonSucceeded,
				Message:            "active",
				LastTransitionTime: metav1.Now(),
			},
			{
				Type:               resumeCondType,
				Status:             metav1.ConditionTrue,
				Reason:             agentsv1alpha1.ProbeReasonSucceeded,
				Message:            strconv.FormatInt(futureTime.Unix(), 10),
				LastTransitionTime: metav1.Now(),
			},
		}
	})
	r, _ := newAutoPauseReconciler(t, scheme, box)

	newStatus := box.Status.DeepCopy()
	requeueAfter, err := r.handleAutoPause(context.Background(), box, newStatus)
	assert.NoError(t, err)

	// Verify Schedules is populated with the resume time and reason
	require.Len(t, newStatus.Schedules, 1)
	assert.NotNil(t, newStatus.Schedules[0].NextResumeTime)
	assert.Equal(t, agentsv1alpha1.ScheduleReasonProbedSchedule, newStatus.Schedules[0].Reason)

	// tryPause does not consider resumeTime; when agent is active (pauseTime is nil),
	// no requeue is needed.
	assert.Equal(t, time.Duration(0), requeueAfter)
}

func TestHandleAutoPause_PendingPhase_NoAction(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	box := makeProbeSandbox("pending-sandbox", agentsv1alpha1.SandboxPending)
	r, _ := newAutoPauseReconciler(t, scheme, box)

	newStatus := box.Status.DeepCopy()
	requeueAfter, err := r.handleAutoPause(context.Background(), box, newStatus)
	assert.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeueAfter)
}

// ------------------------------------------------------------------
// patchSandboxPaused tests
// ------------------------------------------------------------------

func TestPatchSandboxPaused(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)

	t.Run("patch from false to true", func(t *testing.T) {
		box := makeProbeSandbox("patch-test", agentsv1alpha1.SandboxRunning)
		r, fakeClient := newAutoPauseReconciler(t, scheme, box)

		err := r.patchSandboxPaused(context.Background(), box, true)
		assert.NoError(t, err)
		assert.True(t, box.Spec.Paused)

		updated := &agentsv1alpha1.Sandbox{}
		require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKeyFromObject(box), updated))
		assert.True(t, updated.Spec.Paused)
	})

	t.Run("patch from true to false", func(t *testing.T) {
		box := makeProbeSandbox("patch-test-2", agentsv1alpha1.SandboxPaused, func(b *agentsv1alpha1.Sandbox) {
			b.Spec.Paused = true
		})
		r, fakeClient := newAutoPauseReconciler(t, scheme, box)

		err := r.patchSandboxPaused(context.Background(), box, false)
		assert.NoError(t, err)
		assert.False(t, box.Spec.Paused)

		updated := &agentsv1alpha1.Sandbox{}
		require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKeyFromObject(box), updated))
		assert.False(t, updated.Spec.Paused)
	})

	t.Run("no-op when already paused", func(t *testing.T) {
		box := makeProbeSandbox("patch-noop", agentsv1alpha1.SandboxPaused, func(b *agentsv1alpha1.Sandbox) {
			b.Spec.Paused = true
		})
		r, _ := newAutoPauseReconciler(t, scheme, box)

		err := r.patchSandboxPaused(context.Background(), box, true)
		assert.NoError(t, err)
	})
}

// ------------------------------------------------------------------
// hasProbeConditionChanged tests (pod_event_handler.go)
// ------------------------------------------------------------------

func TestHasProbeConditionChanged(t *testing.T) {
	probeCondType := corev1.PodConditionType(agentsv1alpha1.ProbeConditionPrefix + "activity")

	tests := []struct {
		name      string
		oldStatus corev1.PodStatus
		newStatus corev1.PodStatus
		want      bool
	}{
		{
			name:      "probe condition added",
			oldStatus: corev1.PodStatus{},
			newStatus: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{Type: probeCondType, Status: corev1.ConditionTrue},
				},
			},
			want: true,
		},
		{
			name: "probe condition status changed",
			oldStatus: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{Type: probeCondType, Status: corev1.ConditionTrue},
				},
			},
			newStatus: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{Type: probeCondType, Status: corev1.ConditionFalse},
				},
			},
			want: true,
		},
		{
			name: "probe condition reason changed",
			oldStatus: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{Type: probeCondType, Status: corev1.ConditionTrue, Reason: "Succeeded"},
				},
			},
			newStatus: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{Type: probeCondType, Status: corev1.ConditionTrue, Reason: "Timeout"},
				},
			},
			want: true,
		},
		{
			name: "probe condition message changed",
			oldStatus: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{Type: probeCondType, Status: corev1.ConditionTrue, Message: "active"},
				},
			},
			newStatus: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{Type: probeCondType, Status: corev1.ConditionTrue, Message: "inactive"},
				},
			},
			want: true,
		},
		{
			name: "probe condition unchanged",
			oldStatus: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{Type: probeCondType, Status: corev1.ConditionTrue, Reason: "Succeeded", Message: "inactive"},
				},
			},
			newStatus: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{Type: probeCondType, Status: corev1.ConditionTrue, Reason: "Succeeded", Message: "inactive"},
				},
			},
			want: false,
		},
		{
			name: "non-probe condition changed - should not trigger",
			oldStatus: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionFalse},
				},
			},
			newStatus: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				},
			},
			want: false,
		},
		{
			name: "probe condition removed (not detected - function only checks new conditions)",
			oldStatus: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{Type: probeCondType, Status: corev1.ConditionTrue},
				},
			},
			newStatus: corev1.PodStatus{},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hasProbeConditionChanged(&tt.oldStatus, &tt.newStatus))
		})
	}
}

// ------------------------------------------------------------------
// tryPause / tryResume edge-case tests
// ------------------------------------------------------------------

// TestHandleAutoPause_RunningAlreadyPaused_NoEvent covers the branch in
// tryPause where Spec.Paused is already true when the pause time is reached.
// patchSandboxPaused short-circuits and no AutoPaused event is emitted.
func TestHandleAutoPause_RunningAlreadyPaused_NoEvent(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	condType := agentsv1alpha1.ProbeConditionPrefix + "activity"
	lastTransition := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	box := makeProbeSandbox("already-paused", agentsv1alpha1.SandboxRunning, func(b *agentsv1alpha1.Sandbox) {
		b.Spec.Paused = true // already paused externally
		b.Spec.AutoPausePolicy.Pause.WhenProbedIdleState.ThresholdDuration = &metav1.Duration{Duration: 1 * time.Minute}
	})
	r, _ := newAutoPauseReconciler(t, scheme, box)

	newStatus := box.Status.DeepCopy()
	utils.SetSandboxCondition(newStatus, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionTrue,
		Reason:             agentsv1alpha1.ProbeReasonSucceeded,
		Message:            "inactive",
		LastTransitionTime: lastTransition,
	})
	requeueAfter, err := r.handleAutoPause(context.Background(), box, newStatus)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeueAfter)

	// Schedule should still be cleared even though patch was a no-op.
	require.Len(t, newStatus.Schedules, 1)
	assert.Nil(t, newStatus.Schedules[0].NextPauseTime)
}

// TestHandleAutoPause_PausedResumeTimeReached_NotPausedCondition covers the
// branch in tryResume where the resume time has been reached but the
// SandboxConditionPaused condition is not True (or missing). The resume
// should be skipped.
func TestHandleAutoPause_PausedResumeTimeReached_NotPausedCondition(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	condType := agentsv1alpha1.ProbeConditionPrefix + "activity"
	pastTime := time.Now().Add(-1 * time.Minute)

	tests := []struct {
		name       string
		conditions []metav1.Condition
	}{
		{
			name:       "paused condition missing",
			conditions: nil,
		},
		{
			name: "paused condition false",
			conditions: []metav1.Condition{
				{
					Type:   string(agentsv1alpha1.SandboxConditionPaused),
					Status: metav1.ConditionFalse,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			box := makeProbeSandbox("resume-not-paused", agentsv1alpha1.SandboxPaused, func(b *agentsv1alpha1.Sandbox) {
				b.Spec.Paused = true
				b.Spec.AutoPausePolicy.Resume = &agentsv1alpha1.ResumePolicy{
					WhenProbedScheduleTime: &agentsv1alpha1.ProbedScheduleTimeRule{
						Probe:      "activity",
						TimeFormat: "unix",
						LeadTime:   &metav1.Duration{Duration: 5 * time.Minute},
					},
				}
				conds := append(tt.conditions, metav1.Condition{
					Type:               condType,
					Status:             metav1.ConditionTrue,
					Reason:             agentsv1alpha1.ProbeReasonSucceeded,
					Message:            strconv.FormatInt(pastTime.Unix(), 10),
					LastTransitionTime: metav1.Now(),
				})
				b.Status.Conditions = conds
			})
			r, _ := newAutoPauseReconciler(t, scheme, box)

			newStatus := box.Status.DeepCopy()
			requeueAfter, err := r.handleAutoPause(context.Background(), box, newStatus)
			require.NoError(t, err)
			assert.Equal(t, time.Duration(0), requeueAfter)
			// NextResumeTime should NOT be cleared because resume was skipped.
			require.Len(t, newStatus.Schedules, 1)
			assert.NotNil(t, newStatus.Schedules[0].NextResumeTime)
		})
	}
}

// TestHandleAutoPause_PausedResumeTimeReached_AlreadyResumed covers the branch
// in tryResume where the SandboxConditionPaused condition is True but
// Spec.Paused is already false. patchSandboxPaused short-circuits and no
// AutoResumed event is emitted.
func TestHandleAutoPause_PausedResumeTimeReached_AlreadyResumed(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	condType := agentsv1alpha1.ProbeConditionPrefix + "activity"
	pastTime := time.Now().Add(-1 * time.Minute)
	box := makeProbeSandbox("already-resumed", agentsv1alpha1.SandboxPaused, func(b *agentsv1alpha1.Sandbox) {
		b.Spec.Paused = false // already resumed externally but condition still True
		b.Spec.AutoPausePolicy.Resume = &agentsv1alpha1.ResumePolicy{
			WhenProbedScheduleTime: &agentsv1alpha1.ProbedScheduleTimeRule{
				Probe:      "activity",
				TimeFormat: "unix",
				LeadTime:   &metav1.Duration{Duration: 5 * time.Minute},
			},
		}
		b.Status.Conditions = []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionPaused),
					Status:             metav1.ConditionTrue,
					Reason:             "Paused",
					LastTransitionTime: metav1.Now(),
				},
				{
					Type:               condType,
					Status:             metav1.ConditionTrue,
					Reason:             agentsv1alpha1.ProbeReasonSucceeded,
					Message:            strconv.FormatInt(pastTime.Unix(), 10),
					LastTransitionTime: metav1.Now(),
				},
		}
	})
	r, _ := newAutoPauseReconciler(t, scheme, box)

	newStatus := box.Status.DeepCopy()
	requeueAfter, err := r.handleAutoPause(context.Background(), box, newStatus)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), requeueAfter)
	// Schedule should still be cleared even though patch was a no-op.
	require.Len(t, newStatus.Schedules, 1)
	assert.Nil(t, newStatus.Schedules[0].NextResumeTime)
}

// ------------------------------------------------------------------
// recordPauseSchedule / recordResumeSchedule / ensureSchedule / findSchedule
// ------------------------------------------------------------------

func TestRecordPauseSchedule(t *testing.T) {
	tests := []struct {
		name           string
		pauseTime      *metav1.Time
		existingScheds []agentsv1alpha1.Schedule
		wantSchedLen   int
		wantNextPause  bool
	}{
		{
			name:          "nil pauseTime with no existing schedule is no-op",
			pauseTime:     nil,
			wantSchedLen:  0,
			wantNextPause: false,
		},
		{
			name:      "nil pauseTime clears existing NextPauseTime",
			pauseTime: nil,
			existingScheds: []agentsv1alpha1.Schedule{
				{Reason: agentsv1alpha1.ScheduleReasonProbedIdle, NextPauseTime: &metav1.Time{}},
			},
			wantSchedLen:  1,
			wantNextPause: false,
		},
		{
			name:          "non-nil pauseTime creates new schedule entry",
			pauseTime:     &metav1.Time{Time: time.Now().Add(5 * time.Minute)},
			wantSchedLen:  1,
			wantNextPause: true,
		},
		{
			name:      "non-nil pauseTime updates existing schedule entry",
			pauseTime: &metav1.Time{Time: time.Now().Add(10 * time.Minute)},
			existingScheds: []agentsv1alpha1.Schedule{
				{Reason: agentsv1alpha1.ScheduleReasonProbedIdle},
			},
			wantSchedLen:  1,
			wantNextPause: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := &agentsv1alpha1.SandboxStatus{
				Schedules: tt.existingScheds,
			}
			recordPauseSchedule(tt.pauseTime, status)
			assert.Len(t, status.Schedules, tt.wantSchedLen)
			if tt.wantSchedLen > 0 {
				assert.Equal(t, agentsv1alpha1.ScheduleReasonProbedIdle, status.Schedules[0].Reason)
				if tt.wantNextPause {
					assert.NotNil(t, status.Schedules[0].NextPauseTime)
				} else {
					assert.Nil(t, status.Schedules[0].NextPauseTime)
				}
			}
		})
	}
}

func TestRecordResumeSchedule(t *testing.T) {
	tests := []struct {
		name            string
		resumeTime      *metav1.Time
		existingScheds  []agentsv1alpha1.Schedule
		wantSchedLen    int
		wantNextResume  bool
	}{
		{
			name:           "nil resumeTime with no existing schedule is no-op",
			resumeTime:     nil,
			wantSchedLen:   0,
			wantNextResume: false,
		},
		{
			name:       "nil resumeTime clears existing NextResumeTime",
			resumeTime: nil,
			existingScheds: []agentsv1alpha1.Schedule{
				{Reason: agentsv1alpha1.ScheduleReasonProbedSchedule, NextResumeTime: &metav1.Time{}},
			},
			wantSchedLen:   1,
			wantNextResume: false,
		},
		{
			name:           "non-nil resumeTime creates new schedule entry",
			resumeTime:     &metav1.Time{Time: time.Now().Add(5 * time.Minute)},
			wantSchedLen:   1,
			wantNextResume: true,
		},
		{
			name:       "non-nil resumeTime updates existing schedule entry",
			resumeTime: &metav1.Time{Time: time.Now().Add(10 * time.Minute)},
			existingScheds: []agentsv1alpha1.Schedule{
				{Reason: agentsv1alpha1.ScheduleReasonProbedSchedule},
			},
			wantSchedLen:   1,
			wantNextResume: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := &agentsv1alpha1.SandboxStatus{
				Schedules: tt.existingScheds,
			}
			recordResumeSchedule(tt.resumeTime, status)
			assert.Len(t, status.Schedules, tt.wantSchedLen)
			if tt.wantSchedLen > 0 {
				assert.Equal(t, agentsv1alpha1.ScheduleReasonProbedSchedule, status.Schedules[0].Reason)
				if tt.wantNextResume {
					assert.NotNil(t, status.Schedules[0].NextResumeTime)
				} else {
					assert.Nil(t, status.Schedules[0].NextResumeTime)
				}
			}
		})
	}
}

func TestEnsureSchedule(t *testing.T) {
	t.Run("create new when no schedules exist", func(t *testing.T) {
		status := &agentsv1alpha1.SandboxStatus{}
		sched := ensureSchedule(status, agentsv1alpha1.ScheduleReasonProbedIdle)
		require.NotNil(t, sched)
		assert.Equal(t, agentsv1alpha1.ScheduleReasonProbedIdle, sched.Reason)
		assert.Len(t, status.Schedules, 1)
	})

	t.Run("find existing schedule", func(t *testing.T) {
		status := &agentsv1alpha1.SandboxStatus{
			Schedules: []agentsv1alpha1.Schedule{
				{Reason: agentsv1alpha1.ScheduleReasonProbedIdle},
				{Reason: agentsv1alpha1.ScheduleReasonProbedSchedule},
			},
		}
		sched := ensureSchedule(status, agentsv1alpha1.ScheduleReasonProbedSchedule)
		require.NotNil(t, sched)
		assert.Equal(t, agentsv1alpha1.ScheduleReasonProbedSchedule, sched.Reason)
		assert.Len(t, status.Schedules, 2) // no new entry created
	})
}

func TestFindSchedule(t *testing.T) {
	t.Run("find existing", func(t *testing.T) {
		status := &agentsv1alpha1.SandboxStatus{
			Schedules: []agentsv1alpha1.Schedule{
				{Reason: agentsv1alpha1.ScheduleReasonProbedIdle},
			},
		}
		sched := findSchedule(status, agentsv1alpha1.ScheduleReasonProbedIdle)
		require.NotNil(t, sched)
		assert.Equal(t, agentsv1alpha1.ScheduleReasonProbedIdle, sched.Reason)
	})

	t.Run("return nil when not found", func(t *testing.T) {
		status := &agentsv1alpha1.SandboxStatus{
			Schedules: []agentsv1alpha1.Schedule{
				{Reason: agentsv1alpha1.ScheduleReasonProbedIdle},
			},
		}
		sched := findSchedule(status, agentsv1alpha1.ScheduleReasonProbedSchedule)
		assert.Nil(t, sched)
	})

	t.Run("return nil when empty", func(t *testing.T) {
		status := &agentsv1alpha1.SandboxStatus{}
		sched := findSchedule(status, agentsv1alpha1.ScheduleReasonProbedIdle)
		assert.Nil(t, sched)
	})
}

// ------------------------------------------------------------------
// patchSandboxPaused error case
// ------------------------------------------------------------------

func TestPatchSandboxPaused_PatchError(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	box := makeProbeSandbox("patch-error", agentsv1alpha1.SandboxRunning) // Spec.Paused = false
	// Create a reconciler with an empty client — the sandbox is not in the
	// store, so the Patch call will return a NotFound error.
	r, _ := newAutoPauseReconciler(t, scheme)

	err := r.patchSandboxPaused(context.Background(), box, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to patch sandbox paused=true")
}

// ------------------------------------------------------------------
// tryPause / tryResume patch error cases
// ------------------------------------------------------------------

// newAutoPauseReconcilerWithPatchFail builds a reconciler whose fake client
// intercepts every Patch call and returns an error. This lets us exercise the
// error-return paths inside tryPause and tryResume.
func newAutoPauseReconcilerWithPatchFail(t *testing.T, scheme *runtime.Scheme, objs ...client.Object) *SandboxReconciler {
	t.Helper()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&agentsv1alpha1.Sandbox{}).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return fmt.Errorf("simulated patch error")
			},
		}).
		Build()
	fakeRecorder := record.NewFakeRecorder(100)
	return &SandboxReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		recorder: fakeRecorder,
	}
}

func TestTryPause_PatchError(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	box := makeProbeSandbox("try-pause-err", agentsv1alpha1.SandboxRunning)
	// The sandbox must exist in the client so that MergePatch/Get works, but the
	// interceptor will make the Patch call fail.
	r := newAutoPauseReconcilerWithPatchFail(t, scheme, box)

	now := metav1.Now()
	pastTime := metav1.NewTime(now.Add(-1 * time.Hour))
	newStatus := &agentsv1alpha1.SandboxStatus{}

	requeue, err := r.tryPause(context.Background(), box, newStatus, now, &pastTime)
	require.Error(t, err)
	assert.Equal(t, time.Duration(0), requeue)
}

func TestTryResume_PatchError(t *testing.T) {
	scheme := newAutoPauseTestScheme(t)
	box := makeProbeSandbox("try-resume-err", agentsv1alpha1.SandboxPaused)
	box.Spec.Paused = true
	r := newAutoPauseReconcilerWithPatchFail(t, scheme, box)

	now := metav1.Now()
	pastTime := metav1.NewTime(now.Add(-1 * time.Hour))
	newStatus := &agentsv1alpha1.SandboxStatus{
		Conditions: []metav1.Condition{
			{
				Type:   string(agentsv1alpha1.SandboxConditionPaused),
				Status: metav1.ConditionTrue,
			},
		},
	}

	requeue, err := r.tryResume(context.Background(), box, newStatus, now, &pastTime)
	require.Error(t, err)
	assert.Equal(t, time.Duration(0), requeue)
}
