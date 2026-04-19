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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
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
	return &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(100),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
		rateLimiter:          NewRateLimiter(),
		lifecycleHookFunc:    hookFunc,
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
