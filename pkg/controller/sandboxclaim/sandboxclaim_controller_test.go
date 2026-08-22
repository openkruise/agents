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

package sandboxclaim

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/controller/sandboxclaim/core"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"github.com/openkruise/agents/pkg/utils/testutils"
)

func TestReconciler_Reconcile_BasicFlow(t *testing.T) {
	tests := []struct {
		name              string
		claim             *agentsv1alpha1.SandboxClaim
		sandboxSet        *agentsv1alpha1.SandboxSet
		existingSandboxes []*agentsv1alpha1.Sandbox
		expectedPhase     agentsv1alpha1.SandboxClaimPhase
		wantErr           bool
		wantRequeue       bool
	}{
		{
			name: "claim not found",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "nonexistent-claim",
					Namespace: "default",
				},
			},
			expectedPhase: "",
			wantErr:       false,
			wantRequeue:   false,
		},
		{
			name: "sandboxset not found",
			claim: &agentsv1alpha1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-claim",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: agentsv1alpha1.SandboxClaimSpec{
					TemplateName: "nonexistent-sandboxset",
				},
			},
			sandboxSet:    nil, // SandboxSet doesn't exist
			expectedPhase: agentsv1alpha1.SandboxClaimPhaseCompleted,
			wantErr:       false,
			wantRequeue:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = agentsv1alpha1.AddToScheme(scheme)

			objects := []client.Object{}
			if tt.name != "claim not found" {
				objects = append(objects, tt.claim)
			}
			if tt.sandboxSet != nil {
				objects = append(objects, tt.sandboxSet)
			}
			for _, sb := range tt.existingSandboxes {
				objects = append(objects, sb)
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objects...).
				WithStatusSubresource(&agentsv1alpha1.SandboxClaim{}).
				Build()

			fakeRecorder := record.NewFakeRecorder(100)

			reconciler := &Reconciler{
				Client:   fakeClient,
				Scheme:   scheme,
				controls: core.NewClaimControl(fakeClient, fakeRecorder, nil, nil),
				recorder: fakeRecorder,
			}

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      tt.claim.Name,
					Namespace: tt.claim.Namespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)

			if (err != nil) != tt.wantErr {
				t.Errorf("Reconcile() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantRequeue != (result.RequeueAfter > 0 || result.Requeue) {
				t.Errorf("Reconcile() requeue = %v, wantRequeue %v", result, tt.wantRequeue)
			}

			// Verify the claim status if it exists
			if tt.expectedPhase != "" && tt.name != "claim not found" {
				updatedClaim := &agentsv1alpha1.SandboxClaim{}
				err := fakeClient.Get(context.Background(),
					types.NamespacedName{Name: tt.claim.Name, Namespace: tt.claim.Namespace},
					updatedClaim)

				if err != nil {
					t.Fatalf("Failed to get updated claim: %v", err)
				}

				if updatedClaim.Status.Phase != tt.expectedPhase {
					t.Errorf("Reconcile() phase = %v, want %v", updatedClaim.Status.Phase, tt.expectedPhase)
				}
			}
		})
	}
}

func TestReconciler_Reconcile_Claiming(t *testing.T) {
	testutils.InitLogOutput()

	claim := &agentsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-claim",
			Namespace:  "default",
			UID:        "test-uid",
			Generation: 1,
		},
		Spec: agentsv1alpha1.SandboxClaimSpec{
			TemplateName:    "test-sandboxset",
			Replicas:        int32Ptr(2),
			SkipInitRuntime: true,
		},
	}

	sandboxSet := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandboxset",
			Namespace: "default",
		},
	}

	controllerTrue := true
	now := metav1.Now()
	sandbox1 := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-1",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxTemplate:  "test-sandboxset",
				agentsv1alpha1.LabelSandboxIsClaimed: "false",
			},
			CreationTimestamp: now,
			Annotations:       map[string]string{}, // Initialize annotations map
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "agents.kruise.io/v1alpha1",
					Kind:       "SandboxSet",
					Name:       "test-sandboxset",
					UID:        "test-sandboxset-uid",
					Controller: &controllerTrue,
				},
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{
					Type:   string(agentsv1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
					Reason: "PodReady",
				},
			},
			// no pod ip, should be skipped
		},
	}

	sandbox2 := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "sandbox-2",
			Namespace:         "default",
			CreationTimestamp: now,
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxTemplate:  "test-sandboxset",
				agentsv1alpha1.LabelSandboxIsClaimed: "false",
			},
			Annotations: map[string]string{}, // Initialize annotations map
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "agents.kruise.io/v1alpha1",
					Kind:       "SandboxSet",
					Name:       "test-sandboxset",
					UID:        "test-sandboxset-uid",
					Controller: &controllerTrue,
				},
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{
					Type:   string(agentsv1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
					Reason: "PodReady",
				},
			},
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: "1.2.3.4",
			},
		},
	}

	// Create cache with initial objects
	cache, testClient, err := cachetest.NewTestCache(t, claim, sandboxSet, sandbox1, sandbox2)
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// Start cache
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = cache.Run(ctx)
	}()
	time.Sleep(200 * time.Millisecond) // Wait for cache to start

	fakeRecorder := record.NewFakeRecorder(100)

	reconciler := &Reconciler{
		Client:   testClient,
		Scheme:   testClient.Scheme(),
		controls: core.NewClaimControl(testClient, fakeRecorder, cache, nil),
		recorder: fakeRecorder,
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      claim.Name,
			Namespace: claim.Namespace,
		},
	}

	// First reconcile - should transition to Claiming
	_, err = reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("First Reconcile() error = %v", err)
	}

	// Note: requeueAfter = 0 means immediate requeue, but appears as both false
	// The controller will still reconcile immediately

	// Get updated claim
	updatedClaim := &agentsv1alpha1.SandboxClaim{}
	err = testClient.Get(context.Background(),
		types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace},
		updatedClaim)

	if err != nil {
		t.Fatalf("Failed to get updated claim: %v", err)
	}

	if updatedClaim.Status.Phase != agentsv1alpha1.SandboxClaimPhaseClaiming {
		t.Errorf("After first reconcile, phase = %v, want Claiming", updatedClaim.Status.Phase)
	}

	time.Sleep(200 * time.Millisecond)

	// Verify sandboxes are claimed with proper annotations and labels
	allSandboxes := &agentsv1alpha1.SandboxList{}
	err = testClient.List(context.Background(), allSandboxes, client.InNamespace("default"))
	if err != nil {
		t.Fatalf("Failed to list sandboxes: %v", err)
	}

	claimedCount := 0
	for _, sandbox := range allSandboxes.Items {
		if sandbox.Labels[agentsv1alpha1.LabelSandboxIsClaimed] == "true" {
			claimedCount++

			claimTime, exists := sandbox.Annotations[agentsv1alpha1.AnnotationClaimTime]
			if !exists {
				t.Errorf("Claimed sandbox %s missing claim timestamp annotation", sandbox.Name)
			} else {
				parsedTime, err := time.Parse(time.RFC3339, claimTime)
				if err != nil {
					t.Errorf("Sandbox %s has invalid claim timestamp format %q: %v",
						sandbox.Name, claimTime, err)
				}
				if time.Since(parsedTime) > 2*time.Second {
					t.Errorf("Sandbox %s claim timestamp is not recent: %v", sandbox.Name, parsedTime)
				}
			}

			if len(sandbox.OwnerReferences) != 0 {
				t.Errorf("Sandbox %s should have no OwnerReferences after being claimed, got %d",
					sandbox.Name, len(sandbox.OwnerReferences))
			}

			if sandbox.Name != sandbox2.Name {
				t.Errorf("only %s should be claimed, got %s", sandbox2.Name, sandbox.Name)
			}
		}
	}

	if claimedCount == 0 {
		t.Error("Expected at least 1 sandbox to be claimed, got 0")
	}

	t.Logf("Successfully claimed %d sandbox(es)", claimedCount)
}

func TestReconciler_Reconcile_ConditionalRequeue(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)

	t.Run("requeue immediately when sandboxes claimed", func(t *testing.T) {
		// Skip: This test requires cache to be initialized,
		// which is only available in e2e/integration tests
		t.Skip("Requires cache initialization - tested in e2e tests")
	})

	t.Run("requeue with delay when no sandboxes available", func(t *testing.T) {
		// Skip: This test requires cache to be initialized,
		// which is only available in e2e/integration tests
		t.Skip("Requires cache initialization - tested in e2e tests")
	})
}

func TestReconciler_Reconcile_Timeout(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)

	claim := &agentsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-claim",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: agentsv1alpha1.SandboxClaimSpec{
			TemplateName: "test-sandboxset",
			Replicas:     int32Ptr(10),
			ClaimTimeout: &metav1.Duration{Duration: 1 * time.Second}, // Very short timeout
		},
		Status: agentsv1alpha1.SandboxClaimStatus{
			Phase: agentsv1alpha1.SandboxClaimPhaseClaiming,
			ClaimStartTime: &metav1.Time{
				Time: time.Now().Add(-5 * time.Second), // Started 5 seconds ago
			},
			ClaimedReplicas: 3, // Only claimed 3 out of 10
		},
	}

	sandboxSet := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandboxset",
			Namespace: "default",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim, sandboxSet).
		WithStatusSubresource(&agentsv1alpha1.SandboxClaim{}).
		Build()

	fakeRecorder := record.NewFakeRecorder(100)

	reconciler := &Reconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		controls: core.NewClaimControl(fakeClient, fakeRecorder, nil, nil),
		recorder: fakeRecorder,
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      claim.Name,
			Namespace: claim.Namespace,
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	// Get updated claim
	updatedClaim := &agentsv1alpha1.SandboxClaim{}
	err = fakeClient.Get(context.Background(),
		types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace},
		updatedClaim)

	if err != nil {
		t.Fatalf("Failed to get updated claim: %v", err)
	}

	// Should transition to Completed due to timeout
	if updatedClaim.Status.Phase != agentsv1alpha1.SandboxClaimPhaseCompleted {
		t.Errorf("After timeout, phase = %v, want Completed", updatedClaim.Status.Phase)
	}

	// Should have CompletionTime set
	if updatedClaim.Status.CompletionTime == nil {
		t.Error("CompletionTime should be set after timeout")
	}

	// Should not requeue
	if result.Requeue || result.RequeueAfter > 0 {
		t.Errorf("Should not requeue after completion, got %v", result)
	}
}

func TestReconciler_GetControl(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	fakeRecorder := record.NewFakeRecorder(10)

	reconciler := &Reconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		controls: core.NewClaimControl(fakeClient, fakeRecorder, nil, nil),
		recorder: fakeRecorder,
	}

	control := reconciler.getControl()
	if control == nil {
		t.Error("getControl() returned nil")
	}
}

// TestReconciler_SetupWithManager verifies that SetupWithManager registers the
// sandboxclaim controller with a real controller-runtime manager without error.
// The manager is never started, so no apiserver is needed.
func TestReconciler_SetupWithManager(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add clientgo scheme: %v", err)
	}
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add agents scheme: %v", err)
	}
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:0"}, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	r := &Reconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("Reconciler.SetupWithManager: unexpected error: %v", err)
	}
}

// Helper functions
func int32Ptr(i int32) *int32 {
	return &i
}

// newExpectationTestReconciler builds a reconciler over a single claim and its
// SandboxSet, and returns the claim as it was persisted so callers can read the
// resourceVersion the fake client assigned.
func newExpectationTestReconciler(t *testing.T, phase agentsv1alpha1.SandboxClaimPhase) (*Reconciler, *agentsv1alpha1.SandboxClaim, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)

	claim := &agentsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-claim",
			Namespace:  "default",
			UID:        "claim-uid-01",
			Generation: 1,
		},
		Spec: agentsv1alpha1.SandboxClaimSpec{
			TemplateName: "test-sandboxset",
			Replicas:     int32Ptr(1),
		},
		Status: agentsv1alpha1.SandboxClaimStatus{Phase: phase},
	}
	sandboxSet := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sandboxset", Namespace: "default"},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim, sandboxSet).
		WithStatusSubresource(&agentsv1alpha1.SandboxClaim{}).
		Build()
	fakeRecorder := record.NewFakeRecorder(100)

	stored := &agentsv1alpha1.SandboxClaim{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}, stored); err != nil {
		t.Fatalf("failed to read back the claim: %v", err)
	}

	return &Reconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		controls: core.NewClaimControl(fakeClient, fakeRecorder, nil, nil),
		recorder: fakeRecorder,
	}, stored, fakeRecorder
}

func claimRequest() reconcile.Request {
	return reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"},
	}
}

// TestReconciler_Reconcile_ExpectationUnsatisfied pins the resourceVersion gate: while
// the informer is behind the version the controller last wrote, Reconcile must return
// without touching the claim, and must requeue itself rather than waiting only on a
// cache event.
func TestReconciler_Reconcile_ExpectationUnsatisfied(t *testing.T) {
	reconciler, stored, _ := newExpectationTestReconciler(t, "")

	// Expect a version the cache has not reached, then clear it however the test ends.
	ahead := stored.DeepCopy()
	ahead.ResourceVersion = "999999"
	core.ResourceVersionExpectations.Expect(ahead)
	defer core.ResourceVersionExpectations.Delete(ahead)

	result, err := reconciler.Reconcile(context.Background(), claimRequest())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > expectations.ExpectationTimeout {
		t.Fatalf("expected a requeue within the expectation timeout, got %v", result.RequeueAfter)
	}

	after := &agentsv1alpha1.SandboxClaim{}
	if err := reconciler.Get(context.Background(),
		types.NamespacedName{Name: "test-claim", Namespace: "default"}, after); err != nil {
		t.Fatalf("failed to read the claim: %v", err)
	}
	if after.Status.Phase != "" {
		t.Fatalf("phase advanced to %q while the expectation was unsatisfied", after.Status.Phase)
	}
}

// TestReconciler_Reconcile_ExpectationOvertime pins the escape hatch: once the wait
// exceeds ExpectationTimeout the stale expectation is dropped and the reconcile carries
// on, so a lost cache event cannot stall the claim forever. The claim is parked on an
// unknown phase so the assertion stays on the gate itself and not on the claim machinery
// behind it.
func TestReconciler_Reconcile_ExpectationOvertime(t *testing.T) {
	reconciler, stored, _ := newExpectationTestReconciler(t, agentsv1alpha1.SandboxClaimPhase("Bogus"))

	ahead := stored.DeepCopy()
	ahead.ResourceVersion = "999999"
	core.ResourceVersionExpectations.Expect(ahead)
	defer core.ResourceVersionExpectations.Delete(ahead)

	if satisfied, _ := core.ResourceVersionExpectations.IsSatisfied(stored); satisfied {
		t.Fatalf("test setup did not leave an unsatisfied expectation")
	}

	original := expectations.ExpectationTimeout
	expectations.ExpectationTimeout = 0
	defer func() { expectations.ExpectationTimeout = original }()

	result, err := reconciler.Reconcile(context.Background(), claimRequest())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("expected the reconcile to continue past the gate, got RequeueAfter=%v", result.RequeueAfter)
	}
	if satisfied, _ := core.ResourceVersionExpectations.IsSatisfied(stored); !satisfied {
		t.Fatalf("expected the timed-out expectation to be dropped")
	}
}

// TestReconciler_Reconcile_UnknownPhase pins the default arm of the phase switch. A
// phase the controller does not know must stop the reconcile with an event and no
// requeue, so an unrecognised value cannot be driven by whichever Ensure ran last.
func TestReconciler_Reconcile_UnknownPhase(t *testing.T) {
	reconciler, _, recorder := newExpectationTestReconciler(t, agentsv1alpha1.SandboxClaimPhase("Bogus"))

	result, err := reconciler.Reconcile(context.Background(), claimRequest())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("expected no requeue for an unknown phase, got %+v", result)
	}

	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "UnknownPhase") || !strings.Contains(event, "Bogus") {
			t.Fatalf("expected an UnknownPhase warning naming the phase, got %q", event)
		}
	default:
		t.Fatalf("expected an UnknownPhase event to be recorded")
	}
}

// TestReconciler_Reconcile_CompletedPhase covers the Completed arm of the phase switch.
// A completed claim still reconciles, because EnsureClaimCompleted owns the TTL cleanup.
func TestReconciler_Reconcile_CompletedPhase(t *testing.T) {
	reconciler, _, _ := newExpectationTestReconciler(t, agentsv1alpha1.SandboxClaimPhaseCompleted)

	result, err := reconciler.Reconcile(context.Background(), claimRequest())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.Requeue {
		t.Fatalf("expected no immediate requeue for a completed claim, got %+v", result)
	}

	after := &agentsv1alpha1.SandboxClaim{}
	if err := reconciler.Get(context.Background(),
		types.NamespacedName{Name: "test-claim", Namespace: "default"}, after); err != nil {
		t.Fatalf("failed to read the claim: %v", err)
	}
	if after.Status.Phase != agentsv1alpha1.SandboxClaimPhaseCompleted {
		t.Fatalf("expected the phase to stay Completed, got %q", after.Status.Phase)
	}
}

// TestReconciler_UpdateClaimStatus_NoOp pins the equality guard in updateClaimStatus.
// Reconcile calls it on every pass, so an unchanged status must issue no status patch at
// all. Counting patches is the assertion because an identical merge patch is invisible in
// the stored object: the guard exists to keep the write off the wire, not to keep the
// object unchanged.
func TestReconciler_UpdateClaimStatus_NoOp(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)

	claim := &agentsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-claim", Namespace: "default", UID: "claim-uid-01", Generation: 1,
		},
		Spec:   agentsv1alpha1.SandboxClaimSpec{TemplateName: "test-sandboxset", Replicas: int32Ptr(1)},
		Status: agentsv1alpha1.SandboxClaimStatus{Phase: agentsv1alpha1.SandboxClaimPhaseCompleted},
	}

	var statusPatches int
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim).
		WithStatusSubresource(&agentsv1alpha1.SandboxClaim{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string,
				obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if subResourceName == "status" {
					statusPatches++
				}
				return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	reconciler := &Reconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		controls: core.NewClaimControl(fakeClient, record.NewFakeRecorder(10), nil, nil),
		recorder: record.NewFakeRecorder(10),
	}

	stored := &agentsv1alpha1.SandboxClaim{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "test-claim", Namespace: "default"}, stored); err != nil {
		t.Fatalf("failed to read back the claim: %v", err)
	}

	if err := reconciler.updateClaimStatus(context.Background(), stored.Status, stored); err != nil {
		t.Fatalf("updateClaimStatus() error = %v", err)
	}
	if statusPatches != 0 {
		t.Fatalf("an unchanged status issued %d status patch(es)", statusPatches)
	}

	changed := stored.Status.DeepCopy()
	changed.ClaimedReplicas = 7
	if err := reconciler.updateClaimStatus(context.Background(), *changed, stored); err != nil {
		t.Fatalf("updateClaimStatus() error = %v", err)
	}
	if statusPatches != 1 {
		t.Fatalf("expected exactly one status patch for a changed status, got %d", statusPatches)
	}
}
