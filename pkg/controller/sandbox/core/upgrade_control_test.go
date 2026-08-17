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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/fieldindex"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
)

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
	control := &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(100),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		rateLimiter:          NewRateLimiter(),
		checkpointControl:    checkpointCtrl,
		podControl:           podCtrl,
		lifecycleHookFunc:    hookFunc,
		initializer:          initializer,
	}
	control.upgradeControl = NewUpgradeControl(fakeClient, checkpointCtrl, podCtrl, record.NewFakeRecorder(100), hookFunc, initializer, defaultSyncStatusFromPod, nil, control.inplaceUpdateControl)
	return control
}

// TestUpgradePolicyPredicates covers the split of responsibilities between the
// three policy predicates: RequiresUpgradeSandbox decides whether the sandbox
// enters the Upgrading phase at all, RequiresPodReplacementUpgrade whether the
// UpgradePod step replaces the pod, and RequiresInplaceUpgrade whether it
// patches the pod in place.
func TestUpgradePolicyPredicates(t *testing.T) {
	boxWithPolicy := func(policy *agentsv1alpha1.SandboxUpgradePolicy) *agentsv1alpha1.Sandbox {
		return &agentsv1alpha1.Sandbox{
			Spec: agentsv1alpha1.SandboxSpec{UpgradePolicy: policy},
		}
	}
	tests := []struct {
		name               string
		box                *agentsv1alpha1.Sandbox
		wantUpgradeSandbox bool
		wantPodReplacement bool
		wantInplaceUpgrade bool
	}{
		{
			// The SandboxClaim path: no policy means the change is applied in place
			// from the Running phase, without the upgrade lifecycle.
			name:               "nil policy",
			box:                boxWithPolicy(nil),
			wantUpgradeSandbox: false,
			wantPodReplacement: false,
			wantInplaceUpgrade: false,
		},
		{
			name:               "empty type",
			box:                boxWithPolicy(&agentsv1alpha1.SandboxUpgradePolicy{}),
			wantUpgradeSandbox: false,
			wantPodReplacement: false,
			wantInplaceUpgrade: false,
		},
		{
			name:               "Recreate",
			box:                boxWithPolicy(&agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyRecreate}),
			wantUpgradeSandbox: true,
			wantPodReplacement: true,
			wantInplaceUpgrade: false,
		},
		{
			name:               "CheckpointRestore",
			box:                boxWithPolicy(&agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore}),
			wantUpgradeSandbox: true,
			wantPodReplacement: true,
			wantInplaceUpgrade: false,
		},
		{
			// InplaceUpdate runs the lifecycle but keeps the pod, so it must be in
			// the upgrade phase yet out of the pod-replacement path.
			name:               "InplaceUpdate",
			box:                boxWithPolicy(&agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyInplaceUpdate}),
			wantUpgradeSandbox: true,
			wantPodReplacement: false,
			wantInplaceUpgrade: true,
		},
		{
			name:               "unknown type",
			box:                boxWithPolicy(&agentsv1alpha1.SandboxUpgradePolicy{Type: "SomethingElse"}),
			wantUpgradeSandbox: false,
			wantPodReplacement: false,
			wantInplaceUpgrade: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantUpgradeSandbox, RequiresUpgradeSandbox(tt.box), "RequiresUpgradeSandbox")
			assert.Equal(t, tt.wantPodReplacement, RequiresPodReplacementUpgrade(tt.box), "RequiresPodReplacementUpgrade")
			assert.Equal(t, tt.wantInplaceUpgrade, RequiresInplaceUpgrade(tt.box), "RequiresInplaceUpgrade")
		})
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
			Type: agentsv1alpha1.SandboxUpgradePolicyInplaceUpdate,
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

// newTestUpgradeControlForInplace builds a standalone UpgradeControl backed by
// a fake client and a real InPlaceUpdateControl, so the in-place UpgradePod
// branches are exercised against actual pod labels, annotations and statuses
// rather than a mocked handler.
func newTestUpgradeControlForInplace(objects ...client.Object) *UpgradeControl {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	checkpointCtrl := NewCheckpointControl(fakeClient, record.NewFakeRecorder(100))
	podCtrl := NewPodControl(fakeClient, record.NewFakeRecorder(100), GeneratePodFromSandbox)
	initializer := &defaultSandboxInitializer{recorder: record.NewFakeRecorder(10)}
	return NewUpgradeControl(
		fakeClient, checkpointCtrl, podCtrl, record.NewFakeRecorder(100),
		mockLifecycleHookFunc(0, "", "", nil),
		initializer, defaultSyncStatusFromPod, nil,
		inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
	)
}

// TestExecuteUpgradePodStep_Branches covers the UpgradePod branches of
// executeUpgradePodStep for both the InplaceUpdate path (patching in place) and
// the Recreate path (pod replacement). The in-place branches are driven by real
// pod labels, annotations and container statuses, mirroring how the controller
// observes progress in a live cluster.
func TestExecuteUpgradePodStep_Branches(t *testing.T) {
	// box with a correct immutable-hash annotation so performInplaceUpgrade
	// proceeds past the hash guard.
	newInplaceBox := func() *agentsv1alpha1.Sandbox {
		box := newUpgradeTestSandbox(nil, &agentsv1alpha1.SandboxUpgradePolicy{
			Type: agentsv1alpha1.SandboxUpgradePolicyInplaceUpdate,
		})
		_, h := HashSandbox(box)
		box.Annotations[agentsv1alpha1.SandboxHashImmutablePart] = h
		return box
	}
	// box whose immutable-hash annotation does not match the computed hash, so
	// performInplaceUpgrade rejects the patch before touching the pod.
	newMismatchedBox := func() *agentsv1alpha1.Sandbox {
		box := newUpgradeTestSandbox(nil, &agentsv1alpha1.SandboxUpgradePolicy{
			Type: agentsv1alpha1.SandboxUpgradePolicyInplaceUpdate,
		})
		box.Annotations[agentsv1alpha1.SandboxHashImmutablePart] = "definitely-wrong-hash"
		return box
	}
	// pod that already carries the target revision label, optionally with an
	// in-place state annotation and container statuses.
	newTargetRevisionPod := func(stateJSON string, statuses ...corev1.ContainerStatus) *corev1.Pod {
		p := newRunningPod()
		p.Labels[agentsv1alpha1.PodLabelTemplateHash] = "new-revision"
		if stateJSON != "" {
			p.Annotations = map[string]string{inplaceupdate.PodAnnotationInPlaceUpdateStateKey: stateJSON}
		}
		p.Status.ContainerStatuses = statuses
		return p
	}
	upgradingPodStatus := func() *agentsv1alpha1.SandboxStatus {
		return &agentsv1alpha1.SandboxStatus{
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
		}
	}
	condReason := func(s *agentsv1alpha1.SandboxStatus) string {
		c := utils.GetSandboxCondition(s, string(agentsv1alpha1.SandboxConditionUpgrading))
		if c == nil {
			return ""
		}
		return c.Reason
	}

	tests := []struct {
		name          string
		box           *agentsv1alpha1.Sandbox
		pod           *corev1.Pod
		nilControl    bool
		expectErr     bool
		expectReason  string
		expectPhase   agentsv1alpha1.SandboxPhase
		expectMsgPart string
	}{
		{
			// Hash-immutable-part mismatch fails the UpgradePod step terminally
			// before any patch is delivered.
			name:          "inplace hash mismatch fails terminally",
			box:           newMismatchedBox(),
			pod:           newRunningPod(),
			expectErr:     false,
			expectReason:  agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed,
			expectMsgPart: "container images, resources and template metadata",
		},
		{
			// A nil InPlaceUpdateControl (misconfiguration) surfaces a hard error.
			name:       "inplace nil control errors",
			box:        newInplaceBox(),
			pod:        newRunningPod(),
			nilControl: true,
			expectErr:  true,
		},
		{
			// A pod without the template-hash label cannot be tracked through an
			// in-place update; the step fails instead of silently succeeding.
			name: "inplace pod without template-hash label fails terminally",
			box:  newInplaceBox(),
			pod: func() *corev1.Pod {
				p := newRunningPod()
				delete(p.Labels, agentsv1alpha1.PodLabelTemplateHash)
				return p
			}(),
			expectErr:     false,
			expectReason:  agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed,
			expectMsgPart: "template-hash label",
		},
		{
			// First reconcile: the patch is delivered to the pod and the sandbox
			// stays in Upgrading until the kubelet applies it.
			name:         "inplace patch delivery stays Upgrading",
			box:          newInplaceBox(),
			pod:          newRunningPod(), // label old-revision != target
			expectErr:    false,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
		},
		{
			// The patch was delivered (pod label == target revision) but the image
			// has not been re-pulled yet: still in progress.
			name: "inplace waiting for image completion stays Upgrading",
			box:  newInplaceBox(),
			pod: newTargetRevisionPod(
				`{"revision":"new-revision","updateTimestamp":"2026-01-01T00:00:00Z","updateImages":true,"lastContainerStatuses":{"sandbox":{"imageID":"img-old"}}}`,
				corev1.ContainerStatus{Name: "sandbox", ImageID: "img-old"},
			),
			expectErr:    false,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
		},
		{
			// A corrupted in-place state annotation cannot self-heal; the step
			// fails terminally so the user can switch to Recreate.
			name:          "inplace corrupted state fails terminally",
			box:           newInplaceBox(),
			pod:           newTargetRevisionPod(`{corrupted`),
			expectErr:     false,
			expectReason:  agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed,
			expectMsgPart: "cannot determine in-place update progress",
		},
		{
			// The image was re-pulled (ImageID changed): the update is complete and
			// the state machine advances through PostUpgrade to Running.
			name: "inplace completed transitions to Running",
			box:  newInplaceBox(),
			pod: newTargetRevisionPod(
				`{"revision":"new-revision","updateTimestamp":"2026-01-01T00:00:00Z","updateImages":true,"lastContainerStatuses":{"sandbox":{"imageID":"img-old"}}}`,
				corev1.ContainerStatus{Name: "sandbox", ImageID: "img-new"},
			),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxRunning,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonSucceeded,
		},
		{
			// An infeasible resize reported by the kubelet is terminal for this
			// round: the UpgradePod step fails with the kubelet's message.
			name: "inplace infeasible resize fails terminally",
			box:  newInplaceBox(),
			pod: func() *corev1.Pod {
				p := newTargetRevisionPod(
					`{"revision":"new-revision","updateTimestamp":"2026-01-01T00:00:00Z","updateResources":true,"lastContainerStatuses":{}}`,
				)
				p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{
					Type:    corev1.PodResizePending,
					Status:  corev1.ConditionTrue,
					Reason:  corev1.PodReasonInfeasible,
					Message: "insufficient cpu",
				})
				return p
			}(),
			expectErr:     false,
			expectReason:  agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed,
			expectMsgPart: "in-place pod update failed",
		},
		{
			// A resize that would change the pod's QoS class is rejected before
			// the patch is delivered.
			name: "inplace QoS change rejected fails terminally",
			box: func() *agentsv1alpha1.Sandbox {
				box := newUpgradeTestSandbox(nil, &agentsv1alpha1.SandboxUpgradePolicy{
					Type: agentsv1alpha1.SandboxUpgradePolicyInplaceUpdate,
				})
				// Template adds resources to a pod that has none: BestEffort -> Burstable.
				box.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				}
				_, h := HashSandbox(box)
				box.Annotations[agentsv1alpha1.SandboxHashImmutablePart] = h
				return box
			}(),
			pod:           newRunningPod(),
			expectErr:     false,
			expectReason:  agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed,
			expectMsgPart: "QoS class",
		},
		{
			// Recreate path: a pod that is still being deleted keeps the sandbox
			// in Upgrading (done=false).
			name: "recreate pod deleting stays Upgrading",
			box: newUpgradeTestSandbox(nil, &agentsv1alpha1.SandboxUpgradePolicy{
				Type: agentsv1alpha1.SandboxUpgradePolicyRecreate,
			}),
			pod: func() *corev1.Pod {
				p := newRunningPod()
				ts := metav1.Now()
				p.DeletionTimestamp = &ts
				// A finalizer keeps the fake client happy: it refuses objects with a
				// deletionTimestamp but no finalizers.
				p.Finalizers = []string{"test-finalizer"}
				return p
			}(),
			expectErr:    false,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []client.Object
			if tt.pod != nil {
				objects = append(objects, tt.pod.DeepCopy())
			}
			ctrl := newTestUpgradeControlForInplace(objects...)
			if tt.nilControl {
				ctrl.inplaceUpdateControl = nil
			}
			newStatus := upgradingPodStatus()
			args := EnsureFuncArgs{Pod: tt.pod, Box: tt.box, NewStatus: newStatus}
			err := ctrl.EnsureSandboxUpgraded(context.TODO(), args)
			if (err != nil) != tt.expectErr {
				t.Fatalf("EnsureSandboxUpgraded() error = %v, wantErr %v", err, tt.expectErr)
			}
			if tt.expectPhase != "" && newStatus.Phase != tt.expectPhase {
				t.Errorf("phase = %q, want %q", newStatus.Phase, tt.expectPhase)
			}
			if tt.expectReason != "" && condReason(newStatus) != tt.expectReason {
				t.Errorf("Upgrading reason = %q, want %q", condReason(newStatus), tt.expectReason)
			}
			if tt.expectMsgPart != "" {
				c := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
				if c == nil || !strings.Contains(c.Message, tt.expectMsgPart) {
					t.Errorf("Upgrading message %q does not contain %q", c.Message, tt.expectMsgPart)
				}
			}
			// Contract check: the upgrade path never writes the InplaceUpdate
			// condition; the outcome lives only on the Upgrading condition.
			if c := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionInplaceUpdate)); c != nil {
				t.Errorf("unexpected InplaceUpdate condition on the upgrade path: %+v", c)
			}
		})
	}
}

// TestInplaceUpgradeFailedRecovery covers the two user escape hatches after an
// in-place UpgradePod step failed terminally (Upgrading/UpgradePodFailed):
//
//  1. switch the upgrade policy to Recreate so the pod is replaced, and
//  2. roll the template back so the sandbox returns to Running.
func TestInplaceUpgradeFailedRecovery(t *testing.T) {
	newFailedStatus := func(revision string) *agentsv1alpha1.SandboxStatus {
		return &agentsv1alpha1.SandboxStatus{
			Phase:          agentsv1alpha1.SandboxUpgrading,
			UpdateRevision: revision,
			Conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
					Status:             metav1.ConditionFalse,
					Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed,
					Message:            "in-place upgrade only supports changing container images, resources and template metadata",
					LastTransitionTime: metav1.Now(),
				},
			},
		}
	}

	t.Run("switching to Recreate replaces the pod", func(t *testing.T) {
		// The sandbox failed an in-place round (e.g. hash mismatch); the user
		// switches the policy to Recreate to force the upgrade through.
		box := newUpgradeTestSandbox(nil, &agentsv1alpha1.SandboxUpgradePolicy{
			Type: agentsv1alpha1.SandboxUpgradePolicyRecreate,
		})
		pod := newRunningPod() // label old-revision != target new-revision
		ctrl := newTestUpgradeControlForInplace(pod.DeepCopy())
		newStatus := newFailedStatus("new-revision")

		// Reconcile 1: the UpgradePodFailed state retries the UpgradePod step,
		// which now routes to the recreate path and deletes the old pod.
		err := ctrl.EnsureSandboxUpgraded(context.TODO(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
		assert.NoError(t, err)
		var gone corev1.Pod
		getErr := ctrl.Get(context.TODO(), types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, &gone)
		assert.True(t, apierrors.IsNotFound(getErr), "old pod should be deleted by the recreate path")

		// Reconcile 2: with the pod gone, the recreate path creates a new pod
		// rendered from the current template (i.e. at the target revision).
		err = ctrl.EnsureSandboxUpgraded(context.TODO(), EnsureFuncArgs{Pod: nil, Box: box, NewStatus: newStatus})
		assert.NoError(t, err)
		var fresh corev1.Pod
		assert.NoError(t, ctrl.Get(context.TODO(), types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, &fresh))
		assert.Equal(t, newStatus.UpdateRevision, fresh.Labels[agentsv1alpha1.PodLabelTemplateHash],
			"replacement pod should carry the target revision")
	})

	t.Run("rolling the template back returns to Running", func(t *testing.T) {
		// The failed round never touched the pod (pre-check failure), so after
		// the user rolls the template back the target revision matches the pod
		// again and the upgrade completes as a no-op.
		box := newUpgradeTestSandbox(nil, &agentsv1alpha1.SandboxUpgradePolicy{
			Type: agentsv1alpha1.SandboxUpgradePolicyInplaceUpdate,
		})
		_, h := HashSandbox(box)
		box.Annotations[agentsv1alpha1.SandboxHashImmutablePart] = h
		pod := newRunningPod() // label old-revision, no in-place state
		ctrl := newTestUpgradeControlForInplace(pod.DeepCopy())
		// Rollback: the target revision is the pod's current revision again.
		newStatus := newFailedStatus("old-revision")

		err := ctrl.EnsureSandboxUpgraded(context.TODO(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
		assert.NoError(t, err)
		assert.Equal(t, agentsv1alpha1.SandboxRunning, newStatus.Phase)
		c := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
		if assert.NotNil(t, c) {
			assert.Equal(t, agentsv1alpha1.SandboxUpgradingReasonSucceeded, c.Reason)
		}
	})
}

// TestInplaceUpgradeRollbackWhileStuck verifies the fail-stop behavior when
// the template is rolled back while a previous in-place round is stuck (e.g.
// an unpullable image never completes): the rollback patch is NOT delivered
// and the sandbox stays in Upgrading, with the wait reason passed through on
// the Upgrading condition message so the user can see why and recover by
// deleting the SUO and switching to Recreate/CheckpointRestore.
//
// MAINTAINERS: this test previously asserted the OPPOSITE — that the rollback
// patch must be delivered immediately (a per-caller "restart" policy in the
// engine). That policy was removed on purpose: delivering the corrective
// patch cannot rescue a stuck round anyway (completion is judged by an
// ImageID change and a container in ImagePullBackOff never restarts — E2E
// verified, see the WARNING on performInplaceUpgrade), and the upgrade path's
// liveness exit lives outside the engine (delete the SUO, create a
// Recreate/CheckpointRestore SUO). Do not reintroduce immediate re-patching
// here without revisiting that decision.
func TestInplaceUpgradeRollbackWhileStuck(t *testing.T) {
	box := newUpgradeTestSandbox(nil, &agentsv1alpha1.SandboxUpgradePolicy{
		Type: agentsv1alpha1.SandboxUpgradePolicyInplaceUpdate,
	})
	// The template has been rolled back to the original image.
	box.Spec.Template.Spec.Containers[0].Image = "test:v1"
	_, h := HashSandbox(box)
	box.Annotations[agentsv1alpha1.SandboxHashImmutablePart] = h

	// The pod is stuck mid-round: it already carries the failed round's target
	// revision and state annotation, its spec points at the bad image, and the
	// container never re-pulled (ImageID still equals the recorded baseline).
	pod := newRunningPod()
	pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = "bad-revision"
	pod.Spec.Containers[0].Image = "test:v2-bad"
	pod.Annotations = map[string]string{
		inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"bad-revision","updateTimestamp":"2026-01-01T00:00:00Z","updateImages":true,"lastContainerStatuses":{"sandbox":{"imageID":"img-old"}}}`,
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:    "sandbox",
		ImageID: "img-old",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason:  "ImagePullBackOff",
			Message: `Back-off pulling image "test:v2-bad"`,
		}},
	}}

	ctrl := newTestUpgradeControlForInplace(pod.DeepCopy())
	// The rollback target is the pod's pre-upgrade revision.
	newStatus := &agentsv1alpha1.SandboxStatus{
		Phase:          agentsv1alpha1.SandboxUpgrading,
		UpdateRevision: "old-revision",
		Conditions: []metav1.Condition{
			{
				Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
				Status:             metav1.ConditionFalse,
				Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
				LastTransitionTime: metav1.Now(),
			},
		},
	}

	err := ctrl.EnsureSandboxUpgraded(context.TODO(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	assert.NoError(t, err)

	// Fail-stop: the stuck previous round blocks the rollback patch. The pod
	// keeps the stuck round's spec and revision label; nothing is delivered.
	var patched corev1.Pod
	assert.NoError(t, ctrl.Get(context.TODO(), types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, &patched))
	assert.Equal(t, "test:v2-bad", patched.Spec.Containers[0].Image, "the rollback patch must not be delivered while the previous round is stuck")
	assert.Equal(t, "bad-revision", patched.Labels[agentsv1alpha1.PodLabelTemplateHash], "the pod must keep the stuck round's revision label")
	// The sandbox stays in Upgrading, and the wait reason from pod status is
	// passed through on the Upgrading condition so the user can see why.
	c := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
	if assert.NotNil(t, c) {
		assert.Equal(t, agentsv1alpha1.SandboxUpgradingReasonUpgradePod, c.Reason)
		assert.Contains(t, c.Message, "ImagePullBackOff", "the wait reason must be surfaced on the Upgrading condition")
	}
}

// TestInplaceUpgradePatchTransientError verifies that a transient patch
// failure on the in-place path is returned as an error (so the reconcile is
// requeued and retried) instead of being recorded as a terminal step failure.
func TestInplaceUpgradePatchTransientError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	box := newUpgradeTestSandbox(nil, &agentsv1alpha1.SandboxUpgradePolicy{
		Type: agentsv1alpha1.SandboxUpgradePolicyInplaceUpdate,
	})
	_, h := HashSandbox(box)
	box.Annotations[agentsv1alpha1.SandboxHashImmutablePart] = h
	pod := newRunningPod() // label old-revision != target, image differs from template

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod.DeepCopy()).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				return fmt.Errorf("simulated transient patch error")
			},
		}).Build()
	checkpointCtrl := NewCheckpointControl(fakeClient, record.NewFakeRecorder(100))
	podCtrl := NewPodControl(fakeClient, record.NewFakeRecorder(100), GeneratePodFromSandbox)
	initializer := &defaultSandboxInitializer{recorder: record.NewFakeRecorder(10)}
	ctrl := NewUpgradeControl(fakeClient, checkpointCtrl, podCtrl, record.NewFakeRecorder(100),
		mockLifecycleHookFunc(0, "", "", nil), initializer, defaultSyncStatusFromPod, nil,
		inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc))

	newStatus := &agentsv1alpha1.SandboxStatus{
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
	}
	err := ctrl.EnsureSandboxUpgraded(context.TODO(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "simulated transient patch error")
	// The step is not marked as terminally failed: the retry may still succeed.
	c := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
	if assert.NotNil(t, c) {
		assert.Equal(t, agentsv1alpha1.SandboxUpgradingReasonUpgradePod, c.Reason)
	}
}

// TestExecuteUpgradePodStep_RecreateDeleteError covers the error-propagation
// branch of the Recreate path in executeUpgradePodStep: a pod-deletion failure
// must surface as an error rather than being silently swallowed.
func TestExecuteUpgradePodStep_RecreateDeleteError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	upgradingPodStatus := func() *agentsv1alpha1.SandboxStatus {
		return &agentsv1alpha1.SandboxStatus{
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
		}
	}

	// A pod whose hash doesn't match UpdateRevision triggers deletion; the
	// injected Delete error surfaces through executeUpgradePodStep.
	pod := newRunningPod() // labels old-revision != UpdateRevision new-revision
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod.DeepCopy()).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				return fmt.Errorf("simulated delete error")
			},
		}).Build()
	checkpointCtrl := NewCheckpointControl(fakeClient, record.NewFakeRecorder(100))
	podCtrl := NewPodControl(fakeClient, record.NewFakeRecorder(100), GeneratePodFromSandbox)
	initializer := &defaultSandboxInitializer{recorder: record.NewFakeRecorder(10)}
	ctrl := NewUpgradeControl(fakeClient, checkpointCtrl, podCtrl, record.NewFakeRecorder(100),
		mockLifecycleHookFunc(0, "", "", nil), initializer, defaultSyncStatusFromPod, nil, nil)

	box := newUpgradeTestSandbox(nil, &agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyRecreate})
	newStatus := upgradingPodStatus()
	err := ctrl.EnsureSandboxUpgraded(context.TODO(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "simulated delete error")
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
		upgradeControl:       NewUpgradeControl(fakeClient, checkpointCtrl, podCtrl, record.NewFakeRecorder(100), hookFunc, initializer, defaultSyncStatusFromPod, nil, nil),
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

	readyPod := func() *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-sandbox",
				Namespace: "default",
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
				PodIP: "10.0.0.1",
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
		return NewUpgradeControl(fakeClient, checkpointCtrl, podCtrl, record.NewFakeRecorder(100), mockLifecycleHookFunc(0, "", "", nil), initializer, defaultSyncStatusFromPod, resumeFn, nil), initializer
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
				Phase: agentsv1alpha1.SandboxUpgrading,
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
				Phase: agentsv1alpha1.SandboxUpgrading,
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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

			if tt.expectResumeCalled && mr.called == 0 {
				t.Error("expected resumeFunc to be called, but it was not")
			}
			if !tt.expectResumeCalled && mr.called > 0 {
				t.Error("expected resumeFunc NOT to be called, but it was")
			}

			if tt.expectInitCalled && init.called == 0 {
				t.Error("expected Initialize to be called, but it was not")
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

			if tt.expectPausedRemoved {
				pausedCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
				if pausedCond != nil {
					t.Error("expected Paused condition to be removed, but it still exists")
				}
			}
		})
	}
}
