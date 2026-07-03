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

package core

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	jobutil "github.com/openkruise/agents/pkg/controller/commit/job"
)

func newTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	return scheme
}

func newTestCommit(name, namespace string) *agentsv1alpha1.Commit {
	return &agentsv1alpha1.Commit{
		TypeMeta: metav1.TypeMeta{
			APIVersion: agentsv1alpha1.SchemeGroupVersion.String(),
			Kind:       "Commit",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID("test-uid-1234"),
		},
		Spec: agentsv1alpha1.CommitSpec{
			PodName:       "test-pod",
			ContainerName: "workspace",
			Image:         "registry.example.com/team/env:v1",
		},
		Status: agentsv1alpha1.CommitStatus{
			Phase: agentsv1alpha1.CommitPhasePending,
		},
	}
}

func newTestPod(name, namespace, nodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID("pod-uid-5678"),
		},
		Spec: corev1.PodSpec{
			NodeName:           nodeName,
			ServiceAccountName: "default",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:        "workspace",
					ContainerID: "containerd://abc123def456",
				},
			},
		},
	}
}

func newDockerConfigSecret(name, namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			".dockerconfigjson": []byte(`{"auths":{"registry.example.com":{"auth":"dGVzdDpwYXNz"}}}`),
		},
	}
}

func newCommonControlForTest(c client.Client) *commonControl {
	return &commonControl{
		Client:   c,
		Recorder: record.NewFakeRecorder(10),
	}
}

// newFakeClientBuilder returns a fake client builder with the commit-UID field index pre-registered.
func newFakeClientBuilder(scheme *runtime.Scheme) *fake.ClientBuilder {
	commitUIDIndex := func(obj client.Object) []string {
		if uid, ok := obj.GetLabels()[jobutil.LabelCommitUID]; ok {
			return []string{uid}
		}
		return nil
	}
	return fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&batchv1.Job{}, jobutil.IndexFieldCommitUID, commitUIDIndex).
		WithIndex(&corev1.Pod{}, jobutil.IndexFieldCommitUID, commitUIDIndex)
}

// --- resolveRegistrySecret tests ---

func TestResolveRegistrySecretName_Tier1(t *testing.T) {
	scheme := newTestScheme()
	secret := newDockerConfigSecret("push-secret", "default")
	fc := newFakeClientBuilder(scheme).WithObjects(secret).Build()
	ctrl := newCommonControlForTest(fc)

	commit := newTestCommit("test-commit", "default")
	commit.Spec.RegistryAuth = &agentsv1alpha1.RegistryAuth{Secrets: []string{
		"push-secret",
	}}

	name := ctrl.resolveRegistrySecretName(context.TODO(), commit)
	if name != "push-secret" {
		t.Errorf("expected 'push-secret', got %q", name)
	}
}

func TestResolveRegistrySecretName_Tier1_NotFound(t *testing.T) {
	scheme := newTestScheme()
	fc := newFakeClientBuilder(scheme).Build()
	ctrl := newCommonControlForTest(fc)

	commit := newTestCommit("test-commit", "default")
	commit.Spec.RegistryAuth = &agentsv1alpha1.RegistryAuth{Secrets: []string{
		"nonexistent-secret",
	}}

	name := ctrl.resolveRegistrySecretName(context.TODO(), commit)
	if name != "" {
		t.Errorf("expected empty, got %q", name)
	}
}

func TestResolveRegistrySecretName_Tier1_WrongType(t *testing.T) {
	scheme := newTestScheme()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "opaque-secret", Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
	}
	fc := newFakeClientBuilder(scheme).WithObjects(secret).Build()
	ctrl := newCommonControlForTest(fc)

	commit := newTestCommit("test-commit", "default")
	commit.Spec.RegistryAuth = &agentsv1alpha1.RegistryAuth{Secrets: []string{
		"opaque-secret",
	}}

	name := ctrl.resolveRegistrySecretName(context.TODO(), commit)
	if name != "" {
		t.Errorf("expected empty for wrong secret type, got %q", name)
	}
}

func TestResolveRegistrySecretName_NoSecret(t *testing.T) {
	scheme := newTestScheme()
	fc := newFakeClientBuilder(scheme).Build()
	ctrl := newCommonControlForTest(fc)

	commit := newTestCommit("test-commit", "default")

	name := ctrl.resolveRegistrySecretName(context.TODO(), commit)
	if name != "" {
		t.Errorf("expected empty when no secret specified, got %q", name)
	}
}

// --- EnsureCommitUpdated tests ---

func TestEnsureCommitUpdated_JobCompleted(t *testing.T) {
	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")

	jobName := jobutil.MakeJobName(string(commit.UID))
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: "default",
			Labels: map[string]string{
				jobutil.LabelCommitUID: string(commit.UID),
			},
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}
	fc := newFakeClientBuilder(scheme).WithObjects(job).Build()
	ctrl := newCommonControlForTest(fc)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitUpdated(context.TODO(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if newStatus.Phase != agentsv1alpha1.CommitPhaseSucceeded {
		t.Errorf("expected CommitPhaseSucceeded, got %s", newStatus.Phase)
	}
	if newStatus.CompletionTime == nil {
		t.Error("expected CompletionTime to be set")
	}
}

func TestEnsureCommitUpdated_JobFailed(t *testing.T) {
	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")

	jobName := jobutil.MakeJobName(string(commit.UID))
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: "default",
			Labels: map[string]string{
				jobutil.LabelCommitUID: string(commit.UID),
			},
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
		},
	}
	fc := newFakeClientBuilder(scheme).WithObjects(job).Build()
	ctrl := newCommonControlForTest(fc)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitUpdated(context.TODO(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if newStatus.Phase != agentsv1alpha1.CommitPhaseFailed {
		t.Errorf("expected CommitPhaseFailed, got %s", newStatus.Phase)
	}
}

func TestEnsureCommitUpdated_JobNotFound(t *testing.T) {
	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")
	fc := newFakeClientBuilder(scheme).Build()
	ctrl := newCommonControlForTest(fc)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitUpdated(context.TODO(), args)
	if err != nil {
		t.Fatalf("expected nil error for missing job (terminal state set), got %v", err)
	}
	if newStatus.Phase != agentsv1alpha1.CommitPhaseFailed {
		t.Errorf("expected CommitPhaseFailed, got %s", newStatus.Phase)
	}
}

func TestEnsureCommitUpdated_JobStillRunning(t *testing.T) {
	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")

	jobName := jobutil.MakeJobName(string(commit.UID))
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: "default",
			Labels: map[string]string{
				jobutil.LabelCommitUID: string(commit.UID),
			},
		},
		Status: batchv1.JobStatus{
			Active: 1,
		},
	}
	fc := newFakeClientBuilder(scheme).WithObjects(job).Build()
	ctrl := newCommonControlForTest(fc)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitUpdated(context.TODO(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if newStatus.Phase != agentsv1alpha1.CommitPhasePending {
		t.Errorf("expected phase unchanged (Pending), got %s", newStatus.Phase)
	}
	if newStatus.CompletionTime != nil {
		t.Error("expected CompletionTime to remain nil")
	}
}

// --- EnsureCommitRunning tests ---

func TestEnsureCommitRunning_Success(t *testing.T) {
	os.Setenv("AGENT_JOB_IMAGE", "test-image:latest")
	defer os.Unsetenv("AGENT_JOB_IMAGE")

	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")
	pod := newTestPod("test-pod", "default", "node-1")

	fc := newFakeClientBuilder(scheme).WithObjects(commit, pod).Build()
	ctrl := newCommonControlForTest(fc)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Pod: pod, Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitRunning(context.TODO(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if newStatus.Phase != agentsv1alpha1.CommitPhaseRunning {
		t.Errorf("expected CommitPhaseRunning, got %s", newStatus.Phase)
	}
	if newStatus.StartTime == nil {
		t.Error("expected StartTime to be set")
	}
	if newStatus.CommitID != "test-commit" {
		t.Errorf("expected CommitID 'test-commit', got %s", newStatus.CommitID)
	}

	// Verify Job was created (by label since GenerateName produces a random name)
	jobList := &batchv1.JobList{}
	if err := fc.List(context.TODO(), jobList, client.InNamespace("default"), client.MatchingFields{jobutil.IndexFieldCommitUID: string(commit.UID)}); err != nil {
		t.Fatalf("failed to list jobs: %v", err)
	}
	if len(jobList.Items) != 1 {
		t.Fatalf("expected 1 Job, got %d", len(jobList.Items))
	}
}

func TestEnsureCommitRunning_MissingJobImage(t *testing.T) {
	os.Unsetenv("AGENT_JOB_IMAGE")

	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")
	pod := newTestPod("test-pod", "default", "node-1")

	fc := newFakeClientBuilder(scheme).WithObjects(commit, pod).Build()
	ctrl := newCommonControlForTest(fc)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Pod: pod, Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitRunning(context.TODO(), args)
	if err != nil {
		t.Fatalf("expected nil error after setting Failed status, got: %v", err)
	}
	if newStatus.Phase != agentsv1alpha1.CommitPhaseFailed {
		t.Errorf("expected CommitPhaseFailed, got %s", newStatus.Phase)
	}
}

func TestEnsureCommitRunning_JobPodAlreadyExists(t *testing.T) {
	os.Setenv("AGENT_JOB_IMAGE", "test-image:latest")
	defer os.Unsetenv("AGENT_JOB_IMAGE")

	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")
	pod := newTestPod("test-pod", "default", "node-1")

	// Create an existing job pod with matching label
	existingJobPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-job-pod",
			Namespace: "default",
			Labels: map[string]string{
				jobutil.LabelCommitUID: string(commit.UID),
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	fc := newFakeClientBuilder(scheme).WithObjects(commit, pod, existingJobPod).Build()
	ctrl := newCommonControlForTest(fc)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Pod: pod, Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitRunning(context.TODO(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Phase should transition to Running since job pod already exists
	if newStatus.Phase != agentsv1alpha1.CommitPhaseRunning {
		t.Errorf("expected phase to be Running, got %s", newStatus.Phase)
	}
	if newStatus.StartTime == nil {
		t.Error("expected StartTime to be set")
	}
}

// TestApplyCommitJob_AlreadyExists is removed — with GenerateName, the Job name is
// randomly generated so Create collisions are effectively impossible. The
// IsAlreadyExists safety net remains in the production code.

func TestEnsureCommitRunning_WithDockerSecret(t *testing.T) {
	os.Setenv("AGENT_JOB_IMAGE", "test-image:latest")
	defer os.Unsetenv("AGENT_JOB_IMAGE")

	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")
	commit.Spec.RegistryAuth = &agentsv1alpha1.RegistryAuth{Secrets: []string{
		"push-secret",
	}}
	pod := newTestPod("test-pod", "default", "node-1")
	secret := newDockerConfigSecret("push-secret", "default")

	fc := newFakeClientBuilder(scheme).WithObjects(commit, pod, secret).Build()
	ctrl := newCommonControlForTest(fc)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Pod: pod, Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitRunning(context.TODO(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify Job has docker-config volume (by label since GenerateName produces a random name)
	jobList := &batchv1.JobList{}
	if err := fc.List(context.TODO(), jobList, client.InNamespace("default"), client.MatchingFields{jobutil.IndexFieldCommitUID: string(commit.UID)}); err != nil {
		t.Fatalf("failed to list jobs: %v", err)
	}
	if len(jobList.Items) != 1 {
		t.Fatalf("expected 1 Job, got %d", len(jobList.Items))
	}
	createdJob := &jobList.Items[0]
	// Verify docker-config volume is mounted with the secret name
	found := false
	for _, v := range createdJob.Spec.Template.Spec.Volumes {
		if v.Name == "docker-config" {
			found = true
			if v.Secret.SecretName != "push-secret" {
				t.Errorf("expected secret 'push-secret', got %s", v.Secret.SecretName)
			}
		}
	}
	if !found {
		t.Error("expected docker-config volume in Job")
	}
}

// --- EnsureCommitDeleted tests ---

func TestEnsureCommitDeleted(t *testing.T) {
	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")
	commit.Finalizers = []string{agentsv1alpha1.CommitFinalizer}

	fc := newFakeClientBuilder(scheme).WithObjects(commit).Build()
	ctrl := newCommonControlForTest(fc)

	args := &EnsureFuncArgs{Commit: commit, NewStatus: commit.Status.DeepCopy()}
	_, err := ctrl.EnsureCommitDeleted(context.TODO(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureCommitDeleted_NoFinalizer(t *testing.T) {
	// When commit has no finalizer, PatchFinalizer should return early without error
	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")
	// No finalizers set

	fc := newFakeClientBuilder(scheme).WithObjects(commit).Build()
	ctrl := newCommonControlForTest(fc)

	args := &EnsureFuncArgs{Commit: commit, NewStatus: commit.Status.DeepCopy()}
	_, err := ctrl.EnsureCommitDeleted(context.TODO(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- getLatestJobPodExitCode tests ---

func TestGetLatestJobPodExitCode(t *testing.T) {
	tests := []struct {
		name         string
		pods         []corev1.Pod
		expectNil    bool
		expectType   string
		expectStatus metav1.ConditionStatus
		expectReason string
	}{
		{
			name:      "no pods",
			pods:      nil,
			expectNil: true,
		},
		{
			name: "exit code 0 - success",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "job-pod-1",
						Namespace: "default",
						Labels: map[string]string{
							jobutil.LabelCommitUID: "test-uid-1234",
						},
						CreationTimestamp: metav1.Now(),
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:        "agent-job",
								ContainerID: "containerd://abc",
								State: corev1.ContainerState{
									Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
								},
							},
						},
					},
				},
			},
			expectNil:    false,
			expectType:   "PushCommittedImage",
			expectStatus: metav1.ConditionTrue,
			expectReason: "PushCommittedImageSuccess",
		},
		{
			name: "exit code 1 - commit failed",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "job-pod-1",
						Namespace: "default",
						Labels: map[string]string{
							jobutil.LabelCommitUID: "test-uid-1234",
						},
						CreationTimestamp: metav1.Now(),
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:        "agent-job",
								ContainerID: "containerd://abc",
								State: corev1.ContainerState{
									Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
								},
							},
						},
					},
				},
			},
			expectNil:    false,
			expectType:   "CommitContainer",
			expectStatus: metav1.ConditionFalse,
			expectReason: "CommitContainerFailed",
		},
		{
			name: "exit code 2 - get image size failed",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "job-pod-1",
						Namespace: "default",
						Labels: map[string]string{
							jobutil.LabelCommitUID: "test-uid-1234",
						},
						CreationTimestamp: metav1.Now(),
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:        "agent-job",
								ContainerID: "containerd://abc",
								State: corev1.ContainerState{
									Terminated: &corev1.ContainerStateTerminated{ExitCode: 2},
								},
							},
						},
					},
				},
			},
			expectNil:    false,
			expectType:   "CommitContainer",
			expectStatus: metav1.ConditionFalse,
			expectReason: "GetImageSizeFailed",
		},
		{
			name: "exit code 6 - unknown exit code (not in map)",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "job-pod-1",
						Namespace: "default",
						Labels: map[string]string{
							jobutil.LabelCommitUID: "test-uid-1234",
						},
						CreationTimestamp: metav1.Now(),
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:        "agent-job",
								ContainerID: "containerd://abc",
								State: corev1.ContainerState{
									Terminated: &corev1.ContainerStateTerminated{ExitCode: 6},
								},
							},
						},
					},
				},
			},
			expectNil: true,
		},
		{
			name: "pod without terminated state",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "job-pod-1",
						Namespace: "default",
						Labels: map[string]string{
							jobutil.LabelCommitUID: "test-uid-1234",
						},
						CreationTimestamp: metav1.Now(),
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:        "agent-job",
								ContainerID: "containerd://abc",
								State: corev1.ContainerState{
									Running: &corev1.ContainerStateRunning{},
								},
							},
						},
					},
				},
			},
			expectNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newTestScheme()
			builder := newFakeClientBuilder(scheme)
			for i := range tt.pods {
				builder = builder.WithObjects(&tt.pods[i])
			}
			fc := builder.Build()
			ctrl := newCommonControlForTest(fc)

			commit := newTestCommit("test-commit", "default")
			condition := ctrl.getLatestJobPodExitCode(context.TODO(), commit)

			if tt.expectNil {
				if condition != nil {
					t.Errorf("expected nil condition, got %+v", condition)
				}
				return
			}
			if condition == nil {
				t.Fatal("expected non-nil condition, got nil")
			}
			if condition.Type != tt.expectType {
				t.Errorf("expected type %q, got %q", tt.expectType, condition.Type)
			}
			if condition.Status != tt.expectStatus {
				t.Errorf("expected status %q, got %q", tt.expectStatus, condition.Status)
			}
			if condition.Reason != tt.expectReason {
				t.Errorf("expected reason %q, got %q", tt.expectReason, condition.Reason)
			}
		})
	}
}

// --- EnsureCommitRunning error tests ---

// createErrorClient wraps a client.Client and returns a fixed error on Create.
// Used to simulate transient API server failures in unit tests.
type createErrorClient struct {
	client.Client
	createErr error
}

func (c *createErrorClient) Create(_ context.Context, _ client.Object, _ ...client.CreateOption) error {
	return c.createErr
}

func TestEnsureCommitRunning_EmptyContainerID(t *testing.T) {
	os.Setenv("AGENT_JOB_IMAGE", "test-image:latest")
	defer os.Unsetenv("AGENT_JOB_IMAGE")

	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")
	// Pod with empty container ID should cause job generation to fail (permanent error).
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			UID:       types.UID("pod-uid-5678"),
		},
		Spec: corev1.PodSpec{
			NodeName:           "node-1",
			ServiceAccountName: "default",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:        "workspace",
					ContainerID: "", // empty container ID
				},
			},
		},
	}

	fc := newFakeClientBuilder(scheme).WithObjects(commit, pod).Build()
	ctrl := newCommonControlForTest(fc)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Pod: pod, Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitRunning(context.TODO(), args)
	// Permanent error: nil returned (no retry), status marked Failed.
	if err != nil {
		t.Fatalf("expected nil error for permanent error, got: %v", err)
	}
	if newStatus.Phase != agentsv1alpha1.CommitPhaseFailed {
		t.Errorf("expected CommitPhaseFailed, got %s", newStatus.Phase)
	}
}

func TestEnsureCommitRunning_CreateTransientError(t *testing.T) {
	os.Setenv("AGENT_JOB_IMAGE", "test-image:latest")
	defer os.Unsetenv("AGENT_JOB_IMAGE")

	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")
	pod := newTestPod("test-pod", "default", "node-1")

	fc := newFakeClientBuilder(scheme).WithObjects(commit, pod).Build()
	// Wrap the fake client to inject a transient Create error.
	errClient := &createErrorClient{Client: fc, createErr: fmt.Errorf("connection refused")}
	ctrl := newCommonControlForTest(errClient)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Pod: pod, Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitRunning(context.TODO(), args)
	// Transient error: error returned for backoff retry, status NOT marked Failed.
	if err == nil {
		t.Fatal("expected error for transient Create failure")
	}
	if newStatus.Phase == agentsv1alpha1.CommitPhaseFailed {
		t.Error("expected phase to NOT be Failed for transient error")
	}
}

// --- EnsureCommitUpdated multi-Job tests ---

func TestEnsureCommitUpdated_MultipleJobs(t *testing.T) {
	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")

	// Create two Jobs with the same LabelCommitUID but different timestamps.
	// The controller should pick the latest one.
	earlier := metav1.NewTime(metav1.Now().Add(-10 * time.Minute))
	later := metav1.Now()

	jobOld := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "commit-old-abc",
			Namespace:         "default",
			CreationTimestamp: earlier,
			Labels: map[string]string{
				jobutil.LabelCommitUID: string(commit.UID),
			},
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
		},
	}
	jobNew := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "commit-new-xyz",
			Namespace:         "default",
			CreationTimestamp: later,
			Labels: map[string]string{
				jobutil.LabelCommitUID: string(commit.UID),
			},
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}

	fc := newFakeClientBuilder(scheme).WithObjects(jobOld, jobNew).Build()
	ctrl := newCommonControlForTest(fc)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitUpdated(context.TODO(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Latest Job (jobNew) is Complete, so Commit should be Succeeded.
	if newStatus.Phase != agentsv1alpha1.CommitPhaseSucceeded {
		t.Errorf("expected CommitPhaseSucceeded (from latest Job), got %s", newStatus.Phase)
	}
}
