/*
Copyright 2025.
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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/utils"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
	"github.com/openkruise/agents/pkg/utils/sidecarutils"
)

func defaultCommonSyncStatusFromPod(pod *corev1.Pod, newStatus *agentsv1alpha1.SandboxStatus, syncReadyCondition bool) {
	defaultSyncStatusFromPod(pod, newStatus, syncReadyCondition, classifyStartupFailure)
}

func TestDefaultSyncStatusFromPodReadyTransitionTime(t *testing.T) {
	previousTime := metav1.NewTime(time.Unix(1700000000, 0))
	podTime := metav1.NewTime(time.Unix(1600000000, 0))
	tests := []struct {
		name              string
		previousStatus    metav1.ConditionStatus
		podStatus         corev1.ConditionStatus
		podTransitionTime metav1.Time
		normalizerFailure bool
		wantStatus        metav1.ConditionStatus
		wantMessage       string
		wantTime          metav1.Time // A zero value means the reconciliation time is expected.
	}{
		{
			name:           "zero timestamp on transition to ready",
			previousStatus: metav1.ConditionFalse, podStatus: corev1.ConditionTrue,
			wantStatus: metav1.ConditionTrue,
		},
		{
			name:           "zero timestamp on transition to not ready",
			previousStatus: metav1.ConditionTrue, podStatus: corev1.ConditionFalse,
			wantStatus: metav1.ConditionFalse, wantMessage: "pod not ready",
		},
		{
			name:           "nonzero timestamp on transition to ready is preserved",
			previousStatus: metav1.ConditionFalse, podStatus: corev1.ConditionTrue,
			podTransitionTime: podTime, wantStatus: metav1.ConditionTrue, wantTime: podTime,
		},
		{
			name:           "nonzero timestamp on transition to not ready is preserved",
			previousStatus: metav1.ConditionTrue, podStatus: corev1.ConditionFalse,
			podTransitionTime: podTime, wantStatus: metav1.ConditionFalse,
			wantMessage: "pod not ready", wantTime: podTime,
		},
		{
			name:      "new ready condition with zero pod timestamp",
			podStatus: corev1.ConditionTrue, wantStatus: metav1.ConditionTrue,
		},
		{
			name:      "new ready condition with nonzero pod timestamp",
			podStatus: corev1.ConditionTrue, podTransitionTime: podTime,
			wantStatus: metav1.ConditionTrue, wantTime: podTime,
		},
		{
			name:      "new not ready condition with zero pod timestamp",
			podStatus: corev1.ConditionFalse, wantStatus: metav1.ConditionFalse,
		},
		{
			name:           "unchanged ready status retains original timestamp",
			previousStatus: metav1.ConditionTrue, podStatus: corev1.ConditionTrue,
			wantStatus: metav1.ConditionTrue, wantMessage: "previous message", wantTime: previousTime,
		},
		{
			name:           "unchanged not ready status retains original timestamp",
			previousStatus: metav1.ConditionFalse, podStatus: corev1.ConditionFalse,
			wantStatus: metav1.ConditionFalse, wantMessage: "previous message", wantTime: previousTime,
		},
		{
			name:           "normalizer transition uses reconciliation time",
			previousStatus: metav1.ConditionTrue, podStatus: corev1.ConditionTrue,
			podTransitionTime: podTime, normalizerFailure: true,
			wantStatus: metav1.ConditionFalse, wantMessage: "startup failure",
		},
		{
			name:           "normalizer overrides transition to ready with zero timestamp",
			previousStatus: metav1.ConditionFalse, podStatus: corev1.ConditionTrue,
			normalizerFailure: true, wantStatus: metav1.ConditionFalse, wantMessage: "startup failure",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{
					Type: corev1.PodReady, Status: tt.podStatus,
					LastTransitionTime: tt.podTransitionTime, Message: "pod not ready",
				}},
			}}
			status := &agentsv1alpha1.SandboxStatus{}
			if tt.previousStatus != "" {
				status.Conditions = []metav1.Condition{{
					Type: string(agentsv1alpha1.SandboxConditionReady), Status: tt.previousStatus,
					LastTransitionTime: previousTime, Reason: agentsv1alpha1.SandboxReadyReasonPodReady,
					Message: "previous message",
				}}
			}
			var normalize func(*corev1.Pod) (string, string, bool)
			wantReason := agentsv1alpha1.SandboxReadyReasonPodReady
			if tt.normalizerFailure {
				wantReason = agentsv1alpha1.SandboxReadyReasonStartContainerFailed
				normalize = func(*corev1.Pod) (string, string, bool) {
					return agentsv1alpha1.SandboxReadyReasonStartContainerFailed, "startup failure", true
				}
			}

			before := time.Now()
			defaultSyncStatusFromPod(pod, status, true, normalize)
			after := time.Now()

			condition := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionReady))
			require.NotNil(t, condition)
			assert.Equal(t, tt.wantStatus, condition.Status)
			assert.Equal(t, wantReason, condition.Reason)
			assert.Equal(t, tt.wantMessage, condition.Message)
			if !tt.wantTime.IsZero() {
				assert.Equal(t, tt.wantTime, condition.LastTransitionTime)
			} else {
				require.False(t, condition.LastTransitionTime.IsZero(), "Ready transition timestamp must not be zero")
				assert.False(t, condition.LastTransitionTime.Time.Before(before), "fallback timestamp must be from this reconciliation")
				assert.False(t, condition.LastTransitionTime.Time.After(after), "fallback timestamp must not be in the future")
			}
		})
	}
}

func TestCommonControl_EnsureSandboxRunning(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name        string
		args        EnsureFuncArgs
		podExist    bool
		wantErr     bool
		wantRequeue time.Duration
		setupRL     func(rl *RateLimiter) // optional: pre-populate rate limiter
		featureGate bool
	}{
		{
			name: "pod does not exist, should create",
			args: EnsureFuncArgs{
				Pod: nil,
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Spec: agentsv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "test-container",
											Image: "nginx:latest",
										},
									},
								},
							},
						},
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			podExist: false,
			wantErr:  false,
		},
		{
			name: "pod exists but not running",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodPending,
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			podExist: true,
			wantErr:  false,
		},
		{
			name: "pod is running",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						Conditions: []corev1.PodCondition{
							{
								Type:               corev1.PodReady,
								Status:             corev1.ConditionTrue,
								LastTransitionTime: metav1.Now(),
							},
						},
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			podExist: true,
			wantErr:  false,
		},
		{
			name: "pod is running and owned by current sandbox",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: agentsv1alpha1.SchemeGroupVersion.String(),
								Kind:       "Sandbox",
								Name:       "test-sandbox",
								UID:        "current-uid",
							},
						},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
						UID:       "current-uid",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			podExist: true,
			wantErr:  false,
		},
		{
			name: "pod is running but owned by previous sandbox generation, should stay pending",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: agentsv1alpha1.SchemeGroupVersion.String(),
								Kind:       "Sandbox",
								Name:       "test-sandbox",
								UID:        "old-uid",
							},
						},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
						UID:       "current-uid",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPending,
				},
			},
			podExist: true,
			wantErr:  false,
		},
		{
			name:        "feature gate enabled, threshold exceeded, normal sandbox rate-limited",
			featureGate: true,
			setupRL: func(rl *RateLimiter) {
				// pre-fill track to exceed threshold
				oldThreshold := prioritySandboxThreshold
				prioritySandboxThreshold = 0
				t.Cleanup(func() { prioritySandboxThreshold = oldThreshold })
				rl.highPrioritySandboxTrack["ns/hp1"] = &SandboxTrack{Namespace: "ns", Name: "hp1"}
			},
			args: EnsureFuncArgs{
				Pod: nil,
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "normal-sandbox",
						Namespace:         "default",
						CreationTimestamp: metav1.Now(), // within maxCreateSandboxDelay
					},
					Spec: agentsv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{{Name: "c", Image: "nginx"}},
								},
							},
						},
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxPending,
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			podExist:    false,
			wantErr:     false,
			wantRequeue: 3 * time.Second,
		},
		{
			name:        "feature gate enabled, high-priority sandbox bypasses rate limit",
			featureGate: true,
			setupRL: func(rl *RateLimiter) {
				oldThreshold := prioritySandboxThreshold
				prioritySandboxThreshold = 0
				t.Cleanup(func() { prioritySandboxThreshold = oldThreshold })
				rl.highPrioritySandboxTrack["ns/hp1"] = &SandboxTrack{Namespace: "ns", Name: "hp1"}
			},
			args: EnsureFuncArgs{
				Pod: nil,
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "high-sandbox",
						Namespace: "default",
						Annotations: map[string]string{
							agentsv1alpha1.SandboxAnnotationPriority: "1",
						},
					},
					Spec: agentsv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{{Name: "c", Image: "nginx"}},
								},
							},
						},
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			podExist:    false,
			wantErr:     false,
			wantRequeue: 0,
		},
		{
			name:        "feature gate disabled, no rate limiting even when threshold exceeded",
			featureGate: false,
			setupRL: func(rl *RateLimiter) {
				oldThreshold := prioritySandboxThreshold
				prioritySandboxThreshold = 0
				t.Cleanup(func() { prioritySandboxThreshold = oldThreshold })
				rl.highPrioritySandboxTrack["ns/hp1"] = &SandboxTrack{Namespace: "ns", Name: "hp1"}
			},
			args: EnsureFuncArgs{
				Pod: nil,
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "normal-sandbox",
						Namespace: "default",
					},
					Spec: agentsv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{{Name: "c", Image: "nginx"}},
								},
							},
						},
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			podExist:    false,
			wantErr:     false,
			wantRequeue: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// feature gate setup
			if tt.featureGate {
				_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=true")
				t.Cleanup(func() {
					_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=false")
				})
			}

			fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			rl := NewRateLimiter()
			if tt.setupRL != nil {
				tt.setupRL(rl)
			}
			control := &commonControl{
				Client:               fakeClient,
				recorder:             record.NewFakeRecorder(10),
				inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
				rateLimiter:          rl,
				podControl:           NewPodControl(fakeClient, record.NewFakeRecorder(10), GeneratePodFromSandbox),
				syncStatusFromPod:    defaultCommonSyncStatusFromPod,
			}

			requeue, err := control.EnsureSandboxRunning(context.TODO(), tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("EnsureSandboxRunning() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if requeue != tt.wantRequeue {
				t.Errorf("EnsureSandboxRunning() requeue = %v, want %v", requeue, tt.wantRequeue)
			}

			// Verify that pod was created if it didn't exist and not rate-limited
			if !tt.podExist && tt.args.Pod == nil && tt.wantRequeue == 0 {
				pod := &corev1.Pod{}
				err := fakeClient.Get(context.TODO(), types.NamespacedName{Name: tt.args.Box.Name, Namespace: tt.args.Box.Namespace}, pod)
				if err != nil {
					t.Errorf("Expected pod to be created, but it wasn't: %v", err)
				}
			}

			// If pod is running, verify status was updated
			if tt.args.Pod != nil && tt.args.Pod.Status.Phase == corev1.PodRunning {
				if _, stale := StaleSandboxPodOwner(tt.args.Pod, tt.args.Box); stale {
					// A stale pod must not be adopted: phase stays Pending.
					if tt.args.NewStatus.Phase != agentsv1alpha1.SandboxPending {
						t.Errorf("Expected sandbox phase to stay Pending for stale pod, got %v", tt.args.NewStatus.Phase)
					}
					return
				}
				if tt.args.NewStatus.Phase != agentsv1alpha1.SandboxRunning {
					t.Errorf("Expected sandbox phase to be Running, got %v", tt.args.NewStatus.Phase)
				}
			}
		})
	}
}

func TestCommonControl_EnsureSandboxUpdated(t *testing.T) {
	enableProbeGate(t)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	// Claim cases exercise the SandboxClaim delivery path (no UpgradePolicy).
	// expectReason "" means no InplaceUpdate condition may be written. wait
	// means the adapter reports done=false: EnsureSandboxUpdated must
	// early-return, leaving Ready=False/InplaceUpdating, the stale ProbeValid
	// verdict and PodInfo untouched. A Failed target early-returns the same way
	// and keeps Ready=False rather than following the old Pod's readiness.
	// Otherwise the Running path resumes: the probe manager drops the stale
	// verdict and identity is synced.
	tests := []struct {
		name            string
		args            EnsureFuncArgs
		wantErr         bool
		claimKind       string
		expectReason    string
		expectReady     bool
		wait            bool
		expectEvent     string
		readyAfterPatch corev1.ConditionStatus
	}{
		{name: "claim no-op reports success and resumes status sync", claimKind: "noop", expectReady: true, expectReason: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded},
		{name: "claim metadata patches pod and reports success", claimKind: "metadata", expectReady: true, expectReason: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded},
		{name: "claim metadata succeeds but written pod is no longer ready", claimKind: "metadata", readyAfterPatch: corev1.ConditionFalse, expectReason: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded},
		{name: "claim metadata succeeds and written pod becomes ready", claimKind: "metadata", readyAfterPatch: corev1.ConditionTrue, expectReady: true, expectReason: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded},
		{name: "claim resource downscale remains metadata-only when live resources cover it", claimKind: "qos-downscale", expectReady: true, expectReason: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded},
		{name: "claim untracked pod fails and remains unready without requeue", claimKind: "untracked", expectReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed, expectEvent: "InplaceUpdateFailed"},
		{name: "claim corrupted state fails and remains unready without requeue", claimKind: "corrupted", expectReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed, expectEvent: "InplaceUpdateFailed"},
		{name: "claim unsupported template fails and remains unready", claimKind: "unsupported", expectReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed, expectEvent: "InplaceUpdateFailed"},
		{name: "claim terminal failure closes stale readiness without re-evaluation", claimKind: "terminal", expectReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed},
		{name: "claim previous success remains usable", claimKind: "terminal", expectReady: true, expectReason: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded},
		{name: "claim terminal unsupported resize is not re-evaluated", claimKind: "terminal", expectReady: true, expectReason: agentsv1alpha1.SandboxInplaceUpdateReasonUnsupportedResize},
		{name: "claim unsupported resize returns an error and keeps readiness closed", claimKind: "resize-unsupported", wantErr: true, expectReason: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating, wait: true, expectEvent: "InplaceUpdateFailed"},
		{name: "claim infeasible resize fails and remains unready", claimKind: "Infeasible", expectReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed, expectEvent: "InplaceUpdateFailed"},
		{name: "claim deferred resize fails and remains unready", claimKind: "Deferred", expectReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed, expectEvent: "InplaceUpdateFailed"},
		{name: "claim image pending keeps readiness closed", claimKind: "image-pending", expectReason: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating, wait: true},
		{name: "claim delivered image with cleared terminal state closes stale readiness", claimKind: "delivered-image", expectReason: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating, wait: true},
		{name: "claim delivered resize with cleared terminal state closes stale readiness", claimKind: "delivered-resize", expectReason: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating, wait: true},
		{name: "claim completed old record accepts new target", claimKind: "old-complete", expectReason: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating, wait: true},
		{name: "claim pending old record accepts new target", claimKind: "old-pending", expectReason: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating, wait: true},
		{
			name: "pod does not exist, should set failed phase",
			args: EnsureFuncArgs{
				Pod: nil,
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			wantErr: false,
		},
		{
			name: "pod exists, should update status fields",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Spec: corev1.PodSpec{
						NodeName: "node-1",
					},
					Status: corev1.PodStatus{
						PodIP: "10.0.0.1",
						Conditions: []corev1.PodCondition{
							{
								Type:               corev1.PodReady,
								Status:             corev1.ConditionTrue,
								LastTransitionTime: metav1.Now(),
							},
						},
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Spec: agentsv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									NodeName: "node-1",
								},
							},
						},
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
							Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "pod exists and start failed, should update status fields",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Spec: corev1.PodSpec{
						NodeName: "node-1",
					},
					Status: corev1.PodStatus{
						PodIP: "10.0.0.1",
						Conditions: []corev1.PodCondition{
							{
								Type:               corev1.PodReady,
								Status:             corev1.ConditionFalse,
								LastTransitionTime: metav1.Now(),
							},
						},
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name: "test-container",
								State: corev1.ContainerState{
									Waiting: &corev1.ContainerStateWaiting{
										Reason:  "CrashLoopBackOff",
										Message: "back-off 5m0s restarting failed",
									},
								},
								Ready:        false,
								RestartCount: 0,
								Image:        "nginx:latest",
								ImageID:      "docker-pullable://nginx@sha256:...",
								ContainerID:  "docker://abc123",
							},
						},
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Spec: agentsv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									NodeName: "node-1",
								},
							},
						},
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
							Reason:             agentsv1alpha1.SandboxReadyReasonStartContainerFailed,
							Message:            "back-off 5m0s restarting failed",
						},
					},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.claimKind != "" {
				pod := newRunningPod()
				pod.UID = "claim-pod"
				pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = "target"
				box := newUpgradeTestSandbox(nil, nil)
				box.Spec.UpgradePolicy = nil
				box.Spec.Template.Spec = *pod.Spec.DeepCopy()
				box.Generation = 2
				status := &agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning, ObservedGeneration: 2, UpdateRevision: "target",
					PodInfo: agentsv1alpha1.PodInfo{PodIP: pod.Status.PodIP},
					Conditions: []metav1.Condition{
						{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating},
						{Type: string(agentsv1alpha1.SandboxConditionProbeValid), Status: metav1.ConditionFalse, Reason: "OldProbeConfig"},
					},
				}
				switch tt.claimKind {
				case "metadata":
					pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = "old"
					box.Spec.Template.Labels = map[string]string{"claim-metadata": "new"}
					if tt.readyAfterPatch == corev1.ConditionTrue {
						pod.Status.Conditions[0].Status = corev1.ConditionFalse
					}
				case "qos-downscale":
					pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = "old"
					pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					}
					box.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
						Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
					}
				case "untracked":
					delete(pod.Labels, agentsv1alpha1.PodLabelTemplateHash)
				case "corrupted":
					pod.Annotations = map[string]string{inplaceupdate.PodAnnotationInPlaceUpdateStateKey: "{broken"}
				case "resize-unsupported", "Infeasible", "Deferred":
					pod.Spec.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}
					box.Spec.Template.Spec.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}
					if tt.claimKind == "resize-unsupported" {
						pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = "old"
					} else {
						pod.Status.ContainerStatuses[0].Resources = pod.Spec.Containers[0].Resources.DeepCopy()
						pod.Spec.Containers[0].Resources = *box.Spec.Template.Spec.Containers[0].Resources.DeepCopy()
						pod.Annotations = map[string]string{inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"target","updateResources":true}`}
						pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{Type: corev1.PodResizePending, Status: corev1.ConditionTrue, Reason: tt.claimKind})
					}
				case "delivered-image", "delivered-resize":
					// Simulate a delivered new target with the old terminal condition cleared but Ready still True from the previous round.
					utils.SetSandboxCondition(status, metav1.Condition{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue})
					if tt.claimKind == "delivered-image" {
						pod.Spec.Containers[0].Image = "test:v2"
						box.Spec.Template.Spec.Containers[0].Image = "test:v2"
						pod.Annotations = map[string]string{inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"target","updateImages":true,"lastContainerStatuses":{"sandbox":{"imageID":"img-old","targetImage":"test:v2"}}}`}
					} else {
						pod.Spec.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}
						box.Spec.Template.Spec.Containers[0].Resources = *pod.Spec.Containers[0].Resources.DeepCopy()
						pod.Status.ContainerStatuses[0].Resources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}}
						pod.Annotations = map[string]string{inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"target","updateResources":true}`}
					}
				case "image-pending", "old-complete", "old-pending":
					pod.Annotations = map[string]string{inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"old","updateImages":true,"lastContainerStatuses":{"sandbox":{"imageID":"img-old"}}}`}
					if tt.claimKind != "image-pending" {
						pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = "old"
						box.Spec.Template.Spec.Containers[0].Image = "test:v2"
					}
					if tt.claimKind == "old-complete" {
						pod.Status.ContainerStatuses[0].ImageID = "img-new"
					}
				}
				if tt.claimKind == "terminal" {
					box.Status.UpdateRevision = "target"
					priorStatus := metav1.ConditionFalse
					priorMessage := ""
					if tt.expectReason == agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded {
						priorStatus = metav1.ConditionTrue
					}
					if tt.expectReason == agentsv1alpha1.SandboxInplaceUpdateReasonFailed {
						priorMessage = "previous update failed"
						utils.SetSandboxCondition(status, metav1.Condition{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: agentsv1alpha1.SandboxReadyReasonPodReady})
					}
					utils.SetSandboxCondition(status, metav1.Condition{Type: string(agentsv1alpha1.SandboxConditionInplaceUpdate), Status: priorStatus, Reason: tt.expectReason, Message: priorMessage, ObservedGeneration: box.Generation})
				}
				_, immutable := HashSandbox(box)
				box.Annotations[agentsv1alpha1.SandboxHashImmutablePart] = immutable
				if tt.claimKind == "unsupported" {
					box.Annotations[agentsv1alpha1.SandboxHashImmutablePart] = "unsupported"
				}
				tt.args = EnsureFuncArgs{Pod: pod, Box: box, NewStatus: status}
			} else if tt.args.Pod != nil {
				// Status-sync cases observe a tracked Pod at the current revision.
				tt.args.Pod.Labels = map[string]string{agentsv1alpha1.PodLabelTemplateHash: "current-revision"}
				tt.args.NewStatus.UpdateRevision = "current-revision"
			}
			fc := fake.NewClientBuilder().WithScheme(scheme).Build()
			// EnsureProbe resolves the pod's node to pick the probe delivery
			// mechanism, so the node the table places the pod on must exist.
			require.NoError(t, fc.Create(context.TODO(), realNode("node-1")))
			if tt.claimKind == "resize-unsupported" {
				fc = interceptor.NewClient(fc, interceptor.Funcs{
					SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
						return apierrors.NewNotFound(corev1.Resource("pods/resize"), "test-sandbox")
					},
					Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
						return apierrors.NewForbidden(corev1.Resource("pods"), "test-sandbox", fmt.Errorf("resize disabled"))
					},
				})
			}
			if tt.args.Pod != nil {
				err := fc.Create(context.TODO(), tt.args.Pod)
				if err != nil {
					t.Fatalf("create pod failed: %s", err.Error())
				}
			}
			if tt.readyAfterPatch != "" {
				fc = interceptor.NewClient(fc, interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if _, ok := obj.(*corev1.Pod); ok {
							latest := &corev1.Pod{}
							require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), latest))
							latest.Status.Conditions[0].Status = tt.readyAfterPatch
							latest.Status.PodIP = "10.0.0.2"
							require.NoError(t, c.Status().Update(ctx, latest))
						}
						return c.Patch(ctx, obj, patch, opts...)
					},
				})
			}
			recorder := record.NewFakeRecorder(20)
			control := &commonControl{
				Client:               fc,
				recorder:             recorder,
				inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fc, inplaceupdate.DefaultGeneratePatchBodyFunc),
				podControl:           NewPodControl(fc, record.NewFakeRecorder(10), GeneratePodFromSandbox),
				probeManager:         NewPodProbeManager(fc, record.NewFakeRecorder(10)),
				syncStatusFromPod:    defaultCommonSyncStatusFromPod,
			}

			_, err := control.EnsureSandboxUpdated(context.TODO(), tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("EnsureSandboxUpdated() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.claimKind != "" {
				status := tt.args.NewStatus
				if tt.claimKind == "resize-unsupported" {
					var resizeErr *inplaceupdate.ResizeNotSupportedError
					require.ErrorAs(t, err, &resizeErr)
					require.True(t, apierrors.IsForbidden(err))
					update := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
					require.NotNil(t, update)
					require.Equal(t, "cannot deliver in-place update: in-place pod resize not supported: Forbidden (HTTP 403)", update.Message)
					require.ErrorContains(t, err, "resize disabled")
					require.False(t, isInplaceUpdateTerminal(status))
				}
				if tt.expectReason == "" {
					require.Nil(t, utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionInplaceUpdate)))
					ready := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionReady))
					require.NotNil(t, ready)
					if tt.expectReady {
						require.Equal(t, metav1.ConditionTrue, ready.Status)
					} else {
						require.Equal(t, metav1.ConditionFalse, ready.Status)
						require.Equal(t, agentsv1alpha1.SandboxReadyReasonInplaceUpdating, ready.Reason)
					}
				} else {
					assertInplaceConditions(t, status, tt.expectReady, tt.expectReason)
					if tt.expectReason == agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded {
						update := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
						if tt.claimKind == "terminal" {
							require.Empty(t, update.Message)
						} else {
							require.Equal(t, "in-place update completed successfully", update.Message)
							// syncStatusFromPod derives Ready from the written Pod; Succeeded does not guarantee readiness.
							ready := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionReady))
							if tt.expectReady {
								require.Equal(t, agentsv1alpha1.SandboxReadyReasonPodReady, ready.Reason)
							}
							require.Empty(t, ready.Message)
						}
					}
				}
				// done=true resumes the Running path: EnsureProbe drops the stale
				// ProbeValid verdict (no probes configured) and syncStatusFromPod
				// fills PodInfo. done=false and a Failed target early-return and
				// leave both untouched.
				probe := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionProbeValid))
				if tt.wait || tt.expectReason == agentsv1alpha1.SandboxInplaceUpdateReasonFailed {
					require.NotNil(t, probe)
					require.Equal(t, "OldProbeConfig", probe.Reason)
					require.Empty(t, status.PodInfo.PodUID)
				} else {
					require.Nil(t, probe)
					require.Equal(t, tt.args.Pod.UID, status.PodInfo.PodUID)
					require.Equal(t, tt.args.Pod.Status.PodIP, status.SandboxIp)
				}
				var events []string
				for drained := false; !drained; {
					select {
					case event := <-recorder.Events:
						events = append(events, event)
					default:
						drained = true
					}
				}
				if tt.expectEvent != "" {
					require.Contains(t, strings.Join(events, "\n"), tt.expectEvent)
				}
				if tt.claimKind == "terminal" {
					require.Empty(t, events)
					if tt.expectReason == agentsv1alpha1.SandboxInplaceUpdateReasonFailed {
						update := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
						require.Equal(t, "previous update failed", update.Message)
					}
					before := status.DeepCopy()
					_, err := control.EnsureSandboxUpdated(t.Context(), tt.args)
					require.NoError(t, err)
					require.Equal(t, before, status)
					require.Empty(t, recorder.Events)
				}
				storedPod := &corev1.Pod{}
				require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(tt.args.Pod), storedPod))
				require.Equal(t, types.UID("claim-pod"), storedPod.UID)
				if tt.readyAfterPatch != "" {
					require.Equal(t, storedPod, tt.args.Pod, "subsequent Claim steps must use the written Pod")
					require.Equal(t, tt.readyAfterPatch, utils.GetPodCondition(&tt.args.Pod.Status, corev1.PodReady).Status)
					require.Equal(t, "10.0.0.2", status.SandboxIp)
					require.Equal(t, "10.0.0.2", status.PodInfo.PodIP)
				}
				switch tt.claimKind {
				case "metadata":
					require.Equal(t, "new", storedPod.Labels["claim-metadata"])
					require.Equal(t, "target", storedPod.Labels[agentsv1alpha1.PodLabelTemplateHash])
				case "qos-downscale":
					request := storedPod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
					limit := storedPod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
					require.Equal(t, int64(500), request.MilliValue())
					require.Equal(t, int64(500), limit.MilliValue())
				case "old-complete", "old-pending":
					// The latest template supersedes the earlier in-place target.
					require.Equal(t, "target", storedPod.Labels[agentsv1alpha1.PodLabelTemplateHash])
					require.Equal(t, "test:v2", storedPod.Spec.Containers[0].Image)
				}
				// Both an in-progress round and a terminal failure must keep the
				// cache-side readiness wait closed.
				produced := tt.args.Box.DeepCopy()
				produced.Status = *status.DeepCopy()
				cache, _, err := cachetest.NewTestCache(t, produced)
				require.NoError(t, err)
				err = cache.NewSandboxWaitReadyTask(t.Context(), produced).Wait(100 * time.Millisecond)
				if !tt.expectReady {
					require.ErrorContains(t, err, "object is not satisfied")
				} else {
					require.NoError(t, err)
				}
				return
			}
			if tt.args.Pod == nil {
				if tt.args.NewStatus.Phase != agentsv1alpha1.SandboxFailed {
					t.Errorf("Expected sandbox phase to be Failed, got %v", tt.args.NewStatus.Phase)
				}
				if tt.args.NewStatus.Message != "Sandbox Pod Not Found" {
					t.Errorf("Expected message 'Sandbox Pod Not Found', got %v", tt.args.NewStatus.Message)
				}
			} else {
				if tt.args.NewStatus.NodeName != tt.args.Pod.Spec.NodeName {
					t.Errorf("Expected NodeName to be %s, got %s", tt.args.Pod.Spec.NodeName, tt.args.NewStatus.NodeName)
				}
				if tt.args.NewStatus.SandboxIp != tt.args.Pod.Status.PodIP {
					t.Errorf("Expected SandboxIp to be %s, got %s", tt.args.Pod.Status.PodIP, tt.args.NewStatus.SandboxIp)
				}
				if tt.args.NewStatus.PodInfo.PodIP != tt.args.Pod.Status.PodIP {
					t.Errorf("Expected PodInfo.PodIP to be %s, got %s", tt.args.Pod.Status.PodIP, tt.args.NewStatus.PodInfo.PodIP)
				}
				if tt.args.NewStatus.PodInfo.NodeName != tt.args.Pod.Spec.NodeName {
					t.Errorf("Expected PodInfo.NodeName to be %s, got %s", tt.args.Pod.Spec.NodeName, tt.args.NewStatus.PodInfo.NodeName)
				}
			}
		})
	}
}

// TestCommonControl_EnsureSandboxUpdated_InplaceRounds drives the SandboxClaim
// in-place path through consecutive reconciles. Before each reconcile a step
// may accept a new target the way calculateStatus does, let the kubelet act on
// the Pod, and inject Pod write failures; the stored Pod is re-read every time
// so each reconcile observes what the previous one delivered.
func TestCommonControl_EnsureSandboxUpdated_InplaceRounds(t *testing.T) {
	enableProbeGate(t)
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))

	const (
		msgSucceeded   = "in-place update completed successfully"
		msgWriteFailed = "cannot deliver in-place update: InternalError (HTTP 500)"
		msgIncomplete  = "inplace update is incompleted"
	)
	readySince := metav1.NewTime(time.Unix(1000, 0))

	burstable := func(cpu string) corev1.ResourceRequirements {
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse("128Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		}
	}
	guaranteed := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("256Mi")},
	}

	// Template changes accepted as a new target.
	setLabel := func(key, value string) func(*corev1.PodTemplateSpec) {
		return func(tpl *corev1.PodTemplateSpec) {
			if tpl.Labels == nil {
				tpl.Labels = map[string]string{}
			}
			tpl.Labels[key] = value
		}
	}
	setImage := func(image string) func(*corev1.PodTemplateSpec) {
		return func(tpl *corev1.PodTemplateSpec) { tpl.Spec.Containers[0].Image = image }
	}
	setResources := func(res corev1.ResourceRequirements) func(*corev1.PodTemplateSpec) {
		return func(tpl *corev1.PodTemplateSpec) { tpl.Spec.Containers[0].Resources = *res.DeepCopy() }
	}
	setCommand := func(cmd ...string) func(*corev1.PodTemplateSpec) {
		return func(tpl *corev1.PodTemplateSpec) { tpl.Spec.Containers[0].Command = cmd }
	}
	targets := func(fns ...func(*corev1.PodTemplateSpec)) func(*corev1.PodTemplateSpec) {
		return func(tpl *corev1.PodTemplateSpec) {
			for _, fn := range fns {
				fn(tpl)
			}
		}
	}

	// Kubelet actions written through the Pod status subresource.
	runImage := func(image, imageID string) func(*corev1.Pod) {
		return func(pod *corev1.Pod) {
			cs := &pod.Status.ContainerStatuses[0]
			cs.Image, cs.ImageID = image, imageID
			cs.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		}
	}
	waitImage := func(image, reason string) func(*corev1.Pod) {
		return func(pod *corev1.Pod) {
			cs := &pod.Status.ContainerStatuses[0]
			cs.Image = image
			cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "pulling " + image}}
		}
	}
	applyResources := func(pod *corev1.Pod) {
		pod.Status.ContainerStatuses[0].Resources = pod.Spec.Containers[0].Resources.DeepCopy()
	}
	resizePending := func(reason, message string) func(*corev1.Pod) {
		return func(pod *corev1.Pod) {
			pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
				Type: corev1.PodResizePending, Status: corev1.ConditionTrue, Reason: reason, Message: message,
			})
		}
	}
	podReady := func(ready bool) func(*corev1.Pod) {
		return func(pod *corev1.Pod) {
			cond := utils.GetPodCondition(&pod.Status, corev1.PodReady)
			cond.Status, cond.Message = corev1.ConditionTrue, ""
			if !ready {
				cond.Status, cond.Message = corev1.ConditionFalse, "containers with unready status: [sandbox]"
			}
			cond.LastTransitionTime = metav1.NewTime(time.Unix(2000, 0))
		}
	}
	kubelet := func(fns ...func(*corev1.Pod)) func(*corev1.Pod) {
		return func(pod *corev1.Pod) {
			for _, fn := range fns {
				fn(pod)
			}
		}
	}

	podState := func(t *testing.T, pod *corev1.Pod) *inplaceupdate.InPlaceUpdateState {
		t.Helper()
		state, err := inplaceupdate.GetPodInPlaceUpdateState(pod)
		require.NoError(t, err)
		return state
	}
	requestCPU := func(pod *corev1.Pod) int64 {
		q := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
		return q.MilliValue()
	}

	type step struct {
		name    string
		target  func(*corev1.PodTemplateSpec)
		kubelet func(*corev1.Pod)
		// fault is "patch" for the metadata/image Pod patch or "resize" for the
		// pods/resize subresource; the write fails with HTTP 500.
		fault           string
		wantErr         bool
		wantUpdate      string
		wantMessage     string
		wantReady       metav1.ConditionStatus
		wantReadyReason string
		wantEvent       bool
		// wantUnchanged requires the reconcile to leave the status untouched.
		wantUnchanged bool
		check         func(t *testing.T, pod *corev1.Pod, status *agentsv1alpha1.SandboxStatus)
	}
	cases := []struct {
		name  string
		steps []step
	}{
		{
			name: "metadata-only change keeps Ready true",
			steps: []step{
				{
					name: "label delivered", target: setLabel("tier", "gold"),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, wantMessage: msgSucceeded,
					wantReady: metav1.ConditionTrue, wantReadyReason: agentsv1alpha1.SandboxReadyReasonPodReady,
					check: func(t *testing.T, pod *corev1.Pod, status *agentsv1alpha1.SandboxStatus) {
						require.Equal(t, "gold", pod.Labels["tier"])
						require.Equal(t, status.UpdateRevision, pod.Labels[agentsv1alpha1.PodLabelTemplateHash])
						require.Nil(t, podState(t, pod))
						ready := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionReady))
						require.True(t, ready.LastTransitionTime.Equal(&readySince), "Ready must not transition")
					},
				},
				{
					name:       "repeated reconcile is a no-op",
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, wantMessage: msgSucceeded,
					wantReady: metav1.ConditionTrue, wantReadyReason: agentsv1alpha1.SandboxReadyReasonPodReady,
					wantUnchanged: true,
				},
			},
		},
		{
			name: "metadata-only write failure closes Ready until a retry succeeds",
			steps: []step{
				{
					name: "patch fails", target: setLabel("tier", "gold"), fault: "patch", wantErr: true,
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating, wantMessage: msgWriteFailed,
					wantReady: metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
					wantEvent: true,
					check: func(t *testing.T, pod *corev1.Pod, _ *agentsv1alpha1.SandboxStatus) {
						require.Empty(t, pod.Labels["tier"])
					},
				},
				{
					name: "same failure is neither rewritten nor reported again", fault: "patch", wantErr: true,
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating, wantMessage: msgWriteFailed,
					wantReady: metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
					wantUnchanged: true,
				},
				{
					name:       "retry delivers and reopens Ready",
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, wantMessage: msgSucceeded,
					wantReady: metav1.ConditionTrue, wantReadyReason: agentsv1alpha1.SandboxReadyReasonPodReady,
					check: func(t *testing.T, pod *corev1.Pod, status *agentsv1alpha1.SandboxStatus) {
						require.Equal(t, "gold", pod.Labels["tier"])
						ready := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionReady))
						require.True(t, ready.LastTransitionTime.Equal(&readySince), "Ready follows the Pod transition time")
					},
				},
			},
		},
		{
			name: "metadata-only change on an unready Pod follows Pod readiness",
			steps: []step{
				{
					name: "delivered while Pod unready", target: setLabel("tier", "gold"), kubelet: podReady(false),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, wantMessage: msgSucceeded,
					wantReady: metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonPodReady,
				},
				{
					name: "Pod becomes ready", kubelet: podReady(true),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, wantMessage: msgSucceeded,
					wantReady: metav1.ConditionTrue, wantReadyReason: agentsv1alpha1.SandboxReadyReasonPodReady,
				},
			},
		},
		{
			name: "metadata-only target keeps an unfinished resize in progress",
			steps: []step{
				{
					name: "resize delivered", target: setResources(burstable("200m")),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
					wantReady:  metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
				},
				{
					name: "label target before kubelet applies resize", target: setLabel("tier", "gold"),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
					wantReady:  metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
					check: func(t *testing.T, pod *corev1.Pod, status *agentsv1alpha1.SandboxStatus) {
						require.Equal(t, "gold", pod.Labels["tier"])
						require.Equal(t, status.UpdateRevision, pod.Labels[agentsv1alpha1.PodLabelTemplateHash])
						require.True(t, podState(t, pod).UpdateResources, "metadata-only write keeps the earlier resize observation")
					},
				},
				{
					name: "kubelet applies resize", kubelet: applyResources,
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, wantMessage: msgSucceeded,
					wantReady: metav1.ConditionTrue, wantReadyReason: agentsv1alpha1.SandboxReadyReasonPodReady,
				},
			},
		},
		{
			name: "image update waits for the new container and then Pod readiness",
			steps: []step{
				{
					name: "image delivered", target: setImage("test:v2"),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
					wantReady:  metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
					check: func(t *testing.T, pod *corev1.Pod, status *agentsv1alpha1.SandboxStatus) {
						require.Equal(t, "test:v2", pod.Spec.Containers[0].Image)
						require.Equal(t, status.UpdateRevision, pod.Labels[agentsv1alpha1.PodLabelTemplateHash])
						state := podState(t, pod)
						require.True(t, state.UpdateImages)
						require.Equal(t, inplaceupdate.InPlaceUpdateContainerStatus{ImageID: "img-old", TargetImage: "test:v2"}, state.LastContainerStatuses["sandbox"])
					},
				},
				{
					name: "image pull back-off stays in progress silently", kubelet: kubelet(waitImage("test:v2", "ImagePullBackOff"), podReady(false)),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
					wantReady:  metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
					wantUnchanged: true,
				},
				{
					name: "new container runs but Pod is not ready", kubelet: runImage("test:v2", "img-v2"),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, wantMessage: msgSucceeded,
					wantReady: metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
					check: func(t *testing.T, _ *corev1.Pod, status *agentsv1alpha1.SandboxStatus) {
						ready := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionReady))
						require.Equal(t, msgIncomplete, ready.Message)
					},
				},
				{
					name: "Pod becomes ready", kubelet: podReady(true),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, wantMessage: msgSucceeded,
					wantReady: metav1.ConditionTrue, wantReadyReason: agentsv1alpha1.SandboxReadyReasonPodReady,
				},
			},
		},
		{
			name: "image write failure is retried",
			steps: []step{
				{
					name: "patch fails", target: setImage("test:v2"), fault: "patch", wantErr: true,
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating, wantMessage: msgWriteFailed,
					wantReady: metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
					wantEvent: true,
					check: func(t *testing.T, pod *corev1.Pod, _ *agentsv1alpha1.SandboxStatus) {
						require.Equal(t, "test:v1", pod.Spec.Containers[0].Image)
					},
				},
				{
					name:       "retry delivers the image",
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
					wantReady:  metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
					check: func(t *testing.T, pod *corev1.Pod, _ *agentsv1alpha1.SandboxStatus) {
						require.Equal(t, "test:v2", pod.Spec.Containers[0].Image)
					},
				},
				{
					name: "new container runs", kubelet: runImage("test:v2", "img-v2"),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, wantMessage: msgSucceeded,
					wantReady: metav1.ConditionTrue, wantReadyReason: agentsv1alpha1.SandboxReadyReasonPodReady,
				},
			},
		},
		{
			name: "image change with an immutable change fails until the target drops it",
			steps: []step{
				{
					name: "command change rejected", target: targets(setImage("test:v2"), setCommand("sleep", "inf")),
					wantUpdate:  agentsv1alpha1.SandboxInplaceUpdateReasonFailed,
					wantMessage: "in-place update only supports changing container images, resources and template metadata",
					wantReady:   metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed,
					wantEvent: true,
					check: func(t *testing.T, pod *corev1.Pod, status *agentsv1alpha1.SandboxStatus) {
						require.Equal(t, "test:v1", pod.Spec.Containers[0].Image)
						require.NotEqual(t, status.UpdateRevision, pod.Labels[agentsv1alpha1.PodLabelTemplateHash])
						require.Empty(t, status.PodInfo.PodUID, "a failed target skips status synchronization")
					},
				},
				{
					name: "command change reverted", target: setCommand(),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
					wantReady:  metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
				},
				{
					name: "new container runs", kubelet: runImage("test:v2", "img-v2"),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, wantMessage: msgSucceeded,
					wantReady: metav1.ConditionTrue, wantReadyReason: agentsv1alpha1.SandboxReadyReasonPodReady,
				},
			},
		},
		{
			name: "resource update waits for the kubelet to apply the resize",
			steps: []step{
				{
					name: "resize delivered", target: setResources(burstable("200m")),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
					wantReady:  metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
					check: func(t *testing.T, pod *corev1.Pod, status *agentsv1alpha1.SandboxStatus) {
						require.Equal(t, int64(200), requestCPU(pod))
						require.Equal(t, status.UpdateRevision, pod.Labels[agentsv1alpha1.PodLabelTemplateHash])
						state := podState(t, pod)
						require.True(t, state.UpdateResources)
						require.False(t, state.UpdateImages)
					},
				},
				{
					name: "kubelet applies resize", kubelet: applyResources,
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, wantMessage: msgSucceeded,
					wantReady: metav1.ConditionTrue, wantReadyReason: agentsv1alpha1.SandboxReadyReasonPodReady,
				},
			},
		},
		{
			name: "infeasible resize fails terminally",
			steps: []step{
				{
					name: "resize delivered", target: setResources(burstable("200m")),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
					wantReady:  metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
				},
				{
					name: "kubelet reports infeasible", kubelet: resizePending(corev1.PodReasonInfeasible, "Node didn't have enough capacity"),
					wantUpdate:  agentsv1alpha1.SandboxInplaceUpdateReasonFailed,
					wantMessage: "pod resize is infeasible: Node didn't have enough capacity",
					wantReady:   metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed,
					wantEvent: true,
				},
				{
					name:        "terminal failure is not re-evaluated",
					wantUpdate:  agentsv1alpha1.SandboxInplaceUpdateReasonFailed,
					wantMessage: "pod resize is infeasible: Node didn't have enough capacity",
					wantReady:   metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed,
					wantUnchanged: true,
				},
			},
		},
		{
			name: "resize write failure is retried",
			steps: []step{
				{
					name: "resize fails", target: setResources(burstable("200m")), fault: "resize", wantErr: true,
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating, wantMessage: msgWriteFailed,
					wantReady: metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
					wantEvent: true,
					check: func(t *testing.T, pod *corev1.Pod, status *agentsv1alpha1.SandboxStatus) {
						require.Equal(t, int64(100), requestCPU(pod))
						require.NotEqual(t, status.UpdateRevision, pod.Labels[agentsv1alpha1.PodLabelTemplateHash])
					},
				},
				{
					name:       "retry delivers the resize",
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
					wantReady:  metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
					check: func(t *testing.T, pod *corev1.Pod, _ *agentsv1alpha1.SandboxStatus) {
						require.Equal(t, int64(200), requestCPU(pod))
					},
				},
				{
					name: "kubelet applies resize", kubelet: applyResources,
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, wantMessage: msgSucceeded,
					wantReady: metav1.ConditionTrue, wantReadyReason: agentsv1alpha1.SandboxReadyReasonPodReady,
				},
			},
		},
		{
			name: "QoS changing resize is rejected until a compatible target arrives",
			steps: []step{
				{
					name: "Guaranteed target rejected", target: setResources(guaranteed),
					wantUpdate:  agentsv1alpha1.SandboxInplaceUpdateReasonFailed,
					wantMessage: "resource resize would change QoS class from Burstable to Guaranteed, resize rejected",
					wantReady:   metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed,
					wantEvent: true,
					check: func(t *testing.T, pod *corev1.Pod, status *agentsv1alpha1.SandboxStatus) {
						require.Equal(t, int64(100), requestCPU(pod))
						require.NotEqual(t, status.UpdateRevision, pod.Labels[agentsv1alpha1.PodLabelTemplateHash])
					},
				},
				{
					name: "Burstable target delivered", target: setResources(burstable("200m")),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
					wantReady:  metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
				},
				{
					name: "kubelet applies resize", kubelet: applyResources,
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, wantMessage: msgSucceeded,
					wantReady: metav1.ConditionTrue, wantReadyReason: agentsv1alpha1.SandboxReadyReasonPodReady,
				},
			},
		},
		{
			name: "image and resource update completes only when both are observed",
			steps: []step{
				{
					name: "image and resize delivered", target: targets(setImage("test:v2"), setResources(burstable("200m"))),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
					wantReady:  metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
					check: func(t *testing.T, pod *corev1.Pod, _ *agentsv1alpha1.SandboxStatus) {
						require.Equal(t, "test:v2", pod.Spec.Containers[0].Image)
						require.Equal(t, int64(200), requestCPU(pod))
						state := podState(t, pod)
						require.True(t, state.UpdateImages)
						require.True(t, state.UpdateResources)
					},
				},
				{
					name: "resize applied but old container still runs", kubelet: applyResources,
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
					wantReady:  metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
					wantUnchanged: true,
				},
				{
					name: "new container runs", kubelet: runImage("test:v2", "img-v2"),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, wantMessage: msgSucceeded,
					wantReady: metav1.ConditionTrue, wantReadyReason: agentsv1alpha1.SandboxReadyReasonPodReady,
				},
			},
		},
		{
			name: "newer image target supersedes an unfinished image update",
			steps: []step{
				{
					name: "v2 delivered", target: setImage("test:v2"),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
					wantReady:  metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
				},
				{
					name: "v3 delivered before v2 runs", target: setImage("test:v3"),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
					wantReady:  metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
					check: func(t *testing.T, pod *corev1.Pod, status *agentsv1alpha1.SandboxStatus) {
						require.Equal(t, "test:v3", pod.Spec.Containers[0].Image)
						require.Equal(t, status.UpdateRevision, pod.Labels[agentsv1alpha1.PodLabelTemplateHash])
						require.Equal(t, "test:v3", podState(t, pod).LastContainerStatuses["sandbox"].TargetImage)
					},
				},
				{
					name: "superseded v2 container does not complete the round", kubelet: runImage("test:v2", "img-v2"),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
					wantReady:  metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
					wantUnchanged: true,
				},
				{
					name: "v3 container runs", kubelet: runImage("test:v3", "img-v3"),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, wantMessage: msgSucceeded,
					wantReady: metav1.ConditionTrue, wantReadyReason: agentsv1alpha1.SandboxReadyReasonPodReady,
				},
			},
		},
		{
			name: "new target after a completed round closes Ready again",
			steps: []step{
				{
					name: "v2 delivered", target: setImage("test:v2"),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
					wantReady:  metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
				},
				{
					name: "v2 container runs", kubelet: runImage("test:v2", "img-v2"),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, wantMessage: msgSucceeded,
					wantReady: metav1.ConditionTrue, wantReadyReason: agentsv1alpha1.SandboxReadyReasonPodReady,
				},
				{
					name: "v3 delivered", target: setImage("test:v3"),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
					wantReady:  metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
					check: func(t *testing.T, pod *corev1.Pod, _ *agentsv1alpha1.SandboxStatus) {
						require.Equal(t, inplaceupdate.InPlaceUpdateContainerStatus{ImageID: "img-v2", TargetImage: "test:v3"}, podState(t, pod).LastContainerStatuses["sandbox"])
					},
				},
				{
					name: "v3 container runs", kubelet: runImage("test:v3", "img-v3"),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, wantMessage: msgSucceeded,
					wantReady: metav1.ConditionTrue, wantReadyReason: agentsv1alpha1.SandboxReadyReasonPodReady,
				},
			},
		},
		{
			name: "rollback to the running image completes immediately",
			steps: []step{
				{
					name: "v2 delivered", target: setImage("test:v2"),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
					wantReady:  metav1.ConditionFalse, wantReadyReason: agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
				},
				{
					name: "rolled back to v1 before v2 runs", target: setImage("test:v1"),
					wantUpdate: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded, wantMessage: msgSucceeded,
					wantReady: metav1.ConditionTrue, wantReadyReason: agentsv1alpha1.SandboxReadyReasonPodReady,
					check: func(t *testing.T, pod *corev1.Pod, status *agentsv1alpha1.SandboxStatus) {
						require.Equal(t, "test:v1", pod.Spec.Containers[0].Image)
						require.Equal(t, status.UpdateRevision, pod.Labels[agentsv1alpha1.PodLabelTemplateHash])
					},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			pod := newRunningPod()
			pod.UID = "claim-pod"
			pod.Spec.Containers[0].Resources = burstable("100m")
			pod.Status.ContainerStatuses[0].Resources = pod.Spec.Containers[0].Resources.DeepCopy()
			pod.Status.Conditions[0].LastTransitionTime = readySince
			box := newUpgradeTestSandbox(nil, nil)
			box.Spec.UpgradePolicy = nil
			box.Spec.Template.Spec = *pod.Spec.DeepCopy()
			box.Generation = 1
			revision, immutable := HashSandbox(box)
			// The immutable hash is recorded once and never refreshed.
			box.Annotations[agentsv1alpha1.SandboxHashImmutablePart] = immutable
			pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = revision
			status := &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning, ObservedGeneration: 1, UpdateRevision: revision,
				Conditions: []metav1.Condition{{
					Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue,
					Reason: agentsv1alpha1.SandboxReadyReasonPodReady, LastTransitionTime: readySince,
				}},
			}

			base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(realNode("node-1"), pod).Build()
			key := client.ObjectKeyFromObject(pod)
			t.Cleanup(func() { ResourceVersionExpectations.Delete(pod) })
			fault := ""
			fc := interceptor.NewClient(base, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, isPod := obj.(*corev1.Pod); isPod && fault == "patch" {
						return apierrors.NewInternalError(fmt.Errorf("simulated pod patch failure"))
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
				SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if subResource == "resize" && fault == "resize" {
						return apierrors.NewInternalError(fmt.Errorf("simulated pod resize failure"))
					}
					return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
				},
			})
			recorder := record.NewFakeRecorder(20)
			control := &commonControl{
				Client:               fc,
				recorder:             recorder,
				inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fc, inplaceupdate.DefaultGeneratePatchBodyFunc),
				podControl:           NewPodControl(fc, record.NewFakeRecorder(10), GeneratePodFromSandbox),
				probeManager:         NewPodProbeManager(fc, record.NewFakeRecorder(10)),
				syncStatusFromPod:    defaultCommonSyncStatusFromPod,
			}

			for i, s := range tc.steps {
				t.Logf("step %d: %s", i, s.name)
				if s.target != nil {
					// Mirror calculateStatus accepting a new target on the Claim
					// path: a new revision drops the previous round's outcome.
					s.target(box.Spec.Template)
					box.Generation++
					status.ObservedGeneration = box.Generation
					status.UpdateRevision, _ = HashSandbox(box)
					utils.RemoveSandboxCondition(status, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
				}
				if s.kubelet != nil {
					live := &corev1.Pod{}
					require.NoError(t, base.Get(ctx, key, live))
					s.kubelet(live)
					require.NoError(t, base.Status().Update(ctx, live))
				}
				fault = s.fault
				current := &corev1.Pod{}
				require.NoError(t, base.Get(ctx, key, current))
				before := status.DeepCopy()

				_, err := control.EnsureSandboxUpdated(ctx, EnsureFuncArgs{Pod: current, Box: box, NewStatus: status})
				if s.wantErr {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}

				update := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
				require.NotNil(t, update)
				require.Equal(t, s.wantUpdate, update.Reason)
				require.Equal(t, s.wantMessage, update.Message)
				wantUpdateStatus := metav1.ConditionFalse
				if s.wantUpdate == agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded {
					wantUpdateStatus = metav1.ConditionTrue
				}
				require.Equal(t, wantUpdateStatus, update.Status)
				ready := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionReady))
				require.NotNil(t, ready)
				require.Equal(t, s.wantReady, ready.Status)
				require.Equal(t, s.wantReadyReason, ready.Reason)
				switch {
				case s.wantReady == metav1.ConditionTrue:
					require.Empty(t, ready.Message)
				case s.wantReadyReason == agentsv1alpha1.SandboxInplaceUpdateReasonFailed:
					require.Equal(t, update.Message, ready.Message)
				case s.wantUpdate == agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating:
					require.Equal(t, msgIncomplete, ready.Message)
				}
				if s.wantUnchanged {
					require.Equal(t, before, status)
				}

				var events []string
				for drained := false; !drained; {
					select {
					case event := <-recorder.Events:
						events = append(events, event)
					default:
						drained = true
					}
				}
				if s.wantEvent {
					require.Len(t, events, 1)
					require.Contains(t, events[0], "InplaceUpdateFailed")
				} else {
					require.Empty(t, events)
				}

				if s.check != nil {
					stored := &corev1.Pod{}
					require.NoError(t, base.Get(ctx, key, stored))
					s.check(t, stored, status)
				}
			}
		})
	}
}

func TestCommonControl_EnsureSandboxUpdated_InitializePath(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	now := metav1.Now()

	readyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			UID:       "pod-uid-1",
		},
		Spec: corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.1",
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
			},
		},
	}

	notReadyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			UID:       "pod-uid-1",
		},
		Spec: corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.1",
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: now},
			},
		},
	}

	baseSandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
	}

	pendingInitConditions := []metav1.Condition{
		{
			Type:               string(agentsv1alpha1.SandboxConditionReady),
			Status:             metav1.ConditionFalse,
			LastTransitionTime: now,
			Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
		},
		{
			Type:               string(agentsv1alpha1.RuntimeInitialized),
			Status:             metav1.ConditionFalse,
			LastTransitionTime: now,
			Reason:             agentsv1alpha1.SandboxConditionRuntimeInitReasonPending,
			Message:            "Waiting for pod ready before initialization",
		},
	}

	tests := []struct {
		name           string
		pod            *corev1.Pod
		initializer    SandboxInitializer
		conditions     []metav1.Condition
		expectError    string
		expectReady    metav1.ConditionStatus
		expectInitDone bool
	}{
		{
			name:           "pod not ready, init pending, returns nil and waits",
			pod:            notReadyPod,
			initializer:    &mockSandboxInitializer{},
			conditions:     pendingInitConditions,
			expectReady:    metav1.ConditionFalse,
			expectInitDone: false,
		},
		{
			name:           "pod ready, init pending, initialize succeeds, sets Ready=True",
			pod:            readyPod,
			initializer:    &mockSandboxInitializer{},
			conditions:     pendingInitConditions,
			expectReady:    metav1.ConditionTrue,
			expectInitDone: true,
		},
		{
			name:        "pod ready, init pending, initialize fails, returns error",
			pod:         readyPod,
			initializer: &mockSandboxInitializer{err: fmt.Errorf("runtime init failed")},
			conditions:  pendingInitConditions,
			expectError: "runtime init failed",
		},
		{
			name:        "pod ready, init failed, retries initialize and succeeds",
			pod:         readyPod,
			initializer: &mockSandboxInitializer{},
			conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionReady),
					Status:             metav1.ConditionFalse,
					LastTransitionTime: now,
					Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
				},
				{
					Type:               string(agentsv1alpha1.RuntimeInitialized),
					Status:             metav1.ConditionFalse,
					LastTransitionTime: now,
					Reason:             agentsv1alpha1.SandboxConditionRuntimeInitReasonFailed,
					Message:            "Runtime initialization failed: previous error",
				},
			},
			expectReady:    metav1.ConditionTrue,
			expectInitDone: true,
		},
		{
			name:        "no RuntimeInitialized condition, skips init block entirely",
			pod:         readyPod,
			initializer: &mockSandboxInitializer{},
			conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionReady),
					Status:             metav1.ConditionFalse,
					LastTransitionTime: now,
					Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
				},
			},
			expectReady:    metav1.ConditionTrue,
			expectInitDone: false,
		},
		{
			name:        "RuntimeInitialized already True, skips init block",
			pod:         readyPod,
			initializer: &mockSandboxInitializer{},
			conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionReady),
					Status:             metav1.ConditionFalse,
					LastTransitionTime: now,
					Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
				},
				{
					Type:               string(agentsv1alpha1.RuntimeInitialized),
					Status:             metav1.ConditionTrue,
					LastTransitionTime: now,
					Reason:             agentsv1alpha1.SandboxConditionRuntimeInitReasonSucceeded,
				},
			},
			expectReady:    metav1.ConditionTrue,
			expectInitDone: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := fake.NewClientBuilder().WithScheme(scheme).Build()
			// EnsureProbe resolves the pod's node to pick the probe delivery
			// mechanism, so the node the table places the pod on must exist.
			require.NoError(t, fc.Create(context.TODO(), realNode("node-1")))
			control := &commonControl{
				Client:               fc,
				recorder:             record.NewFakeRecorder(10),
				inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fc, inplaceupdate.DefaultGeneratePatchBodyFunc),
				initializer:          tt.initializer,
				podControl:           NewPodControl(fc, record.NewFakeRecorder(10), GeneratePodFromSandbox),
				probeManager:         NewPodProbeManager(fc, record.NewFakeRecorder(10)),
				syncStatusFromPod:    defaultCommonSyncStatusFromPod,
			}

			pod := tt.pod.DeepCopy()
			pod.Labels = map[string]string{agentsv1alpha1.PodLabelTemplateHash: "current-revision"}
			box := baseSandbox.DeepCopy()
			box.Spec.Template = &corev1.PodTemplateSpec{Spec: *pod.Spec.DeepCopy()}
			newStatus := &agentsv1alpha1.SandboxStatus{
				Phase:          agentsv1alpha1.SandboxRunning,
				UpdateRevision: "current-revision",
				Conditions:     append([]metav1.Condition{}, tt.conditions...),
			}

			_, err := control.EnsureSandboxUpdated(context.TODO(), EnsureFuncArgs{
				Pod:       pod,
				Box:       box,
				NewStatus: newStatus,
			})

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)

			readyCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady))
			require.NotNil(t, readyCond, "expected Ready condition to exist")
			if readyCond.Status != tt.expectReady {
				t.Errorf("expected Ready=%s, got %s", tt.expectReady, readyCond.Status)
			}

			if tt.expectInitDone {
				initCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.RuntimeInitialized))
				require.NotNil(t, initCond, "expected RuntimeInitialized condition to exist")
			}

			// Verify Initialize was called exactly when expected
			mock := tt.initializer.(*mockSandboxInitializer)
			if tt.expectInitDone {
				assert.Equal(t, 1, mock.called, "Initialize should have been called once")
			} else {
				assert.Equal(t, 0, mock.called, "Initialize should not have been called")
			}
		})
	}
}

func TestCommonControl_EnsureSandboxPaused(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name      string
		args      EnsureFuncArgs
		podExists bool
		wantErr   bool
	}{
		{
			name: "pod does not exist, should mark paused",
			args: EnsureFuncArgs{
				Pod: nil,
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "test-sandbox",
						Namespace:  "default",
						Finalizers: []string{SandboxFinalizer},
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					// preparePausedPhase initializes Paused condition with Pending
					// and sets Ready to False before EnsureSandboxPaused is called.
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
							Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
						},
						{
							Type:               string(agentsv1alpha1.SandboxConditionPaused),
							Status:             metav1.ConditionFalse,
							Reason:             agentsv1alpha1.SandboxPausedReasonPending,
							LastTransitionTime: metav1.Now(),
						},
					},
				},
			},
			podExists: false,
			wantErr:   false,
		},
		{
			name: "pod exists but being deleted, should wait",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "test-sandbox",
						Namespace:         "default",
						DeletionTimestamp: &metav1.Time{Time: metav1.Now().Time},
						Finalizers:        []string{"fake"},
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "test-sandbox",
						Namespace:  "default",
						Finalizers: []string{SandboxFinalizer},
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
							Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
						},
						{
							Type:               string(agentsv1alpha1.SandboxConditionPaused),
							Status:             metav1.ConditionFalse,
							Reason:             agentsv1alpha1.SandboxPausedReasonPending,
							LastTransitionTime: metav1.Now(),
						},
					},
				},
			},
			podExists: true,
			wantErr:   false,
		},
		{
			name: "pod exists, should delete it",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "test-sandbox",
						Namespace:  "default",
						Finalizers: []string{SandboxFinalizer},
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
							Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
						},
						{
							Type:               string(agentsv1alpha1.SandboxConditionPaused),
							Status:             metav1.ConditionFalse,
							Reason:             agentsv1alpha1.SandboxPausedReasonPending,
							LastTransitionTime: metav1.Now(),
						},
					},
				},
			},
			podExists: true,
			wantErr:   false,
		},
		{
			name: "re-pause after resume, finalizer already present, no Paused condition",
			args: EnsureFuncArgs{
				Pod: nil,
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "test-sandbox",
						Namespace:  "default",
						Finalizers: []string{SandboxFinalizer},
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					// preparePausedPhase initializes Paused condition with Pending
					// and sets Ready to False before EnsureSandboxPaused is called.
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
							Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
						},
						{
							Type:               string(agentsv1alpha1.SandboxConditionPaused),
							Status:             metav1.ConditionFalse,
							Reason:             agentsv1alpha1.SandboxPausedReasonPending,
							LastTransitionTime: metav1.Now(),
						},
					},
				},
			},
			podExists: false,
			wantErr:   false,
		},
		{
			name: "re-reconcile during pausing, finalizer already present, Paused condition exists",
			args: EnsureFuncArgs{
				Pod: nil,
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "test-sandbox",
						Namespace:  "default",
						Finalizers: []string{SandboxFinalizer},
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					// Paused condition exists with Status=False — pausing in progress.
					// The cond != nil branch should NOT attempt to add the finalizer.
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionPaused),
							Status:             metav1.ConditionFalse,
							Reason:             agentsv1alpha1.SandboxPausedReasonPending,
							LastTransitionTime: metav1.Now(),
						},
					},
				},
			},
			podExists: false,
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := []client.Object{tt.args.Box}
			if tt.args.Pod != nil {
				objects = append(objects, tt.args.Pod)
			}

			fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			control := &commonControl{
				Client:               fc,
				recorder:             record.NewFakeRecorder(10),
				inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fc, inplaceupdate.DefaultGeneratePatchBodyFunc),
				podControl:           NewPodControl(fc, record.NewFakeRecorder(10), GeneratePodFromSandbox),
				syncStatusFromPod:    defaultCommonSyncStatusFromPod,
				checkpointControl:    NewCheckpointControl(fc, record.NewFakeRecorder(10)),
			}

			err := control.EnsureSandboxPaused(context.TODO(), tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("EnsureSandboxPaused() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			// Verify finalizer is present (added by preparePausedPhase before EnsureSandboxPaused)
			if !tt.wantErr {
				updatedBox := &agentsv1alpha1.Sandbox{}
				if getErr := fc.Get(context.TODO(), types.NamespacedName{Name: tt.args.Box.Name, Namespace: tt.args.Box.Namespace}, updatedBox); getErr != nil {
					t.Fatalf("Failed to get updated sandbox: %v", getErr)
				}
				if !controllerutil.ContainsFinalizer(updatedBox, SandboxFinalizer) {
					t.Errorf("Expected finalizer %q to be added to sandbox", SandboxFinalizer)
				}
			}

			// Verify pod was deleted if it existed initially
			if tt.podExists && tt.args.Pod != nil && tt.args.Pod.DeletionTimestamp == nil {
				pod := &corev1.Pod{}
				err := fc.Get(context.TODO(), types.NamespacedName{Name: tt.args.Pod.Name, Namespace: tt.args.Pod.Namespace}, pod)
				if err == nil && pod.DeletionTimestamp.IsZero() {
					t.Errorf("Expected pod to be deleted, but it still exists")
				}
			}
		})
	}
}

func TestCommonControl_EnsureSandboxResumed(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	now := metav1.Now()
	tests := []struct {
		name           string
		args           EnsureFuncArgs
		podExist       bool
		wantErr        bool
		expectError    string
		initializer    SandboxInitializer // nil defaults to &defaultSandboxInitializer{}
		expectedStatus *agentsv1alpha1.SandboxStatus
	}{
		{
			name: "pod does not exist, should create",
			args: EnsureFuncArgs{
				Pod: nil,
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Spec: agentsv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "test-container",
											Image: "nginx:latest",
										},
									},
								},
							},
						},
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxResuming,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: now,
							Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
						},
					},
				},
			},
			podExist: false,
			wantErr:  false,
			expectedStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxResuming,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReady),
						Status:             metav1.ConditionFalse,
						LastTransitionTime: now,
						Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
					},
				},
			},
		},
		{
			name: "pod exists but not running",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodPending,
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxResuming,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: now,
							Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
						},
					},
				},
			},
			podExist: true,
			wantErr:  false,
			expectedStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxResuming,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReady),
						Status:             metav1.ConditionFalse,
						LastTransitionTime: now,
						Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
					},
				},
			},
		},
		{
			name: "pod is running and ready, transitions to Running with RuntimeInitialized=Pending",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						Conditions: []corev1.PodCondition{
							{
								Type:               corev1.PodReady,
								Status:             corev1.ConditionTrue,
								LastTransitionTime: now,
							},
						},
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxResuming,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: now,
							Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
						},
						{
							Type:               string(agentsv1alpha1.SandboxConditionResumed),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: now,
							Reason:             agentsv1alpha1.SandboxResumeReasonCreatePod,
						},
					},
				},
			},
			podExist: true,
			wantErr:  false,
			expectedStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReady),
						Status:             metav1.ConditionFalse,
						LastTransitionTime: now,
						Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
					},
					{
						Type:               string(agentsv1alpha1.SandboxConditionResumed),
						Status:             metav1.ConditionTrue,
						LastTransitionTime: now,
						Reason:             agentsv1alpha1.SandboxResumeReasonResumePod,
					},
					{
						Type:               string(agentsv1alpha1.RuntimeInitialized),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxConditionRuntimeInitReasonPending,
						Message:            "Waiting for pod ready before initialization",
						LastTransitionTime: now,
					},
				},
			},
		},
		{
			name: "pod is running and ready, initializer not called during resume",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
						UID:       "pod-uid-123",
					},
					Spec: corev1.PodSpec{
						NodeName: "node-1",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						PodIP: "10.0.0.5",
						Conditions: []corev1.PodCondition{
							{
								Type:               corev1.PodReady,
								Status:             corev1.ConditionTrue,
								LastTransitionTime: now,
							},
						},
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxResuming,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: now,
							Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
						},
						{
							Type:               string(agentsv1alpha1.SandboxConditionResumed),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: now,
							Reason:             agentsv1alpha1.SandboxResumeReasonCreatePod,
						},
					},
				},
			},
			podExist:    true,
			wantErr:     false,
			initializer: &mockSandboxInitializer{err: fmt.Errorf("runtime re-init failed")},
			expectedStatus: &agentsv1alpha1.SandboxStatus{
				Phase:     agentsv1alpha1.SandboxRunning,
				NodeName:  "node-1",
				SandboxIp: "10.0.0.5",
				PodInfo: agentsv1alpha1.PodInfo{
					PodIP:    "10.0.0.5",
					NodeName: "node-1",
					PodUID:   "pod-uid-123",
				},
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReady),
						Status:             metav1.ConditionFalse,
						LastTransitionTime: now,
						Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
					},
					{
						Type:               string(agentsv1alpha1.SandboxConditionResumed),
						Status:             metav1.ConditionTrue,
						LastTransitionTime: now,
						Reason:             agentsv1alpha1.SandboxResumeReasonResumePod,
					},
					{
						Type:               string(agentsv1alpha1.RuntimeInitialized),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxConditionRuntimeInitReasonPending,
						Message:            "Waiting for pod ready before initialization",
						LastTransitionTime: now,
					},
				},
			},
		},
		{
			name: "pod is running but not ready, transitions to Running",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						Conditions: []corev1.PodCondition{
							{
								Type:               corev1.PodReady,
								Status:             corev1.ConditionFalse,
								LastTransitionTime: now,
							},
						},
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxResuming,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: now,
							Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
						},
						{
							Type:               string(agentsv1alpha1.SandboxConditionResumed),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: now,
							Reason:             agentsv1alpha1.SandboxResumeReasonCreatePod,
						},
					},
				},
			},
			podExist: true,
			wantErr:  false,
			expectedStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReady),
						Status:             metav1.ConditionFalse,
						LastTransitionTime: now,
						Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
					},
					{
						Type:               string(agentsv1alpha1.SandboxConditionResumed),
						Status:             metav1.ConditionTrue,
						LastTransitionTime: now,
						Reason:             agentsv1alpha1.SandboxResumeReasonResumePod,
					},
					{
						Type:               string(agentsv1alpha1.RuntimeInitialized),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxConditionRuntimeInitReasonPending,
						Message:            "Waiting for pod ready before initialization",
						LastTransitionTime: now,
					},
				},
			},
		},
		{
			name: "pod is running with resumed condition, sets Resumed=True",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
						UID:       "pod-uid-456",
					},
					Spec: corev1.PodSpec{
						NodeName: "node-2",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						PodIP: "10.0.0.10",
						Conditions: []corev1.PodCondition{
							{
								Type:               corev1.PodReady,
								Status:             corev1.ConditionTrue,
								LastTransitionTime: now,
							},
						},
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxResuming,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: now,
							Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
						},
						{
							Type:               string(agentsv1alpha1.SandboxConditionResumed),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: now,
							Reason:             agentsv1alpha1.SandboxResumeReasonCreatePod,
						},
					},
				},
			},
			podExist: true,
			wantErr:  false,
			expectedStatus: &agentsv1alpha1.SandboxStatus{
				Phase:     agentsv1alpha1.SandboxRunning,
				NodeName:  "node-2",
				SandboxIp: "10.0.0.10",
				PodInfo: agentsv1alpha1.PodInfo{
					PodIP:    "10.0.0.10",
					NodeName: "node-2",
					PodUID:   "pod-uid-456",
				},
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReady),
						Status:             metav1.ConditionFalse,
						LastTransitionTime: now,
						Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
					},
					{
						Type:               string(agentsv1alpha1.SandboxConditionResumed),
						Status:             metav1.ConditionTrue,
						LastTransitionTime: now,
						Reason:             agentsv1alpha1.SandboxResumeReasonResumePod,
					},
					{
						Type:               string(agentsv1alpha1.RuntimeInitialized),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxConditionRuntimeInitReasonPending,
						Message:            "Waiting for pod ready before initialization",
						LastTransitionTime: now,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := fake.NewClientBuilder().WithScheme(scheme).Build()
			init := tt.initializer
			if init == nil {
				init = &defaultSandboxInitializer{recorder: record.NewFakeRecorder(10)}
			}
			control := &commonControl{
				Client:               fc,
				recorder:             record.NewFakeRecorder(10),
				inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fc, inplaceupdate.DefaultGeneratePatchBodyFunc),
				initializer:          init,
				checkpointControl:    NewCheckpointControl(fc, record.NewFakeRecorder(10)),
				podControl:           NewPodControl(fc, record.NewFakeRecorder(10), GeneratePodFromSandbox),
				syncStatusFromPod:    defaultCommonSyncStatusFromPod,
			}

			err := control.EnsureSandboxResumed(context.TODO(), tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("EnsureSandboxResumed() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}

			// Verify that pod was created if it didn't exist
			if !tt.podExist && tt.args.Pod == nil {
				pod := &corev1.Pod{}
				err := fc.Get(context.TODO(), types.NamespacedName{Name: tt.args.Box.Name, Namespace: tt.args.Box.Namespace}, pod)
				if err != nil {
					t.Errorf("Expected pod to be created, but it wasn't: %v", err)
				}
			}

			if !reflect.DeepEqual(tt.args.NewStatus, tt.expectedStatus) {
				// Normalize LastTransitionTime for conditions set via metav1.Now() inside the function
				// (e.g., PostResumeInit) to avoid nanosecond-level mismatch with test's `now`.
				for i := range tt.args.NewStatus.Conditions {
					for j := range tt.expectedStatus.Conditions {
						if tt.args.NewStatus.Conditions[i].Type == tt.expectedStatus.Conditions[j].Type {
							tt.expectedStatus.Conditions[j].LastTransitionTime = tt.args.NewStatus.Conditions[i].LastTransitionTime
						}
					}
				}
				if !reflect.DeepEqual(tt.args.NewStatus, tt.expectedStatus) {
					t.Errorf("Expected sandbox(%s), got(%s)", utils.DumpJson(tt.expectedStatus), utils.DumpJson(tt.args.NewStatus))
				}
			}
		})
	}
}

func TestCommonControl_EnsureSandboxTerminated(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name      string
		args      EnsureFuncArgs
		podExists bool
		wantErr   bool
	}{
		{
			name: "pod does not exist, should remove finalizer",
			args: EnsureFuncArgs{
				Pod: nil,
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "test-sandbox",
						Namespace:  "default",
						Finalizers: []string{SandboxFinalizer},
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			podExists: false,
			wantErr:   false,
		},
		{
			name: "pod exists but being deleted, should wait",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "test-sandbox",
						Namespace:         "default",
						DeletionTimestamp: &metav1.Time{Time: metav1.Now().Time},
						Finalizers:        []string{"fake"},
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			podExists: true,
			wantErr:   false,
		},
		{
			name: "pod exists, should delete it",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			podExists: true,
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := []client.Object{}
			if tt.args.Box != nil {
				objects = append(objects, tt.args.Box)
			}
			if tt.args.Pod != nil {
				objects = append(objects, tt.args.Pod)
			}

			fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			control := &commonControl{
				Client:               fc,
				recorder:             record.NewFakeRecorder(10),
				inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fc, inplaceupdate.DefaultGeneratePatchBodyFunc),
				podControl:           NewPodControl(fc, record.NewFakeRecorder(10), GeneratePodFromSandbox),
				syncStatusFromPod:    defaultCommonSyncStatusFromPod,
			}

			err := control.EnsureSandboxTerminated(context.TODO(), tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("EnsureSandboxTerminated() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			// Verify pod was deleted if it existed initially and wasn't already being deleted
			if tt.podExists && tt.args.Pod != nil && tt.args.Pod.DeletionTimestamp == nil {
				pod := &corev1.Pod{}
				err := fc.Get(context.TODO(), types.NamespacedName{Name: tt.args.Pod.Name, Namespace: tt.args.Pod.Namespace}, pod)
				if err == nil && pod.DeletionTimestamp.IsZero() {
					t.Errorf("Expected pod to be deleted, but it still exists")
				}
			}
		})
	}
}

func TestPodControl_CreatePod(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	cli := fake.NewClientBuilder().WithScheme(scheme).Build()
	control := NewPodControl(cli, record.NewFakeRecorder(10), GeneratePodFromSandbox)

	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels:      map[string]string{"app": "test"},
						Annotations: map[string]string{"annotation": "value"},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:latest",
							},
						},
					},
				},
			},
		},
	}

	status := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "rev1",
	}

	pod, err := control.CreatePod(context.TODO(), CreatePodArgs{Box: sandbox, NewStatus: status})
	if err != nil {
		t.Fatalf("CreatePod() error = %v", err)
	}

	if pod.Name != sandbox.Name {
		t.Errorf("Expected pod name %s, got %s", sandbox.Name, pod.Name)
	}
	if pod.Namespace != sandbox.Namespace {
		t.Errorf("Expected pod namespace %s, got %s", sandbox.Namespace, pod.Namespace)
	}
	if pod.Labels[agentsv1alpha1.PodLabelTemplateHash] != status.UpdateRevision {
		t.Errorf("Expected pod label %s to be %s, got %s", agentsv1alpha1.PodLabelTemplateHash, status.UpdateRevision, pod.Labels[agentsv1alpha1.PodLabelTemplateHash])
	}
	if pod.Annotations[utils.PodAnnotationCreatedBy] != utils.CreatedBySandbox {
		t.Errorf("Expected pod annotation %s to be %s, got %s", utils.PodAnnotationCreatedBy, utils.CreatedBySandbox, pod.Annotations[utils.PodAnnotationCreatedBy])
	}
	if pod.Labels[utils.PodLabelCreatedBy] != utils.CreatedBySandbox {
		t.Errorf("Expected pod label %s to be %s, got %s", utils.PodLabelCreatedBy, utils.CreatedBySandbox, pod.Labels[utils.PodLabelCreatedBy])
	}

	sandboxWithPVC := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox-with-pvc",
			Namespace: "default",
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels:      map[string]string{"app": "test"},
						Annotations: map[string]string{"annotation": "value"},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:latest",
								VolumeMounts: []corev1.VolumeMount{
									{
										Name:      "www",
										MountPath: "/var/www",
									},
								},
							},
						},
					},
				},
				VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "www",
						},
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
							Resources: corev1.VolumeResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceStorage: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
			},
		},
	}

	podWithPVC, err := control.CreatePod(context.TODO(), CreatePodArgs{Box: sandboxWithPVC, NewStatus: status})
	if err != nil {
		t.Fatalf("CreatePod() with PVC error = %v", err)
	}

	expectedPVCName, err := GeneratePVCName("www", "test-sandbox-with-pvc")
	if err != nil {
		t.Fatalf("GeneratePVCName() error = %v", err)
	}

	if len(podWithPVC.Spec.Volumes) != 1 {
		t.Errorf("Expected 1 volume, got %d", len(podWithPVC.Spec.Volumes))
	} else {
		volume := podWithPVC.Spec.Volumes[0]
		if volume.Name != "www" {
			t.Errorf("Expected volume name to be 'www', got %s", volume.Name)
		}
		if volume.VolumeSource.PersistentVolumeClaim == nil {
			t.Error("Expected volume source to be PersistentVolumeClaim")
		} else if volume.VolumeSource.PersistentVolumeClaim.ClaimName != expectedPVCName {
			t.Errorf("Expected PVC claim name to be %s, got %s", expectedPVCName, volume.VolumeSource.PersistentVolumeClaim.ClaimName)
		}
	}

	if len(podWithPVC.Spec.Containers) != 1 {
		t.Errorf("Expected 1 container, got %d", len(podWithPVC.Spec.Containers))
	} else {
		container := podWithPVC.Spec.Containers[0]
		if len(container.VolumeMounts) != 1 {
			t.Errorf("Expected 1 volume mount, got %d", len(container.VolumeMounts))
		} else {
			volumeMount := container.VolumeMounts[0]
			if volumeMount.Name != "www" {
				t.Errorf("Expected volume mount name to be 'www', got %s", volumeMount.Name)
			}
			if volumeMount.MountPath != "/var/www" {
				t.Errorf("Expected volume mount path to be '/var/www', got %s", volumeMount.MountPath)
			}
		}
	}
}

func TestPodControl_CreatePod_WithCheckpointDelta(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name         string
		delta        *runtime.RawExtension
		expectLabels map[string]string
		expectError  string
	}{
		{
			name:  "nil delta - no patch applied",
			delta: nil,
		},
		{
			name: "valid delta - patches pod",
			delta: &runtime.RawExtension{
				Raw: []byte(`{"metadata":{"labels":{"checkpoint":"restored"}}}`),
			},
			expectLabels: map[string]string{"checkpoint": "restored"},
		},
		{
			name: "invalid delta - logs warning but continues",
			delta: &runtime.RawExtension{
				Raw: []byte(`{invalid json`),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := record.NewFakeRecorder(10)
			cli := fake.NewClientBuilder().WithScheme(scheme).Build()
			control := NewPodControl(cli, recorder, GeneratePodFromSandbox)
			sandbox := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "nginx:latest"}},
							},
						},
					},
				},
			}
			pod, err := control.CreatePod(context.TODO(), CreatePodArgs{
				Box:              sandbox,
				NewStatus:        &agentsv1alpha1.SandboxStatus{UpdateRevision: "rev1"},
				PodTemplateDelta: tt.delta,
			})
			if tt.expectError != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.expectError)
				}
				if !reflect.DeepEqual(err.Error(), tt.expectError) {
					t.Fatalf("expected error %q, got %q", tt.expectError, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("CreatePod() error = %v", err)
			}
			for k, v := range tt.expectLabels {
				if pod.Labels[k] != v {
					t.Errorf("expected label %s=%s, got %s", k, v, pod.Labels[k])
				}
			}
		})
	}
}

func TestPodControl_CreatePod_GenerateError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	generateErr := func(_ context.Context, _ PodGenerateArgs) (*corev1.Pod, error) {
		return nil, fmt.Errorf("generate failed")
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()
	control := NewPodControl(cli, record.NewFakeRecorder(10), generateErr)

	_, err := control.CreatePod(context.TODO(), CreatePodArgs{
		Box: &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
		},
		NewStatus: &agentsv1alpha1.SandboxStatus{UpdateRevision: "rev1"},
	})
	if err == nil || err.Error() != "generate failed" {
		t.Fatalf("expected generate failed error, got %v", err)
	}
}

func TestPodControl_CreatePod_AlreadyExists(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	existingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "nginx:latest"}},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingPod).Build()
	control := NewPodControl(cli, record.NewFakeRecorder(10), GeneratePodFromSandbox)

	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "main", Image: "nginx:latest"}},
					},
				},
			},
		},
	}
	pod, err := control.CreatePod(context.TODO(), CreatePodArgs{
		Box:       sandbox,
		NewStatus: &agentsv1alpha1.SandboxStatus{UpdateRevision: "rev1"},
	})
	if err != nil {
		t.Fatalf("CreatePod() should succeed on AlreadyExists, got error = %v", err)
	}
	if pod == nil {
		t.Fatal("expected non-nil pod")
	}
}

func TestPodControl_CreatePod_WithSidecarInjection(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name                    string
		sandbox                 *agentsv1alpha1.Sandbox
		configMap               *corev1.ConfigMap
		featureGateEnabled      bool
		expectInitContainers    int
		expectContainers        int
		expectMainContainerEnvs int
		expectMainVolumeMounts  int
		expectVolumes           int
		expectRuntimeSidecar    bool // expect runtime-sidecar in InitContainers
		expectCSISidecar        bool // expect csi-sidecar in InitContainers
	}{
		{
			name: "no injection - no annotations",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx"},
								},
							},
						},
					},
				},
			},
			configMap:               nil,
			featureGateEnabled:      true,
			expectInitContainers:    0,
			expectContainers:        1,
			expectMainContainerEnvs: 0,
			expectMainVolumeMounts:  0,
			expectVolumes:           0,
			expectRuntimeSidecar:    false,
			expectCSISidecar:        false,
		},
		{
			name: "runtime injection only",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime,
						},
					},
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx"},
								},
							},
						},
					},
				},
			},
			configMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sidecarutils.SandboxInjectionConfigName,
					Namespace: utils.DefaultSandboxDeployNamespace,
				},
				Data: map[string]string{
					sidecarutils.KEY_RUNTIME_INJECTION_CONFIG: `{
						"mainContainer": {
							"name": "",
							"env": [
								{"name": "RUNTIME_ENV", "value": "test"},
								{"name": "DEBUG", "value": "true"}
							],
							"volumeMounts": []
						},
						"csiSidecar": [{
							"name": "runtime-sidecar",
							"image": "runtime:v1"
						}],
						"volume": []
					}`,
				},
			},
			featureGateEnabled:      true,
			expectInitContainers:    1,
			expectContainers:        1,
			expectMainContainerEnvs: 2,
			expectMainVolumeMounts:  0,
			expectVolumes:           0,
			expectRuntimeSidecar:    true,
			expectCSISidecar:        false,
		},
		{
			name: "csi injection only",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount,
						},
					},
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx"},
								},
							},
						},
					},
				},
			},
			configMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sidecarutils.SandboxInjectionConfigName,
					Namespace: utils.DefaultSandboxDeployNamespace,
				},
				Data: map[string]string{
					sidecarutils.KEY_CSI_INJECTION_CONFIG: `{
						"mainContainer": {
							"name": "",
							"env": [],
							"volumeMounts": [
								{"name": "csi-volume", "mountPath": "/mnt/csi"},
								{"name": "data-volume", "mountPath": "/data"}
							]
						},
						"csiSidecar": [{
							"name": "csi-sidecar",
							"image": "csi:v1"
						}],
						"volume": [
							{"name": "csi-volume", "emptyDir": {}},
							{"name": "data-volume", "emptyDir": {}}
						]
					}`,
				},
			},
			featureGateEnabled:      true,
			expectInitContainers:    1, // CSI sidecar is injected to InitContainers
			expectContainers:        1, // only main container
			expectMainContainerEnvs: 0,
			expectMainVolumeMounts:  2,
			expectVolumes:           2,
			expectRuntimeSidecar:    false,
			expectCSISidecar:        true,
		},
		{
			name: "both runtime and csi injection",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime,
						},
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount,
						},
					},
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx"},
								},
							},
						},
					},
				},
			},
			configMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sidecarutils.SandboxInjectionConfigName,
					Namespace: utils.DefaultSandboxDeployNamespace,
				},
				Data: map[string]string{
					sidecarutils.KEY_RUNTIME_INJECTION_CONFIG: `{
						"mainContainer": {
							"name": "",
							"env": [{"name": "RUNTIME", "value": "enabled"}],
							"volumeMounts": []
						},
						"csiSidecar": [{
							"name": "runtime-sidecar",
							"image": "runtime:v1"
						}],
						"volume": []
					}`,
					sidecarutils.KEY_CSI_INJECTION_CONFIG: `{
						"mainContainer": {
							"name": "",
							"env": [],
							"volumeMounts": [{"name": "csi-vol", "mountPath": "/csi"}]
						},
						"csiSidecar": [{
							"name": "csi-sidecar",
							"image": "csi:v1"
						}],
						"volume": [{"name": "csi-vol", "emptyDir": {}}]
					}`,
				},
			},
			featureGateEnabled:      true,
			expectInitContainers:    2, // runtime-sidecar + csi-sidecar both injected to InitContainers
			expectContainers:        1, // only main container
			expectMainContainerEnvs: 1,
			expectMainVolumeMounts:  1,
			expectVolumes:           1,
			expectRuntimeSidecar:    true,
			expectCSISidecar:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Feature gate setup
			if tt.featureGateEnabled {
				_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodInjectConfigGate=true")
				t.Cleanup(func() {
					_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodInjectConfigGate=false")
				})
			} else {
				_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodInjectConfigGate=false")
			}

			// Build client with configmap if needed
			objs := []client.Object{tt.sandbox}
			if tt.configMap != nil {
				objs = append(objs, tt.configMap)
			}

			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			control := NewPodControl(fakeClient, record.NewFakeRecorder(10), GeneratePodFromSandbox)

			newStatus := &agentsv1alpha1.SandboxStatus{
				UpdateRevision: "test-revision",
			}

			// Call CreatePod
			pod, err := control.CreatePod(context.TODO(), CreatePodArgs{Box: tt.sandbox, NewStatus: newStatus})
			if err != nil {
				t.Fatalf("CreatePod() unexpected error: %v", err)
			}

			// Verify InitContainers
			if len(pod.Spec.InitContainers) != tt.expectInitContainers {
				t.Errorf("expected %d InitContainers, got %d", tt.expectInitContainers, len(pod.Spec.InitContainers))
			}

			// Verify Containers
			if len(pod.Spec.Containers) != tt.expectContainers {
				t.Errorf("expected %d Containers, got %d", tt.expectContainers, len(pod.Spec.Containers))
			}

			// Find main container
			var mainContainer *corev1.Container
			for i := range pod.Spec.Containers {
				if pod.Spec.Containers[i].Name == "main" {
					mainContainer = &pod.Spec.Containers[i]
					break
				}
			}

			if mainContainer == nil {
				t.Fatal("main container not found")
			}

			// Verify main container envs
			if len(mainContainer.Env) != tt.expectMainContainerEnvs {
				t.Errorf("expected %d envs in main container, got %d", tt.expectMainContainerEnvs, len(mainContainer.Env))
			}

			// Verify main container volume mounts
			if len(mainContainer.VolumeMounts) != tt.expectMainVolumeMounts {
				t.Errorf("expected %d volume mounts in main container, got %d", tt.expectMainVolumeMounts, len(mainContainer.VolumeMounts))
			}

			// Verify volumes
			if len(pod.Spec.Volumes) != tt.expectVolumes {
				t.Errorf("expected %d volumes, got %d", tt.expectVolumes, len(pod.Spec.Volumes))
			}

			// Verify runtime sidecar in InitContainers
			if tt.expectRuntimeSidecar {
				runtimeFound := false
				for _, ic := range pod.Spec.InitContainers {
					if ic.Name == "runtime-sidecar" {
						runtimeFound = true
						break
					}
				}
				if !runtimeFound {
					t.Error("runtime sidecar not found in InitContainers")
				}
			}

			// Verify csi sidecar in InitContainers
			if tt.expectCSISidecar {
				csiFound := false
				for _, ic := range pod.Spec.InitContainers {
					if ic.Name == "csi-sidecar" {
						csiFound = true
						break
					}
				}
				if !csiFound {
					t.Error("csi sidecar not found in InitContainers")
				}
			}
		})
	}
}

func TestNewCommonControl(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	recorder := record.NewFakeRecorder(10)
	rl := NewRateLimiter()
	hooks := NewLifecycleHookManager(2, nil, recorder)

	control := NewCommonControl(SandboxControlArgs{
		Client:         fakeClient,
		Recorder:       recorder,
		RateLimiter:    rl,
		LifecycleHooks: hooks,
	})
	if control == nil {
		t.Fatal("NewCommonControl returned nil")
	}

	// Verify it implements SandboxControl interface
	var _ SandboxControl = control
	assert.Same(t, hooks, control.(*commonControl).lifecycleHooks)

	// Verify internal fields by calling methods (smoke test)
	args := EnsureFuncArgs{
		Pod: nil,
		Box: &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-sandbox",
				Namespace: "default",
			},
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "test", Image: "nginx"},
							},
						},
					},
				},
			},
		},
		NewStatus: &agentsv1alpha1.SandboxStatus{},
	}

	// EnsureSandboxUpdated with nil pod should set Failed phase
	_, err := control.EnsureSandboxUpdated(context.TODO(), args)
	if err != nil {
		t.Errorf("EnsureSandboxUpdated() unexpected error: %v", err)
	}
	if args.NewStatus.Phase != agentsv1alpha1.SandboxFailed {
		t.Errorf("Expected phase Failed, got %s", args.NewStatus.Phase)
	}
}

func TestCommonControl_EnsureSandboxUpdated_InplaceNotDone(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))

	pod := newRunningPod()
	t.Cleanup(func() { ResourceVersionExpectations.Delete(pod) })
	box := buildMatchingHashBox(pod.Name, pod.Namespace, *pod.Spec.DeepCopy())
	box.Spec.Template.Spec.Containers[0].Image = "test:v2"
	revision, _ := HashSandbox(box)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	recorder := record.NewFakeRecorder(10)
	control := &commonControl{
		Client:               fakeClient,
		recorder:             recorder,
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		probeManager:         NewPodProbeManager(fakeClient, recorder),
		syncStatusFromPod:    defaultCommonSyncStatusFromPod,
	}
	newStatus := &agentsv1alpha1.SandboxStatus{
		Phase:          agentsv1alpha1.SandboxRunning,
		UpdateRevision: revision,
		SandboxIp:      "previous-ip",
		Conditions: []metav1.Condition{
			{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: agentsv1alpha1.SandboxReadyReasonPodReady},
			{Type: string(agentsv1alpha1.SandboxConditionProbeValid), Status: metav1.ConditionFalse, Reason: "OldProbeConfig"},
		},
	}

	_, err := control.EnsureSandboxUpdated(t.Context(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	require.NoError(t, err)
	// Delivering an image without kubelet completion must skip probe and status synchronization.
	assertInplaceConditions(t, newStatus, false, agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating)
	require.Equal(t, agentsv1alpha1.SandboxRunning, newStatus.Phase)
	require.Equal(t, "previous-ip", newStatus.SandboxIp)
	require.Empty(t, newStatus.PodInfo)
	require.Equal(t, "OldProbeConfig", utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionProbeValid)).Reason)
	require.Equal(t, revision, pod.Labels[agentsv1alpha1.PodLabelTemplateHash])
	require.Equal(t, "test:v2", pod.Spec.Containers[0].Image)
	require.Equal(t, "test:v1", pod.Status.ContainerStatuses[0].Image)
	require.Empty(t, recorder.Events)
}

func TestCommonControl_EnsureSandboxResumed_TerminatingPod(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	now := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-sandbox",
			Namespace:         "default",
			DeletionTimestamp: &metav1.Time{Time: now.Time},
			Finalizers:        []string{"fake-finalizer"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	control := &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		podControl:           NewPodControl(fakeClient, record.NewFakeRecorder(10), GeneratePodFromSandbox),
		syncStatusFromPod:    defaultCommonSyncStatusFromPod,
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		Phase: agentsv1alpha1.SandboxResuming,
	}

	err := control.EnsureSandboxResumed(context.TODO(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err == nil {
		t.Error("Expected error for terminating pod, got nil")
	}
}

func TestCommonControl_EnsureSandboxResumed_SetResumedCondition(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	now := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	control := &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		podControl:           NewPodControl(fakeClient, record.NewFakeRecorder(10), GeneratePodFromSandbox),
		syncStatusFromPod:    defaultCommonSyncStatusFromPod,
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		Phase: agentsv1alpha1.SandboxResuming,
		Conditions: []metav1.Condition{
			{
				Type:               string(agentsv1alpha1.SandboxConditionResumed),
				Status:             metav1.ConditionFalse,
				LastTransitionTime: now,
				Reason:             agentsv1alpha1.SandboxResumeReasonCreatePod,
			},
			{
				Type:               string(agentsv1alpha1.SandboxConditionReady),
				Status:             metav1.ConditionFalse,
				LastTransitionTime: now,
				Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
			},
		},
	}

	err := control.EnsureSandboxResumed(context.TODO(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("EnsureSandboxResumed() unexpected error: %v", err)
	}

	// Pod is Pending, so resumed condition stays False and phase stays Resuming
	resumedCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionResumed))
	if resumedCond == nil {
		t.Fatal("Expected Resumed condition to exist")
	}
	if resumedCond.Status != metav1.ConditionFalse {
		t.Errorf("Expected Resumed condition to be False (pod not running yet), got %s", resumedCond.Status)
	}

	if newStatus.Phase != agentsv1alpha1.SandboxResuming {
		t.Errorf("Expected phase Resuming, got %s", newStatus.Phase)
	}
}

func TestCommonControl_EnsureSandboxResumed_LegacyBackfill(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	control := &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		podControl:           NewPodControl(fakeClient, record.NewFakeRecorder(10), GeneratePodFromSandbox),
		checkpointControl:    NewCheckpointControl(fakeClient, record.NewFakeRecorder(10)),
		syncStatusFromPod:    defaultCommonSyncStatusFromPod,
	}

	tests := []struct {
		name       string
		conditions []metav1.Condition
	}{
		{
			name:       "legacy Resuming without RuntimeInitialized should set Pending",
			conditions: []metav1.Condition{},
		},
		{
			name: "stale Succeeded from prior resume should be reset to Pending",
			conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.RuntimeInitialized),
					Status:             metav1.ConditionTrue,
					Reason:             agentsv1alpha1.SandboxConditionRuntimeInitReasonSucceeded,
					LastTransitionTime: metav1.Now(),
				},
			},
		},
		{
			name: "Failed from prior attempt should be reset to Pending",
			conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.RuntimeInitialized),
					Status:             metav1.ConditionFalse,
					Reason:             agentsv1alpha1.SandboxConditionRuntimeInitReasonFailed,
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
				Spec:       corev1.PodSpec{NodeName: "node1"},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					PodIP: "10.0.0.1",
				},
			}
			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
			}
			// Ensure Resumed condition is present so handleResume can flip it to True
			conditions := append([]metav1.Condition{}, tt.conditions...)
			conditions = append(conditions, metav1.Condition{
				Type:               string(agentsv1alpha1.SandboxConditionResumed),
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             agentsv1alpha1.SandboxResumeReasonCreatePod,
			})
			newStatus := &agentsv1alpha1.SandboxStatus{
				Phase:      agentsv1alpha1.SandboxResuming,
				Conditions: conditions,
			}

			err := control.EnsureSandboxResumed(context.TODO(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if newStatus.Phase != agentsv1alpha1.SandboxRunning {
				t.Fatalf("expected phase Running, got %s", newStatus.Phase)
			}

			initCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.RuntimeInitialized))
			if initCond == nil {
				t.Fatal("expected RuntimeInitialized condition to exist")
			}
			if initCond.Reason != agentsv1alpha1.SandboxConditionRuntimeInitReasonPending {
				t.Errorf("expected reason Pending, got %s", initCond.Reason)
			}
			if initCond.Status != metav1.ConditionFalse {
				t.Errorf("expected status False, got %s", initCond.Status)
			}
		})
	}
}

func TestCommonControl_EnsureSandboxPaused_FinalizerPatchError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-sandbox",
			Namespace:  "default",
			Finalizers: []string{SandboxFinalizer},
		},
	}
	// preparePausedPhase initializes Paused condition with Pending and sets
	// Ready to False before EnsureSandboxPaused is called.
	newStatus := &agentsv1alpha1.SandboxStatus{
		Conditions: []metav1.Condition{
			{
				Type:               string(agentsv1alpha1.SandboxConditionReady),
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
			},
			{
				Type:               string(agentsv1alpha1.SandboxConditionPaused),
				Status:             metav1.ConditionFalse,
				Reason:             agentsv1alpha1.SandboxPausedReasonPending,
				LastTransitionTime: metav1.Now(),
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(box).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				return fmt.Errorf("simulated patch error")
			},
		}).
		Build()

	control := &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		podControl:           NewPodControl(fakeClient, record.NewFakeRecorder(10), GeneratePodFromSandbox),
		checkpointControl:    NewCheckpointControl(fakeClient, record.NewFakeRecorder(10)),
	}

	// With Pod=nil and cond.Status=False, EnsureSandboxPaused marks the sandbox
	// as paused without any patch operations, so no error is expected.
	err := control.EnsureSandboxPaused(context.TODO(), EnsureFuncArgs{Pod: nil, Box: box, NewStatus: newStatus})
	assert.NoError(t, err)

	// Paused condition should be True with StopPauseSucceed reason
	pausedCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
	assert.NotNil(t, pausedCond)
	assert.Equal(t, metav1.ConditionTrue, pausedCond.Status)
	assert.Equal(t, agentsv1alpha1.SandboxPausedReasonStopPauseSucceed, pausedCond.Reason)
}

func TestCommonControl_EnsureSandboxTerminated_PodNotExist_NoFinalizer(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(box).Build()
	control := &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		podControl:           NewPodControl(fakeClient, record.NewFakeRecorder(10), GeneratePodFromSandbox),
		syncStatusFromPod:    defaultCommonSyncStatusFromPod,
	}

	err := control.EnsureSandboxTerminated(context.TODO(), EnsureFuncArgs{Pod: nil, Box: box, NewStatus: &agentsv1alpha1.SandboxStatus{}})
	if err != nil {
		t.Errorf("EnsureSandboxTerminated() unexpected error: %v", err)
	}
}

func TestCommonControl_EnsureSandboxPaused_AlreadyPaused(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	now := metav1.Now()
	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	control := &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		podControl:           NewPodControl(fakeClient, record.NewFakeRecorder(10), GeneratePodFromSandbox),
		syncStatusFromPod:    defaultCommonSyncStatusFromPod,
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		Phase: agentsv1alpha1.SandboxPaused,
		Conditions: []metav1.Condition{
			{
				Type:               string(agentsv1alpha1.SandboxConditionPaused),
				Status:             metav1.ConditionTrue,
				Reason:             agentsv1alpha1.SandboxPausedReasonStopPauseSucceed,
				LastTransitionTime: now,
			},
		},
	}

	err := control.EnsureSandboxPaused(context.TODO(), EnsureFuncArgs{Pod: nil, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Errorf("EnsureSandboxPaused() unexpected error: %v", err)
	}

	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Error("Expected Paused condition to remain True")
	}
}

func TestCommonControl_performRecreateUpgrade_PodTerminating(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	now := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-sandbox",
			Namespace:         "default",
			DeletionTimestamp: &metav1.Time{Time: now.Time},
			Finalizers:        []string{"fake"},
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-rev",
			},
		},
	}

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	podCtrl := NewPodControl(fakeClient, record.NewFakeRecorder(10), GeneratePodFromSandbox)
	checkpointCtrl := NewCheckpointControl(fakeClient, record.NewFakeRecorder(10))
	initializer := &defaultSandboxInitializer{recorder: record.NewFakeRecorder(10)}
	control := &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		podControl:           podCtrl,
		checkpointControl:    checkpointCtrl,
		initializer:          initializer,
		upgradeControl:       NewUpgradeControl(fakeClient, checkpointCtrl, podCtrl, record.NewFakeRecorder(10), NewLifecycleHookFunc(nil), initializer, defaultCommonSyncStatusFromPod, nil, nil),
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-rev",
	}

	done, err := control.upgradeControl.performRecreateUpgrade(context.TODO(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("performRecreateUpgrade() unexpected error: %v", err)
	}
	if done {
		t.Error("Expected done=false for terminating pod")
	}
}

func TestCommonControl_performRecreateUpgrade_NewPodNotReady(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "new-rev",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	podCtrl := NewPodControl(fakeClient, record.NewFakeRecorder(10), GeneratePodFromSandbox)
	checkpointCtrl := NewCheckpointControl(fakeClient, record.NewFakeRecorder(10))
	initializer := &defaultSandboxInitializer{recorder: record.NewFakeRecorder(10)}
	control := &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		podControl:           podCtrl,
		checkpointControl:    checkpointCtrl,
		initializer:          initializer,
		upgradeControl:       NewUpgradeControl(fakeClient, checkpointCtrl, podCtrl, record.NewFakeRecorder(10), NewLifecycleHookFunc(nil), initializer, defaultCommonSyncStatusFromPod, nil, nil),
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-rev",
	}

	done, err := control.upgradeControl.performRecreateUpgrade(context.TODO(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("performRecreateUpgrade() unexpected error: %v", err)
	}
	if done {
		t.Error("Expected done=false for pod not ready")
	}
}

// ---------------------------------------------------------------------------
// CA Certificate Injection Tests (driven by identity.CABundleSpec registry)
// ---------------------------------------------------------------------------

func TestPodControl_CreatePod_WithCAInjection(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name                    string
		featureGateEnabled      bool
		seedSourceSecret        bool
		withTrafficProxyRuntime bool
		expectCAVolume          bool
		expectCAMount           bool
		expectError             string
	}{
		{
			name:                    "feature gate enabled, traffic-proxy runtime, source secret present - CA injected",
			featureGateEnabled:      true,
			withTrafficProxyRuntime: true,
			seedSourceSecret:        true,
			expectCAVolume:          true,
			expectCAMount:           true,
		},
		{
			name:                    "feature gate disabled - no CA injection regardless of source",
			featureGateEnabled:      false,
			withTrafficProxyRuntime: true,
			seedSourceSecret:        false,
			expectCAVolume:          false,
			expectCAMount:           false,
		},
		{
			name:                    "feature gate enabled but no traffic-proxy runtime - no CA injection",
			featureGateEnabled:      true,
			withTrafficProxyRuntime: false,
			seedSourceSecret:        false,
			expectCAVolume:          false,
			expectCAMount:           false,
		},
		{
			name:                    "feature gate enabled, traffic-proxy runtime, source secret missing - block creation",
			featureGateEnabled:      true,
			withTrafficProxyRuntime: true,
			seedSourceSecret:        false,
			expectError:             "source CA secret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.featureGateEnabled {
				_ = utilfeature.DefaultMutableFeatureGate.Set("SecurityIdentityProvider=true")
				// Mirror the production binding in cmd/agent-sandbox-controller/ca_binding.go
				// so the gateway CA spec only applies to sandboxes that declare the
				// traffic-proxy runtime. This is the single place where runtime-level
				// gating lives now (callers no longer pre-check IsRuntimeEnabled).
				identity.BindCAEnabledFor(identity.GatewayCABundleName, func(sbx *agentsv1alpha1.Sandbox) bool {
					return sidecarutils.IsRuntimeEnabled(sbx, agentsv1alpha1.RuntimeConfigForInjectTrafficProxy)
				})
				t.Cleanup(func() {
					_ = utilfeature.DefaultMutableFeatureGate.Set("SecurityIdentityProvider=false")
					identity.BindCAEnabledFor(identity.GatewayCABundleName, nil)
				})
			}

			objs := []client.Object{}
			if tt.seedSourceSecret {
				objs = append(objs, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      identity.GatewayCASecretName,
						Namespace: utils.DefaultSandboxDeployNamespace,
					},
					Type: corev1.SecretTypeOpaque,
					Data: map[string][]byte{
						identity.GatewayCAKey: []byte("test-ca-bundle-content"),
					},
				})
			}
			// When the sandbox declares the traffic-proxy runtime, InjectSandboxRuntimes
			// will look up the corresponding template in the sandbox-injection ConfigMap.
			// Seed a minimal valid template so createPod's sidecar-injection step does
			// not fail and we can isolate CA injection behaviour under test.
			if tt.withTrafficProxyRuntime {
				objs = append(objs, &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      sidecarutils.SandboxInjectionConfigName,
						Namespace: utils.DefaultSandboxDeployNamespace,
					},
					Data: map[string]string{
						sidecarutils.KEY_TRAFFIC_PROXY_INJECTION_CONFIG: `{"mainContainer":{},"csiSidecar":[],"volume":[]}`,
					},
				})
			}

			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			control := NewPodControl(fakeClient, record.NewFakeRecorder(10), GeneratePodFromSandbox)

			sandbox := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx:latest"},
								},
							},
						},
					},
				},
			}
			if tt.withTrafficProxyRuntime {
				sandbox.Spec.Runtimes = []agentsv1alpha1.RuntimeConfig{
					{Name: agentsv1alpha1.RuntimeConfigForInjectTrafficProxy},
				}
			}

			status := &agentsv1alpha1.SandboxStatus{UpdateRevision: "rev1"}
			pod, err := control.CreatePod(context.TODO(), CreatePodArgs{Box: sandbox, NewStatus: status})

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)

			foundCAVolume := false
			for _, v := range pod.Spec.Volumes {
				if v.Name == "sandbox-gateway-ca" {
					foundCAVolume = true
					if v.Secret == nil || v.Secret.SecretName != identity.GatewayCASecretName {
						t.Errorf("CA volume has wrong secret source")
					}
					break
				}
			}
			if foundCAVolume != tt.expectCAVolume {
				t.Errorf("expected CA volume present=%v, got %v", tt.expectCAVolume, foundCAVolume)
			}

			foundCAMount := false
			if len(pod.Spec.Containers) > 0 {
				for _, vm := range pod.Spec.Containers[0].VolumeMounts {
					if vm.Name == "sandbox-gateway-ca" {
						foundCAMount = true
						if vm.MountPath != "/etc/ssl/certs/agent-identity/gateway-ca.crt" {
							t.Errorf("expected mount path '/etc/ssl/certs/agent-identity/gateway-ca.crt', got %q", vm.MountPath)
						}
						break
					}
				}
			}
			if foundCAMount != tt.expectCAMount {
				t.Errorf("expected CA mount present=%v, got %v", tt.expectCAMount, foundCAMount)
			}

			if tt.featureGateEnabled && tt.seedSourceSecret && tt.withTrafficProxyRuntime {
				var replicated corev1.Secret
				err = control.Get(context.TODO(), types.NamespacedName{
					Name:      identity.GatewayCASecretName,
					Namespace: "default",
				}, &replicated)
				if err != nil {
					t.Fatalf("expected gateway CA secret to be replicated to target ns, got error: %v", err)
				}
				if string(replicated.Data[identity.GatewayCAKey]) != "test-ca-bundle-content" {
					t.Errorf("expected replicated secret data to match source, got %q",
						string(replicated.Data[identity.GatewayCAKey]))
				}
			}
		})
	}
}

func TestCommonControl_performRecreateUpgrade_PodReadyFalse(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "new-rev",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
		},
	}

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	podCtrl := NewPodControl(fakeClient, record.NewFakeRecorder(10), GeneratePodFromSandbox)
	checkpointCtrl := NewCheckpointControl(fakeClient, record.NewFakeRecorder(10))
	initializer := &defaultSandboxInitializer{recorder: record.NewFakeRecorder(10)}
	control := &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		podControl:           podCtrl,
		checkpointControl:    checkpointCtrl,
		initializer:          initializer,
		upgradeControl:       NewUpgradeControl(fakeClient, checkpointCtrl, podCtrl, record.NewFakeRecorder(10), NewLifecycleHookFunc(nil), initializer, defaultCommonSyncStatusFromPod, nil, nil),
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-rev",
	}

	done, err := control.upgradeControl.performRecreateUpgrade(context.TODO(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("performRecreateUpgrade() unexpected error: %v", err)
	}
	if done {
		t.Error("Expected done=false for pod with Ready=False")
	}
}

func Test_isContainersConsistent(t *testing.T) {
	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
	}

	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected bool
	}{
		{
			name: "no init containers - consistent",
			pod: &corev1.Pod{
				Spec:   corev1.PodSpec{},
				Status: corev1.PodStatus{},
			},
			expected: true,
		},
		{
			name: "single init container - images match",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{Name: "init-a", Image: "busybox:1.0"},
					},
				},
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: "init-a", Image: "busybox:1.0"},
					},
				},
			},
			expected: true,
		},
		{
			name: "single init container - status not found",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{Name: "init-a", Image: "busybox:1.0"},
					},
				},
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{},
				},
			},
			expected: false,
		},
		{
			name: "single init container - image mismatch",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{Name: "init-a", Image: "busybox:2.0"},
					},
				},
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: "init-a", Image: "busybox:1.0"},
					},
				},
			},
			expected: false,
		},
		{
			name: "multiple init containers - all match",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{Name: "init-a", Image: "busybox:1.0"},
						{Name: "init-b", Image: "alpine:3.18"},
					},
				},
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: "init-a", Image: "busybox:1.0"},
						{Name: "init-b", Image: "alpine:3.18"},
					},
				},
			},
			expected: true,
		},
		{
			name: "multiple init containers - second mismatches",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{Name: "init-a", Image: "busybox:1.0"},
						{Name: "init-b", Image: "alpine:3.19"},
					},
				},
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: "init-a", Image: "busybox:1.0"},
						{Name: "init-b", Image: "alpine:3.18"},
					},
				},
			},
			expected: false,
		},
		{
			name: "multiple init containers - one status missing",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{Name: "init-a", Image: "busybox:1.0"},
						{Name: "init-b", Image: "alpine:3.18"},
					},
				},
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: "init-a", Image: "busybox:1.0"},
					},
				},
			},
			expected: false,
		},
		{
			name: "short name in spec vs normalized name in status - should match",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{Name: "runtime", Image: "agent-runtime:latest"},
					},
				},
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: "runtime", Image: "docker.io/library/agent-runtime:latest"},
					},
				},
			},
			expected: true,
		},
		{
			name: "implicit latest tag in spec vs explicit in status - should match",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{Name: "runtime", Image: "agent-runtime"},
					},
				},
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: "runtime", Image: "docker.io/library/agent-runtime:latest"},
					},
				},
			},
			expected: true,
		},
		{
			name: "fully qualified registry image - different tags still mismatch",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{Name: "runtime", Image: "registry.example.com/org/runtime:v1"},
					},
				},
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: "runtime", Image: "registry.example.com/org/runtime:v2"},
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isContainersConsistent(context.Background(), tt.pod, box)
			if result != tt.expected {
				t.Errorf("isContainersConsistent() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

func TestCommonControl_EnsureSandboxResumed_InitContainerInconsistent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	now := metav1.Now()

	tests := []struct {
		name          string
		pod           *corev1.Pod
		expectPhase   agentsv1alpha1.SandboxPhase
		expectInitCon bool // expect RuntimeInitialized condition to be set
	}{
		{
			name: "init container image mismatch - should wait and not transition",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					UID:       "pod-uid",
				},
				Spec: corev1.PodSpec{
					NodeName: "node-1",
					InitContainers: []corev1.Container{
						{Name: "init-a", Image: "busybox:2.0"},
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					PodIP: "10.0.0.1",
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
					},
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: "init-a", Image: "busybox:1.0"},
					},
				},
			},
			expectPhase:   agentsv1alpha1.SandboxResuming,
			expectInitCon: false,
		},
		{
			name: "init container status missing - should wait and not transition",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					UID:       "pod-uid",
				},
				Spec: corev1.PodSpec{
					NodeName: "node-1",
					InitContainers: []corev1.Container{
						{Name: "init-a", Image: "busybox:1.0"},
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					PodIP: "10.0.0.1",
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
					},
					InitContainerStatuses: []corev1.ContainerStatus{},
				},
			},
			expectPhase:   agentsv1alpha1.SandboxResuming,
			expectInitCon: false,
		},
		{
			name: "init containers consistent - should proceed to initialize",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					UID:       "pod-uid",
				},
				Spec: corev1.PodSpec{
					NodeName: "node-1",
					InitContainers: []corev1.Container{
						{Name: "init-a", Image: "busybox:1.0"},
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					PodIP: "10.0.0.1",
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
					},
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: "init-a", Image: "busybox:1.0"},
					},
				},
			},
			expectPhase:   agentsv1alpha1.SandboxRunning,
			expectInitCon: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := fake.NewClientBuilder().WithScheme(scheme).Build()
			control := &commonControl{
				Client:               fc,
				recorder:             record.NewFakeRecorder(10),
				inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fc, inplaceupdate.DefaultGeneratePatchBodyFunc),
				initializer:          &mockSandboxInitializer{err: nil},
				podControl:           NewPodControl(fc, record.NewFakeRecorder(10), GeneratePodFromSandbox),
				checkpointControl:    NewCheckpointControl(fc, record.NewFakeRecorder(10)),
				syncStatusFromPod:    defaultCommonSyncStatusFromPod,
			}

			newStatus := &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxResuming,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReady),
						Status:             metav1.ConditionFalse,
						LastTransitionTime: now,
						Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
					},
					{
						Type:               string(agentsv1alpha1.SandboxConditionResumed),
						Status:             metav1.ConditionFalse,
						LastTransitionTime: now,
						Reason:             agentsv1alpha1.SandboxResumeReasonCreatePod,
					},
				},
			}

			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
			}

			err := control.EnsureSandboxResumed(context.TODO(), EnsureFuncArgs{Pod: tt.pod, Box: box, NewStatus: newStatus})
			if err != nil {
				t.Fatalf("EnsureSandboxResumed() unexpected error: %v", err)
			}

			if newStatus.Phase != tt.expectPhase {
				t.Errorf("expected phase %s, got %s", tt.expectPhase, newStatus.Phase)
			}

			// When containers are inconsistent, no side effects should occur
			if tt.expectPhase == agentsv1alpha1.SandboxResuming {
				assert.Empty(t, newStatus.NodeName, "NodeName should not be set when containers are inconsistent")
				assert.Empty(t, newStatus.SandboxIp, "SandboxIp should not be set when containers are inconsistent")
				assert.Equal(t, agentsv1alpha1.PodInfo{}, newStatus.PodInfo, "PodInfo should not be set when containers are inconsistent")
			}

			initCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.RuntimeInitialized))
			if tt.expectInitCon {
				if initCond == nil {
					t.Error("expected RuntimeInitialized condition to be set, but it was not")
				}
			} else {
				if initCond != nil {
					t.Error("expected RuntimeInitialized condition to NOT be set, but it was")
				}
			}
		})
	}
}

// mockSandboxInitializer is a test double for SandboxInitializer.
type mockSandboxInitializer struct {
	err               error
	called            int
	initializedStatus *agentsv1alpha1.SandboxStatus
}

func (m *mockSandboxInitializer) Initialize(_ context.Context, _ *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus) error {
	m.called++
	m.initializedStatus = newStatus.DeepCopy()
	return m.err
}

func TestCommonControl_performRecreateUpgrade_InitializerPath(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	// The recreate path requires PodReady before initialization, not just a Running container.
	readyPod := func() *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-sandbox",
				Namespace: "default",
				Labels: map[string]string{
					agentsv1alpha1.PodLabelTemplateHash: "new-rev",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "10.0.0.1",
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				},
				ContainerStatuses: []corev1.ContainerStatus{
					{Name: "test", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
				},
			},
			Spec: corev1.PodSpec{
				NodeName:   "node-1",
				Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
			},
		}
	}

	baseSandbox := func() *agentsv1alpha1.Sandbox {
		return &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-sandbox",
				Namespace: "default",
			},
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
						},
					},
				},
			},
		}
	}

	tests := []struct {
		name        string
		initErr     error
		expectError string
		expectDone  bool
		notReady    bool
	}{
		{
			name: "container running but pod not ready waits without initialization", notReady: true,
		},
		{
			name:        "initializer succeeds, upgrade completes",
			initErr:     nil,
			expectError: "",
			expectDone:  true,
		},
		{
			name:        "initializer fails, returns error",
			initErr:     fmt.Errorf("failed to perform ReCSIMount after resume"),
			expectError: "failed to perform ReCSIMount after resume",
			expectDone:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			podCtrl := NewPodControl(fakeClient, record.NewFakeRecorder(10), GeneratePodFromSandbox)
			checkpointCtrl := NewCheckpointControl(fakeClient, record.NewFakeRecorder(10))
			initializer := &mockSandboxInitializer{err: tt.initErr}
			control := &commonControl{
				Client:               fakeClient,
				recorder:             record.NewFakeRecorder(10),
				inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
				podControl:           podCtrl,
				checkpointControl:    checkpointCtrl,
				initializer:          initializer,
				upgradeControl:       NewUpgradeControl(fakeClient, checkpointCtrl, podCtrl, record.NewFakeRecorder(10), NewLifecycleHookFunc(nil), initializer, defaultCommonSyncStatusFromPod, nil, nil),
			}

			newStatus := &agentsv1alpha1.SandboxStatus{
				UpdateRevision: "new-rev",
			}

			pod := readyPod()
			if tt.notReady {
				pod.Status.Conditions[0].Status = corev1.ConditionFalse
			}
			done, err := control.upgradeControl.performRecreateUpgrade(context.TODO(), EnsureFuncArgs{
				Pod:       pod,
				Box:       baseSandbox(),
				NewStatus: newStatus,
			})

			if tt.expectError == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}

			if done != tt.expectDone {
				t.Errorf("expected done=%v, got done=%v", tt.expectDone, done)
			}
			if tt.notReady {
				require.Zero(t, initializer.called)
			} else {
				require.Equal(t, 1, initializer.called)
			}
		})
	}
}

func TestCommonControlEnsureSandboxUpdatedPostResumeGatesReadyUntilTerminalResult(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	calls := 0
	hooks := NewLifecycleHookManager(1, func(context.Context, *agentsv1alpha1.Sandbox, *agentsv1alpha1.LifecycleAction) (int32, string, string, error) {
		calls++
		return 0, "", "", nil
	}, record.NewFakeRecorder(10))
	control := &commonControl{
		Client:               fc,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fc, inplaceupdate.DefaultGeneratePatchBodyFunc),
		probeManager:         NewPodProbeManager(fc, record.NewFakeRecorder(10)),
		lifecycleHooks:       hooks,
		syncStatusFromPod:    defaultCommonSyncStatusFromPod,
	}
	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default", UID: types.UID("sandbox-uid"), Generation: 2},
		Spec: agentsv1alpha1.SandboxSpec{
			Lifecycle:     &agentsv1alpha1.SandboxLifecycle{PostResume: &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"restore"}}}},
			UpgradePolicy: &agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyRecreate},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: box.Name, Namespace: box.Namespace, UID: types.UID("pod-uid")},
		Spec:       corev1.PodSpec{NodeName: "node"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1", Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now(),
		}}},
	}
	status := &agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning, Conditions: []metav1.Condition{
		{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: agentsv1alpha1.SandboxReadyReasonPodReady},
		{Type: string(agentsv1alpha1.RuntimeInitialized), Status: metav1.ConditionTrue, Reason: agentsv1alpha1.SandboxConditionRuntimeInitReasonSucceeded},
	}}
	markResumeSucceeded(box, status)
	// Runtime initialization already completed before this test exercises the
	// hook gating; markResumeSucceeded resets it to Pending, so restore the
	// completed state the gating path expects.
	utils.SetSandboxCondition(status, metav1.Condition{
		Type:   string(agentsv1alpha1.RuntimeInitialized),
		Status: metav1.ConditionTrue,
		Reason: agentsv1alpha1.SandboxConditionRuntimeInitReasonSucceeded,
	})

	// One reconcile end to end: the hook executes, reaches its terminal
	// result, and Ready publishes in the same pass — the fastest resume path.
	_, err := control.EnsureSandboxUpdated(t.Context(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: status})
	require.NoError(t, err)
	assert.Equal(t, agentsv1alpha1.SandboxLifecycleHookReasonSucceeded,
		hookCondition(t, status, agentsv1alpha1.SandboxConditionPostResumeHook).Reason)
	assert.Equal(t, metav1.ConditionTrue, hookCondition(t, status, agentsv1alpha1.SandboxConditionReady).Status)
	assert.Equal(t, 1, calls)

	// A later reconcile observes the terminal condition and runs nothing.
	_, err = control.EnsureSandboxUpdated(t.Context(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: status})
	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionTrue, hookCondition(t, status, agentsv1alpha1.SandboxConditionReady).Status)
	assert.Equal(t, 1, calls)
}

func TestCommonControlEnsureSandboxUpdatedDoesNotRunPostResumeOnInitialCreate(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node"},
	}).Build()
	calls := 0
	hooks := NewLifecycleHookManager(1, func(context.Context, *agentsv1alpha1.Sandbox, *agentsv1alpha1.LifecycleAction) (int32, string, string, error) {
		calls++
		return 0, "", "", nil
	}, nil)
	control := &commonControl{
		Client:               fc,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fc, inplaceupdate.DefaultGeneratePatchBodyFunc),
		probeManager:         NewPodProbeManager(fc, record.NewFakeRecorder(10)),
		lifecycleHooks:       hooks,
		syncStatusFromPod:    defaultCommonSyncStatusFromPod,
	}
	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default", UID: types.UID("sandbox-uid")},
		Spec: agentsv1alpha1.SandboxSpec{
			Lifecycle:     &agentsv1alpha1.SandboxLifecycle{PostResume: &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"restore"}}}},
			UpgradePolicy: &agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyRecreate},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: box.Name, Namespace: box.Namespace, UID: types.UID("pod-uid")},
		Spec:       corev1.PodSpec{NodeName: "node"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue,
		}}},
	}
	status := &agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning, Conditions: []metav1.Condition{
		{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse},
		{Type: string(agentsv1alpha1.RuntimeInitialized), Status: metav1.ConditionTrue, Reason: agentsv1alpha1.SandboxConditionRuntimeInitReasonSucceeded},
	}}

	_, err := control.EnsureSandboxUpdated(t.Context(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: status})
	require.NoError(t, err)
	assert.Zero(t, calls)
	assert.Nil(t, utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionPostResumeHook)))
	assert.Equal(t, metav1.ConditionTrue, hookCondition(t, status, agentsv1alpha1.SandboxConditionReady).Status)
}

func TestCommonControlEnsureSandboxUpdatedPostResumeFailurePassesThrough(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node"},
	}).Build()
	hooks := NewLifecycleHookManager(1, func(context.Context, *agentsv1alpha1.Sandbox, *agentsv1alpha1.LifecycleAction) (int32, string, string, error) {
		return 9, "sensitive stdout", "sensitive stderr", nil
	}, nil)
	control := &commonControl{
		Client:               fc,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fc, inplaceupdate.DefaultGeneratePatchBodyFunc),
		probeManager:         NewPodProbeManager(fc, record.NewFakeRecorder(10)),
		lifecycleHooks:       hooks,
		syncStatusFromPod:    defaultCommonSyncStatusFromPod,
	}
	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default", UID: types.UID("sandbox-uid")},
		Spec: agentsv1alpha1.SandboxSpec{
			Lifecycle:     &agentsv1alpha1.SandboxLifecycle{PostResume: &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"restore"}}}},
			UpgradePolicy: &agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyRecreate},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: box.Name, Namespace: box.Namespace, UID: types.UID("pod-uid")},
		Spec:       corev1.PodSpec{NodeName: "node"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue,
		}}},
	}
	status := &agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning, Conditions: []metav1.Condition{
		{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse},
		{Type: string(agentsv1alpha1.RuntimeInitialized), Status: metav1.ConditionTrue, Reason: agentsv1alpha1.SandboxConditionRuntimeInitReasonSucceeded},
	}}
	markResumeSucceeded(box, status)
	// Runtime initialization already completed before this test exercises the
	// hook gating; markResumeSucceeded resets it to Pending, so restore the
	// completed state the gating path expects.
	utils.SetSandboxCondition(status, metav1.Condition{
		Type:   string(agentsv1alpha1.RuntimeInitialized),
		Status: metav1.ConditionTrue,
		Reason: agentsv1alpha1.SandboxConditionRuntimeInitReasonSucceeded,
	})
	args := EnsureFuncArgs{Pod: pod, Box: box, NewStatus: status}

	// One reconcile end to end: the hook fails terminally (no retries) and
	// Ready still publishes in the same pass — failure passes through.
	_, err := control.EnsureSandboxUpdated(t.Context(), args)
	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionTrue,
		hookCondition(t, status, agentsv1alpha1.SandboxConditionReady).Status)
	cond := hookCondition(t, status, agentsv1alpha1.SandboxConditionPostResumeHook)
	assert.Equal(t, agentsv1alpha1.SandboxLifecycleHookReasonFailed, cond.Reason)
	assert.NotContains(t, cond.Message, "sensitive")

	// A later reconcile observes the terminal condition and runs nothing.
	_, err2 := control.EnsureSandboxUpdated(t.Context(), args)
	require.NoError(t, err2)
	assert.Equal(t, metav1.ConditionTrue,
		hookCondition(t, status, agentsv1alpha1.SandboxConditionReady).Status)
	assert.Equal(t, agentsv1alpha1.SandboxLifecycleHookReasonFailed,
		hookCondition(t, status, agentsv1alpha1.SandboxConditionPostResumeHook).Reason)
}

func TestMarkResumeSucceededSeedsPostResumeMarker(t *testing.T) {
	tests := []struct {
		name         string
		postResume   bool
		priorReason  string
		expectReason string
		expectAbsent bool
	}{
		{
			name:         "configured hook seeds pending marker",
			postResume:   true,
			expectReason: agentsv1alpha1.SandboxLifecycleHookReasonPending,
		},
		{
			name:         "not configured leaves no marker",
			expectAbsent: true,
		},
		{
			name:         "prior terminal result is replaced on the new cycle",
			postResume:   true,
			priorReason:  agentsv1alpha1.SandboxLifecycleHookReasonSucceeded,
			expectReason: agentsv1alpha1.SandboxLifecycleHookReasonPending,
		},
		{
			name:         "stale marker from a removed hook is cleared",
			priorReason:  agentsv1alpha1.SandboxLifecycleHookReasonFailed,
			expectAbsent: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default", Generation: 3},
			}
			if tt.postResume {
				box.Spec.Lifecycle = &agentsv1alpha1.SandboxLifecycle{
					PostResume: &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"restore"}}},
				}
			}
			status := &agentsv1alpha1.SandboxStatus{}
			if tt.priorReason != "" {
				conditionStatus := metav1.ConditionFalse
				if tt.priorReason == agentsv1alpha1.SandboxLifecycleHookReasonSucceeded {
					conditionStatus = metav1.ConditionTrue
				}
				utils.SetSandboxCondition(status, metav1.Condition{
					Type:   string(agentsv1alpha1.SandboxConditionPostResumeHook),
					Status: conditionStatus,
					Reason: tt.priorReason,
				})
			}

			markResumeSucceeded(box, status)

			assert.Equal(t, metav1.ConditionTrue,
				utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionResumed)).Status)
			assert.Equal(t, metav1.ConditionFalse,
				utils.GetSandboxCondition(status, string(agentsv1alpha1.RuntimeInitialized)).Status)
			marker := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionPostResumeHook))
			if tt.expectAbsent {
				assert.Nil(t, marker)
			} else {
				require.NotNil(t, marker)
				assert.Equal(t, tt.expectReason, marker.Reason)
				assert.Equal(t, metav1.ConditionFalse, marker.Status)
				assert.Equal(t, box.Generation, marker.ObservedGeneration)
			}
		})
	}
}

func TestCommonControlEnsureSandboxUpgradedClearsPostResumeMarkerOnSuccess(t *testing.T) {
	// The Succeeded reason falls through the upgrade control's dispatch
	// without touching its dependencies, so a bare UpgradeControl suffices.
	control := &commonControl{
		lifecycleHooks: NewLifecycleHookManager(1, nil, nil),
		upgradeControl: &UpgradeControl{},
	}

	tests := []struct {
		name         string
		upgradeState string
		markerReason string
		expectAbsent bool
	}{
		{
			name:         "successful upgrade clears active marker",
			upgradeState: agentsv1alpha1.SandboxUpgradingReasonSucceeded,
			markerReason: agentsv1alpha1.SandboxLifecycleHookReasonPending,
			expectAbsent: true,
		},
		{
			name:         "successful upgrade retains terminal marker",
			upgradeState: agentsv1alpha1.SandboxUpgradingReasonSucceeded,
			markerReason: agentsv1alpha1.SandboxLifecycleHookReasonSucceeded,
		},
		{
			// ResumeSucceed with the trigger annotation removed abandons the
			// upgrade back to Running: the marker must survive so the normal
			// Running path executes the hook.
			name:         "abandoned upgrade keeps active marker",
			upgradeState: agentsv1alpha1.SandboxUpgradingReasonResumeSucceed,
			markerReason: agentsv1alpha1.SandboxLifecycleHookReasonPending,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default"},
				Spec: agentsv1alpha1.SandboxSpec{Lifecycle: &agentsv1alpha1.SandboxLifecycle{
					PostResume: &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"restore"}}},
				}},
			}
			status := &agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxUpgrading}
			utils.SetSandboxCondition(status, metav1.Condition{
				Type:   string(agentsv1alpha1.SandboxConditionUpgrading),
				Status: metav1.ConditionTrue,
				Reason: tt.upgradeState,
			})
			markerStatus := metav1.ConditionFalse
			if tt.markerReason == agentsv1alpha1.SandboxLifecycleHookReasonSucceeded {
				markerStatus = metav1.ConditionTrue
			}
			utils.SetSandboxCondition(status, metav1.Condition{
				Type:   string(agentsv1alpha1.SandboxConditionPostResumeHook),
				Status: markerStatus,
				Reason: tt.markerReason,
			})

			// The final upgrade reconcile is the one that sets Succeeded and
			// phase=Running; replay that terminal state through the wrapper.
			if tt.upgradeState == agentsv1alpha1.SandboxUpgradingReasonSucceeded {
				status.Phase = agentsv1alpha1.SandboxRunning
			}
			require.NoError(t, control.EnsureSandboxUpgraded(t.Context(), EnsureFuncArgs{Box: box, NewStatus: status}))

			marker := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionPostResumeHook))
			if tt.expectAbsent {
				assert.Nil(t, marker)
			} else {
				require.NotNil(t, marker)
				assert.Equal(t, tt.markerReason, marker.Reason)
			}
		})
	}
}
