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

package commit

import (
	"context"
	"fmt"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/commit/core"
	"github.com/openkruise/agents/pkg/utils"
)

func newTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	return scheme
}

type mockControl struct {
	ensureRunningErr    error
	ensureRunningReq    time.Duration
	ensureUpdatedErr    error
	ensureUpdatedReq    time.Duration
	ensureDeletedErr    error
	ensureDeletedReq    time.Duration
	runningCalled       bool
	updatedCalled       bool
	deletedCalled       bool
	setPhaseOnRunning   agentsv1alpha1.CommitPhase
	setPhaseOnUpdated   agentsv1alpha1.CommitPhase
}

func (m *mockControl) EnsureCommitRunning(_ context.Context, args *core.EnsureFuncArgs) (time.Duration, error) {
	m.runningCalled = true
	if m.setPhaseOnRunning != "" {
		args.NewStatus.Phase = m.setPhaseOnRunning
	}
	return m.ensureRunningReq, m.ensureRunningErr
}

func (m *mockControl) EnsureCommitUpdated(_ context.Context, args *core.EnsureFuncArgs) (time.Duration, error) {
	m.updatedCalled = true
	if m.setPhaseOnUpdated != "" {
		args.NewStatus.Phase = m.setPhaseOnUpdated
	}
	return m.ensureUpdatedReq, m.ensureUpdatedErr
}

func (m *mockControl) EnsureCommitDeleted(_ context.Context, _ *core.EnsureFuncArgs) (time.Duration, error) {
	m.deletedCalled = true
	return m.ensureDeletedReq, m.ensureDeletedErr
}

func newTestReconciler(objs ...client.Object) (*CommitReconciler, *mockControl) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(&agentsv1alpha1.Commit{}).Build()
	mock := &mockControl{}
	r := &CommitReconciler{
		Client:   fc,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
		controls: map[string]core.CommitControl{
			core.CommonControlName: mock,
		},
	}
	return r, mock
}

func newCommit(name, namespace string, phase agentsv1alpha1.CommitPhase) *agentsv1alpha1.Commit {
	return &agentsv1alpha1.Commit{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: agentsv1alpha1.CommitSpec{
			PodName:       "test-pod",
			ContainerName: "workspace",
			Image:         "registry.example.com/env:v1",
		},
		Status: agentsv1alpha1.CommitStatus{
			Phase: phase,
		},
	}
}

func TestReconcile_CommitNotFound(t *testing.T) {
	r, _ := newTestReconciler()
	result, err := r.Reconcile(context.TODO(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}
}

func TestReconcile_CommitPending_PodNotFound(t *testing.T) {
	commit := newCommit("test-commit", "default", agentsv1alpha1.CommitPending)
	commit.Finalizers = []string{agentsv1alpha1.CommitFinalizer}
	r, mock := newTestReconciler(commit)

	_, err := r.Reconcile(context.TODO(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-commit", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.runningCalled {
		t.Error("EnsureCommitRunning should not be called when pod is missing")
	}

	// Verify status was updated to Failed
	updated := &agentsv1alpha1.Commit{}
	_ = r.Get(context.TODO(), client.ObjectKey{Name: "test-commit", Namespace: "default"}, updated)
	if updated.Status.Phase != agentsv1alpha1.CommitFailed {
		t.Errorf("expected Failed phase, got %s", updated.Status.Phase)
	}
}

func TestReconcile_CommitPending_PodExists(t *testing.T) {
	commit := newCommit("test-commit", "default", agentsv1alpha1.CommitPending)
	commit.Finalizers = []string{agentsv1alpha1.CommitFinalizer}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	r, mock := newTestReconciler(commit, pod)
	mock.setPhaseOnRunning = agentsv1alpha1.CommitRunning

	_, err := r.Reconcile(context.TODO(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-commit", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mock.runningCalled {
		t.Error("expected EnsureCommitRunning to be called")
	}
}

func TestReconcile_CommitRunning(t *testing.T) {
	commit := newCommit("test-commit", "default", agentsv1alpha1.CommitRunning)
	commit.Finalizers = []string{agentsv1alpha1.CommitFinalizer}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	r, mock := newTestReconciler(commit, pod)
	mock.setPhaseOnUpdated = agentsv1alpha1.CommitSucceeded

	_, err := r.Reconcile(context.TODO(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-commit", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mock.updatedCalled {
		t.Error("expected EnsureCommitUpdated to be called")
	}
}

func TestReconcile_CommitDeleting(t *testing.T) {
	now := metav1.Now()
	commit := newCommit("test-commit", "default", agentsv1alpha1.CommitRunning)
	commit.DeletionTimestamp = &now
	commit.Finalizers = []string{agentsv1alpha1.CommitFinalizer}
	r, mock := newTestReconciler(commit)

	_, err := r.Reconcile(context.TODO(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-commit", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mock.deletedCalled {
		t.Error("expected EnsureCommitDeleted to be called")
	}
}

func TestReconcile_CommitSucceeded_NoTTL(t *testing.T) {
	commit := newCommit("test-commit", "default", agentsv1alpha1.CommitSucceeded)
	r, mock := newTestReconciler(commit)

	result, err := r.Reconcile(context.TODO(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-commit", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for succeeded without TTL")
	}
	if mock.runningCalled || mock.updatedCalled || mock.deletedCalled {
		t.Error("no control methods should be called for terminal phase")
	}
}

func TestReconcile_CommitSucceeded_TTLNotExpired(t *testing.T) {
	ttl := 10 * time.Minute
	completionTime := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	commit := newCommit("test-commit", "default", agentsv1alpha1.CommitSucceeded)
	commit.Spec.Ttl = &metav1.Duration{Duration: ttl}
	commit.Status.CompletionTime = &completionTime
	r, _ := newTestReconciler(commit)

	result, err := r.Reconcile(context.TODO(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-commit", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue for non-expired TTL")
	}
	if result.RequeueAfter > 6*time.Minute {
		t.Errorf("requeue too long: %v", result.RequeueAfter)
	}
}

func TestReconcile_CommitSucceeded_TTLExpired(t *testing.T) {
	ttl := 5 * time.Minute
	completionTime := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	commit := newCommit("test-commit", "default", agentsv1alpha1.CommitSucceeded)
	commit.Spec.Ttl = &metav1.Duration{Duration: ttl}
	commit.Status.CompletionTime = &completionTime
	r, _ := newTestReconciler(commit)

	_, err := r.Reconcile(context.TODO(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-commit", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify commit was deleted
	got := &agentsv1alpha1.Commit{}
	err = r.Get(context.TODO(), client.ObjectKey{Name: "test-commit", Namespace: "default"}, got)
	if err == nil {
		t.Error("expected commit to be deleted after TTL expiry")
	}
}

func TestReconcile_CommitRunning_EnsureUpdatedError(t *testing.T) {
	commit := newCommit("test-commit", "default", agentsv1alpha1.CommitRunning)
	commit.Finalizers = []string{agentsv1alpha1.CommitFinalizer}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	r, mock := newTestReconciler(commit, pod)
	mock.ensureUpdatedErr = fmt.Errorf("job not found")
	mock.setPhaseOnUpdated = agentsv1alpha1.CommitFailed

	_, err := r.Reconcile(context.TODO(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-commit", Namespace: "default"},
	})
	// The error from EnsureCommitUpdated is not returned directly; it triggers status update
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mock.updatedCalled {
		t.Error("expected EnsureCommitUpdated to be called")
	}

	// Verify status was updated to Failed
	updated := &agentsv1alpha1.Commit{}
	_ = r.Get(context.TODO(), client.ObjectKey{Name: "test-commit", Namespace: "default"}, updated)
	if updated.Status.Phase != agentsv1alpha1.CommitFailed {
		t.Errorf("expected Failed phase, got %s", updated.Status.Phase)
	}
}

func TestReconcile_CommitRunning_StatusUnchanged(t *testing.T) {
	// When status hasn't changed (DeepEqual returns true), updateCommitStatus should be a no-op
	commit := newCommit("test-commit", "default", agentsv1alpha1.CommitRunning)
	commit.Finalizers = []string{agentsv1alpha1.CommitFinalizer}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	r, mock := newTestReconciler(commit, pod)
	// Do not set any phase change in mock — status remains CommitRunning (same as commit.Status.Phase)

	result, err := r.Reconcile(context.TODO(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-commit", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mock.updatedCalled {
		t.Error("expected EnsureCommitUpdated to be called")
	}
	// Since status hasn't changed, no patch should happen (no error expected)
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}
}

func TestGetControl_Default(t *testing.T) {
	mock := &mockControl{}
	r := &CommitReconciler{
		controls: map[string]core.CommitControl{
			core.CommonControlName: mock,
		},
	}
	commit := newCommit("test", "default", agentsv1alpha1.CommitPending)

	ctrl, err := r.getControl(commit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ctrl != mock {
		t.Error("expected default common control")
	}
}

func TestGetControl_AnnotationMode(t *testing.T) {
	customMock := &mockControl{}
	r := &CommitReconciler{
		controls: map[string]core.CommitControl{
			core.CommonControlName: &mockControl{},
			"custom":               customMock,
		},
	}
	commit := newCommit("test", "default", agentsv1alpha1.CommitPending)
	commit.Annotations = map[string]string{
		utils.CommitAnnotationModeKey: "custom",
	}

	ctrl, err := r.getControl(commit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ctrl != customMock {
		t.Error("expected custom control from annotation")
	}
}

func TestGetControl_UnknownMode(t *testing.T) {
	r := &CommitReconciler{
		controls: map[string]core.CommitControl{
			core.CommonControlName: &mockControl{},
		},
	}
	commit := newCommit("test", "default", agentsv1alpha1.CommitPending)
	commit.Annotations = map[string]string{
		utils.CommitAnnotationModeKey: "nonexistent",
	}

	_, err := r.getControl(commit)
	if err == nil {
		t.Error("expected error for unknown mode")
	}
}
