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

package poolautoscaler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/features"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func newTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	return scheme
}

func newTestReconciler(objs ...client.Object) *Reconciler {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&agentsv1alpha1.PoolAutoscaler{}).
		WithIndex(&agentsv1alpha1.PoolAutoscaler{}, scaleTargetRefNameIndex, func(obj client.Object) []string {
			pa := obj.(*agentsv1alpha1.PoolAutoscaler)
			if pa.Spec.ScaleTargetRef.Name == "" {
				return nil
			}
			return []string{pa.Spec.ScaleTargetRef.Name}
		}).
		Build()
	return &Reconciler{
		Client:   fc,
		recorder: record.NewFakeRecorder(100),
		monitors: make(map[types.NamespacedName]*capacityMonitor),
	}
}

func boolPtr(v bool) *bool {
	return &v
}

func intOrStrPtr(v intstr.IntOrString) *intstr.IntOrString {
	return &v
}

func newPoolAutoscaler(name, namespace, sbsName string, maxReplicas int32, capacityPolicy *agentsv1alpha1.CapacityPolicy) *agentsv1alpha1.PoolAutoscaler {
	return &agentsv1alpha1.PoolAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: agentsv1alpha1.PoolAutoscalerSpec{
			ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
				Kind: "SandboxSet",
				Name: sbsName,
			},
			MaxReplicas:    maxReplicas,
			CapacityPolicy: capacityPolicy,
		},
	}
}

func newSandboxSet(name, namespace string, specReplicas, statusReplicas, availableReplicas int32) *agentsv1alpha1.SandboxSet {
	return &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: agentsv1alpha1.SandboxSetSpec{
			Replicas: specReplicas,
		},
		Status: agentsv1alpha1.SandboxSetStatus{
			Replicas:          statusReplicas,
			AvailableReplicas: availableReplicas,
		},
	}
}

// ---------------------------------------------------------------------------
// Reconcile tests
// ---------------------------------------------------------------------------

func TestReconcile(t *testing.T) {
	tests := []struct {
		name              string
		objs              []client.Object
		setupMonitors     func(r *Reconciler)
		req               ctrl.Request
		expectError       string
		expectSBSReplicas *int32
		expectDesired     *int32
		expectSuspended   *bool
	}{
		{
			name:        "PoolAutoscaler not found - returns nil",
			objs:        nil,
			req:         ctrl.Request{NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"}},
			expectError: "",
		},
		{
			name: "PoolAutoscaler suspended - updates status with suspended=true",
			objs: []client.Object{
				&agentsv1alpha1.PoolAutoscaler{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pa",
						Namespace: "default",
					},
					Spec: agentsv1alpha1.PoolAutoscalerSpec{
						ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
							Kind: "SandboxSet",
							Name: "test-sbs",
						},
						MaxReplicas: 20,
						Suspend:     boolPtr(true),
					},
				},
			},
			req:             ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-pa", Namespace: "default"}},
			expectError:     "",
			expectSuspended: boolPtr(true),
		},
		{
			name: "SandboxSet not found - returns error",
			objs: []client.Object{
				newPoolAutoscaler("test-pa", "default", "nonexistent-sbs", 20,
					&agentsv1alpha1.CapacityPolicy{
						TargetAvailable: intstr.FromInt32(10),
						Tolerance:       intOrStrPtr(intstr.FromInt32(2)),
					}),
			},
			req:         ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-pa", Namespace: "default"}},
			expectError: "not found",
		},
		{
			name: "scale up - available below lower watermark",
			objs: []client.Object{
				newPoolAutoscaler("test-pa", "default", "test-sbs", 20,
					&agentsv1alpha1.CapacityPolicy{
						TargetAvailable: intstr.FromInt32(10),
						Tolerance:       intOrStrPtr(intstr.FromInt32(2)),
					}),
				newSandboxSet("test-sbs", "default", 5, 5, 5),
			},
			req:               ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-pa", Namespace: "default"}},
			expectError:       "",
			expectSBSReplicas: int32Ptr(10),
			expectDesired:     int32Ptr(10),
		},
		{
			name: "scale down - available above upper watermark",
			objs: []client.Object{
				newPoolAutoscaler("test-pa", "default", "test-sbs", 20,
					&agentsv1alpha1.CapacityPolicy{
						TargetAvailable: intstr.FromInt32(7),
						Tolerance:       intOrStrPtr(intstr.FromInt32(1)),
					}),
				newSandboxSet("test-sbs", "default", 10, 10, 9),
			},
			// Scale-down requires a fully populated observation window; pre-fill
			// the monitor with a full window of samples matching the fixture.
			setupMonitors: func(r *Reconciler) {
				now := time.Now()
				interval := time.Duration(samplingIntervalSeconds) * time.Second
				monitor := &capacityMonitor{targetRef: "test-sbs", lastSampleAt: now}
				for i := observationWindowSeconds / samplingIntervalSeconds; i > 0; i-- {
					monitor.samples = append(monitor.samples, sample{
						timestamp:      now.Add(-time.Duration(i-1) * interval),
						available:      9,
						statusReplicas: 10,
					})
				}
				r.monitors[types.NamespacedName{Namespace: "default", Name: "test-pa"}] = monitor
			},
			req:               ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-pa", Namespace: "default"}},
			expectError:       "",
			expectSBSReplicas: int32Ptr(8),
			expectDesired:     int32Ptr(8),
		},
		{
			name: "no scaling needed - available within tolerance",
			objs: []client.Object{
				newPoolAutoscaler("test-pa", "default", "test-sbs", 20,
					&agentsv1alpha1.CapacityPolicy{
						TargetAvailable: intstr.FromInt32(10),
						Tolerance:       intOrStrPtr(intstr.FromInt32(5)),
					}),
				newSandboxSet("test-sbs", "default", 10, 10, 10),
			},
			req:               ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-pa", Namespace: "default"}},
			expectError:       "",
			expectSBSReplicas: int32Ptr(10),
			expectDesired:     int32Ptr(10),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestReconciler(tt.objs...)
			if tt.setupMonitors != nil {
				tt.setupMonitors(r)
			}
			result, err := r.Reconcile(context.Background(), tt.req)

			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}

			// Check SBS replicas if expected
			if tt.expectSBSReplicas != nil {
				sbs := &agentsv1alpha1.SandboxSet{}
				err := r.Get(context.Background(), types.NamespacedName{Name: "test-sbs", Namespace: "default"}, sbs)
				require.NoError(t, err)
				assert.Equal(t, *tt.expectSBSReplicas, sbs.Spec.Replicas, "SandboxSet spec.replicas mismatch")
			}

			// Check PA status if expected
			if tt.expectDesired != nil || tt.expectSuspended != nil {
				pa := &agentsv1alpha1.PoolAutoscaler{}
				err := r.Get(context.Background(), tt.req.NamespacedName, pa)
				require.NoError(t, err)
				if tt.expectDesired != nil {
					assert.Equal(t, *tt.expectDesired, pa.Status.DesiredReplicas, "DesiredReplicas mismatch")
				}
				if tt.expectSuspended != nil {
					assert.Equal(t, *tt.expectSuspended, pa.Status.Suspended, "Suspended mismatch")
				}
			}

			// For normal reconcile cases with a SandboxSet present, verify requeue
			if tt.expectError == "" && tt.expectSBSReplicas != nil {
				assert.True(t, result.RequeueAfter > 0, "expected requeue for normal reconcile")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Conflict detection tests
// ---------------------------------------------------------------------------

func TestReconcile_ConflictingAutoscaler(t *testing.T) {
	newPAWithCreation := func(name string, created time.Time) *agentsv1alpha1.PoolAutoscaler {
		pa := newPoolAutoscaler(name, "default", "test-sbs", 20,
			&agentsv1alpha1.CapacityPolicy{
				TargetAvailable: intstr.FromInt32(10),
				Tolerance:       intOrStrPtr(intstr.FromInt32(5)),
			})
		pa.CreationTimestamp = metav1.NewTime(created)
		return pa
	}
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name              string
		objs              []client.Object
		reconcileName     string
		expectConflict    bool
		expectWinnerInMsg string
		expectSBSReplicas int32
	}{
		{
			name: "newer PA loses to older PA",
			objs: []client.Object{
				newPAWithCreation("old-pa", base),
				newPAWithCreation("new-pa", base.Add(time.Hour)),
				newSandboxSet("test-sbs", "default", 10, 10, 10),
			},
			reconcileName:     "new-pa",
			expectConflict:    true,
			expectWinnerInMsg: "old-pa",
			expectSBSReplicas: 10,
		},
		{
			name: "older PA wins and keeps reconciling",
			objs: []client.Object{
				newPAWithCreation("old-pa", base),
				newPAWithCreation("new-pa", base.Add(time.Hour)),
				newSandboxSet("test-sbs", "default", 10, 10, 10),
			},
			reconcileName:     "old-pa",
			expectConflict:    false,
			expectSBSReplicas: 10,
		},
		{
			name: "creation timestamp tie broken by name",
			objs: []client.Object{
				newPAWithCreation("a-pa", base),
				newPAWithCreation("b-pa", base),
				newSandboxSet("test-sbs", "default", 10, 10, 10),
			},
			reconcileName:     "b-pa",
			expectConflict:    true,
			expectWinnerInMsg: "a-pa",
			expectSBSReplicas: 10,
		},
		{
			name: "single PA has no conflict",
			objs: []client.Object{
				newPAWithCreation("solo-pa", base),
				newSandboxSet("test-sbs", "default", 10, 10, 10),
			},
			reconcileName:     "solo-pa",
			expectConflict:    false,
			expectSBSReplicas: 10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestReconciler(tt.objs...)
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: tt.reconcileName, Namespace: "default"}}
			_, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err)

			pa := &agentsv1alpha1.PoolAutoscaler{}
			require.NoError(t, r.Get(context.Background(), req.NamespacedName, pa))

			var scalingActive *metav1.Condition
			for i := range pa.Status.Conditions {
				if pa.Status.Conditions[i].Type == string(agentsv1alpha1.ScalingActive) {
					scalingActive = &pa.Status.Conditions[i]
				}
			}

			if tt.expectConflict {
				require.NotNil(t, scalingActive, "conflict condition must be persisted to status")
				assert.Equal(t, metav1.ConditionFalse, scalingActive.Status)
				assert.Equal(t, "ConflictingAutoscaler", scalingActive.Reason)
				assert.Contains(t, scalingActive.Message, tt.expectWinnerInMsg)
			} else {
				require.NotNil(t, scalingActive)
				assert.NotEqual(t, "ConflictingAutoscaler", scalingActive.Reason)
			}

			// The losing PA must not have touched the SandboxSet.
			sbs := &agentsv1alpha1.SandboxSet{}
			require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "test-sbs", Namespace: "default"}, sbs))
			assert.Equal(t, tt.expectSBSReplicas, sbs.Spec.Replicas)
		})
	}
}

// ---------------------------------------------------------------------------
// doScale tests
// ---------------------------------------------------------------------------

func TestDoScale(t *testing.T) {
	t.Run("patches SandboxSet spec replicas", func(t *testing.T) {
		sbs := newSandboxSet("test-sbs", "default", 10, 10, 10)
		r := newTestReconciler(sbs)

		err := r.doScale(context.Background(), sbs, 15)
		require.NoError(t, err)

		got := &agentsv1alpha1.SandboxSet{}
		err = r.Get(context.Background(), types.NamespacedName{Name: "test-sbs", Namespace: "default"}, got)
		require.NoError(t, err)
		assert.Equal(t, int32(15), got.Spec.Replicas)
	})

	t.Run("patches to lower value", func(t *testing.T) {
		sbs := newSandboxSet("test-sbs", "default", 10, 10, 10)
		r := newTestReconciler(sbs)

		err := r.doScale(context.Background(), sbs, 5)
		require.NoError(t, err)

		got := &agentsv1alpha1.SandboxSet{}
		err = r.Get(context.Background(), types.NamespacedName{Name: "test-sbs", Namespace: "default"}, got)
		require.NoError(t, err)
		assert.Equal(t, int32(5), got.Spec.Replicas)
	})
}

// ---------------------------------------------------------------------------
// updateStatus tests
// ---------------------------------------------------------------------------

func TestUpdateStatus(t *testing.T) {
	tests := []struct {
		name                string
		currentReplicas     int32
		desiredReplicas     int32
		available           int32
		suspended           bool
		scaled              bool
		expectLastScaleTime bool
	}{
		{
			name:                "scaled is true - sets LastScaleTime",
			currentReplicas:     10,
			desiredReplicas:     15,
			available:           5,
			suspended:           false,
			scaled:              true,
			expectLastScaleTime: true,
		},
		{
			name:                "scaled is false - no LastScaleTime",
			currentReplicas:     10,
			desiredReplicas:     10,
			available:           10,
			suspended:           false,
			scaled:              false,
			expectLastScaleTime: false,
		},
		{
			name:                "suspended status",
			currentReplicas:     5,
			desiredReplicas:     5,
			available:           3,
			suspended:           true,
			scaled:              false,
			expectLastScaleTime: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pa := newPoolAutoscaler("test-pa", "default", "test-sbs", 20, nil)
			r := newTestReconciler(pa)

			err := r.updateStatus(context.Background(), pa, tt.currentReplicas, tt.desiredReplicas, tt.available, nil, tt.suspended, tt.scaled, false, "")
			require.NoError(t, err)

			got := &agentsv1alpha1.PoolAutoscaler{}
			err = r.Get(context.Background(), types.NamespacedName{Name: "test-pa", Namespace: "default"}, got)
			require.NoError(t, err)
			assert.Equal(t, tt.currentReplicas, got.Status.CurrentReplicas)
			assert.Equal(t, tt.desiredReplicas, got.Status.DesiredReplicas)
			assert.Equal(t, tt.available, got.Status.CurrentCapacity.Available)
			assert.Equal(t, tt.suspended, got.Status.Suspended)

			if tt.expectLastScaleTime {
				assert.NotNil(t, got.Status.LastScaleTime)
			} else {
				assert.Nil(t, got.Status.LastScaleTime)
			}

			// Should have conditions set
			assert.NotEmpty(t, got.Status.Conditions)
		})
	}
}

// ---------------------------------------------------------------------------
// setConditions tests
// ---------------------------------------------------------------------------

func TestSetConditions(t *testing.T) {
	tests := []struct {
		name                string
		desiredReplicas     int32
		limited             bool
		limitReason         string
		expectLimitedStatus metav1.ConditionStatus
		expectLimitedReason string
	}{
		{
			name:                "desired within range",
			desiredReplicas:     10,
			limited:             false,
			expectLimitedStatus: metav1.ConditionFalse,
			expectLimitedReason: "DesiredWithinRange",
		},
		{
			name:                "clamped to max",
			desiredReplicas:     20,
			limited:             true,
			limitReason:         "TooManyReplicas",
			expectLimitedStatus: metav1.ConditionTrue,
			expectLimitedReason: "TooManyReplicas",
		},
		{
			name:                "clamped to min",
			desiredReplicas:     5,
			limited:             true,
			limitReason:         "TooFewReplicas",
			expectLimitedStatus: metav1.ConditionTrue,
			expectLimitedReason: "TooFewReplicas",
		},
		{
			name:                "desired equals bound but not limited",
			desiredReplicas:     0,
			limited:             false,
			expectLimitedStatus: metav1.ConditionFalse,
			expectLimitedReason: "DesiredWithinRange",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reconciler{}
			pa := &agentsv1alpha1.PoolAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Generation: 3},
				Spec: agentsv1alpha1.PoolAutoscalerSpec{
					MinReplicas: 5,
					MaxReplicas: 20,
				},
			}
			r.setConditions(pa, tt.desiredReplicas, tt.limited, tt.limitReason)

			// Should have 3 conditions: ScalingActive, AbleToScale, and ScalingLimited
			assert.Len(t, pa.Status.Conditions, 3)

			var scalingActive, scalingLimited *metav1.Condition
			for i := range pa.Status.Conditions {
				c := &pa.Status.Conditions[i]
				switch c.Type {
				case string(agentsv1alpha1.ScalingActive):
					scalingActive = c
				case string(agentsv1alpha1.ScalingLimited):
					scalingLimited = c
				}
			}

			require.NotNil(t, scalingActive)
			assert.Equal(t, metav1.ConditionTrue, scalingActive.Status)
			assert.Equal(t, "ValidPolicy", scalingActive.Reason)

			require.NotNil(t, scalingLimited)
			assert.Equal(t, tt.expectLimitedStatus, scalingLimited.Status)
			assert.Equal(t, tt.expectLimitedReason, scalingLimited.Reason)

			// All conditions must carry the observed generation
			for _, c := range pa.Status.Conditions {
				assert.Equal(t, pa.Generation, c.ObservedGeneration, "condition %s observedGeneration", c.Type)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// setCondition tests
// ---------------------------------------------------------------------------

func TestSetCondition(t *testing.T) {
	t.Run("new condition is appended", func(t *testing.T) {
		pa := &agentsv1alpha1.PoolAutoscaler{}
		cond := metav1.Condition{
			Type:               "ScalingActive",
			Status:             metav1.ConditionTrue,
			Reason:             "ValidPolicy",
			Message:            "the autoscaler is able to scale",
			LastTransitionTime: metav1.Now(),
		}
		setCondition(pa, cond)
		assert.Len(t, pa.Status.Conditions, 1)
		assert.Equal(t, "ScalingActive", pa.Status.Conditions[0].Type)
		assert.Equal(t, metav1.ConditionTrue, pa.Status.Conditions[0].Status)
		assert.Equal(t, "ValidPolicy", pa.Status.Conditions[0].Reason)
	})

	t.Run("existing condition with status change - replaces with new LastTransitionTime", func(t *testing.T) {
		oldTime := metav1.NewTime(time.Now().Add(-1 * time.Hour))
		pa := &agentsv1alpha1.PoolAutoscaler{
			Status: agentsv1alpha1.PoolAutoscalerStatus{
				Conditions: []metav1.Condition{
					{
						Type:               "ScalingLimited",
						Status:             metav1.ConditionFalse,
						Reason:             "DesiredWithinRange",
						Message:            "old message",
						LastTransitionTime: oldTime,
					},
				},
			},
		}
		newCond := metav1.Condition{
			Type:               "ScalingLimited",
			Status:             metav1.ConditionTrue,
			Reason:             "TooManyReplicas",
			Message:            "new message",
			LastTransitionTime: metav1.Now(),
		}
		setCondition(pa, newCond)

		assert.Len(t, pa.Status.Conditions, 1)
		assert.Equal(t, metav1.ConditionTrue, pa.Status.Conditions[0].Status)
		assert.Equal(t, "TooManyReplicas", pa.Status.Conditions[0].Reason)
		assert.Equal(t, "new message", pa.Status.Conditions[0].Message)
		// LastTransitionTime should be updated to the new time
		assert.True(t, pa.Status.Conditions[0].LastTransitionTime.After(oldTime.Time))
	})

	t.Run("existing condition same status different reason - no LastTransitionTime change", func(t *testing.T) {
		oldTime := metav1.NewTime(time.Now().Add(-1 * time.Hour))
		pa := &agentsv1alpha1.PoolAutoscaler{
			Status: agentsv1alpha1.PoolAutoscalerStatus{
				Conditions: []metav1.Condition{
					{
						Type:               "ScalingLimited",
						Status:             metav1.ConditionTrue,
						Reason:             "TooManyReplicas",
						Message:            "old message",
						LastTransitionTime: oldTime,
					},
				},
			},
		}
		newCond := metav1.Condition{
			Type:               "ScalingLimited",
			Status:             metav1.ConditionTrue, // same status
			Reason:             "TooFewReplicas",     // different reason
			Message:            "new message",
			LastTransitionTime: metav1.Now(), // should NOT be applied
		}
		setCondition(pa, newCond)

		assert.Len(t, pa.Status.Conditions, 1)
		assert.Equal(t, "TooFewReplicas", pa.Status.Conditions[0].Reason)
		assert.Equal(t, "new message", pa.Status.Conditions[0].Message)
		// LastTransitionTime should remain unchanged
		assert.Equal(t, oldTime, pa.Status.Conditions[0].LastTransitionTime)
	})

	t.Run("existing condition same status reason and message - no change", func(t *testing.T) {
		oldTime := metav1.NewTime(time.Now().Add(-1 * time.Hour))
		pa := &agentsv1alpha1.PoolAutoscaler{
			Status: agentsv1alpha1.PoolAutoscalerStatus{
				Conditions: []metav1.Condition{
					{
						Type:               "ScalingLimited",
						Status:             metav1.ConditionTrue,
						Reason:             "TooManyReplicas",
						Message:            "same message",
						LastTransitionTime: oldTime,
					},
				},
			},
		}
		newCond := metav1.Condition{
			Type:               "ScalingLimited",
			Status:             metav1.ConditionTrue,
			Reason:             "TooManyReplicas",
			Message:            "same message",
			LastTransitionTime: metav1.Now(),
		}
		setCondition(pa, newCond)

		assert.Len(t, pa.Status.Conditions, 1)
		// Everything should remain unchanged
		assert.Equal(t, oldTime, pa.Status.Conditions[0].LastTransitionTime)
	})
}

// ---------------------------------------------------------------------------
// getSandboxSet tests
// ---------------------------------------------------------------------------

func TestGetSandboxSet(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		sbs := newSandboxSet("test-sbs", "default", 10, 10, 10)
		r := newTestReconciler(sbs)
		pa := &agentsv1alpha1.PoolAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pa", Namespace: "default"},
			Spec: agentsv1alpha1.PoolAutoscalerSpec{
				ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
					Kind: "SandboxSet",
					Name: "test-sbs",
				},
			},
		}
		got, err := r.getSandboxSet(context.Background(), pa)
		require.NoError(t, err)
		assert.Equal(t, "test-sbs", got.Name)
		assert.Equal(t, int32(10), got.Spec.Replicas)
	})

	t.Run("not found", func(t *testing.T) {
		r := newTestReconciler()
		pa := &agentsv1alpha1.PoolAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pa", Namespace: "default"},
			Spec: agentsv1alpha1.PoolAutoscalerSpec{
				ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
					Kind: "SandboxSet",
					Name: "nonexistent",
				},
			},
		}
		_, err := r.getSandboxSet(context.Background(), pa)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

// ---------------------------------------------------------------------------
// sandboxSetToPoolAutoscaler tests
// ---------------------------------------------------------------------------

func TestSandboxSetToPoolAutoscaler(t *testing.T) {
	tests := []struct {
		name           string
		paObjs         []client.Object
		sbsName        string
		sbsNamespace   string
		expectRequests int
		expectPAName   string
	}{
		{
			name: "matching PA found - returns request",
			paObjs: []client.Object{
				newPoolAutoscaler("test-pa", "default", "test-sbs", 20, nil),
			},
			sbsName:        "test-sbs",
			sbsNamespace:   "default",
			expectRequests: 1,
			expectPAName:   "test-pa",
		},
		{
			name: "no matching PA - returns empty",
			paObjs: []client.Object{
				newPoolAutoscaler("other-pa", "default", "other-sbs", 20, nil),
			},
			sbsName:        "test-sbs",
			sbsNamespace:   "default",
			expectRequests: 0,
		},
		{
			name:           "empty list - returns empty",
			paObjs:         nil,
			sbsName:        "test-sbs",
			sbsNamespace:   "default",
			expectRequests: 0,
		},
		{
			name: "multiple PAs - only matching one returned",
			paObjs: []client.Object{
				newPoolAutoscaler("pa-1", "default", "sbs-1", 20, nil),
				newPoolAutoscaler("pa-2", "default", "sbs-2", 20, nil),
				newPoolAutoscaler("pa-3", "default", "sbs-2", 20, nil),
			},
			sbsName:        "sbs-2",
			sbsNamespace:   "default",
			expectRequests: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestReconciler(tt.paObjs...)
			sbs := &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tt.sbsName,
					Namespace: tt.sbsNamespace,
				},
			}
			requests := r.sandboxSetToPoolAutoscaler(context.Background(), sbs)
			assert.Len(t, requests, tt.expectRequests)
			if tt.expectPAName != "" {
				require.NotEmpty(t, requests)
				assert.Equal(t, tt.expectPAName, requests[0].Name)
				assert.Equal(t, tt.sbsNamespace, requests[0].Namespace)
			}
		})
	}

	t.Run("list error returns nil", func(t *testing.T) {
		// Use a scheme without agentsv1alpha1 to cause a List error
		bareScheme := runtime.NewScheme()
		_ = clientgoscheme.AddToScheme(bareScheme)
		fc := fake.NewClientBuilder().WithScheme(bareScheme).Build()
		r := &Reconciler{
			Client:   fc,
			recorder: record.NewFakeRecorder(100),
			monitors: make(map[types.NamespacedName]*capacityMonitor),
		}
		sbs := &agentsv1alpha1.SandboxSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-sbs", Namespace: "default"},
		}
		requests := r.sandboxSetToPoolAutoscaler(context.Background(), sbs)
		assert.Nil(t, requests)
	})
}

// ---------------------------------------------------------------------------
// Add tests
// ---------------------------------------------------------------------------

func TestAdd(t *testing.T) {
	t.Run("returns nil when feature gate is disabled", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PoolAutoscalerGate, false)
		err := Add(nil)
		assert.NoError(t, err)
	})

	t.Run("returns nil when discovery is not available", func(t *testing.T) {
		// No generic client set in tests, so discovery.DiscoverGVK returns false.
		// Feature gate is enabled by default.
		err := Add(nil)
		assert.NoError(t, err)
	})
}
