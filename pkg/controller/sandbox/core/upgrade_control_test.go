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
			Phase:             corev1.PodRunning,
			PodIP:             "10.0.0.1",
			ContainerStatuses: []corev1.ContainerStatus{{Name: "sandbox", Image: "test:v1", ImageID: "img-old", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
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
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Sandbox{}).WithObjects(objects...).Build()
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
	control.upgradeControl = NewUpgradeControl(fakeClient, checkpointCtrl, podCtrl, record.NewFakeRecorder(100), hookFunc, initializer, defaultCommonSyncStatusFromPod, nil, control.inplaceUpdateControl)
	return control
}

// prepareUpgradeStepTest seeds the Sandbox into the fake client for later patches by EnsureSandboxUpgraded.
func prepareUpgradeStepTest(t *testing.T, c client.Client, args EnsureFuncArgs) {
	t.Helper()
	stored := &agentsv1alpha1.Sandbox{}
	err := c.Get(t.Context(), client.ObjectKeyFromObject(args.Box), stored)
	if apierrors.IsNotFound(err) {
		stored = args.Box.DeepCopy()
		stored.Status = *args.NewStatus.DeepCopy()
		require.NoError(t, c.Create(t.Context(), stored))
	} else {
		require.NoError(t, err)
	}
	args.Box.ResourceVersion = stored.ResourceVersion
	args.Box.Status = *stored.Status.DeepCopy()
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
	// Counts the actual hook executions for the "retry after failure" cases.
	retryHookCalls := 0

	tests := []struct {
		name            string
		pod             *corev1.Pod
		box             *agentsv1alpha1.Sandbox
		existingStatus  *agentsv1alpha1.SandboxStatus
		mockHookFunc    LifecycleHookFunc
		expectErr       bool
		expectPhase     agentsv1alpha1.SandboxPhase
		expectCondition map[string]metav1.ConditionStatus
		// When hookCalls is non-nil, verify how many times the hook was called, to
		// distinguish retry from stopping replay.
		hookCalls       *int
		expectHookCalls int
		// A non-empty boxStatusRevision overrides box.Status.UpdateRevision after storing it in the fake client,
		// simulating a new UpdateRevision calculated by calculateStatus.
		boxStatusRevision string
		// expectNoUpgrading asserts that the Upgrading condition was removed.
		expectNoUpgrading bool
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
			name: "preUpgrade failed retries on next reconcile",
			pod:  newRunningPod(),
			box: newUpgradeTestSandbox(&agentsv1alpha1.SandboxLifecycle{
				PreUpgrade: preUpgradeHook,
			}, nil),
			// Previous round already failed: this round re-runs the hook, and if it
			// still fails it stays PreUpgradeFailed.
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:      agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Status: metav1.ConditionFalse, Reason: agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed}},
			},
			mockHookFunc: func(context.Context, *agentsv1alpha1.Sandbox, *agentsv1alpha1.UpgradeAction) (int32, string, string, error) {
				retryHookCalls++
				return 1, "", "still failing", nil
			},
			expectErr:   false,
			expectPhase: agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionReady):     metav1.ConditionFalse,
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
			},
			hookCalls:       &retryHookCalls,
			expectHookCalls: 1,
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
			// An already-failed PostUpgrade re-runs, and if it still fails it stays
			// PostUpgradeFailed.
			mockHookFunc: mockLifecycleHookFunc(1, "", "still failing", nil),
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
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
				p.Spec.Containers[0].Image = "test:v2"
				p.Status.ContainerStatuses[0].Image = "test:v2"
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
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
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
			expectNoUpgrading: true,
		},
		{
			// The annotation was removed but the template was patched (UpdateRevision changed); keep waiting for PreUpgrade instead of abandoning.
			name: "ResumeSucceed with annotation removed but revision changed keeps waiting",
			pod:  newRunningPod(),
			box:  newUpgradeTestSandbox(nil, nil),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:          agentsv1alpha1.SandboxUpgrading,
				UpdateRevision: "new-revision",
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonResumeSucceed,
						LastTransitionTime: now,
					},
				},
			},
			boxStatusRevision: "old-revision",
			mockHookFunc:      mockLifecycleHookFunc(0, "", "", nil),
			expectErr:         false,
			expectPhase:       agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
				string(agentsv1alpha1.SandboxConditionReady):     metav1.ConditionFalse,
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

			prepareUpgradeStepTest(t, control.Client, args)
			if tt.boxStatusRevision != "" {
				tt.box.Status.UpdateRevision = tt.boxStatusRevision
			}
			err := control.EnsureSandboxUpgraded(context.TODO(), args)

			// Check error
			if (err != nil) != tt.expectErr {
				t.Errorf("EnsureSandboxUpgraded() error = %v, wantErr %v", err, tt.expectErr)
				return
			}
			if tt.expectNoUpgrading {
				assert.Nil(t, utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading)), "Upgrading condition should be removed")
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

			if tt.hookCalls != nil {
				require.Equal(t, tt.expectHookCalls, *tt.hookCalls, "actual hook call count does not match expectation")
			}
		})
	}

	for _, strategy := range []agentsv1alpha1.SandboxUpgradePolicyType{
		agentsv1alpha1.SandboxUpgradePolicyRecreate,
		agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore,
		agentsv1alpha1.SandboxUpgradePolicyInplaceUpdate,
	} {
		for _, hook := range []struct {
			name   string
			reason string
			pre    bool
		}{
			{name: "PreUpgrade", reason: agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed, pre: true},
			{name: "PostUpgrade", reason: agentsv1alpha1.SandboxUpgradingReasonPostUpgradeFailed},
		} {
			t.Run(fmt.Sprintf("%s/%s retries and completes", strategy, hook.name), func(t *testing.T) {
				pod := newRunningPod()
				pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = "target"
				// Do not add a shared PodReady gate once PostUpgrade has started.
				if !hook.pre {
					pod.Status.Conditions[0].Status = corev1.ConditionFalse
				}
				box := newUpgradeTestSandbox(&agentsv1alpha1.SandboxLifecycle{}, &agentsv1alpha1.SandboxUpgradePolicy{Type: strategy})
				box.UID = "hook-retry-box"
				box.Generation = 2
				box.Spec.Template.Spec = *pod.Spec.DeepCopy()
				_, immutableHash := HashSandbox(box)
				box.Annotations[agentsv1alpha1.SandboxHashImmutablePart] = immutableHash
				if hook.pre {
					box.Spec.Lifecycle.PreUpgrade = preUpgradeHook
				} else {
					box.Spec.Lifecycle.PostUpgrade = postUpgradeHook
				}
				status := &agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxUpgrading, UpdateRevision: "target",
					Conditions: []metav1.Condition{{
						Type: string(agentsv1alpha1.SandboxConditionUpgrading), Status: metav1.ConditionFalse,
						Reason: hook.reason, ObservedGeneration: 1,
					}},
				}
				cp := newUpgradeCheckpoint("hook-checkpoint", box, agentsv1alpha1.CheckpointSucceeded)
				control := newTestCommonControlWithCheckpointIndex(nil, pod.DeepCopy(), cp)
				control.upgradeControl.initializer = &mockSandboxInitializer{}
				control.upgradeControl.inplaceUpdateControl = control.inplaceUpdateControl
				calls := 0
				control.upgradeControl.lifecycleHookFunc = func(context.Context, *agentsv1alpha1.Sandbox, *agentsv1alpha1.UpgradeAction) (int32, string, string, error) {
					calls++
					// Do not clean up the Checkpoint before the hook succeeds.
					require.NoError(t, control.Get(t.Context(), client.ObjectKeyFromObject(cp), &agentsv1alpha1.Checkpoint{}))
					if calls <= 2 {
						return 1, "", "still failing", nil
					}
					return 0, "ok", "", nil
				}
				args := EnsureFuncArgs{Pod: pod, Box: box, NewStatus: status}
				prepareUpgradeStepTest(t, control.Client, args)
				events := control.upgradeControl.recorder.(*record.FakeRecorder).Events
				for attempt := 1; attempt <= 3; attempt++ {
					box.Status = *status.DeepCopy()
					require.NoError(t, control.EnsureSandboxUpgraded(t.Context(), args))
					require.Equal(t, attempt, calls)
					cond := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionUpgrading))
					require.NotNil(t, cond)
					require.EqualValues(t, 1, cond.ObservedGeneration)
					if attempt <= 2 {
						require.Equal(t, hook.reason, cond.Reason)
						require.Equal(t, "hook failed with exit code 1, stderr: still failing, stdout: ", cond.Message)
						require.Equal(t, agentsv1alpha1.SandboxUpgrading, status.Phase)
						require.Equal(t, metav1.ConditionFalse, utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionReady)).Status)
						select {
						case event := <-events:
							require.Equal(t, fmt.Sprintf("Warning %s %s failed: %s", hook.reason, hook.name, cond.Message), event)
						default:
							t.Fatal("each hook failure must emit the existing event")
						}
						require.Empty(t, events)
						continue
					}
					require.Equal(t, agentsv1alpha1.SandboxUpgradingReasonSucceeded, cond.Reason)
					require.Empty(t, cond.Message)
					require.Equal(t, agentsv1alpha1.SandboxRunning, status.Phase)
					require.Equal(t, metav1.ConditionTrue, utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionReady)).Status)
					var recorded []string
					for len(events) > 0 {
						recorded = append(recorded, <-events)
					}
					require.Contains(t, recorded, "Normal UpgradeSucceeded Upgrade completed successfully")
				}
				err := control.Get(t.Context(), client.ObjectKeyFromObject(cp), &agentsv1alpha1.Checkpoint{})
				if strategy == agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore {
					require.True(t, apierrors.IsNotFound(err), "successful completion must clean up the Checkpoint: %v", err)
				} else {
					require.NoError(t, err)
				}
				ScaleExpectation.DeleteExpectations(GetControllerKey(box))
			})
		}
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
				// spec, actual image and target match, and the Pod is Ready, so the
				// lifecycle can complete.
				p.Spec.Containers[0].Image = "test:v2"
				p.Status.ContainerStatuses[0].Image = "test:v2"
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
						Status:             metav1.ConditionFalse,
						Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
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
				// spec is delivered, but the container has not yet switched to the
				// target image.
				p.Spec.Containers[0].Image = "test:v2"
				p.Labels[agentsv1alpha1.PodLabelTemplateHash] = "new-revision"
				// Keep the old ImageID to verify the wait for configuration effect does
				// not falsely report success.
				if p.Annotations == nil {
					p.Annotations = map[string]string{}
				}
				p.Annotations[inplaceupdate.PodAnnotationInPlaceUpdateStateKey] =
					`{"revision":"new-revision","updateImages":true,"lastContainerStatuses":{"sandbox":{"imageID":"new-image-id","targetImage":"test:v2"}}}`
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
			// Keep the shared Ready=False during upgrade; unapplied configuration must not succeed early.
			expectPhase: agentsv1alpha1.SandboxUpgrading,
			expectCondition: map[string]metav1.ConditionStatus{
				string(agentsv1alpha1.SandboxConditionReady):     metav1.ConditionFalse,
				string(agentsv1alpha1.SandboxConditionUpgrading): metav1.ConditionFalse,
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
				string(agentsv1alpha1.RuntimeInitialized):        metav1.ConditionFalse,
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
				string(agentsv1alpha1.RuntimeInitialized):        metav1.ConditionFalse,
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

			prepareUpgradeStepTest(t, control.Client, args)
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
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Sandbox{}).WithObjects(objects...).Build()
	checkpointCtrl := NewCheckpointControl(fakeClient, record.NewFakeRecorder(100))
	podCtrl := NewPodControl(fakeClient, record.NewFakeRecorder(100), GeneratePodFromSandbox)
	initializer := &defaultSandboxInitializer{recorder: record.NewFakeRecorder(10)}
	return NewUpgradeControl(
		fakeClient, checkpointCtrl, podCtrl, record.NewFakeRecorder(100),
		mockLifecycleHookFunc(0, "", "", nil),
		initializer, defaultCommonSyncStatusFromPod, nil,
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
		p.Spec.Containers[0].Image = "test:v2"
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
		name            string
		box             *agentsv1alpha1.Sandbox
		pod             *corev1.Pod
		expectErr       bool
		expectReason    string
		expectPhase     agentsv1alpha1.SandboxPhase
		expectMsgPart   string
		initStatus      metav1.ConditionStatus
		initErr         error
		initCalls       int
		denyPodGet      bool
		readyAfterPatch corev1.ConditionStatus
		patchErrors     []error
		expectMessage   string
		// startReason overrides the initial Upgrading reason (default UpgradePod) to test re-entry from UpgradePodFailed.
		startReason string
		// createErr is returned by Pod Create; podAbsent means the Pod is absent from the fake client.
		createErr error
		podAbsent bool
		// expectEvent requires an event with this prefix in the current round (only for cases without patchErrors).
		expectEvent string
		// expectInitPending asserts that RuntimeInitialized was reset to False/Pending.
		expectInitPending bool
		// A non-zero expectObservedGen asserts the failure condition's ObservedGeneration.
		expectObservedGen int64
	}{
		{
			name: "inplace conflict preserves retry error and stable message",
			box:  newInplaceBox(), pod: newRunningPod(), initStatus: metav1.ConditionFalse,
			patchErrors: []error{
				apierrors.NewConflict(corev1.Resource("pods"), "test-sandbox", fmt.Errorf("resourceVersion=101")),
				apierrors.NewConflict(corev1.Resource("pods"), "test-sandbox", fmt.Errorf("resourceVersion=202")),
			},
			expectErr: true, expectReason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
			expectMessage: "cannot deliver in-place update: Conflict (HTTP 409)",
		},
		{
			name: "inplace timeout preserves retry error and stable message",
			box:  newInplaceBox(), pod: newRunningPod(), initStatus: metav1.ConditionFalse,
			patchErrors: []error{
				fmt.Errorf("patch attempt 1: %w", context.DeadlineExceeded),
				fmt.Errorf("patch attempt 2: %w", context.DeadlineExceeded),
			},
			expectErr: true, expectReason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
			expectMessage: "cannot deliver in-place update: timeout",
		},
		{
			name: "inplace forbidden write reports each failure with stable message",
			box:  newInplaceBox(), pod: newRunningPod(), initStatus: metav1.ConditionFalse,
			patchErrors: []error{
				apierrors.NewForbidden(corev1.Resource("pods"), "test-sandbox", fmt.Errorf("PodSpec diff at resourceVersion=101")),
				apierrors.NewForbidden(corev1.Resource("pods"), "test-sandbox", fmt.Errorf("PodSpec diff at resourceVersion=202")),
			},
			expectReason:  agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed,
			expectMessage: "cannot deliver in-place update: Forbidden (HTTP 403)",
		},
		{
			name: "inplace invalid write reports each failure with stable fields",
			box:  newInplaceBox(), pod: newRunningPod(), initStatus: metav1.ConditionFalse,
			patchErrors: []error{
				&apierrors.StatusError{ErrStatus: metav1.Status{
					Reason: metav1.StatusReasonInvalid, Code: 422, Message: "PodSpec diff at resourceVersion=101",
					Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{
						{Field: "spec.containers[0].resources"}, {Field: "spec.containers[0].image"},
					}},
				}},
				&apierrors.StatusError{ErrStatus: metav1.Status{
					Reason: metav1.StatusReasonInvalid, Code: 422, Message: "PodSpec diff at resourceVersion=202",
					Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{
						{Field: "spec.containers[0].image"}, {Field: "spec.containers[0].resources"}, {Field: "spec.containers[0].image"},
					}},
				}},
			},
			expectReason:  agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed,
			expectMessage: "cannot deliver in-place update: Invalid (HTTP 422); fields: spec.containers[0].image, spec.containers[0].resources",
		},
		{
			name: "inplace metadata written pod loses readiness and blocks initialization and post hook",
			box:  newInplaceBox(), pod: newTargetRevisionPod(""),
			readyAfterPatch: corev1.ConditionFalse, initStatus: metav1.ConditionFalse,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod, expectMsgPart: "CrashLoopBackOff",
		},
		{
			name: "inplace metadata written pod becomes ready and supplies fresh status to initialization and post hook",
			box:  newInplaceBox(), pod: newTargetRevisionPod(""),
			readyAfterPatch: corev1.ConditionTrue, initStatus: metav1.ConditionFalse, initCalls: 1,
			expectPhase: agentsv1alpha1.SandboxRunning, expectReason: agentsv1alpha1.SandboxUpgradingReasonSucceeded,
		},
		{
			// Hash-immutable-part mismatch fails the UpgradePod step terminally
			// before any patch is delivered.
			name: "inplace hash mismatch fails terminally",
			box: func() *agentsv1alpha1.Sandbox {
				box := newMismatchedBox()
				box.Generation = 3
				return box
			}(),
			pod:               newRunningPod(),
			expectErr:         false,
			expectReason:      agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed,
			expectMsgPart:     "container images, resources and template metadata",
			expectEvent:       "Warning " + agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed,
			expectObservedGen: 3,
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
			// The in-place step reports corrupt state as a failure; the current SUO strategy type cannot be switched.
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
			name: "inplace completed without initialization condition initializes and transitions to Running",
			box:  newInplaceBox(),
			pod: newTargetRevisionPod(
				`{"revision":"new-revision","updateTimestamp":"2026-01-01T00:00:00Z","updateImages":true,"lastContainerStatuses":{"sandbox":{"imageID":"img-old"}}}`,
				corev1.ContainerStatus{Name: "sandbox", ImageID: "img-new"},
			),
			initCalls:    1,
			expectErr:    false,
			expectPhase:  agentsv1alpha1.SandboxRunning,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonSucceeded,
			expectEvent:  "Normal UpgradePodInplaceUpdate Pod updated in place successfully",
		},
		{
			name: "inplace applied but not ready waits before initialization and post hook",
			box:  newInplaceBox(),
			pod: func() *corev1.Pod {
				p := newTargetRevisionPod("")
				p.Status.Conditions[0].Status = corev1.ConditionFalse
				return p
			}(),
			initStatus:   metav1.ConditionFalse,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
		},
		{
			name: "inplace resumed pod initializes before post hook",
			box:  newInplaceBox(), pod: newTargetRevisionPod(""),
			initStatus: metav1.ConditionFalse, initCalls: 1,
			expectPhase:  agentsv1alpha1.SandboxRunning,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonSucceeded,
		},
		{
			name: "inplace initialized pod still initializes before post hook",
			box:  newInplaceBox(), pod: newTargetRevisionPod(""),
			initStatus: metav1.ConditionTrue, initCalls: 1,
			expectPhase:  agentsv1alpha1.SandboxRunning,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonSucceeded,
		},
		{
			name: "inplace initialization error is propagated",
			box:  newInplaceBox(), pod: newTargetRevisionPod(""),
			initStatus: metav1.ConditionFalse, initCalls: 1,
			initErr: fmt.Errorf("runtime init failed"), expectErr: true,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
		},
		{
			name: "recreate completion uses observed pod without re-reading it",
			box:  newUpgradeTestSandbox(nil, nil), pod: newTargetRevisionPod(""),
			denyPodGet: true, initCalls: 1,
			expectPhase:  agentsv1alpha1.SandboxRunning,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonSucceeded,
			expectEvent:  "Normal UpgradePodReplaced Pod replaced successfully",
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
				p.Generation = 2
				p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{
					ObservedGeneration: 2,
					Type:               corev1.PodResizePending,
					Status:             corev1.ConditionTrue,
					Reason:             corev1.PodReasonInfeasible,
					Message:            "insufficient cpu",
				})
				return p
			}(),
			expectErr:     false,
			expectReason:  agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed,
			expectMsgPart: "pod resize is infeasible: insufficient cpu",
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
		{
			// The old Pod was deleted: create a Pod at the target revision and reset RuntimeInitialized to Pending,
			// without retaining the old Pod's successful initialization status.
			name: "recreate pod missing creates target pod and resets initialization",
			box:  newUpgradeTestSandbox(nil, nil), pod: nil,
			initStatus:        metav1.ConditionTrue,
			expectReason:      agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
			expectInitPending: true,
		},
		{
			// Return the original create error for controller retries without writing Pending.
			name: "recreate pod create error is propagated without resetting initialization",
			box:  newUpgradeTestSandbox(nil, nil), pod: nil,
			initStatus:   metav1.ConditionTrue,
			createErr:    apierrors.NewForbidden(corev1.Resource("pods"), "test-sandbox", fmt.Errorf("exceeded quota")),
			expectErr:    true,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
		},
		{
			// Treat NotFound from Delete as success when another component deleted the old Pod; recreate it in the next round.
			name: "recreate delete NotFound is treated as deleted",
			box:  newUpgradeTestSandbox(nil, nil), pod: newRunningPod(),
			podAbsent:    true,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
		},
		{
			// The previous startup failure recorded UpgradePodFailed; the same step can complete after Pod readiness recovers.
			name: "recreate retries from UpgradePodFailed and completes when pod is ready",
			box:  newUpgradeTestSandbox(nil, nil), pod: newTargetRevisionPod(""),
			startReason:  agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed,
			initCalls:    1,
			expectPhase:  agentsv1alpha1.SandboxRunning,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonSucceeded,
			expectEvent:  "Normal UpgradePodReplaced Pod replaced successfully",
		},
		{
			// After a previous terminal in-place failure, the same step can complete once a new target is applied.
			name: "inplace retries from UpgradePodFailed and completes when target is applied",
			box:  newInplaceBox(), pod: newTargetRevisionPod(""),
			startReason:  agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed,
			initCalls:    1,
			expectPhase:  agentsv1alpha1.SandboxRunning,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonSucceeded,
			expectEvent:  "Normal UpgradePodInplaceUpdate Pod updated in place successfully",
		},
		{
			// A ready new Pod with failed initialization returns an error for retries without a replacement success event or PostUpgrade.
			name: "recreate initialization error is propagated",
			box:  newUpgradeTestSandbox(nil, nil), pod: newTargetRevisionPod(""),
			initCalls: 1, initErr: fmt.Errorf("runtime init failed"), expectErr: true,
			expectReason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.readyAfterPatch != "" {
				tt.pod.UID = "preserved-pod"
				tt.pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = "old-revision"
				tt.pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "sandbox", Image: "test:v2", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
				if tt.readyAfterPatch == corev1.ConditionTrue {
					tt.pod.Status.Conditions[0].Status = corev1.ConditionFalse
				}
			}
			var objects []client.Object
			if tt.pod != nil && !tt.podAbsent {
				objects = append(objects, tt.pod.DeepCopy())
			}
			ctrl := newTestUpgradeControlForInplace(objects...)
			t.Cleanup(func() { ScaleExpectation.DeleteExpectations(GetControllerKey(tt.box)) })
			recorder := record.NewFakeRecorder(100)
			ctrl.recorder = recorder
			if tt.createErr != nil {
				ctrl.podControl = NewPodControl(interceptor.NewClient(ctrl.Client.(client.WithWatch), interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						if _, ok := obj.(*corev1.Pod); ok {
							return tt.createErr
						}
						return c.Create(ctx, obj, opts...)
					},
				}), record.NewFakeRecorder(10), GeneratePodFromSandbox)
			}
			patchAttempts := 0
			if len(tt.patchErrors) > 0 {
				ctrl.inplaceUpdateControl = inplaceupdate.NewInPlaceUpdateControl(interceptor.NewClient(ctrl.Client.(client.WithWatch), interceptor.Funcs{
					Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
						require.Less(t, patchAttempts, len(tt.patchErrors))
						err := tt.patchErrors[patchAttempts]
						patchAttempts++
						return err
					},
				}), inplaceupdate.DefaultGeneratePatchBodyFunc)
			}
			if tt.readyAfterPatch != "" {
				ctrl.inplaceUpdateControl = inplaceupdate.NewInPlaceUpdateControl(interceptor.NewClient(ctrl.Client.(client.WithWatch), interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						latest := &corev1.Pod{}
						require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), latest))
						latest.Status.Conditions[0].Status = tt.readyAfterPatch
						latest.Status.PodIP = "10.0.0.2"
						if tt.readyAfterPatch == corev1.ConditionFalse {
							latest.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "newly unready"}}
						}
						require.NoError(t, c.Status().Update(ctx, latest))
						return c.Patch(ctx, obj, patch, opts...)
					},
				}), inplaceupdate.DefaultGeneratePatchBodyFunc)
			}
			initializer := &mockSandboxInitializer{err: tt.initErr}
			ctrl.initializer = initializer
			postCalls := 0
			tt.box.Spec.Lifecycle = &agentsv1alpha1.SandboxLifecycle{PostUpgrade: &agentsv1alpha1.UpgradeAction{
				Exec: &corev1.ExecAction{Command: []string{"post"}},
			}}
			ctrl.lifecycleHookFunc = func(_ context.Context, box *agentsv1alpha1.Sandbox, _ *agentsv1alpha1.UpgradeAction) (int32, string, string, error) {
				postCalls++
				require.Equal(t, tt.initCalls, initializer.called, "initialization must precede PostUpgrade")
				if tt.readyAfterPatch != "" {
					require.Equal(t, "10.0.0.2", box.Status.SandboxIp, "PostUpgrade must use the written Pod IP")
					require.Equal(t, types.UID("preserved-pod"), box.Status.PodInfo.PodUID)
					require.NotNil(t, initializer.initializedStatus)
					require.Equal(t, "10.0.0.2", initializer.initializedStatus.PodInfo.PodIP, "synchronize the written Pod status before initialization")
					require.Equal(t, types.UID("preserved-pod"), initializer.initializedStatus.PodInfo.PodUID)
				}
				return 0, "", "", nil
			}
			if tt.denyPodGet {
				// The completion path must use the Pod observed in this round without reading it again.
				ctrl.Client = interceptor.NewClient(ctrl.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*corev1.Pod); ok {
						return apierrors.NewForbidden(corev1.Resource("pods"), key.Name, fmt.Errorf("read denied"))
					}
					return c.Get(ctx, key, obj, opts...)
				}})
			}
			newStatus := upgradingPodStatus()
			if tt.startReason != "" {
				newStatus.Conditions[0].Reason = tt.startReason
				newStatus.Conditions[0].Message = "previous UpgradePod failure"
			}
			if tt.initStatus != "" {
				utils.SetSandboxCondition(newStatus, metav1.Condition{Type: string(agentsv1alpha1.RuntimeInitialized), Status: tt.initStatus, Reason: agentsv1alpha1.SandboxConditionRuntimeInitReasonPending})
			}
			args := EnsureFuncArgs{Pod: tt.pod, Box: tt.box, NewStatus: newStatus}
			prepareUpgradeStepTest(t, ctrl.Client, args)
			err := ctrl.EnsureSandboxUpgraded(context.TODO(), args)
			if (err != nil) != tt.expectErr {
				t.Fatalf("EnsureSandboxUpgraded() error = %v, wantErr %v", err, tt.expectErr)
			}
			if tt.initErr != nil {
				require.ErrorIs(t, err, tt.initErr)
			}
			if len(tt.patchErrors) > 0 && tt.expectErr {
				require.ErrorIs(t, err, tt.patchErrors[0], "the retry error must preserve the original write error chain")
			}
			if tt.createErr != nil {
				require.ErrorIs(t, err, tt.createErr)
				initCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.RuntimeInitialized))
				require.NotNil(t, initCond)
				require.Equal(t, tt.initStatus, initCond.Status, "create failures must not reset initialization status")
			}
			if tt.expectInitPending {
				initCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.RuntimeInitialized))
				require.NotNil(t, initCond)
				require.Equal(t, metav1.ConditionFalse, initCond.Status)
				require.Equal(t, agentsv1alpha1.SandboxConditionRuntimeInitReasonPending, initCond.Reason)
				created := &corev1.Pod{}
				require.NoError(t, ctrl.Get(t.Context(), client.ObjectKeyFromObject(tt.box), created))
				require.Equal(t, "new-revision", created.Labels[agentsv1alpha1.PodLabelTemplateHash])
			}
			require.Equal(t, tt.initCalls, initializer.called)
			if tt.readyAfterPatch != "" {
				stored := &corev1.Pod{}
				require.NoError(t, ctrl.Get(t.Context(), client.ObjectKeyFromObject(tt.pod), stored))
				require.Equal(t, stored, tt.pod, "the Ready gate and PostUpgrade must use the final written Pod")
				require.Equal(t, tt.readyAfterPatch, utils.GetPodCondition(&tt.pod.Status, corev1.PodReady).Status)
				require.Equal(t, "new-revision", tt.pod.Labels[agentsv1alpha1.PodLabelTemplateHash])
				if tt.readyAfterPatch == corev1.ConditionFalse {
					require.Nil(t, initializer.initializedStatus)
					require.Empty(t, newStatus.PodInfo.PodIP)
				}
			}
			ready := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady))
			require.NotNil(t, ready)
			if tt.expectPhase == agentsv1alpha1.SandboxRunning {
				require.Equal(t, 1, postCalls)
				require.Equal(t, metav1.ConditionTrue, ready.Status)
			} else {
				require.Zero(t, postCalls)
				require.Equal(t, metav1.ConditionFalse, ready.Status)
				expectReadyReason := agentsv1alpha1.SandboxReadyReasonUpgrading
				if tt.createErr != nil {
					// CreatePod failures set the Ready reason to PodCreateFailed to identify the creation failure.
					expectReadyReason = agentsv1alpha1.SandboxReadyReasonPodCreateFailed
				}
				require.Equal(t, expectReadyReason, ready.Reason)
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
			if tt.expectMessage != "" {
				c := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
				require.NotNil(t, c)
				require.Equal(t, tt.expectMessage, c.Message)
			}
			if tt.expectObservedGen != 0 {
				c := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
				require.NotNil(t, c)
				require.Equal(t, tt.expectObservedGen, c.ObservedGeneration)
			}
			if len(tt.patchErrors) == 0 {
				var recorded []string
				for len(recorder.Events) > 0 {
					recorded = append(recorded, <-recorder.Events)
				}
				hasPrefix := func(prefix string) bool {
					for _, e := range recorded {
						if strings.HasPrefix(e, prefix) {
							return true
						}
					}
					return false
				}
				if tt.expectEvent != "" {
					require.True(t, hasPrefix(tt.expectEvent), "missing event %q in %v", tt.expectEvent, recorded)
				}
				if tt.expectPhase == agentsv1alpha1.SandboxRunning {
					require.Contains(t, recorded, "Normal UpgradeSucceeded Upgrade completed successfully")
				} else {
					// Incomplete rounds must not emit success events.
					for _, prefix := range []string{"Normal UpgradePodReplaced", "Normal UpgradePodInplaceUpdate", "Normal UpgradeSucceeded"} {
						require.False(t, hasPrefix(prefix), "unexpected event %q in %v", prefix, recorded)
					}
				}
			}
			if len(tt.patchErrors) > 0 {
				require.Empty(t, newStatus.PodInfo, "write failures must skip status synchronization")
				if !tt.expectErr {
					select {
					case event := <-recorder.Events:
						require.Equal(t, "Warning "+agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed+" "+tt.expectMessage, event)
					default:
						t.Fatal("terminal write failures must emit a Warning event")
					}
				}
				require.Empty(t, recorder.Events)
				// Persist the current status, then re-enter the same target with different dynamic error details.
				previousStatus := newStatus.DeepCopy()
				tt.box.Status = *previousStatus.DeepCopy()
				err := ctrl.EnsureSandboxUpgraded(t.Context(), args)
				if tt.expectErr {
					require.ErrorIs(t, err, tt.patchErrors[1])
				} else {
					require.NoError(t, err)
				}
				require.Equal(t, 2, patchAttempts)
				require.Equal(t, previousStatus, newStatus, "dynamic error details must not change the condition for the same target")
				if !tt.expectErr {
					select {
					case event := <-recorder.Events:
						require.Equal(t, "Warning "+agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed+" "+tt.expectMessage, event)
					default:
						t.Fatal("every terminal write failure must emit a Warning event")
					}
				}
				require.Empty(t, recorder.Events)
				require.Zero(t, initializer.called)
				require.Zero(t, postCalls)
				stored := &corev1.Pod{}
				require.NoError(t, ctrl.Get(t.Context(), client.ObjectKeyFromObject(tt.pod), stored))
				require.Equal(t, "old-revision", stored.Labels[agentsv1alpha1.PodLabelTemplateHash])
				require.Equal(t, "test:v1", stored.Spec.Containers[0].Image)
			}
			// Contract check: the upgrade path never writes the InplaceUpdate
			// condition; the outcome lives only on the Upgrading condition.
			if c := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionInplaceUpdate)); c != nil {
				t.Errorf("unexpected InplaceUpdate condition on the upgrade path: %+v", c)
			}
		})
	}
}

// A new operation can apply a rollback target after an earlier image pull
// failure while retaining the original Pod. Completion requires the target
// image to run and the Pod to become Ready.
func TestInplaceUpgradeRollbackWhileStuck(t *testing.T) {
	box := newUpgradeTestSandbox(nil, &agentsv1alpha1.SandboxUpgradePolicy{
		Type: agentsv1alpha1.SandboxUpgradePolicyInplaceUpdate,
	})
	// The new SUO rolls the template back to the original image.
	box.Spec.Template.Spec.Containers[0].Image = "test:v1"
	_, h := HashSandbox(box)
	box.Annotations[agentsv1alpha1.SandboxHashImmutablePart] = h

	// The old image was not pulled successfully, the Pod is not Ready yet, and the
	// ImageID is still the original baseline.
	pod := newRunningPod()
	pod.UID = "preserved-pod"
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
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
	// The new operation targets the image the old Pod originally used.
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

	prepareUpgradeStepTest(t, ctrl.Client, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	err := ctrl.EnsureSandboxUpgraded(context.TODO(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	assert.NoError(t, err)

	// The correct target is delivered; the Pod is not deleted and recreated.
	var patched corev1.Pod
	require.NoError(t, ctrl.Get(t.Context(), client.ObjectKeyFromObject(pod), &patched))
	require.Equal(t, "test:v1", patched.Spec.Containers[0].Image)
	require.Equal(t, "old-revision", patched.Labels[agentsv1alpha1.PodLabelTemplateHash])
	require.Equal(t, pod.UID, patched.UID)
	state, err := inplaceupdate.GetPodInPlaceUpdateState(&patched)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, "old-revision", state.Revision)
	require.True(t, state.UpdateImages)
	require.Equal(t, "img-old", state.LastContainerStatuses["sandbox"].ImageID)
	require.Equal(t, "test:v1", state.LastContainerStatuses["sandbox"].TargetImage)

	// Delivery does not mean it took effect; the real backoff reason is still
	// written into Message.
	c := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
	if assert.NotNil(t, c) {
		assert.Equal(t, agentsv1alpha1.SandboxUpgradingReasonUpgradePod, c.Reason)
		assert.Contains(t, c.Message, "ImagePullBackOff", "the wait reason must be surfaced on the Upgrading condition")
	}

	// Check image completion observation and the Ready gate separately, using independent Pod and Sandbox status for each case.
	for _, tt := range []struct {
		name          string
		imageID       string
		podReady      bool
		wantSucceeded bool
	}{
		{name: "same image ID and not ready waits", imageID: "img-old"},
		{name: "same image ID and ready succeeds", imageID: "img-old", podReady: true, wantSucceeded: true},
		{name: "changed image ID but not ready waits", imageID: "img-new"},
		{name: "changed image ID and ready succeeds", imageID: "img-new", podReady: true, wantSucceeded: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			observedPod := patched.DeepCopy()
			observedPod.Status.ContainerStatuses[0].Image = "test:v1"
			observedPod.Status.ContainerStatuses[0].ImageID = tt.imageID
			observedPod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
			observedPod.Status.ContainerStatuses[0].Ready = tt.podReady
			observedPod.Status.Conditions[0].Status = corev1.ConditionFalse
			if tt.podReady {
				observedPod.Status.Conditions[0].Status = corev1.ConditionTrue
			}
			status := newStatus.DeepCopy()
			require.NoError(t, ctrl.EnsureSandboxUpgraded(t.Context(), EnsureFuncArgs{Pod: observedPod, Box: box, NewStatus: status}))
			upgradeCond := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionUpgrading))
			readyCond := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionReady))
			require.NotNil(t, upgradeCond)
			require.NotNil(t, readyCond)
			require.Nil(t, utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionInplaceUpdate)))
			if tt.wantSucceeded {
				require.Equal(t, agentsv1alpha1.SandboxRunning, status.Phase)
				require.Equal(t, agentsv1alpha1.SandboxUpgradingReasonSucceeded, upgradeCond.Reason)
				require.Equal(t, metav1.ConditionTrue, upgradeCond.Status)
				require.Equal(t, metav1.ConditionTrue, readyCond.Status)
			} else {
				require.Equal(t, agentsv1alpha1.SandboxUpgrading, status.Phase)
				require.Equal(t, agentsv1alpha1.SandboxUpgradingReasonUpgradePod, upgradeCond.Reason)
				require.Equal(t, metav1.ConditionFalse, upgradeCond.Status)
				require.Equal(t, metav1.ConditionFalse, readyCond.Status)
			}
		})
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
		mockLifecycleHookFunc(0, "", "", nil), initializer, defaultCommonSyncStatusFromPod, nil,
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
	prepareUpgradeStepTest(t, ctrl.Client, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	err := ctrl.EnsureSandboxUpgraded(context.TODO(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "simulated transient patch error")
	// The step is not marked as terminally failed: the retry may still succeed.
	c := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
	if assert.NotNil(t, c) {
		assert.Equal(t, agentsv1alpha1.SandboxUpgradingReasonUpgradePod, c.Reason)
		assert.Equal(t, "cannot deliver in-place update: unexpected error (see controller logs for details)", c.Message)
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
		mockLifecycleHookFunc(0, "", "", nil), initializer, defaultCommonSyncStatusFromPod, nil, nil)

	box := newUpgradeTestSandbox(nil, &agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyRecreate})
	newStatus := upgradingPodStatus()
	prepareUpgradeStepTest(t, ctrl.Client, EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
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
		WithStatusSubresource(&agentsv1alpha1.Checkpoint{}, &agentsv1alpha1.Sandbox{}).
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
		upgradeControl:       NewUpgradeControl(fakeClient, checkpointCtrl, podCtrl, record.NewFakeRecorder(100), hookFunc, initializer, defaultCommonSyncStatusFromPod, nil, nil),
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

			prepareUpgradeStepTest(t, control.Client, args)
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
			name: "pod crash loop reports UpgradePodFailed",
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
			name: "terminated container reports UpgradePodFailed",
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

			prepareUpgradeStepTest(t, control.Client, args)
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
			events := control.upgradeControl.recorder.(*record.FakeRecorder).Events
			if tt.expectReason == agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed {
				var message, eventMessage string
				state := tt.pod.Status.ContainerStatuses[0].State
				if state.Waiting != nil {
					message = "container sandbox: CrashLoopBackOff - container is in crash loop"
					eventMessage = "Container sandbox waiting: CrashLoopBackOff - container is in crash loop"
				} else {
					message = "container sandbox: terminated with exit code 1 - Error"
					eventMessage = "Container sandbox terminated with exit code 1 - Error"
				}
				require.Equal(t, message, utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading)).Message)
				select {
				case event := <-events:
					require.Equal(t, "Warning UpgradePodFailed "+eventMessage, event)
				default:
					t.Fatal("recreate startup failures must emit an UpgradePodFailed event")
				}
			}
			require.Empty(t, events)
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
	prepareUpgradeStepTest(t, control.Client, args)
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

	// Directly prepare persisted progress that has already entered UpgradePod, to
	// verify the error on the create path.
	prepareUpgradeStepTest(t, control.Client, EnsureFuncArgs{Box: box, NewStatus: newStatus})
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

// A direct in-place upgrade call with a missing Pod waits without changing status; the dispatcher recreates missing Pods.
func TestPerformInplaceUpgrade_NilPod(t *testing.T) {
	ctrl := newTestUpgradeControlForInplace()
	box := newUpgradeTestSandbox(nil, &agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyInplaceUpdate})
	status := &agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxUpgrading, UpdateRevision: "new-revision"}
	cond := &metav1.Condition{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Status: metav1.ConditionFalse, Reason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod}
	before := status.DeepCopy()
	done, err := ctrl.performInplaceUpgrade(t.Context(), EnsureFuncArgs{Box: box, NewStatus: status}, cond)
	require.NoError(t, err)
	require.False(t, done)
	require.Equal(t, before, status)
	require.Equal(t, agentsv1alpha1.SandboxUpgradingReasonUpgradePod, cond.Reason)
}

// Recording an event with a nil recorder must not panic.
func TestRecordUpgradeEvent_NilRecorder(t *testing.T) {
	ctrl := &UpgradeControl{}
	require.NotPanics(t, func() {
		ctrl.recordUpgradeEvent(newUpgradeTestSandbox(nil, nil), corev1.EventTypeNormal, EventUpgradeSucceeded, "Upgrade completed successfully")
	})
}

// TestEnsureSandboxUpgraded_FullFlow drives the upgrade state machine across rounds in real reconcile order,
// persisting status between rounds to cover complete Recreate and InplaceUpdate happy paths with Pre/Post hooks.
func TestEnsureSandboxUpgraded_FullFlow(t *testing.T) {
	lifecycle := func() *agentsv1alpha1.SandboxLifecycle {
		return &agentsv1alpha1.SandboxLifecycle{
			PreUpgrade:  &agentsv1alpha1.UpgradeAction{Exec: &corev1.ExecAction{Command: []string{"pre"}}},
			PostUpgrade: &agentsv1alpha1.UpgradeAction{Exec: &corev1.ExecAction{Command: []string{"post"}}},
		}
	}
	type flow struct {
		ctrl        *UpgradeControl
		box         *agentsv1alpha1.Sandbox
		status      *agentsv1alpha1.SandboxStatus
		initializer *mockSandboxInitializer
		preCalls    int
		postCalls   int
		postIP      string
	}
	newFlow := func(t *testing.T, policy agentsv1alpha1.SandboxUpgradePolicyType, pod *corev1.Pod) *flow {
		f := &flow{initializer: &mockSandboxInitializer{}}
		f.box = newUpgradeTestSandbox(lifecycle(), &agentsv1alpha1.SandboxUpgradePolicy{Type: policy})
		f.box.UID = "full-flow-box"
		_, h := HashSandbox(f.box)
		f.box.Annotations[agentsv1alpha1.SandboxHashImmutablePart] = h
		f.status = &agentsv1alpha1.SandboxStatus{
			Phase:          agentsv1alpha1.SandboxUpgrading,
			UpdateRevision: "new-revision",
			SandboxIp:      pod.Status.PodIP,
			Conditions: []metav1.Condition{{
				Type: string(agentsv1alpha1.RuntimeInitialized), Status: metav1.ConditionTrue,
				Reason: agentsv1alpha1.SandboxConditionRuntimeInitReasonSucceeded,
			}},
		}
		f.ctrl = newTestUpgradeControlForInplace(pod.DeepCopy())
		f.ctrl.initializer = f.initializer
		f.ctrl.lifecycleHookFunc = func(_ context.Context, box *agentsv1alpha1.Sandbox, action *agentsv1alpha1.UpgradeAction) (int32, string, string, error) {
			switch action.Exec.Command[0] {
			case "pre":
				f.preCalls++
			case "post":
				f.postCalls++
				require.Equal(t, 1, f.initializer.called, "PostUpgrade must run after initialization")
				f.postIP = box.Status.SandboxIp
			}
			return 0, "", "", nil
		}
		prepareUpgradeStepTest(t, f.ctrl.Client, EnsureFuncArgs{Box: f.box, NewStatus: f.status})
		t.Cleanup(func() { ScaleExpectation.DeleteExpectations(GetControllerKey(f.box)) })
		return f
	}
	// reconcile simulates one round using the previously persisted status as box.Status.
	reconcile := func(t *testing.T, f *flow, pod *corev1.Pod) {
		f.box.Status = *f.status.DeepCopy()
		require.NoError(t, f.ctrl.EnsureSandboxUpgraded(t.Context(), EnsureFuncArgs{Pod: pod, Box: f.box, NewStatus: f.status}))
	}
	upgradingReason := func(f *flow) string {
		c := utils.GetSandboxCondition(f.status, string(agentsv1alpha1.SandboxConditionUpgrading))
		require.NotNil(t, c)
		return c.Reason
	}
	drainEvents := func(f *flow) []string {
		events := f.ctrl.recorder.(*record.FakeRecorder).Events
		var recorded []string
		for len(events) > 0 {
			recorded = append(recorded, <-events)
		}
		return recorded
	}
	assertSucceeded := func(t *testing.T, f *flow, podEvent string) {
		require.Equal(t, agentsv1alpha1.SandboxRunning, f.status.Phase)
		c := utils.GetSandboxCondition(f.status, string(agentsv1alpha1.SandboxConditionUpgrading))
		require.NotNil(t, c)
		require.Equal(t, agentsv1alpha1.SandboxUpgradingReasonSucceeded, c.Reason)
		require.Equal(t, metav1.ConditionTrue, c.Status)
		require.Empty(t, c.Message)
		ready := utils.GetSandboxCondition(f.status, string(agentsv1alpha1.SandboxConditionReady))
		require.Equal(t, metav1.ConditionTrue, ready.Status)
		require.Equal(t, agentsv1alpha1.SandboxReadyReasonPodReady, ready.Reason)
		require.Equal(t, 1, f.preCalls)
		require.Equal(t, 1, f.postCalls)
		require.Equal(t, 1, f.initializer.called)
		require.Equal(t, []string{podEvent, "Normal UpgradeSucceeded Upgrade completed successfully"}, drainEvents(f))
	}
	assertWaiting := func(t *testing.T, f *flow) {
		require.Equal(t, agentsv1alpha1.SandboxUpgrading, f.status.Phase)
		require.Equal(t, agentsv1alpha1.SandboxUpgradingReasonUpgradePod, upgradingReason(f))
		ready := utils.GetSandboxCondition(f.status, string(agentsv1alpha1.SandboxConditionReady))
		require.Equal(t, metav1.ConditionFalse, ready.Status)
		require.Zero(t, f.postCalls)
		require.Zero(t, f.initializer.called)
		require.Empty(t, drainEvents(f))
	}

	t.Run("Recreate", func(t *testing.T) {
		oldPod := newRunningPod()
		f := newFlow(t, agentsv1alpha1.SandboxUpgradePolicyRecreate, oldPod)

		// Round 1: run PreUpgrade, then delete the old Pod.
		reconcile(t, f, oldPod)
		require.Equal(t, 1, f.preCalls)
		assertWaiting(t, f)
		err := f.ctrl.Get(t.Context(), client.ObjectKeyFromObject(oldPod), &corev1.Pod{})
		require.True(t, apierrors.IsNotFound(err), "the old Pod must be deleted: %v", err)

		// Round 2: recreate the missing Pod at the target revision and reset initialization to Pending.
		reconcile(t, f, nil)
		assertWaiting(t, f)
		initCond := utils.GetSandboxCondition(f.status, string(agentsv1alpha1.RuntimeInitialized))
		require.Equal(t, metav1.ConditionFalse, initCond.Status)
		require.Equal(t, agentsv1alpha1.SandboxConditionRuntimeInitReasonPending, initCond.Reason)
		newPod := &corev1.Pod{}
		require.NoError(t, f.ctrl.Get(t.Context(), client.ObjectKeyFromObject(f.box), newPod))
		require.Equal(t, "new-revision", newPod.Labels[agentsv1alpha1.PodLabelTemplateHash])

		// Round 3: the new Pod is not Ready yet; keep waiting.
		reconcile(t, f, newPod)
		assertWaiting(t, f)
		require.Equal(t, 1, f.preCalls, "PreUpgrade must run only once")

		// Round 4: the new Pod is Ready; initialize, emit replacement success, run PostUpgrade, and complete.
		newPod.Status = corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIP:      "10.0.0.9",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		}
		require.NoError(t, f.ctrl.Status().Update(t.Context(), newPod))
		reconcile(t, f, newPod)
		assertSucceeded(t, f, "Normal UpgradePodReplaced Pod replaced successfully, proceeding to PostUpgrade")
		require.Equal(t, "10.0.0.9", f.status.SandboxIp)
		require.Equal(t, "10.0.0.9", f.postIP, "PostUpgrade must use the new Pod IP")
	})

	t.Run("InplaceUpdate", func(t *testing.T) {
		pod := newRunningPod()
		pod.UID = "inplace-pod-uid"
		f := newFlow(t, agentsv1alpha1.SandboxUpgradePolicyInplaceUpdate, pod)

		// Round 1: run PreUpgrade, then deliver the target revision in place.
		reconcile(t, f, pod)
		require.Equal(t, 1, f.preCalls)
		assertWaiting(t, f)
		stored := &corev1.Pod{}
		require.NoError(t, f.ctrl.Get(t.Context(), client.ObjectKeyFromObject(pod), stored))
		require.Equal(t, "new-revision", stored.Labels[agentsv1alpha1.PodLabelTemplateHash])
		require.Equal(t, "test:v2", stored.Spec.Containers[0].Image)
		require.NotEmpty(t, stored.Annotations[inplaceupdate.PodAnnotationInPlaceUpdateStateKey])

		// Round 2: kubelet has pulled the new image and the Pod is Ready; initialize, emit in-place success, run PostUpgrade, and complete.
		stored.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "sandbox", Image: "test:v2", ImageID: "img-new",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}
		require.NoError(t, f.ctrl.Status().Update(t.Context(), stored))
		reconcile(t, f, stored)
		assertSucceeded(t, f, "Normal UpgradePodInplaceUpdate Pod updated in place successfully, proceeding to PostUpgrade")
		require.Equal(t, types.UID("inplace-pod-uid"), f.status.PodInfo.PodUID, "in-place upgrades must preserve the original Pod")
		require.Equal(t, pod.Status.PodIP, f.postIP)
	})
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
	resumingEvent := "Normal UpgradeResuming Resuming paused sandbox for upgrade"
	resumedEvent := "Normal UpgradeResumed Sandbox resumed, waiting for template patch"
	resumeWaitMessage := "Resume triggered, waiting for pod readiness"

	// mockResume tracks resume calls and controls behavior.
	type mockResume struct {
		err        error
		setResumed bool
		called     int
	}

	// newControlWithResume creates an UpgradeControl with a mockable resumeFunc
	// and returns the mock initializer so tests can assert call counts.
	newControlWithResume := func(mr *mockResume, initErr error) (*UpgradeControl, *mockSandboxInitializer) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Sandbox{}).Build()
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
		return NewUpgradeControl(fakeClient, checkpointCtrl, podCtrl, record.NewFakeRecorder(100), mockLifecycleHookFunc(0, "", "", nil), initializer, defaultCommonSyncStatusFromPod, resumeFn, nil), initializer
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
		expectMessage       string
		expectEvents        []string
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
			// Do not resume without Paused; wait for the pause flow to supply the condition.
			name: "Resuming without Paused condition waits without resuming",
			pod:  readyPod(),
			box:  baseBox(),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:      agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{resumingCond},
			},
			expectResumeCalled: false,
			expectReason:       agentsv1alpha1.SandboxUpgradingReasonResuming,
		},
		{
			// On first entry after pausing completes, resume and enter ResumeSucceed in the same round.
			name: "paused sandbox first enters upgrade with Paused=True - resumes in the same reconcile",
			pod:  readyPod(),
			box:  baseBox(),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:      agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{pausedTrueCond},
			},
			resumeSetResumed:    true,
			expectResumeCalled:  true,
			expectReason:        agentsv1alpha1.SandboxUpgradingReasonResumeSucceed,
			expectEvents:        []string{resumingEvent, resumedEvent},
			expectPausedRemoved: true,
		},
		{
			// A non-empty Message indicates that the resume event was emitted; do not emit it again while waiting.
			name: "Resuming already triggered does not repeat resume event",
			pod:  notReadyPod(),
			box:  baseBox(),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{
					func() metav1.Condition {
						c := resumingCond
						c.Message = resumeWaitMessage
						return c
					}(),
					pausedTrueCond,
				},
			},
			resumeSetResumed:   true,
			expectResumeCalled: true,
			expectReason:       agentsv1alpha1.SandboxUpgradingReasonResuming,
			expectMessage:      resumeWaitMessage,
		},
		{
			// If resume created a Pod that is not yet observed, wait for the next round without initialization or removing Paused.
			name: "Resuming with Resumed=True but pod missing waits",
			pod:  nil,
			box:  boxWithPreUpgrade(),
			existingStatus: &agentsv1alpha1.SandboxStatus{
				Phase:      agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{resumingCond, pausedTrueCond},
			},
			resumeSetResumed:   true,
			expectResumeCalled: true,
			expectReason:       agentsv1alpha1.SandboxUpgradingReasonResuming,
			expectMessage:      resumeWaitMessage,
			expectEvents:       []string{resumingEvent},
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
			expectMessage:      resumeWaitMessage,
			expectEvents:       []string{resumingEvent},
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
			expectMessage:      resumeWaitMessage,
			expectEvents:       []string{resumingEvent},
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
			expectMessage:      resumeWaitMessage,
			expectEvents:       []string{resumingEvent},
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
			expectMessage:      resumeWaitMessage,
			expectEvents:       []string{resumingEvent},
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
			expectEvents:        []string{resumingEvent, resumedEvent},
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
			expectEvents:        []string{resumingEvent, resumedEvent},
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

			prepareUpgradeStepTest(t, ctrl.Client, args)
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

			if tt.expectInitCalled {
				if init.called == 0 {
					t.Error("expected Initialize to be called, but it was not")
				}
				if init.initializedStatus == nil {
					t.Error("expected Initialize to receive sandbox status")
				} else {
					assert.Equal(t, newPodIP, init.initializedStatus.SandboxIp, "SandboxIp passed to Initialize")
					assert.Equal(t, newPodIP, init.initializedStatus.PodInfo.PodIP, "PodInfo.PodIP passed to Initialize")
					assert.Equal(t, newPodUID, init.initializedStatus.PodInfo.PodUID, "PodInfo.PodUID passed to Initialize")
				}
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
			} else {
				assert.Equal(t, tt.expectReason, upgradingCond.Reason, "Upgrading reason")
				assert.Equal(t, tt.expectMessage, upgradingCond.Message, "Upgrading message")
			}

			events := ctrl.recorder.(*record.FakeRecorder).Events
			var recorded []string
			for len(events) > 0 {
				recorded = append(recorded, <-events)
			}
			assert.Equal(t, tt.expectEvents, recorded, "upgrade events")

			pausedCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
			if tt.expectPausedRemoved {
				assert.Nil(t, pausedCond, "Paused condition should be removed")
			} else if utils.GetSandboxCondition(tt.existingStatus, string(agentsv1alpha1.SandboxConditionPaused)) != nil {
				assert.NotNil(t, pausedCond, "Paused condition should be kept until resume succeeds")
			}
		})
	}
}
