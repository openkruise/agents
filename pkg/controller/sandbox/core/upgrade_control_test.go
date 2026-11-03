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
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/utils"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
	"github.com/openkruise/agents/pkg/utils/fieldindex"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
)

// SandboxUpgradePolicyInplaceUpdate is a test-only constant for InplaceUpdate upgrade policy type.
const SandboxUpgradePolicyInplaceUpdate agentsv1alpha1.SandboxUpgradePolicyType = "InplaceUpdate"

// mockLifecycleHookFunc creates a mock LifecycleHookFunc for testing.
func mockLifecycleHookFunc(exitCode int32, stdout, stderr string, err error) LifecycleHookFunc {
	return func(ctx context.Context, box *agentsv1alpha1.Sandbox, action *agentsv1alpha1.UpgradeAction) (int32, string, string, error) {
		return exitCode, stdout, stderr, err
	}
}

func newUpgradeTestSandbox(lifecycle *agentsv1alpha1.SandboxLifecycle, upgradePolicy *agentsv1alpha1.SandboxUpgradePolicy) *agentsv1alpha1.Sandbox {
	// Default to Recreate policy if nil for backward compatibility in tests
	if upgradePolicy == nil {
		upgradePolicy = &agentsv1alpha1.SandboxUpgradePolicy{
			Type: agentsv1alpha1.SandboxUpgradePolicyRecreate,
		}
	}
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationRuntimeURL:         "http://10.0.0.1:49983",
				agentsv1alpha1.AnnotationRuntimeAccessToken: "test-token",
				agentsv1alpha1.SandboxHashImmutablePart:     "old-hash",
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			Lifecycle:     lifecycle,
			UpgradePolicy: upgradePolicy,
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "sandbox", Image: "test:v2"},
						},
					},
				},
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase:     agentsv1alpha1.SandboxUpgrading,
			SandboxIp: "10.0.0.1",
		},
	}
}

func newRunningPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-revision",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "sandbox", Image: "test:v1"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.0.0.1",
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func newTestCommonControl(hookFunc LifecycleHookFunc, objects ...client.Object) *commonControl {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	checkpointCtrl := NewCheckpointControl(fakeClient, record.NewFakeRecorder(100))
	podCtrl := NewPodControl(fakeClient, record.NewFakeRecorder(100), GeneratePodFromSandbox)
	initializer := &defaultSandboxInitializer{recorder: record.NewFakeRecorder(10)}
	return &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(100),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		rateLimiter:          NewRateLimiter(),
		checkpointControl:    checkpointCtrl,
		podControl:           podCtrl,
		lifecycleHookFunc:    hookFunc,
		initializer:          initializer,
		upgradeControl:       NewUpgradeControl(fakeClient, checkpointCtrl, podCtrl, record.NewFakeRecorder(100), hookFunc, initializer, defaultCommonSyncStatusFromPod, nil),
	}
}

func TestExecuteUpgradeAction(t *testing.T) {
	action := &agentsv1alpha1.UpgradeAction{
		Exec:           &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "echo test"}},
		TimeoutSeconds: 30,
	}
	pod := newRunningPod()
	box := newUpgradeTestSandbox(&agentsv1alpha1.SandboxLifecycle{PreUpgrade: action}, nil)

	tests := []struct {
		name           string
		hookFunc       LifecycleHookFunc
		expectSuccess  bool
		expectContains string
	}{
		{
			name:           "error with stderr included in message",
			hookFunc:       mockLifecycleHookFunc(-1, "", "permission denied", fmt.Errorf("connection refused")),
			expectSuccess:  false,
			expectContains: "permission denied",
		},
		{
			name:           "error with stdout when stderr is empty",
			hookFunc:       mockLifecycleHookFunc(-1, "partial output", "", fmt.Errorf("timeout")),
			expectSuccess:  false,
			expectContains: "partial output",
		},
		{
			name:           "non-zero exit code with stderr included",
			hookFunc:       mockLifecycleHookFunc(1, "", "command not found", nil),
			expectSuccess:  false,
			expectContains: "command not found",
		},
		{
			name:           "message truncated when exceeding max length",
			hookFunc:       mockLifecycleHookFunc(-1, "", strings.Repeat("x", 1100), fmt.Errorf("exec failed")),
			expectSuccess:  false,
			expectContains: "...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := newTestCommonControl(tt.hookFunc, box.DeepCopy(), pod.DeepCopy())
			result := ctrl.upgradeControl.executeUpgradeAction(context.Background(), pod, box, action)
			assert.Equal(t, tt.expectSuccess, result.Succeeded)
			assert.Contains(t, result.Message, tt.expectContains)
			// Verify truncation: message must not exceed MaxConditionMessageLen + len("...")
			assert.LessOrEqual(t, len(result.Message), utils.MaxConditionMessageLen+3)
		})
	}
}

func TestEnsureSandboxUpgraded(t *testing.T) {
	preUpgradeHook := &agentsv1alpha1.UpgradeAction{
		Exec:           &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "echo backup"}},
		TimeoutSeconds: 30,
	}
	postUpgradeHook := &agentsv1alpha1.UpgradeAction{
		Exec:           &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "echo restore"}},
		TimeoutSeconds: 30,
	}
	now := metav1.Now()

	tests := []struct {
		name            string
		pod             *corev1.Pod
		box             *agentsv1alpha1.Sandbox
		existingStatus  *agentsv1alpha1.SandboxStatus
		mockHookFunc    LifecycleHookFunc
		expectErr       bool
		expectPhase     agentsv1alpha1.SandboxPhase
		expectCondition map[string]metav1.ConditionStatus
	}{
		{
			name: "no lifecycle configured skips preUpgrade and proceeds to Phase 2",
			pod:  newRunningPod(),
			box:  newUpgradeTestSandbox(nil, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
			},
			mockHookFunc:    mockLifecycleHookFunc(0, "", "", nil),
			expectErr:       false,
			expectPhase:     agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{},
		},
		{
			name: "preUpgrade hook succeeds",
			pod:  newRunningPod(),
			box: newUpgradeTestSandbox(&agentsv1alpha1.SandboxLifecycle{
				PreUpgrade: preUpgradeHook,
			}, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
			},
			mockHookFunc: mockLifecycleHookFunc(0, "ok", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
				string(agentsv1alpha1.SandboxConditionReady):     metav1.ConditionFalse,
			},
		},
		{
			name: "preUpgrade hook fails with non-zero exit code",
			pod:  newRunningPod(),
			box: newUpgradeTestSandbox(&agentsv1alpha1.SandboxLifecycle{
				PreUpgrade: preUpgradeHook,
			}, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
			},
			mockHookFunc: mockLifecycleHookFunc(1, "", "error occurred", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionReady):     metav1.ConditionFalse,
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
			},
		},
		{
			name: "preUpgrade hook fails with executor error",
			pod:  newRunningPod(),
			box: newUpgradeTestSandbox(&agentsv1alpha1.SandboxLifecycle{
				PreUpgrade: preUpgradeHook,
			}, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
			},
			mockHookFunc: mockLifecycleHookFunc(-1, "", "", fmt.Errorf("connection refused")),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionReady):     metav1.ConditionFalse,
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
			},
		},
		{
			name: "preUpgrade hook fails when pod is nil",
			pod:  nil,
			box: newUpgradeTestSandbox(&agentsv1alpha1.SandboxLifecycle{
				PreUpgrade: preUpgradeHook,
			}, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionReady):     metav1.ConditionFalse,
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
			},
		},
		{
			name: "preUpgrade failed retries and fails again",
			pod:  newRunningPod(),
			box: newUpgradeTestSandbox(&agentsv1alpha1.SandboxLifecycle{
				PreUpgrade: preUpgradeHook,
			}, nil),
			// After a preUpgrade failure, the sandbox stays in Upgrading phase.
			// On re-trigger the controller re-enters with Phase=Upgrading and no Upgrading condition.
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
			},
			// Mock still returns failure so the retry also fails
			mockHookFunc: mockLifecycleHookFunc(1, "", "still failing", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionReady):     metav1.ConditionFalse,
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
			},
		},
		{
			name: "delete pod after preUpgrade succeeded (Phase 2)",
			pod:  newRunningPod(),
			box:  newUpgradeTestSandbox(nil, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
						LastTransitionTime: now,
					},
				},
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
			},
		},
		{
			name: "wait for pod deletion when pod is terminating",
			pod: func() *corev1.Pod {
				p := newRunningPod()
				p.DeletionTimestamp = &metav1.Time{Time: now.Time}
				p.Finalizers = []string{"fake-finalizer"}
				return p
			}(),
			box: newUpgradeTestSandbox(nil, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
						LastTransitionTime: now,
					},
				},
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
			},
		},
		{
			name: "create new pod when old pod deleted",
			pod:  nil,
			box:  newUpgradeTestSandbox(nil, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
						LastTransitionTime: now,
					},
				},
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
			},
		},
		{
			name: "wait for new pod to be ready before postUpgrade",
			pod: func() *corev1.Pod {
				p := newRunningPod()
				p.Status.Phase = corev1.PodPending
				p.Status.Conditions = nil
				return p
			}(),
			box: newUpgradeTestSandbox(nil, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
						LastTransitionTime: now,
					},
				},
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
			},
		},
		{
			name: "upgrade completed cleans up conditions (pod nil)",
			pod:  nil,
			box:  newUpgradeTestSandbox(nil, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionTrue,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
						Message:            "upgrade completed",
						LastTransitionTime: now,
					},
				},
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionTrue,
				string(agentsv1alpha1.SandboxConditionReady):     metav1.ConditionFalse,
			},
		},
		{
			name: "postUpgrade failed blocks upgrade",
			pod:  newRunningPod(),
			box: newUpgradeTestSandbox(&agentsv1alpha1.SandboxLifecycle{
				PostUpgrade: postUpgradeHook,
			}, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonPostUpgradeFailed,
						Message:            "postUpgrade hook failed",
						LastTransitionTime: now,
					},
				},
			},
			// PostUpgrade still fails on retry
			mockHookFunc: mockLifecycleHookFunc(1, "", "still failing", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
			},
		},
		{
			name: "postUpgrade succeeded with pod present transitions to Running",
			pod:  newRunningPod(),
			box:  newUpgradeTestSandbox(nil, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionTrue,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
						Message:            "upgrade completed",
						LastTransitionTime: now,
					},
				},
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionReady):     metav1.ConditionFalse,
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionTrue,
			},
		},
		{
			name: "upgrade completed cleans up conditions (with pod present for pod info)",
			pod:  nil,
			box:  newUpgradeTestSandbox(nil, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionTrue,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
						Message:            "upgrade completed",
						LastTransitionTime: now,
					},
					{
						Type:               string(agentsv1alpha1.SandboxConditionReady),
						Status:             metav1.ConditionFalse,
						Reason:             "Upgrading",
						LastTransitionTime: now,
					},
				},
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionTrue,
				string(agentsv1alpha1.SandboxConditionReady):     metav1.ConditionFalse,
			},
		},
		{
			name: "new pod with matching revision completes upgrade without postUpgrade",
			pod: func() *corev1.Pod {
				p := newRunningPod()
				p.Labels[agentsv1alpha1.PodLabelTemplateHash] = "new-revision"
				return p
			}(),
			box: newUpgradeTestSandbox(nil, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:          agentsv1alpha1.SandboxUpgrading,
				UpdateRevision: "new-revision",
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionTrue,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
						Message:            "upgrade completed",
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxRunning,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionReady): metav1.ConditionTrue,
			},
		},
		{
			name: "Recreate upgrade without lifecycle should still recreate pod",
			pod: func() *corev1.Pod {
				p := newRunningPod()
				p.Labels[agentsv1alpha1.PodLabelTemplateHash] = "old-revision"
				return p
			}(),
			box: newUpgradeTestSandbox(nil, &agentsv1alpha1.SandboxUpgradePolicy{
				Type: agentsv1alpha1.SandboxUpgradePolicyRecreate,
			}),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:          agentsv1alpha1.SandboxUpgrading,
				UpdateRevision: "new-revision",
			},
			mockHookFunc:    mockLifecycleHookFunc(0, "", "", nil),
			expectErr:       false,
			expectPhase:     agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{},
		},
		{
			name: "old pod with mismatching revision should be deleted in phase 2",
			pod: func() *corev1.Pod {
				p := newRunningPod()
				p.Labels[agentsv1alpha1.PodLabelTemplateHash] = "old-revision"
				return p
			}(),
			box: newUpgradeTestSandbox(nil, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:          agentsv1alpha1.SandboxUpgrading,
				UpdateRevision: "new-revision",
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
						LastTransitionTime: now,
					},
				},
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
			},
		},
		{
			name: "ResumeSucceed with annotation removed and revision unchanged -> abandon to Running",
			pod:  newRunningPod(),
			box: func() *agentsv1alpha1.Sandbox {
				b := newUpgradeTestSandbox(nil, nil)
				b.Status.UpdateRevision = "same-revision"
				return b
			}(),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:          agentsv1alpha1.SandboxUpgrading,
				UpdateRevision: "same-revision",
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonResumeSucceed,
						LastTransitionTime: now,
					},
				},
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxRunning,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionReady): metav1.ConditionFalse,
			},
		},
		{
			name: "ResumeSucceed with annotation present stays Upgrading",
			pod:  newRunningPod(),
			box: func() *agentsv1alpha1.Sandbox {
				b := newUpgradeTestSandbox(nil, nil)
				b.Status.UpdateRevision = "same-revision"
				b.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger] = agentsv1alpha1.True
				return b
			}(),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:          agentsv1alpha1.SandboxUpgrading,
				UpdateRevision: "same-revision",
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonResumeSucceed,
						LastTransitionTime: now,
					},
				},
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
				string(agentsv1alpha1.SandboxConditionReady):     metav1.ConditionFalse,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build objects for fake client
			var objects []client.Object
			if tt.pod != nil {
				objects = append(objects, tt.pod.DeepCopy())
			}

			control := newTestCommonControl(tt.mockHookFunc, objects...)

			// Prepare newStatus from existingStatus
			newStatus := tt.existingStatus.DeepCopy()

			args := EnsureFuncArgs{
				Pod:       tt.pod,
				Box:       tt.box,
				NewStatus: newStatus,
			}

			err := control.EnsureSandboxUpgraded(context.TODO(), args)

			// Check error
			if (err != nil) != tt.expectErr {
				t.Errorf("EnsureSandboxUpgraded() error = %v, wantErr %v", err, tt.expectErr)
				return
			}

			// Check phase
			if tt.expectPhase != "" && newStatus.Phase != tt.expectPhase {
				t.Errorf("Expected phase %q, got %q", tt.expectPhase, newStatus.Phase)
			}

			// Check conditions
			for condType, expectedStatus := range tt.expectCondition {
				cond := utils.GetSandboxCondition(newStatus, condType)
				if cond == nil {
					t.Errorf("Expected condition %q to exist, but it was not found", condType)
					continue
				}
				if cond.Status != expectedStatus {
					t.Errorf("Expected condition %q status to be %q, got %q (reason: %s, message: %s)",
						condType, expectedStatus, cond.Status, cond.Reason, cond.Message)
				}
			}

			// For upgrade in-progress tests (pod nil with UpgradePod reason), verify Upgrading condition is preserved
			if tt.name == "upgrade completed cleans up conditions (pod nil)" ||
				tt.name == "upgrade completed cleans up conditions (with pod present for pod info)" {
				upgradingCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
				if upgradingCond == nil {
					t.Errorf("Expected Upgrading condition to exist during in-progress upgrade, but it was removed")
				}
			}
		})
	}
}

func TestEnsureInplaceUpgrade(t *testing.T) {
	// Build a sandbox with correct immutable hash for inplace update tests
	newInplaceSandbox := func() *agentsv1alpha1.Sandbox {
		box := newUpgradeTestSandbox(nil, &agentsv1alpha1.SandboxUpgradePolicy{
			Type: SandboxUpgradePolicyInplaceUpdate,
		})
		// Compute and set the correct immutable hash so inplace update logic proceeds
		_, hashImmutablePart := HashSandbox(box)
		box.Annotations[agentsv1alpha1.SandboxHashImmutablePart] = hashImmutablePart
		return box
	}

	tests := []struct {
		name            string
		pod             *corev1.Pod
		box             *agentsv1alpha1.Sandbox
		existingStatus  *agentsv1alpha1.SandboxStatus
		mockHookFunc    LifecycleHookFunc
		expectErr       bool
		expectPhase     agentsv1alpha1.SandboxPhase
		expectCondition map[string]metav1.ConditionStatus
		expectMessage   string
	}{
		{
			name: "inplace upgrade - update done transitions to Running",
			pod: func() *corev1.Pod {
				p := newRunningPod()
				// Labels hash matches UpdateRevision means inplace update already applied
				p.Labels[agentsv1alpha1.PodLabelTemplateHash] = "new-revision"
				return p
			}(),
			box: newInplaceSandbox(),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:          agentsv1alpha1.SandboxUpgrading,
				UpdateRevision: "new-revision",
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionTrue,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
						Message:            "upgrade completed",
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxRunning,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionReady): metav1.ConditionTrue,
			},
		},
		{
			name: "inplace upgrade - update in progress stays Upgrading",
			pod: func() *corev1.Pod {
				p := newRunningPod()
				// Labels hash matches UpdateRevision and pod is running+ready
				p.Labels[agentsv1alpha1.PodLabelTemplateHash] = "new-revision"
				// Add inplace update state annotation to simulate in-progress update
				if p.Annotations == nil {
					p.Annotations = map[string]string{}
				}
				p.Annotations[inplaceupdate.PodAnnotationInPlaceUpdateStateKey] =
					`{"revision":"new-revision","lastContainerStatuses":{"sandbox":{"imageID":"new-image-id"}}}`
				p.Status.ContainerStatuses = []corev1.ContainerStatus{
					{Name: "sandbox", ImageID: "new-image-id"},
				}
				return p
			}(),
			box: newInplaceSandbox(),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:          agentsv1alpha1.SandboxUpgrading,
				UpdateRevision: "new-revision",
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			// performRecreateUpgrade sees pod running+ready with matching hash → done → PostUpgrade → Succeeded → Running
			expectPhase: agentsv1alpha1.SandboxRunning,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionReady):     metav1.ConditionTrue,
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionTrue,
			},
		},
		{
			name: "inplace upgrade - pod nil skips preUpgrade and creates pod via Recreate",
			pod:  nil,
			box:  newInplaceSandbox(),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:          agentsv1alpha1.SandboxUpgrading,
				UpdateRevision: "new-revision",
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			// No lifecycle → skip preUpgrade → UpgradePod → performRecreateUpgrade creates pod → stays Upgrading
			expectPhase: agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
				string(agentsv1alpha1.SandboxConditionReady):     metav1.ConditionFalse,
			},
		},
		{
			name: "inplace upgrade - pod nil after preUpgrade creates pod via Recreate",
			pod:  nil,
			box:  newInplaceSandbox(),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:          agentsv1alpha1.SandboxUpgrading,
				UpdateRevision: "new-revision",
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			// performRecreateUpgrade creates pod when pod=nil → stays Upgrading
			expectPhase: agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
				string(agentsv1alpha1.SandboxConditionReady):     metav1.ConditionFalse,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []client.Object
			if tt.pod != nil {
				objects = append(objects, tt.pod.DeepCopy())
			}

			control := newTestCommonControl(tt.mockHookFunc, objects...)
			newStatus := tt.existingStatus.DeepCopy()

			args := EnsureFuncArgs{
				Pod:       tt.pod,
				Box:       tt.box,
				NewStatus: newStatus,
			}

			err := control.EnsureSandboxUpgraded(context.TODO(), args)

			if (err != nil) != tt.expectErr {
				t.Errorf("EnsureSandboxUpgraded() error = %v, wantErr %v", err, tt.expectErr)
				return
			}

			if tt.expectPhase != "" && newStatus.Phase != tt.expectPhase {
				t.Errorf("Expected phase %q, got %q", tt.expectPhase, newStatus.Phase)
			}

			if tt.expectMessage != "" && newStatus.Message != tt.expectMessage {
				t.Errorf("Expected message %q, got %q", tt.expectMessage, newStatus.Message)
			}

			for condType, expectedStatus := range tt.expectCondition {
				cond := utils.GetSandboxCondition(newStatus, condType)
				if cond == nil {
					t.Errorf("Expected condition %q to exist, but it was not found", condType)
					continue
				}
				if cond.Status != expectedStatus {
					t.Errorf("Expected condition %q status to be %q, got %q (reason: %s, message: %s)",
						condType, expectedStatus, cond.Status, cond.Reason, cond.Message)
				}
			}
		})
	}
}

// newTestCommonControlWithCheckpointIndex creates a commonControl with field index support
// for Checkpoint CRs, needed for CheckpointRestore upgrade tests.
func newTestCommonControlWithCheckpointIndex(hookFunc LifecycleHookFunc, objects ...client.Object) *commonControl {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&agentsv1alpha1.Checkpoint{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
		WithStatusSubresource(&agentsv1alpha1.Checkpoint{}).
		WithObjects(objects...).Build()
	checkpointCtrl := NewCheckpointControl(fakeClient, record.NewFakeRecorder(100))
	podCtrl := NewPodControl(fakeClient, record.NewFakeRecorder(100), GeneratePodFromSandbox)
	initializer := &defaultSandboxInitializer{recorder: record.NewFakeRecorder(10)}
	return &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(100),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		rateLimiter:          NewRateLimiter(),
		checkpointControl:    checkpointCtrl,
		podControl:           podCtrl,
		lifecycleHookFunc:    hookFunc,
		initializer:          initializer,
		upgradeControl:       NewUpgradeControl(fakeClient, checkpointCtrl, podCtrl, record.NewFakeRecorder(100), hookFunc, initializer, defaultCommonSyncStatusFromPod, nil),
	}
}

func newCheckpointRestoreSandbox(lifecycle *agentsv1alpha1.SandboxLifecycle) *agentsv1alpha1.Sandbox {
	box := newUpgradeTestSandbox(lifecycle, &agentsv1alpha1.SandboxUpgradePolicy{
		Type: agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore,
	})
	box.UID = types.UID("sandbox-uid-001")
	return box
}

func newUpgradeCheckpoint(name string, box *agentsv1alpha1.Sandbox, phase agentsv1alpha1.CheckpointPhase) *agentsv1alpha1.Checkpoint {
	return &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: box.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(box, sandboxControllerKind),
			},
			Labels: map[string]string{
				agentsv1alpha1.CheckpointLabelSandboxUID:  string(box.UID),
				agentsv1alpha1.CheckpointLabelSandboxName: box.Name,
				agentsv1alpha1.CheckpointLabelType:        agentsv1alpha1.CheckpointPersistentContentFilesystem,
			},
		},
		Status: agentsv1alpha1.CheckpointStatus{
			Phase: phase,
		},
	}
}

func TestEnsureSandboxUpgraded_CheckpointRestore(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name            string
		pod             *corev1.Pod
		box             *agentsv1alpha1.Sandbox
		existingStatus  *agentsv1alpha1.SandboxStatus
		existingCPs     []client.Object
		mockHookFunc    LifecycleHookFunc
		expectErr       bool
		expectPhase     agentsv1alpha1.SandboxPhase
		expectReason    string
		expectCondition map[string]metav1.ConditionStatus
	}{
		{
			name: "CheckpointRestore - PreUpgrade transitions to Checkpointing",
			pod:  newRunningPod(),
			box:  newCheckpointRestoreSandbox(nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonCheckpointing,
		},
		{
			name: "CheckpointRestore - Checkpointing in progress, waits",
			pod:  newRunningPod(),
			box:  newCheckpointRestoreSandbox(nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonCheckpointing,
						LastTransitionTime: now,
					},
				},
			},
			existingCPs: []client.Object{
				newUpgradeCheckpoint("test-sandbox-cp1", newCheckpointRestoreSandbox(nil), agentsv1alpha1.CheckpointCreating),
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonCheckpointing,
		},
		{
			name: "CheckpointRestore - Checkpoint succeeded, transitions to UpgradePod",
			pod: func() *corev1.Pod {
				p := newRunningPod()
				p.Labels[agentsv1alpha1.PodLabelTemplateHash] = "old-revision"
				return p
			}(),
			box: newCheckpointRestoreSandbox(nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:          agentsv1alpha1.SandboxUpgrading,
				UpdateRevision: "new-revision",
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonCheckpointing,
						LastTransitionTime: now,
					},
				},
			},
			existingCPs: []client.Object{
				newUpgradeCheckpoint("test-sandbox-cp1", newCheckpointRestoreSandbox(nil), agentsv1alpha1.CheckpointSucceeded),
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
		},
		{
			name: "CheckpointRestore - Checkpoint failed, returns error",
			pod:  newRunningPod(),
			box:  newCheckpointRestoreSandbox(nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonCheckpointing,
						LastTransitionTime: now,
					},
				},
			},
			existingCPs: []client.Object{
				func() *agentsv1alpha1.Checkpoint {
					cp := newUpgradeCheckpoint("test-sandbox-cp1", newCheckpointRestoreSandbox(nil), agentsv1alpha1.CheckpointFailed)
					cp.Status.Message = "checkpoint timeout"
					return cp
				}(),
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    true,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonCheckpointFailed,
		},
		{
			name: "CheckpointRestore - PostUpgrade succeeds with cleanup",
			pod: func() *corev1.Pod {
				p := newRunningPod()
				p.Labels[agentsv1alpha1.PodLabelTemplateHash] = "new-revision"
				p.Spec.NodeName = "node-1"
				p.Status.PodIP = "10.0.0.2"
				return p
			}(),
			box: newCheckpointRestoreSandbox(nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:          agentsv1alpha1.SandboxUpgrading,
				UpdateRevision: "new-revision",
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonPostUpgrade,
						LastTransitionTime: now,
					},
				},
			},
			existingCPs: []client.Object{
				newUpgradeCheckpoint("test-sandbox-cp1", newCheckpointRestoreSandbox(nil), agentsv1alpha1.CheckpointSucceeded),
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxRunning,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonSucceeded,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionReady):     metav1.ConditionTrue,
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionTrue,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []client.Object
			if tt.pod != nil {
				objects = append(objects, tt.pod.DeepCopy())
			}
			objects = append(objects, tt.existingCPs...)

			control := newTestCommonControlWithCheckpointIndex(tt.mockHookFunc, objects...)
			newStatus := tt.existingStatus.DeepCopy()

			args := EnsureFuncArgs{
				Pod:       tt.pod,
				Box:       tt.box,
				NewStatus: newStatus,
			}

			err := control.EnsureSandboxUpgraded(context.TODO(), args)

			if (err != nil) != tt.expectErr {
				t.Errorf("EnsureSandboxUpgraded() error = %v, wantErr %v", err, tt.expectErr)
				return
			}

			if tt.expectPhase != "" && newStatus.Phase != tt.expectPhase {
				t.Errorf("Expected phase %q, got %q", tt.expectPhase, newStatus.Phase)
			}

			if tt.expectReason != "" {
				cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
				if cond == nil {
					t.Errorf("Expected Upgrading condition to exist")
				} else if cond.Reason != tt.expectReason {
					t.Errorf("Expected reason %q, got %q", tt.expectReason, cond.Reason)
				}
			}

			for condType, expectedStatus := range tt.expectCondition {
				cond := utils.GetSandboxCondition(newStatus, condType)
				if cond == nil {
					t.Errorf("Expected condition %q to exist", condType)
					continue
				}
				if cond.Status != expectedStatus {
					t.Errorf("Expected condition %q status %q, got %q", condType, expectedStatus, cond.Status)
				}
			}
		})
	}
}

func TestPerformRecreateUpgrade_ContainerStatuses(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name            string
		pod             *corev1.Pod
		box             *agentsv1alpha1.Sandbox
		existingStatus  *agentsv1alpha1.SandboxStatus
		mockHookFunc    LifecycleHookFunc
		expectErr       bool
		expectPhase     agentsv1alpha1.SandboxPhase
		expectReason    string
		expectCondition map[string]metav1.ConditionStatus
	}{
		{
			name: "pod not ready with container waiting abnormal reason sets UpgradePodFailed",
			pod: func() *corev1.Pod {
				p := newRunningPod()
				p.Labels[agentsv1alpha1.PodLabelTemplateHash] = "new-revision"
				p.Status.Phase = corev1.PodPending
				p.Status.Conditions = nil
				p.Status.ContainerStatuses = []corev1.ContainerStatus{
					{
						Name: "sandbox",
						State: corev1.ContainerState{
							Waiting: &corev1.ContainerStateWaiting{
								Reason:  "CrashLoopBackOff",
								Message: "container is in crash loop",
							},
						},
					},
				}
				return p
			}(),
			box: newUpgradeTestSandbox(nil, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:          agentsv1alpha1.SandboxUpgrading,
				UpdateRevision: "new-revision",
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
						LastTransitionTime: now,
					},
				},
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
			},
		},
		{
			name: "pod not ready with container terminated sets UpgradePodFailed",
			pod: func() *corev1.Pod {
				p := newRunningPod()
				p.Labels[agentsv1alpha1.PodLabelTemplateHash] = "new-revision"
				p.Status.Phase = corev1.PodPending
				p.Status.Conditions = nil
				p.Status.ContainerStatuses = []corev1.ContainerStatus{
					{
						Name: "sandbox",
						State: corev1.ContainerState{
							Terminated: &corev1.ContainerStateTerminated{
								Reason:   "Error",
								ExitCode: 1,
								Message:  "container exited with error",
							},
						},
					},
				}
				return p
			}(),
			box: newUpgradeTestSandbox(nil, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:          agentsv1alpha1.SandboxUpgrading,
				UpdateRevision: "new-revision",
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
						LastTransitionTime: now,
					},
				},
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
			},
		},
		{
			name: "pod not ready with PodInitializing (normal transient) does not set UpgradePodFailed",
			pod: func() *corev1.Pod {
				p := newRunningPod()
				p.Labels[agentsv1alpha1.PodLabelTemplateHash] = "new-revision"
				p.Status.Phase = corev1.PodPending
				p.Status.Conditions = nil
				p.Status.ContainerStatuses = []corev1.ContainerStatus{
					{
						Name: "sandbox",
						State: corev1.ContainerState{
							Waiting: &corev1.ContainerStateWaiting{
								Reason:  WaitingReasonPodInitializing,
								Message: "pod is initializing",
							},
						},
					},
				}
				return p
			}(),
			box: newUpgradeTestSandbox(nil, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:          agentsv1alpha1.SandboxUpgrading,
				UpdateRevision: "new-revision",
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
						LastTransitionTime: now,
					},
				},
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
			},
		},
		{
			name: "pod not ready with ContainerCreating (normal transient) does not set UpgradePodFailed",
			pod: func() *corev1.Pod {
				p := newRunningPod()
				p.Labels[agentsv1alpha1.PodLabelTemplateHash] = "new-revision"
				p.Status.Phase = corev1.PodPending
				p.Status.Conditions = nil
				p.Status.ContainerStatuses = []corev1.ContainerStatus{
					{
						Name: "sandbox",
						State: corev1.ContainerState{
							Waiting: &corev1.ContainerStateWaiting{
								Reason:  WaitingReasonContainerCreating,
								Message: "container is being created",
							},
						},
					},
				}
				return p
			}(),
			box: newUpgradeTestSandbox(nil, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:          agentsv1alpha1.SandboxUpgrading,
				UpdateRevision: "new-revision",
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
						LastTransitionTime: now,
					},
				},
			},
			mockHookFunc: mockLifecycleHookFunc(0, "", "", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []client.Object
			if tt.pod != nil {
				objects = append(objects, tt.pod.DeepCopy())
			}

			control := newTestCommonControl(tt.mockHookFunc, objects...)
			newStatus := tt.existingStatus.DeepCopy()

			args := EnsureFuncArgs{
				Pod:       tt.pod,
				Box:       tt.box,
				NewStatus: newStatus,
			}

			err := control.EnsureSandboxUpgraded(context.TODO(), args)

			if (err != nil) != tt.expectErr {
				t.Errorf("EnsureSandboxUpgraded() error = %v, wantErr %v", err, tt.expectErr)
				return
			}

			if tt.expectPhase != "" && newStatus.Phase != tt.expectPhase {
				t.Errorf("Expected phase %q, got %q", tt.expectPhase, newStatus.Phase)
			}

			if tt.expectReason != "" {
				cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
				if cond == nil {
					t.Errorf("Expected Upgrading condition to exist")
				} else if cond.Reason != tt.expectReason {
					t.Errorf("Expected reason %q, got %q", tt.expectReason, cond.Reason)
				}
			}

			for condType, expectedStatus := range tt.expectCondition {
				cond := utils.GetSandboxCondition(newStatus, condType)
				if cond == nil {
					t.Errorf("Expected condition %q to exist", condType)
					continue
				}
				if cond.Status != expectedStatus {
					t.Errorf("Expected condition %q status %q, got %q", condType, expectedStatus, cond.Status)
				}
			}
		})
	}
}

func TestPerformRecreateUpgrade_CheckpointRestore_CreatePod(t *testing.T) {
	now := metav1.Now()

	// CheckpointRestore with pod=nil in UpgradePod state should create a new pod
	// with the checkpoint ID annotation.
	box := newCheckpointRestoreSandbox(nil)
	pod := newRunningPod()
	pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = "old-revision"

	// Create a checkpoint carrying both the recorded delta and the ID, as a
	// real succeeded filesystem checkpoint does, so GetCheckpointResumeData
	// returns it.
	cp := newUpgradeCheckpoint("test-sandbox-cp1", box, agentsv1alpha1.CheckpointSucceeded)
	cp.Status.PodTemplateDelta = runtime.RawExtension{Raw: []byte(`{"spec":{"containers":[]}}`)}
	cp.Status.CheckpointId = "cp-id-restore-123"

	control := newTestCommonControlWithCheckpointIndex(
		mockLifecycleHookFunc(0, "", "", nil),
		pod.DeepCopy(),
		cp,
	)

	newStatus := &agentsv1alpha1.SandboxStatus{
		Phase:          agentsv1alpha1.SandboxUpgrading,
		UpdateRevision: "new-revision",
		Conditions: []metav1.Condition{
			{
				Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
				Status:             metav1.ConditionFalse,
				Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
				LastTransitionTime: now,
			},
		},
	}

	// First call: pod has old-revision hash, so it gets deleted (Step 1)
	args := EnsureFuncArgs{
		Pod:       pod,
		Box:       box,
		NewStatus: newStatus,
	}
	err := control.EnsureSandboxUpgraded(context.TODO(), args)
	assert.NoError(t, err)

	// Second call: pod is nil (deleted), so it creates a new pod with checkpoint ID (Step 2)
	newStatus2 := &agentsv1alpha1.SandboxStatus{
		Phase:          agentsv1alpha1.SandboxUpgrading,
		UpdateRevision: "new-revision",
		Conditions: []metav1.Condition{
			{
				Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
				Status:             metav1.ConditionFalse,
				Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
				LastTransitionTime: now,
			},
		},
	}
	args2 := EnsureFuncArgs{
		Pod:       nil,
		Box:       box,
		NewStatus: newStatus2,
	}
	err = control.EnsureSandboxUpgraded(context.TODO(), args2)
	assert.NoError(t, err)

	// Verify a new pod was created
	createdPod := &corev1.Pod{}
	err = control.Get(context.TODO(), types.NamespacedName{Namespace: box.Namespace, Name: box.Name}, createdPod)
	assert.NoError(t, err)
	assert.Equal(t, "new-revision", createdPod.Labels[agentsv1alpha1.PodLabelTemplateHash])
}

func TestPerformRecreateUpgrade_CheckpointRestore_MissingCheckpointID(t *testing.T) {
	now := metav1.Now()

	// A checkpoint that recorded a delta but lost its ID should never happen.
	// The upgrade must block instead of creating a pod that cannot restore
	// its writable layer.
	box := newCheckpointRestoreSandbox(nil)
	cp := newUpgradeCheckpoint("test-sandbox-cp1", box, agentsv1alpha1.CheckpointSucceeded)
	cp.Status.PodTemplateDelta = runtime.RawExtension{Raw: []byte(`{"spec":{"containers":[]}}`)}

	control := newTestCommonControlWithCheckpointIndex(
		mockLifecycleHookFunc(0, "", "", nil),
		cp,
	)

	newStatus := &agentsv1alpha1.SandboxStatus{
		Phase:          agentsv1alpha1.SandboxUpgrading,
		UpdateRevision: "new-revision",
		Conditions: []metav1.Condition{
			{
				Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
				Status:             metav1.ConditionFalse,
				Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
				LastTransitionTime: now,
			},
		},
	}

	// pod is nil (already deleted), so the upgrade reaches the pod creation
	// step and must fail there.
	err := control.EnsureSandboxUpgraded(context.TODO(), EnsureFuncArgs{
		Pod:       nil,
		Box:       box,
		NewStatus: newStatus,
	})
	assert.ErrorContains(t, err, "checkpoint ID not found")

	// No pod may have been created.
	createdPod := &corev1.Pod{}
	getErr := control.Get(context.TODO(), types.NamespacedName{Namespace: box.Namespace, Name: box.Name}, createdPod)
	assert.Error(t, getErr)
}

func TestExecuteUpgradeAction_NilAction(t *testing.T) {
	ctrl := newTestCommonControl(mockLifecycleHookFunc(0, "", "", nil))
	box := newUpgradeTestSandbox(nil, nil)
	result := ctrl.upgradeControl.executeUpgradeAction(context.Background(), newRunningPod(), box, nil)
	assert.True(t, result.Succeeded)
	assert.Contains(t, result.Message, "no hook configured")
}

func TestExecuteUpgradeAction_NilPod(t *testing.T) {
	ctrl := newTestCommonControl(mockLifecycleHookFunc(0, "", "", nil))
	box := newUpgradeTestSandbox(nil, nil)
	action := &agentsv1alpha1.UpgradeAction{
		Exec:           &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "echo test"}},
		TimeoutSeconds: 30,
	}
	result := ctrl.upgradeControl.executeUpgradeAction(context.Background(), nil, box, action)
	assert.False(t, result.Succeeded)
	assert.Contains(t, result.Message, "pod not found")
}

func TestHasUpgradeAction(t *testing.T) {
	tests := []struct {
		name     string
		box      *agentsv1alpha1.Sandbox
		pre      bool
		expected bool
	}{
		{
			name:     "nil lifecycle returns false",
			box:      &agentsv1alpha1.Sandbox{},
			pre:      true,
			expected: false,
		},
		{
			name: "lifecycle with nil preUpgrade action returns false",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Lifecycle: &agentsv1alpha1.SandboxLifecycle{},
				},
			},
			pre:      true,
			expected: false,
		},
		{
			name: "lifecycle with nil postUpgrade action returns false",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Lifecycle: &agentsv1alpha1.SandboxLifecycle{},
				},
			},
			pre:      false,
			expected: false,
		},
		{
			name: "lifecycle with preUpgrade action but nil exec returns false",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Lifecycle: &agentsv1alpha1.SandboxLifecycle{
						PreUpgrade: &agentsv1alpha1.UpgradeAction{},
					},
				},
			},
			pre:      true,
			expected: false,
		},
		{
			name: "lifecycle with preUpgrade action and empty command returns false",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Lifecycle: &agentsv1alpha1.SandboxLifecycle{
						PreUpgrade: &agentsv1alpha1.UpgradeAction{
							Exec: &corev1.ExecAction{},
						},
					},
				},
			},
			pre:      true,
			expected: false,
		},
		{
			name: "lifecycle with preUpgrade action and exec command returns true",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Lifecycle: &agentsv1alpha1.SandboxLifecycle{
						PreUpgrade: &agentsv1alpha1.UpgradeAction{
							Exec: &corev1.ExecAction{Command: []string{"echo", "test"}},
						},
					},
				},
			},
			pre:      true,
			expected: true,
		},
		{
			name: "lifecycle with postUpgrade action and exec command returns true",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Lifecycle: &agentsv1alpha1.SandboxLifecycle{
						PostUpgrade: &agentsv1alpha1.UpgradeAction{
							Exec: &corev1.ExecAction{Command: []string{"echo", "test"}},
						},
					},
				},
			},
			pre:      false,
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, hasUpgradeAction(tt.box, tt.pre))
		})
	}
}

// TestEnsureSandboxUpgraded_Resuming covers the Resuming phase of the upgrade
// state machine, which handles paused sandboxes that need to be woken up before
// proceeding with the upgrade lifecycle.
func TestEnsureSandboxUpgraded_Resuming(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	now := metav1.Now()
	oldPodIP := "10.0.0.1"
	newPodIP := "10.0.0.2"
	newPodUID := types.UID("new-pod-uid")

	readyPod := func() *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-sandbox",
				Namespace: "default",
				UID:       newPodUID,
				Labels: map[string]string{
					agentsv1alpha1.PodLabelTemplateHash: "old-revision",
				},
			},
			Spec: corev1.PodSpec{
				NodeName: "node-1",
				Containers: []corev1.Container{
					{Name: "sandbox", Image: "test:v1"},
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: newPodIP,
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
				},
			},
		}
	}

	notReadyPod := func() *corev1.Pod {
		p := readyPod()
		p.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: now},
		}
		return p
	}

	baseBox := func() *agentsv1alpha1.Sandbox {
		return &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-sandbox",
				Namespace: "default",
			},
			Spec: agentsv1alpha1.SandboxSpec{
				UpgradePolicy: &agentsv1alpha1.SandboxUpgradePolicy{
					Type: agentsv1alpha1.SandboxUpgradePolicyRecreate,
				},
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "sandbox", Image: "test:v2"},
							},
						},
					},
				},
			},
		}
	}

	// boxWithPreUpgrade returns a sandbox with a PreUpgrade lifecycle hook,
	// which causes the controller to call Initialize during the Resuming stage.
	boxWithPreUpgrade := func() *agentsv1alpha1.Sandbox {
		box := baseBox()
		box.Spec.Lifecycle = &agentsv1alpha1.SandboxLifecycle{
			PreUpgrade: &agentsv1alpha1.UpgradeAction{
				Exec: &corev1.ExecAction{Command: []string{"/bin/echo", "pre"}},
			},
		}
		return box
	}

	installerBox := func(postUpgrade bool) *agentsv1alpha1.Sandbox {
		box := baseBox()
		box.Spec.UpgradePolicy = &agentsv1alpha1.SandboxUpgradePolicy{
			Type: agentsv1alpha1.SandboxUpgradePolicyInstallerUpdate, PatchVersion: "ops-uid-1",
			Installer: &agentsv1alpha1.InstallerSpec{Image: "installer:v2"},
		}
		box.Spec.Template.Spec.Containers[0].Image = "test:v1"
		if postUpgrade {
			box.Spec.Lifecycle = &agentsv1alpha1.SandboxLifecycle{
				PostUpgrade: &agentsv1alpha1.UpgradeAction{Exec: &corev1.ExecAction{Command: []string{"restart"}}},
			}
		}
		return box
	}

	pausedTrueCond := metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxConditionPaused),
		Status:             metav1.ConditionTrue,
		Reason:             agentsv1alpha1.SandboxPausedReasonStopPauseSucceed,
		LastTransitionTime: now,
	}
	pausedFalseCond := metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxConditionPaused),
		Status:             metav1.ConditionFalse,
		Reason:             agentsv1alpha1.SandboxPausedReasonPending,
		LastTransitionTime: now,
	}
	resumedTrueCond := metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxConditionResumed),
		Status:             metav1.ConditionTrue,
		Reason:             agentsv1alpha1.SandboxResumeReasonCreatePod,
		LastTransitionTime: now,
	}
	resumingCond := metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
		Status:             metav1.ConditionFalse,
		Reason:             agentsv1alpha1.SandboxUpgradingReasonResuming,
		LastTransitionTime: now,
	}

	installerResumeStatus := func() *agentsv1alpha1.SandboxStatus {
		return &agentsv1alpha1.SandboxStatus{
			Phase:                 agentsv1alpha1.SandboxUpgrading,
			UpgradingPatchVersion: "ops-uid-1",
			SandboxIp:             oldPodIP,
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: oldPodIP, PodUID: "old-pod-uid",
			},
			Conditions: []metav1.Condition{resumingCond, pausedTrueCond},
		}
	}

	// mockResume tracks resume calls and controls behavior.
	type mockResume struct {
		err        error
		setResumed bool
		called     int
	}

	// newControlWithResume creates an UpgradeControl with a mockable resumeFunc
	// and returns the mock initializer so tests can assert call counts.
	newControlWithResume := func(mr *mockResume, initErr error) (*UpgradeControl, *mockSandboxInitializer) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		checkpointCtrl := NewCheckpointControl(fakeClient, record.NewFakeRecorder(100))
		podCtrl := NewPodControl(fakeClient, record.NewFakeRecorder(100), GeneratePodFromSandbox)
		initializer := &mockSandboxInitializer{err: initErr}
		resumeFn := func(ctx context.Context, args EnsureFuncArgs) error {
			mr.called++
			if mr.err != nil {
				return mr.err
			}
			if mr.setResumed {
				utils.SetSandboxCondition(args.NewStatus, metav1.Condition{
					Type:               string(agentsv1alpha1.SandboxConditionResumed),
					Status:             metav1.ConditionTrue,
					Reason:             agentsv1alpha1.SandboxResumeReasonCreatePod,
					LastTransitionTime: metav1.Now(),
				})
			}
			return nil
		}
		return NewUpgradeControl(fakeClient, checkpointCtrl, podCtrl, record.NewFakeRecorder(100), mockLifecycleHookFunc(0, "", "", nil), initializer, defaultCommonSyncStatusFromPod, resumeFn), initializer
	}

	tests := []struct {
		name                string
		pod                 *corev1.Pod
		box                 *agentsv1alpha1.Sandbox
		existingStatus      *agentsv1alpha1.SandboxStatus
		resumeErr           error
		resumeSetResumed    bool
		expectResumeCalled  bool
		initErr             error
		expectInitCalled    bool
		expectErr           bool
		expectReason        string
		expectPausedRemoved bool
		installerGateOn     bool
	}{
		{
			name: "paused sandbox first enters upgrade with Paused=False - initial reason is Resuming, waits",
			pod:  readyPod(),
			box:  baseBox(),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{
					pausedFalseCond,
				},
			},
			expectResumeCalled: false,
			expectErr:          false,
			expectReason:       agentsv1alpha1.SandboxUpgradingReasonResuming,
		},
		{
			name: "Resuming with Paused=True, resumeFunc returns error",
			pod:  readyPod(),
			box:  baseBox(),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{
					resumingCond,
					pausedTrueCond,
				},
			},
			resumeErr:          fmt.Errorf("resume failed"),
			expectResumeCalled: true,
			expectErr:          true,
			expectReason:       agentsv1alpha1.SandboxUpgradingReasonResuming,
		},
		{
			name: "Resuming with Paused=True, resumeFunc succeeds but Resumed not set - waits",
			pod:  readyPod(),
			box:  baseBox(),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{
					resumingCond,
					pausedTrueCond,
				},
			},
			expectResumeCalled: true,
			expectErr:          false,
			expectReason:       agentsv1alpha1.SandboxUpgradingReasonResuming,
		},
		{
			name: "Resuming with Paused=True, Resumed=True, PodReady=False - waits",
			pod:  notReadyPod(),
			box:  baseBox(),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{
					resumingCond,
					pausedTrueCond,
					resumedTrueCond,
				},
			},
			resumeSetResumed:   true,
			expectResumeCalled: true,
			expectErr:          false,
			expectReason:       agentsv1alpha1.SandboxUpgradingReasonResuming,
		},
		{
			name: "Resuming with Paused=True, Resumed=True, PodReady=True, Initialize fails",
			pod:  readyPod(),
			box:  boxWithPreUpgrade(),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:     agentsv1alpha1.SandboxUpgrading,
				SandboxIp: oldPodIP,
				PodInfo: agentsv1alpha1.PodInfo{
					PodIP:  oldPodIP,
					PodUID: types.UID("old-pod-uid"),
				},
				Conditions: []metav1.Condition{
					resumingCond,
					pausedTrueCond,
					resumedTrueCond,
				},
			},
			resumeSetResumed:   true,
			expectResumeCalled: true,
			initErr:            fmt.Errorf("init failed"),
			expectInitCalled:   true,
			expectErr:          true,
			expectReason:       agentsv1alpha1.SandboxUpgradingReasonResuming,
		},
		{
			name: "Resuming with Paused=True, Resumed=True, PodReady=True, Initialize succeeds - transitions to ResumeSucceed and removes Paused",
			pod:  readyPod(),
			box:  boxWithPreUpgrade(),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:     agentsv1alpha1.SandboxUpgrading,
				SandboxIp: oldPodIP,
				PodInfo: agentsv1alpha1.PodInfo{
					PodIP:  oldPodIP,
					PodUID: types.UID("old-pod-uid"),
				},
				Conditions: []metav1.Condition{
					resumingCond,
					pausedTrueCond,
					resumedTrueCond,
				},
			},
			resumeSetResumed:    true,
			expectResumeCalled:  true,
			expectInitCalled:    true,
			expectErr:           false,
			expectReason:        agentsv1alpha1.SandboxUpgradingReasonResumeSucceed,
			expectPausedRemoved: true,
		},
		{
			name: "Resuming with Paused=True, Resumed=True, PodReady=True, no PreUpgrade hook - skips Initialize, transitions to ResumeSucceed",
			pod:  readyPod(),
			box:  baseBox(),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{
					resumingCond,
					pausedTrueCond,
					resumedTrueCond,
				},
			},
			resumeSetResumed:    true,
			expectResumeCalled:  true,
			expectInitCalled:    false,
			expectErr:           false,
			expectReason:        agentsv1alpha1.SandboxUpgradingReasonResumeSucceed,
			expectPausedRemoved: true,
		},
		{
			name:                "installer without lifecycle hooks initializes before advancing",
			pod:                 readyPod(),
			box:                 installerBox(false),
			existingStatus:      installerResumeStatus(),
			resumeSetResumed:    true,
			expectResumeCalled:  true,
			expectInitCalled:    true,
			expectReason:        agentsv1alpha1.SandboxUpgradingReasonResumeSucceed,
			expectPausedRemoved: true,
			installerGateOn:     true,
		},
		{
			name:                "PostUpgrade-only installer initializes the resumed pod",
			pod:                 readyPod(),
			box:                 installerBox(true),
			existingStatus:      installerResumeStatus(),
			resumeSetResumed:    true,
			expectResumeCalled:  true,
			expectInitCalled:    true,
			expectReason:        agentsv1alpha1.SandboxUpgradingReasonResumeSucceed,
			expectPausedRemoved: true,
			installerGateOn:     true,
		},
		{
			name:                "existing resuming installer still initializes when its gate is disabled",
			pod:                 readyPod(),
			box:                 installerBox(true),
			existingStatus:      installerResumeStatus(),
			resumeSetResumed:    true,
			expectResumeCalled:  true,
			expectInitCalled:    true,
			expectReason:        agentsv1alpha1.SandboxUpgradingReasonResumeSucceed,
			expectPausedRemoved: true,
		},
		{
			name:               "installer initialization failure blocks upgrade progression",
			pod:                readyPod(),
			box:                installerBox(true),
			existingStatus:     installerResumeStatus(),
			resumeSetResumed:   true,
			expectResumeCalled: true,
			expectInitCalled:   true,
			initErr:            fmt.Errorf("runtime initialization failed"),
			expectErr:          true,
			expectReason:       agentsv1alpha1.SandboxUpgradingReasonResuming,
			installerGateOn:    true,
		},
		{
			name:               "installer waits for pod readiness before initialization",
			pod:                notReadyPod(),
			box:                installerBox(true),
			existingStatus:     installerResumeStatus(),
			resumeSetResumed:   true,
			expectResumeCalled: true,
			expectReason:       agentsv1alpha1.SandboxUpgradingReasonResuming,
			installerGateOn:    true,
		},
		{
			name:               "installer waits for resumed pod to become visible",
			box:                installerBox(true),
			existingStatus:     installerResumeStatus(),
			resumeSetResumed:   true,
			expectResumeCalled: true,
			expectReason:       agentsv1alpha1.SandboxUpgradingReasonResuming,
			installerGateOn:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.box.Spec.UpgradePolicy.Type == agentsv1alpha1.SandboxUpgradePolicyInstallerUpdate {
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SandboxInstallerUpdateGate, tt.installerGateOn)
			}
			mr := &mockResume{err: tt.resumeErr, setResumed: tt.resumeSetResumed}
			ctrl, init := newControlWithResume(mr, tt.initErr)

			newStatus := tt.existingStatus.DeepCopy()
			args := EnsureFuncArgs{
				Pod:       tt.pod,
				Box:       tt.box,
				NewStatus: newStatus,
			}

			err := ctrl.EnsureSandboxUpgraded(context.TODO(), args)

			if (err != nil) != tt.expectErr {
				t.Errorf("EnsureSandboxUpgraded() error = %v, wantErr %v", err, tt.expectErr)
			}
			if tt.initErr != nil {
				require.ErrorIs(t, err, tt.initErr)
			}

			if tt.expectResumeCalled && mr.called == 0 {
				t.Error("expected resumeFunc to be called, but it was not")
			}
			if !tt.expectResumeCalled && mr.called > 0 {
				t.Error("expected resumeFunc NOT to be called, but it was")
			}

			if tt.expectInitCalled {
				require.Equal(t, 1, init.called)
				require.NotNil(t, init.initializedStatus)
				assert.Equal(t, newPodIP, init.initializedStatus.SandboxIp, "SandboxIp passed to Initialize")
				assert.Equal(t, newPodIP, init.initializedStatus.PodInfo.PodIP, "PodInfo.PodIP passed to Initialize")
				assert.Equal(t, newPodUID, init.initializedStatus.PodInfo.PodUID, "PodInfo.PodUID passed to Initialize")
				atInit := utils.GetSandboxCondition(init.initializedStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
				require.NotNil(t, atInit)
				assert.Equal(t, agentsv1alpha1.SandboxUpgradingReasonResuming, atInit.Reason)
				assert.Equal(t, newPodIP, newStatus.SandboxIp, "SandboxIp in newStatus")
				assert.Equal(t, newPodIP, newStatus.PodInfo.PodIP, "PodInfo.PodIP in newStatus")
				assert.Equal(t, newPodUID, newStatus.PodInfo.PodUID, "PodInfo.PodUID in newStatus")
			}
			if !tt.expectInitCalled && init.called > 0 {
				t.Error("expected Initialize NOT to be called, but it was")
			}

			upgradingCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
			if upgradingCond == nil {
				t.Error("expected Upgrading condition to exist")
			} else if upgradingCond.Reason != tt.expectReason {
				t.Errorf("expected Upgrading reason %q, got %q", tt.expectReason, upgradingCond.Reason)
			}

			pausedCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
			if tt.expectPausedRemoved {
				assert.Nil(t, pausedCond)
			} else {
				assert.NotNil(t, pausedCond)
			}
		})
	}
}

// enableInstallerUpdateGate turns on the SandboxInstallerUpdate feature gate
// for the duration of the test.
func enableInstallerUpdateGate(t *testing.T) {
	t.Helper()
	if err := utilfeature.DefaultMutableFeatureGate.Set("SandboxInstallerUpdate=true"); err != nil {
		t.Fatalf("failed to enable SandboxInstallerUpdate gate: %v", err)
	}
	t.Cleanup(func() {
		_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxInstallerUpdate=false")
	})
}

func newInstallerTestSandbox(installer *agentsv1alpha1.InstallerSpec, patchVersion string) *agentsv1alpha1.Sandbox {
	box := newUpgradeTestSandbox(nil, &agentsv1alpha1.SandboxUpgradePolicy{
		Type:         agentsv1alpha1.SandboxUpgradePolicyInstallerUpdate,
		PatchVersion: patchVersion,
		Installer:    installer,
	})
	box.Status.UpgradingPatchVersion = patchVersion
	return box
}

func newInstallerRunningPod() *corev1.Pod {
	pod := newRunningPod()
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "sandbox",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
	}}
	return pod
}

// newInstallerTestControl builds an UpgradeControl whose client intercepts
// pods/ephemeralcontainers subresource patches — unsupported by the fake
// client — by merging the patched container into the stored Pod. The returned
// pointer counts subresource patch calls.
func newInstallerTestControl(t *testing.T, injectionError error, objects ...client.Object) (*UpgradeControl, *int) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	patchCalls := 0
	wrapped := interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if sub != "ephemeralcontainers" {
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			}
			patchCalls++
			if injectionError != nil {
				return injectionError
			}
			data, err := patch.Data(obj)
			if err != nil {
				return err
			}
			var body struct {
				Spec struct {
					EphemeralContainers []corev1.EphemeralContainer `json:"ephemeralContainers"`
				} `json:"spec"`
			}
			if err := json.Unmarshal(data, &body); err != nil {
				return err
			}
			var pod corev1.Pod
			if err := c.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}, &pod); err != nil {
				return err
			}
			for _, ec := range body.Spec.EphemeralContainers {
				found := false
				for i := range pod.Spec.EphemeralContainers {
					if pod.Spec.EphemeralContainers[i].Name == ec.Name {
						pod.Spec.EphemeralContainers[i] = ec
						found = true
						break
					}
				}
				if !found {
					pod.Spec.EphemeralContainers = append(pod.Spec.EphemeralContainers, ec)
				}
			}
			return c.Update(ctx, &pod)
		},
	})

	checkpointCtrl := NewCheckpointControl(wrapped, record.NewFakeRecorder(100))
	podCtrl := NewPodControl(wrapped, record.NewFakeRecorder(100), GeneratePodFromSandbox)
	initializer := &mockSandboxInitializer{}
	return NewUpgradeControl(wrapped, checkpointCtrl, podCtrl, record.NewFakeRecorder(100),
		mockLifecycleHookFunc(0, "", "", nil), initializer, defaultCommonSyncStatusFromPod, nil), &patchCalls
}

func TestRequiresInstallerUpgrade(t *testing.T) {
	tests := []struct {
		name     string
		gateOn   bool
		policy   *agentsv1alpha1.SandboxUpgradePolicy
		expected bool
	}{
		{name: "nil policy", gateOn: true, policy: nil, expected: false},
		{name: "recreate policy", gateOn: true, policy: &agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyRecreate}, expected: false},
		{name: "checkpoint restore policy", gateOn: true, policy: &agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore}, expected: false},
		{name: "installer policy gate on", gateOn: true, policy: &agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyInstallerUpdate}, expected: true},
		{name: "installer policy gate off", gateOn: false, policy: &agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyInstallerUpdate}, expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.gateOn {
				enableInstallerUpdateGate(t)
			}
			box := &agentsv1alpha1.Sandbox{Spec: agentsv1alpha1.SandboxSpec{UpgradePolicy: tt.policy}}
			assert.Equal(t, tt.expected, RequiresInstallerUpgrade(box))
			// RequiresPodReplacementUpgrade is unchanged for the new value.
			assert.Equal(t, tt.policy != nil &&
				(tt.policy.Type == agentsv1alpha1.SandboxUpgradePolicyRecreate ||
					tt.policy.Type == agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore),
				RequiresPodReplacementUpgrade(box))
		})
	}
}

func TestInstallerContainerName(t *testing.T) {
	name := installerContainerName("ops-uid-1")
	// Deterministic: same input, same name.
	assert.Equal(t, name, installerContainerName("ops-uid-1"))
	// DNS-label shape: installer-<8 hex chars>.
	assert.Regexp(t, `^installer-[0-9a-f]{8}$`, name)
	// Distinct patch versions produce distinct names.
	assert.NotEqual(t, name, installerContainerName("ops-uid-2"))
}

func TestBuildInstallerEphemeralContainer(t *testing.T) {
	patchVersion := "ops-uid-1"
	box := newInstallerTestSandbox(&agentsv1alpha1.InstallerSpec{Image: "installer:1.4.0"}, patchVersion)
	target := &corev1.Container{
		Name:            "sandbox",
		SecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To[int64](1000)},
	}

	t.Run("defaults", func(t *testing.T) {
		ec := buildInstallerEphemeralContainer("installer-abcd1234", box.Spec.UpgradePolicy.Installer, patchVersion, target, box)
		assert.Equal(t, "installer-abcd1234", ec.Name)
		assert.Equal(t, "installer:1.4.0", ec.Image)
		assert.Equal(t, "sandbox", ec.TargetContainerName)
		assert.Equal(t, corev1.PullIfNotPresent, ec.ImagePullPolicy)
		assert.Equal(t, corev1.TerminationMessageFallbackToLogsOnError, ec.TerminationMessagePolicy)
		assert.NotNil(t, ec.SecurityContext)
		assert.Equal(t, []corev1.Capability{"SYS_PTRACE"}, ec.SecurityContext.Capabilities.Add)
		assert.Equal(t, ptr.To[int64](1000), ec.SecurityContext.RunAsUser)

		envNames := map[string]string{}
		for _, e := range ec.Env {
			envNames[e.Name] = e.Value
		}
		assert.Equal(t, "/proc/1/root", envNames["SANDBOX_INSTALLER_TARGET_ROOT"])
		assert.Equal(t, "sandbox", envNames["SANDBOX_INSTALLER_TARGET_CONTAINER"])
		assert.Equal(t, patchVersion, envNames["SANDBOX_INSTALLER_PATCH_VERSION"])
		assert.Equal(t, box.Name, envNames["SANDBOX_NAME"])
		assert.Equal(t, box.Namespace, envNames["SANDBOX_NAMESPACE"])
		assert.Equal(t, string(box.UID), envNames["SANDBOX_UID"])
	})

	t.Run("security context override is wholesale", func(t *testing.T) {
		installer := &agentsv1alpha1.InstallerSpec{
			Image: "installer:1.4.0",
			SecurityContext: &corev1.SecurityContext{
				Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_PTRACE", "SYS_ADMIN"}},
			},
		}
		ec := buildInstallerEphemeralContainer("installer-abcd1234", installer, patchVersion, target, box)
		assert.Equal(t, []corev1.Capability{"SYS_PTRACE", "SYS_ADMIN"}, ec.SecurityContext.Capabilities.Add)
		assert.Nil(t, ec.SecurityContext.RunAsUser)
	})

	t.Run("operator env appended after contract env", func(t *testing.T) {
		installer := &agentsv1alpha1.InstallerSpec{
			Image: "installer:1.4.0",
			Env:   []corev1.EnvVar{{Name: "HARNESS_CHANNEL", Value: "stable"}},
		}
		ec := buildInstallerEphemeralContainer("installer-abcd1234", installer, patchVersion, target, box)
		assert.Equal(t, "HARNESS_CHANNEL", ec.Env[len(ec.Env)-1].Name)
	})
}

func TestPerformInstallerUpgrade(t *testing.T) {
	enableInstallerUpdateGate(t)
	now := metav1.Now()

	defaultInstaller := func() *agentsv1alpha1.InstallerSpec {
		return &agentsv1alpha1.InstallerSpec{Image: "installer:1.4.0"}
	}

	// installingStatus returns a newStatus whose Upgrading condition sits in
	// the Installing reason, plus the condition pointer.
	installingStatus := func(lastTransition metav1.Time) (*agentsv1alpha1.SandboxStatus, *metav1.Condition) {
		status := &agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxUpgrading,
			Conditions: []metav1.Condition{{
				Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
				Status:             metav1.ConditionFalse,
				Reason:             agentsv1alpha1.SandboxUpgradingReasonInstalling,
				LastTransitionTime: lastTransition,
			}},
		}
		return status, &status.Conditions[0]
	}

	podWithEphemeral := func(ec *corev1.EphemeralContainer, status *corev1.ContainerStatus) *corev1.Pod {
		pod := newInstallerRunningPod()
		if ec != nil {
			pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{*ec}
		}
		if status != nil {
			pod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{*status}
		}
		return pod
	}

	terminatedStatus := func(exitCode int32, message string) *corev1.ContainerStatus {
		return &corev1.ContainerStatus{
			Name: installerContainerName("ops-uid-1"),
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   exitCode,
				Message:    message,
				FinishedAt: now,
			}},
		}
	}

	tests := []struct {
		name             string
		installer        *agentsv1alpha1.InstallerSpec
		pod              *corev1.Pod
		lastTransition   metav1.Time
		injectionError   error
		expectDone       bool
		expectErr        bool
		expectPatches    int
		expectReason     string
		expectMsgContain string
	}{
		{
			name:           "first injection creates the ephemeral container",
			installer:      defaultInstaller(),
			pod:            newInstallerRunningPod(),
			lastTransition: now,
			expectDone:     false,
			expectPatches:  1,
			expectReason:   agentsv1alpha1.SandboxUpgradingReasonInstalling,
		},
		{
			name: "explicit target container name resolves",
			installer: &agentsv1alpha1.InstallerSpec{
				Image:               "installer:1.4.0",
				TargetContainerName: "sandbox",
			},
			pod:            newInstallerRunningPod(),
			lastTransition: now,
			expectDone:     false,
			expectPatches:  1,
			expectReason:   agentsv1alpha1.SandboxUpgradingReasonInstalling,
		},
		{
			name:             "unknown target container name fails",
			installer:        &agentsv1alpha1.InstallerSpec{Image: "installer:1.4.0", TargetContainerName: "missing"},
			pod:              newInstallerRunningPod(),
			expectDone:       false,
			expectReason:     agentsv1alpha1.SandboxUpgradingReasonInstallingFailed,
			expectMsgContain: `target container "missing" not found`,
		},
		{
			name: "shareProcessNamespace fails before injection",
			installer: func() *agentsv1alpha1.InstallerSpec {
				i := defaultInstaller()
				return i
			}(),
			pod: func() *corev1.Pod {
				p := newInstallerRunningPod()
				p.Spec.ShareProcessNamespace = ptr.To(true)
				return p
			}(),
			expectDone:       false,
			expectReason:     agentsv1alpha1.SandboxUpgradingReasonInstallingFailed,
			expectMsgContain: "shareProcessNamespace",
		},
		{
			name:      "hostPID fails before injection",
			installer: defaultInstaller(),
			pod: func() *corev1.Pod {
				p := newInstallerRunningPod()
				p.Spec.HostPID = true
				return p
			}(),
			expectDone:       false,
			expectReason:     agentsv1alpha1.SandboxUpgradingReasonInstallingFailed,
			expectMsgContain: "hostPID",
		},
		{
			name:      "pod not running waits",
			installer: defaultInstaller(),
			pod: func() *corev1.Pod {
				p := newInstallerRunningPod()
				p.Status.Phase = corev1.PodPending
				return p
			}(),
			expectDone:   false,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonInstalling,
		},
		{
			name:      "target container not running waits",
			installer: defaultInstaller(),
			pod: func() *corev1.Pod {
				p := newInstallerRunningPod()
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "sandbox"}}
				return p
			}(),
			expectDone:   false,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonInstalling,
		},
		{
			name: "exit 0 completes the install",
			pod: podWithEphemeral(
				&corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: installerContainerName("ops-uid-1")}},
				terminatedStatus(0, "")),
			lastTransition: now,
			expectDone:     true,
			expectReason:   agentsv1alpha1.SandboxUpgradingReasonInstalling,
		},
		{
			name: "exit 125 reports namespace targeting unsupported",
			pod: podWithEphemeral(
				&corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: installerContainerName("ops-uid-1")}},
				terminatedStatus(125, "")),
			expectDone:       false,
			expectReason:     agentsv1alpha1.SandboxUpgradingReasonInstallingFailed,
			expectMsgContain: "namespace targeting",
		},
		{
			name: "exit 126 reports non-relocatable toolchain",
			pod: podWithEphemeral(
				&corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: installerContainerName("ops-uid-1")}},
				terminatedStatus(126, "")),
			expectDone:       false,
			expectReason:     agentsv1alpha1.SandboxUpgradingReasonInstallingFailed,
			expectMsgContain: "/proc/1/root",
		},
		{
			name: "generic non-zero exit reports truncated message",
			pod: podWithEphemeral(
				&corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: installerContainerName("ops-uid-1")}},
				terminatedStatus(1, "cp: no space left on device")),
			expectDone:       false,
			expectReason:     agentsv1alpha1.SandboxUpgradingReasonInstallingFailed,
			expectMsgContain: "no space left on device",
		},
		{
			name: "image pull backoff keeps waiting within the deadline",
			pod: podWithEphemeral(
				&corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: installerContainerName("ops-uid-1")}},
				&corev1.ContainerStatus{
					Name:  installerContainerName("ops-uid-1"),
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
				}),
			lastTransition: now,
			expectDone:     false,
			expectReason:   agentsv1alpha1.SandboxUpgradingReasonInstalling,
		},
		{
			name: "image pull backoff past the deadline fails",
			pod: podWithEphemeral(
				&corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: installerContainerName("ops-uid-1")}},
				&corev1.ContainerStatus{
					Name:  installerContainerName("ops-uid-1"),
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
				}),
			lastTransition:   metav1.NewTime(time.Now().Add(-2 * time.Hour)),
			expectDone:       false,
			expectReason:     agentsv1alpha1.SandboxUpgradingReasonInstallingFailed,
			expectMsgContain: "timeoutSeconds",
		},
		{
			name: "running past the deadline fails",
			pod: podWithEphemeral(
				&corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: installerContainerName("ops-uid-1")}},
				&corev1.ContainerStatus{
					Name:  installerContainerName("ops-uid-1"),
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-2 * time.Hour))}},
				}),
			lastTransition:   metav1.NewTime(time.Now().Add(-2 * time.Hour)),
			expectDone:       false,
			expectReason:     agentsv1alpha1.SandboxUpgradingReasonInstallingFailed,
			expectMsgContain: "timeoutSeconds",
		},
		{
			name: "operator timeout is honoured",
			installer: &agentsv1alpha1.InstallerSpec{
				Image:          "installer:1.4.0",
				TimeoutSeconds: ptr.To[int32](1),
			},
			pod: podWithEphemeral(
				&corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: installerContainerName("ops-uid-1")}},
				&corev1.ContainerStatus{
					Name:  installerContainerName("ops-uid-1"),
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-2 * time.Hour))}},
				}),
			lastTransition:   metav1.NewTime(time.Now().Add(-2 * time.Hour)),
			expectDone:       false,
			expectReason:     agentsv1alpha1.SandboxUpgradingReasonInstallingFailed,
			expectMsgContain: "timeoutSeconds=1",
		},
		{
			name:           "conflict on injection returns error without second injection",
			installer:      defaultInstaller(),
			pod:            newInstallerRunningPod(),
			injectionError: apierrors.NewConflict(corev1.Resource("pods"), "test-sandbox", fmt.Errorf("object was modified")),
			expectErr:      true,
			expectPatches:  1,
			expectReason:   agentsv1alpha1.SandboxUpgradingReasonInstalling,
		},
		{
			name:           "transient injection error stays retryable",
			pod:            newInstallerRunningPod(),
			injectionError: apierrors.NewInternalError(fmt.Errorf("temporary API failure")),
			expectErr:      true,
			expectPatches:  1,
			expectReason:   agentsv1alpha1.SandboxUpgradingReasonInstalling,
		},
		{
			name:             "forbidden injection fails permanently",
			pod:              newInstallerRunningPod(),
			injectionError:   apierrors.NewForbidden(corev1.Resource("pods/ephemeralcontainers"), "test-sandbox", fmt.Errorf("permission denied")),
			expectPatches:    1,
			expectReason:     agentsv1alpha1.SandboxUpgradingReasonInstallingFailed,
			expectMsgContain: "permission denied",
		},
		{
			name: "invalid injection fails permanently",
			pod:  newInstallerRunningPod(),
			injectionError: apierrors.NewInvalid(corev1.SchemeGroupVersion.WithKind("Pod").GroupKind(), "test-sandbox", field.ErrorList{
				field.Invalid(field.NewPath("spec", "ephemeralContainers"), "installer", "invalid installer configuration"),
			}),
			expectPatches:    1,
			expectReason:     agentsv1alpha1.SandboxUpgradingReasonInstallingFailed,
			expectMsgContain: "invalid installer configuration",
		},
		{
			name: "pod readiness wait cannot exceed the deadline",
			pod: func() *corev1.Pod {
				p := newInstallerRunningPod()
				p.Status.Phase = corev1.PodPending
				return p
			}(),
			lastTransition:   metav1.NewTime(time.Now().Add(-2 * time.Hour)),
			expectReason:     agentsv1alpha1.SandboxUpgradingReasonInstallingFailed,
			expectMsgContain: "timeoutSeconds",
		},
		{
			name: "target readiness wait cannot exceed the deadline",
			pod: func() *corev1.Pod {
				p := newInstallerRunningPod()
				p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
				return p
			}(),
			lastTransition:   metav1.NewTime(time.Now().Add(-2 * time.Hour)),
			expectReason:     agentsv1alpha1.SandboxUpgradingReasonInstallingFailed,
			expectMsgContain: "timeoutSeconds",
		},
		{
			name:             "expired attempt never tries another injection",
			pod:              newInstallerRunningPod(),
			injectionError:   apierrors.NewConflict(corev1.Resource("pods"), "test-sandbox", fmt.Errorf("object was modified")),
			lastTransition:   metav1.NewTime(time.Now().Add(-2 * time.Hour)),
			expectReason:     agentsv1alpha1.SandboxUpgradingReasonInstallingFailed,
			expectMsgContain: "timeoutSeconds",
		},
		{
			name: "timely completion is accepted on a delayed reconcile",
			pod: func() *corev1.Pod {
				s := terminatedStatus(0, "")
				s.State.Terminated.FinishedAt = metav1.NewTime(now.Add(-115 * time.Minute))
				return podWithEphemeral(
					&corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: installerContainerName("ops-uid-1")}}, s)
			}(),
			lastTransition: metav1.NewTime(now.Add(-2 * time.Hour)),
			expectDone:     true,
			expectReason:   agentsv1alpha1.SandboxUpgradingReasonInstalling,
		},
		{
			name: "exit zero after the deadline fails",
			pod: podWithEphemeral(
				&corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: installerContainerName("ops-uid-1")}},
				terminatedStatus(0, "")),
			lastTransition:   metav1.NewTime(now.Add(-2 * time.Hour)),
			expectReason:     agentsv1alpha1.SandboxUpgradingReasonInstallingFailed,
			expectMsgContain: "timeoutSeconds",
		},
		{
			name: "existing installer is evaluated even if the target stopped",
			pod: func() *corev1.Pod {
				p := podWithEphemeral(
					&corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: installerContainerName("ops-uid-1")}},
					terminatedStatus(0, ""))
				p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}
				return p
			}(),
			expectDone:   true,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonInstalling,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installer := tt.installer
			if installer == nil {
				installer = defaultInstaller()
			}
			box := newInstallerTestSandbox(installer, "ops-uid-1")
			control, patchCalls := newInstallerTestControl(t, tt.injectionError, tt.pod.DeepCopy(), box.DeepCopy())
			lastTransition := tt.lastTransition
			if lastTransition.IsZero() {
				lastTransition = now
			}
			newStatus, upgradeCond := installingStatus(lastTransition)

			done, err := control.performInstallerUpgrade(context.Background(),
				EnsureFuncArgs{Pod: tt.pod, Box: box, NewStatus: newStatus}, upgradeCond)

			if (err != nil) != tt.expectErr {
				t.Fatalf("performInstallerUpgrade() error = %v, wantErr %v", err, tt.expectErr)
			}
			assert.Equal(t, tt.expectDone, done)
			assert.Equal(t, tt.expectPatches, *patchCalls)
			if tt.expectReason != "" {
				assert.Equal(t, tt.expectReason, upgradeCond.Reason)
			}
			if tt.expectMsgContain != "" {
				assert.Contains(t, upgradeCond.Message, tt.expectMsgContain)
			}
			assert.Equal(t, lastTransition, upgradeCond.LastTransitionTime)
			if tt.expectErr {
				_, retryErr := control.performInstallerUpgrade(t.Context(),
					EnsureFuncArgs{Pod: tt.pod, Box: box, NewStatus: newStatus}, upgradeCond)
				require.Error(t, retryErr)
				assert.Equal(t, lastTransition, upgradeCond.LastTransitionTime)
				assert.Equal(t, 2, *patchCalls)

				upgradeCond.LastTransitionTime = metav1.NewTime(time.Now().Add(-2 * time.Hour))
				done, err := control.performInstallerUpgrade(t.Context(),
					EnsureFuncArgs{Pod: tt.pod, Box: box, NewStatus: newStatus}, upgradeCond)
				require.NoError(t, err)
				assert.False(t, done)
				assert.Equal(t, agentsv1alpha1.SandboxUpgradingReasonInstallingFailed, upgradeCond.Reason)
				assert.Equal(t, 2, *patchCalls)
			}
		})
	}
}

func TestPerformInstallerUpgrade_SingleInjection(t *testing.T) {
	enableInstallerUpdateGate(t)
	installer := &agentsv1alpha1.InstallerSpec{Image: "installer:1.4.0"}

	t.Run("already injected pod is not injected twice", func(t *testing.T) {
		box := newInstallerTestSandbox(installer, "ops-uid-1")
		ecName := installerContainerName("ops-uid-1")
		pod := newInstallerRunningPod()
		pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
			EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: ecName},
			TargetContainerName:      "sandbox",
		}}
		pod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{
			Name:  ecName,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}}

		control, patchCalls := newInstallerTestControl(t, nil, pod.DeepCopy(), box.DeepCopy())
		newStatus := &agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxUpgrading,
			Conditions: []metav1.Condition{{
				Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
				Status:             metav1.ConditionFalse,
				Reason:             agentsv1alpha1.SandboxUpgradingReasonInstalling,
				LastTransitionTime: metav1.Now(),
			}},
		}
		upgradeCond := &newStatus.Conditions[0]

		done, err := control.performInstallerUpgrade(context.Background(),
			EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus}, upgradeCond)
		assert.NoError(t, err)
		assert.False(t, done)
		assert.Equal(t, 0, *patchCalls)
	})

	t.Run("injected container carries the expected fields", func(t *testing.T) {
		box := newInstallerTestSandbox(installer, "ops-uid-1")
		pod := newInstallerRunningPod()

		control, patchCalls := newInstallerTestControl(t, nil, pod.DeepCopy(), box.DeepCopy())
		newStatus := &agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxUpgrading,
			Conditions: []metav1.Condition{{
				Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
				Status:             metav1.ConditionFalse,
				Reason:             agentsv1alpha1.SandboxUpgradingReasonInstalling,
				LastTransitionTime: metav1.Now(),
			}},
		}
		upgradeCond := &newStatus.Conditions[0]

		done, err := control.performInstallerUpgrade(context.Background(),
			EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus}, upgradeCond)
		assert.NoError(t, err)
		assert.False(t, done)
		assert.Equal(t, 1, *patchCalls)

		var updated corev1.Pod
		err = control.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-sandbox"}, &updated)
		assert.NoError(t, err)
		require.Len(t, updated.Spec.EphemeralContainers, 1)
		ec := updated.Spec.EphemeralContainers[0]
		assert.Equal(t, installerContainerName("ops-uid-1"), ec.Name)
		assert.Equal(t, "installer:1.4.0", ec.Image)
		assert.Equal(t, "sandbox", ec.TargetContainerName)
		assert.NotNil(t, ec.SecurityContext)
		assert.Equal(t, []corev1.Capability{"SYS_PTRACE"}, ec.SecurityContext.Capabilities.Add)
		envNames := map[string]string{}
		for _, e := range ec.Env {
			envNames[e.Name] = e.Value
		}
		assert.Equal(t, "/proc/1/root", envNames["SANDBOX_INSTALLER_TARGET_ROOT"])
		assert.Equal(t, "ops-uid-1", envNames["SANDBOX_INSTALLER_PATCH_VERSION"])
	})

	t.Run("nil pod fails the install instead of panicking", func(t *testing.T) {
		box := newInstallerTestSandbox(installer, "ops-uid-1")

		control, patchCalls := newInstallerTestControl(t, nil, box.DeepCopy())
		newStatus := &agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxUpgrading,
			Conditions: []metav1.Condition{{
				Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
				Status:             metav1.ConditionFalse,
				Reason:             agentsv1alpha1.SandboxUpgradingReasonInstalling,
				LastTransitionTime: metav1.Now(),
			}},
		}
		upgradeCond := &newStatus.Conditions[0]

		done, err := control.performInstallerUpgrade(context.Background(),
			EnsureFuncArgs{Pod: nil, Box: box, NewStatus: newStatus}, upgradeCond)
		assert.NoError(t, err)
		assert.False(t, done)
		assert.Equal(t, 0, *patchCalls)
		assert.Equal(t, agentsv1alpha1.SandboxUpgradingReasonInstallingFailed, upgradeCond.Reason)
		assert.Contains(t, upgradeCond.Message, "pod not found")
	})

	t.Run("terminating pod fails the install without injecting", func(t *testing.T) {
		box := newInstallerTestSandbox(installer, "ops-uid-1")
		pod := newInstallerRunningPod()
		deletionTimestamp := metav1.Now()
		pod.DeletionTimestamp = &deletionTimestamp
		pod.Finalizers = []string{"e2e.test/keep-terminating-pod"}

		control, patchCalls := newInstallerTestControl(t, nil, pod.DeepCopy(), box.DeepCopy())
		newStatus := &agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxUpgrading,
			Conditions: []metav1.Condition{{
				Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
				Status:             metav1.ConditionFalse,
				Reason:             agentsv1alpha1.SandboxUpgradingReasonInstalling,
				LastTransitionTime: metav1.Now(),
			}},
		}
		upgradeCond := &newStatus.Conditions[0]

		done, err := control.performInstallerUpgrade(context.Background(),
			EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus}, upgradeCond)
		assert.NoError(t, err)
		assert.False(t, done)
		assert.Equal(t, 0, *patchCalls)
		assert.Equal(t, agentsv1alpha1.SandboxUpgradingReasonInstallingFailed, upgradeCond.Reason)
		assert.Contains(t, upgradeCond.Message, "terminating")
	})
}

func TestEnsureSandboxUpgraded_InstallerUpdate(t *testing.T) {
	enableInstallerUpdateGate(t)
	now := metav1.Now()
	installer := &agentsv1alpha1.InstallerSpec{Image: "installer:1.4.0"}

	newStatusWithReason := func(reason string) *agentsv1alpha1.SandboxStatus {
		return &agentsv1alpha1.SandboxStatus{
			Phase:                 agentsv1alpha1.SandboxUpgrading,
			UpgradingPatchVersion: "ops-uid-1",
			Conditions: []metav1.Condition{{
				Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
				Status:             metav1.ConditionFalse,
				Reason:             reason,
				LastTransitionTime: now,
			}},
		}
	}

	t.Run("ResumeSucceed proceeds to PreUpgrade without a template patch", func(t *testing.T) {
		box := newInstallerTestSandbox(installer, "ops-uid-1")
		box.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger] = agentsv1alpha1.True
		control, _ := newInstallerTestControl(t, nil, newInstallerRunningPod().DeepCopy(), box.DeepCopy())
		newStatus := newStatusWithReason(agentsv1alpha1.SandboxUpgradingReasonResumeSucceed)

		err := control.EnsureSandboxUpgraded(context.Background(), EnsureFuncArgs{
			Pod: newInstallerRunningPod(), Box: box, NewStatus: newStatus,
		})
		assert.NoError(t, err)
		cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
		require.NotNil(t, cond)
		assert.Equal(t, agentsv1alpha1.SandboxUpgradingReasonPreUpgrade, cond.Reason)
	})

	t.Run("ResumeSucceed abandons when the install already completed", func(t *testing.T) {
		box := newInstallerTestSandbox(installer, "ops-uid-1")
		box.Status.CompletedPatchVersion = "ops-uid-1"
		control, _ := newInstallerTestControl(t, nil, newInstallerRunningPod().DeepCopy(), box.DeepCopy())
		newStatus := newStatusWithReason(agentsv1alpha1.SandboxUpgradingReasonResumeSucceed)
		newStatus.CompletedPatchVersion = "ops-uid-1"

		err := control.EnsureSandboxUpgraded(context.Background(), EnsureFuncArgs{
			Pod: newInstallerRunningPod(), Box: box, NewStatus: newStatus,
		})
		assert.NoError(t, err)
		assert.Nil(t, utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading)))
		assert.Equal(t, agentsv1alpha1.SandboxRunning, newStatus.Phase)
	})

	t.Run("PreUpgrade transitions to Installing, skipping Checkpointing", func(t *testing.T) {
		box := newInstallerTestSandbox(installer, "ops-uid-1")
		control, _ := newInstallerTestControl(t, nil, newInstallerRunningPod().DeepCopy(), box.DeepCopy())
		newStatus := newStatusWithReason(agentsv1alpha1.SandboxUpgradingReasonPreUpgrade)

		err := control.EnsureSandboxUpgraded(context.Background(), EnsureFuncArgs{
			Pod: newInstallerRunningPod(), Box: box, NewStatus: newStatus,
		})
		assert.NoError(t, err)
		cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
		require.NotNil(t, cond)
		assert.Equal(t, agentsv1alpha1.SandboxUpgradingReasonInstalling, cond.Reason)
	})

	t.Run("first entry starts the install clock after PreUpgrade finishes", func(t *testing.T) {
		box := newInstallerTestSandbox(installer, "ops-uid-1")
		box.Spec.Lifecycle = &agentsv1alpha1.SandboxLifecycle{
			PreUpgrade: &agentsv1alpha1.UpgradeAction{Exec: &corev1.ExecAction{Command: []string{"backup"}}},
		}
		control, _ := newInstallerTestControl(t, nil, newInstallerRunningPod(), box.DeepCopy())
		var hookFinishedAt time.Time
		control.lifecycleHookFunc = func(context.Context, *agentsv1alpha1.Sandbox, *agentsv1alpha1.UpgradeAction) (int32, string, string, error) {
			hookFinishedAt = time.Now()
			return 0, "", "", nil
		}
		newStatus := box.Status.DeepCopy()
		require.NoError(t, control.EnsureSandboxUpgraded(t.Context(), EnsureFuncArgs{
			Pod: newInstallerRunningPod(), Box: box, NewStatus: newStatus,
		}))
		cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
		require.NotNil(t, cond)
		assert.Equal(t, agentsv1alpha1.SandboxUpgradingReasonInstalling, cond.Reason)
		assert.False(t, hookFinishedAt.IsZero())
		assert.False(t, cond.LastTransitionTime.Time.Before(hookFinishedAt))
		assert.Equal(t, cond.LastTransitionTime.Add(600*time.Second), InstallerUpgradeDeadline(box, newStatus))
	})

	t.Run("Installing exit 0 records completion and moves to PostUpgrade", func(t *testing.T) {
		box := newInstallerTestSandbox(installer, "ops-uid-1")
		ecName := installerContainerName("ops-uid-1")
		pod := newInstallerRunningPod()
		pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
			EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: ecName},
			TargetContainerName:      "sandbox",
		}}
		pod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{
			Name:  ecName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}}
		control, patchCalls := newInstallerTestControl(t, nil, pod.DeepCopy(), box.DeepCopy())
		newStatus := newStatusWithReason(agentsv1alpha1.SandboxUpgradingReasonInstalling)

		err := control.EnsureSandboxUpgraded(context.Background(), EnsureFuncArgs{
			Pod: pod, Box: box, NewStatus: newStatus,
		})
		assert.NoError(t, err)
		cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
		require.NotNil(t, cond)
		assert.Equal(t, agentsv1alpha1.SandboxUpgradingReasonPostUpgrade, cond.Reason)
		assert.Equal(t, "ops-uid-1", newStatus.UpgradingPatchVersion)
		assert.Equal(t, "ops-uid-1", newStatus.CompletedPatchVersion)
		assert.Equal(t, 0, *patchCalls)
	})

	t.Run("Installing non-zero exit fails terminally", func(t *testing.T) {
		box := newInstallerTestSandbox(installer, "ops-uid-1")
		ecName := installerContainerName("ops-uid-1")
		pod := newInstallerRunningPod()
		pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
			EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: ecName},
			TargetContainerName:      "sandbox",
		}}
		pod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{
			Name:  ecName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Message: "boom"}},
		}}
		control, _ := newInstallerTestControl(t, nil, pod.DeepCopy(), box.DeepCopy())
		newStatus := newStatusWithReason(agentsv1alpha1.SandboxUpgradingReasonInstalling)

		err := control.EnsureSandboxUpgraded(context.Background(), EnsureFuncArgs{
			Pod: pod, Box: box, NewStatus: newStatus,
		})
		assert.NoError(t, err)
		cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
		require.NotNil(t, cond)
		assert.Equal(t, agentsv1alpha1.SandboxUpgradingReasonInstallingFailed, cond.Reason)
		assert.Contains(t, cond.Message, "boom")
		assert.Equal(t, agentsv1alpha1.SandboxUpgrading, newStatus.Phase)
	})

	t.Run("PostUpgrade without a hook completes the upgrade", func(t *testing.T) {
		box := newInstallerTestSandbox(installer, "ops-uid-1")
		control, _ := newInstallerTestControl(t, nil, newInstallerRunningPod().DeepCopy(), box.DeepCopy())
		newStatus := newStatusWithReason(agentsv1alpha1.SandboxUpgradingReasonPostUpgrade)
		newStatus.CompletedPatchVersion = "ops-uid-1"

		err := control.EnsureSandboxUpgraded(context.Background(), EnsureFuncArgs{
			Pod: newInstallerRunningPod(), Box: box, NewStatus: newStatus,
		})
		assert.NoError(t, err)
		cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, agentsv1alpha1.SandboxUpgradingReasonSucceeded, cond.Reason)
		assert.Equal(t, agentsv1alpha1.SandboxRunning, newStatus.Phase)
		assert.Equal(t, "ops-uid-1", newStatus.UpgradingPatchVersion)
		assert.Equal(t, "ops-uid-1", newStatus.CompletedPatchVersion)
	})

	t.Run("failed attempts remain terminal", func(t *testing.T) {
		tests := []struct {
			name             string
			reason           string
			completedVersion string
			lateExitZero     bool
		}{
			{
				name:   "PreUpgradeFailed does not replay hooks",
				reason: agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed,
			},
			{
				name:   "InstallingFailed does not inject another container",
				reason: agentsv1alpha1.SandboxUpgradingReasonInstallingFailed,
			},
			{
				name:         "timeout followed by exit zero does not revive the attempt",
				reason:       agentsv1alpha1.SandboxUpgradingReasonInstallingFailed,
				lateExitZero: true,
			},
			{
				name:             "PostUpgradeFailed does not replay hooks",
				reason:           agentsv1alpha1.SandboxUpgradingReasonPostUpgradeFailed,
				completedVersion: "ops-uid-1",
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				box := newInstallerTestSandbox(installer, "ops-uid-1")
				box.Spec.Lifecycle = &agentsv1alpha1.SandboxLifecycle{
					PreUpgrade:  &agentsv1alpha1.UpgradeAction{Exec: &corev1.ExecAction{Command: []string{"backup"}}},
					PostUpgrade: &agentsv1alpha1.UpgradeAction{Exec: &corev1.ExecAction{Command: []string{"restart"}}},
				}
				pod := newInstallerRunningPod()
				if tt.lateExitZero {
					ecName := installerContainerName("ops-uid-1")
					pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
						EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: ecName},
						TargetContainerName:      "sandbox",
					}}
					pod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{
						Name:  ecName,
						State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
					}}
				}
				control, patchCalls := newInstallerTestControl(t, nil, pod.DeepCopy(), box.DeepCopy())
				hookCalls := 0
				control.lifecycleHookFunc = func(context.Context, *agentsv1alpha1.Sandbox, *agentsv1alpha1.UpgradeAction) (int32, string, string, error) {
					hookCalls++
					return 0, "", "", nil
				}

				status := newStatusWithReason(tt.reason)
				status.CompletedPatchVersion = tt.completedVersion
				status.Conditions[0].Message = "attempt failed"
				status.Conditions[0].LastTransitionTime = metav1.NewTime(time.Now().Add(-2 * time.Hour))
				utils.SetSandboxCondition(status, metav1.Condition{
					Type:               string(agentsv1alpha1.SandboxConditionReady),
					Status:             metav1.ConditionFalse,
					Reason:             agentsv1alpha1.SandboxReadyReasonUpgrading,
					Message:            "sandbox is upgrading",
					LastTransitionTime: now,
				})
				box.Status = *status.DeepCopy()
				for i := 0; i < 3; i++ {
					newStatus := box.Status.DeepCopy()
					require.NoError(t, control.EnsureSandboxUpgraded(t.Context(), EnsureFuncArgs{
						Pod: pod, Box: box, NewStatus: newStatus,
					}))
					assert.Equal(t, status, newStatus)
					box.Status = *newStatus.DeepCopy()
				}
				assert.Zero(t, hookCalls)
				assert.Zero(t, *patchCalls)
			})
		}
	})
}
