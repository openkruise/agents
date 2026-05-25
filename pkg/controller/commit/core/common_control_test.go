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
	"os"
	"testing"

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
	jobutil "github.com/openkruise/agents/pkg/job"
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
			Phase: agentsv1alpha1.CommitPending,
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

// --- resolveRegistrySecret tests ---

func TestResolveRegistrySecret_Tier1_SameNamespace(t *testing.T) {
	scheme := newTestScheme()
	secret := newDockerConfigSecret("push-secret", "default")
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	ctrl := newCommonControlForTest(fc)

	commit := newTestCommit("test-commit", "default")
	commit.Spec.PushSecrets = []agentsv1alpha1.ReferenceObject{
		{Name: "push-secret"},
	}
	pod := newTestPod("test-pod", "default", "node-1")

	resolved := ctrl.resolveRegistrySecret(context.TODO(), commit, pod)
	if resolved == nil {
		t.Fatal("expected resolved secret, got nil")
	}
	if resolved.Name != "push-secret" {
		t.Errorf("expected secret name 'push-secret', got %s", resolved.Name)
	}
	if resolved.Namespace != "default" {
		t.Errorf("expected namespace 'default', got %s", resolved.Namespace)
	}
}

func TestResolveRegistrySecret_Tier1_CrossNamespace(t *testing.T) {
	scheme := newTestScheme()
	secret := newDockerConfigSecret("push-secret", "team-a")
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	ctrl := newCommonControlForTest(fc)

	commit := newTestCommit("test-commit", "default")
	commit.Spec.PushSecrets = []agentsv1alpha1.ReferenceObject{
		{Name: "push-secret", Namespace: "team-a"},
	}
	pod := newTestPod("test-pod", "default", "node-1")

	resolved := ctrl.resolveRegistrySecret(context.TODO(), commit, pod)
	if resolved == nil {
		t.Fatal("expected resolved secret, got nil")
	}
	if resolved.Namespace != "team-a" {
		t.Errorf("expected namespace 'team-a', got %s", resolved.Namespace)
	}
}

func TestResolveRegistrySecret_Tier1_NotFound(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctrl := newCommonControlForTest(fc)

	commit := newTestCommit("test-commit", "default")
	commit.Spec.PushSecrets = []agentsv1alpha1.ReferenceObject{
		{Name: "nonexistent-secret"},
	}

	resolved := ctrl.resolveRegistrySecret(context.TODO(), commit, nil)
	if resolved != nil {
		t.Errorf("expected nil, got %v", resolved)
	}
}

func TestResolveRegistrySecret_Tier1_WrongType(t *testing.T) {
	scheme := newTestScheme()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "opaque-secret", Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	ctrl := newCommonControlForTest(fc)

	commit := newTestCommit("test-commit", "default")
	commit.Spec.PushSecrets = []agentsv1alpha1.ReferenceObject{
		{Name: "opaque-secret"},
	}

	resolved := ctrl.resolveRegistrySecret(context.TODO(), commit, nil)
	if resolved != nil {
		t.Errorf("expected nil for wrong secret type, got %v", resolved)
	}
}

func TestResolveRegistrySecret_Tier3_SAImagePullSecrets(t *testing.T) {
	scheme := newTestScheme()
	secret := newDockerConfigSecret("sa-pull-secret", "default")
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		ImagePullSecrets: []corev1.LocalObjectReference{
			{Name: "sa-pull-secret"},
		},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret, sa).Build()
	ctrl := newCommonControlForTest(fc)

	commit := newTestCommit("test-commit", "default")
	pod := newTestPod("test-pod", "default", "node-1")

	resolved := ctrl.resolveRegistrySecret(context.TODO(), commit, pod)
	if resolved == nil {
		t.Fatal("expected resolved secret from SA, got nil")
	}
	if resolved.Name != "sa-pull-secret" {
		t.Errorf("expected 'sa-pull-secret', got %s", resolved.Name)
	}
}

func TestResolveRegistrySecret_NoSecret(t *testing.T) {
	scheme := newTestScheme()
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sa).Build()
	ctrl := newCommonControlForTest(fc)

	commit := newTestCommit("test-commit", "default")
	pod := newTestPod("test-pod", "default", "node-1")

	resolved := ctrl.resolveRegistrySecret(context.TODO(), commit, pod)
	if resolved != nil {
		t.Errorf("expected nil when no secret matches, got %v", resolved)
	}
}

// --- ensureMirrorSecret tests ---

func TestEnsureMirrorSecret_CreatesMirror(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctrl := newCommonControlForTest(fc)

	commit := newTestCommit("my-commit", "default")
	source := newDockerConfigSecret("push-secret", "team-a")

	mirror, err := ctrl.ensureMirrorSecret(context.TODO(), commit, source, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mirror.Name != "commit-my-commit-registry-auth" {
		t.Errorf("expected mirror name 'commit-my-commit-registry-auth', got %s", mirror.Name)
	}
	if mirror.Namespace != "default" {
		t.Errorf("expected namespace 'default', got %s", mirror.Namespace)
	}
	if len(mirror.OwnerReferences) != 1 || mirror.OwnerReferences[0].Name != "my-commit" {
		t.Errorf("expected OwnerReference to commit 'my-commit'")
	}

	// Verify it was actually created
	got := &corev1.Secret{}
	err = fc.Get(context.TODO(), client.ObjectKey{Namespace: "default", Name: "commit-my-commit-registry-auth"}, got)
	if err != nil {
		t.Fatalf("mirror secret not found in fake client: %v", err)
	}
	if got.Type != corev1.SecretTypeDockerConfigJson {
		t.Errorf("expected dockerconfigjson type, got %s", got.Type)
	}
}

func TestEnsureMirrorSecret_AlreadyExists(t *testing.T) {
	scheme := newTestScheme()
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "commit-my-commit-registry-auth", Namespace: "default"},
		Type:       corev1.SecretTypeDockerConfigJson,
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	ctrl := newCommonControlForTest(fc)

	commit := newTestCommit("my-commit", "default")
	source := newDockerConfigSecret("push-secret", "team-a")

	mirror, err := ctrl.ensureMirrorSecret(context.TODO(), commit, source, "default")
	if err != nil {
		t.Fatalf("unexpected error on already-exists: %v", err)
	}
	if mirror.Name != "commit-my-commit-registry-auth" {
		t.Errorf("expected mirror name, got %s", mirror.Name)
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
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job).Build()
	ctrl := newCommonControlForTest(fc)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitUpdated(context.TODO(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if newStatus.Phase != agentsv1alpha1.CommitSucceeded {
		t.Errorf("expected CommitSucceeded, got %s", newStatus.Phase)
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
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
		},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job).Build()
	ctrl := newCommonControlForTest(fc)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitUpdated(context.TODO(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if newStatus.Phase != agentsv1alpha1.CommitFailed {
		t.Errorf("expected CommitFailed, got %s", newStatus.Phase)
	}
}

func TestEnsureCommitUpdated_JobNotFound(t *testing.T) {
	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctrl := newCommonControlForTest(fc)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitUpdated(context.TODO(), args)
	if err == nil {
		t.Fatal("expected error for missing job")
	}
	if newStatus.Phase != agentsv1alpha1.CommitFailed {
		t.Errorf("expected CommitFailed, got %s", newStatus.Phase)
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
		},
		Status: batchv1.JobStatus{
			Active: 1,
		},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job).Build()
	ctrl := newCommonControlForTest(fc)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitUpdated(context.TODO(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if newStatus.Phase != agentsv1alpha1.CommitPending {
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

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(commit, pod).Build()
	ctrl := newCommonControlForTest(fc)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Pod: pod, Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitRunning(context.TODO(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if newStatus.Phase != agentsv1alpha1.CommitRunning {
		t.Errorf("expected CommitRunning, got %s", newStatus.Phase)
	}
	if newStatus.StartTime == nil {
		t.Error("expected StartTime to be set")
	}
	if newStatus.CommitID != "test-commit" {
		t.Errorf("expected CommitID 'test-commit', got %s", newStatus.CommitID)
	}

	// Verify Job was created
	jobName := jobutil.MakeJobName(string(commit.UID))
	createdJob := &batchv1.Job{}
	if err := fc.Get(context.TODO(), client.ObjectKey{Namespace: "default", Name: jobName}, createdJob); err != nil {
		t.Fatalf("expected Job to be created: %v", err)
	}
}

func TestEnsureCommitRunning_MissingJobImage(t *testing.T) {
	os.Unsetenv("AGENT_JOB_IMAGE")

	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")
	pod := newTestPod("test-pod", "default", "node-1")

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(commit, pod).Build()
	ctrl := newCommonControlForTest(fc)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Pod: pod, Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitRunning(context.TODO(), args)
	if err == nil {
		t.Fatal("expected error when AGENT_JOB_IMAGE is empty")
	}
	if newStatus.Phase != agentsv1alpha1.CommitFailed {
		t.Errorf("expected CommitFailed, got %s", newStatus.Phase)
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

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(commit, pod, existingJobPod).Build()
	ctrl := newCommonControlForTest(fc)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Pod: pod, Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitRunning(context.TODO(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Phase should remain pending since job pod already exists (early return)
	if newStatus.Phase != agentsv1alpha1.CommitPending {
		t.Errorf("expected phase to remain Pending, got %s", newStatus.Phase)
	}
}

func TestApplyCommitJob_AlreadyExists(t *testing.T) {
	os.Setenv("AGENT_JOB_IMAGE", "test-image:latest")
	defer os.Unsetenv("AGENT_JOB_IMAGE")

	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")
	pod := newTestPod("test-pod", "default", "node-1")

	// Pre-create the job so applyCommitJob hits IsAlreadyExists path
	jobName := jobutil.MakeJobName(string(commit.UID))
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: "default",
		},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(commit, pod, existingJob).Build()
	ctrl := newCommonControlForTest(fc)

	err := ctrl.applyCommitJob(context.TODO(), commit, pod)
	if err != nil {
		t.Fatalf("expected no error for already-exists job, got: %v", err)
	}
}

func TestEnsureCommitRunning_WithDockerSecret(t *testing.T) {
	os.Setenv("AGENT_JOB_IMAGE", "test-image:latest")
	defer os.Unsetenv("AGENT_JOB_IMAGE")

	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")
	commit.Spec.PushSecrets = []agentsv1alpha1.ReferenceObject{
		{Name: "push-secret"},
	}
	pod := newTestPod("test-pod", "default", "node-1")
	secret := newDockerConfigSecret("push-secret", "default")

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(commit, pod, secret).Build()
	ctrl := newCommonControlForTest(fc)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Pod: pod, Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitRunning(context.TODO(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify Job has docker-config volume
	jobName := jobutil.MakeJobName(string(commit.UID))
	createdJob := &batchv1.Job{}
	if err := fc.Get(context.TODO(), client.ObjectKey{Namespace: "default", Name: jobName}, createdJob); err != nil {
		t.Fatalf("expected Job: %v", err)
	}
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

func TestEnsureCommitRunning_CrossNamespaceSecret(t *testing.T) {
	os.Setenv("AGENT_JOB_IMAGE", "test-image:latest")
	defer os.Unsetenv("AGENT_JOB_IMAGE")

	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")
	commit.Spec.PushSecrets = []agentsv1alpha1.ReferenceObject{
		{Name: "push-secret", Namespace: "team-a"},
	}
	pod := newTestPod("test-pod", "default", "node-1")
	secret := newDockerConfigSecret("push-secret", "team-a")

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(commit, pod, secret).Build()
	ctrl := newCommonControlForTest(fc)

	newStatus := commit.Status.DeepCopy()
	args := &EnsureFuncArgs{Pod: pod, Commit: commit, NewStatus: newStatus}

	_, err := ctrl.EnsureCommitRunning(context.TODO(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify mirror secret was created in default namespace
	mirrorSecret := &corev1.Secret{}
	err = fc.Get(context.TODO(), client.ObjectKey{Namespace: "default", Name: "commit-test-commit-registry-auth"}, mirrorSecret)
	if err != nil {
		t.Fatalf("expected mirror secret to be created: %v", err)
	}
	if mirrorSecret.Type != corev1.SecretTypeDockerConfigJson {
		t.Errorf("expected dockerconfigjson type for mirror, got %s", mirrorSecret.Type)
	}

	// Verify Job references the mirror secret
	jobName := jobutil.MakeJobName(string(commit.UID))
	createdJob := &batchv1.Job{}
	_ = fc.Get(context.TODO(), client.ObjectKey{Namespace: "default", Name: jobName}, createdJob)
	for _, v := range createdJob.Spec.Template.Spec.Volumes {
		if v.Name == "docker-config" {
			if v.Secret.SecretName != "commit-test-commit-registry-auth" {
				t.Errorf("expected Job to reference mirror secret, got %s", v.Secret.SecretName)
			}
		}
	}
}

// --- EnsureCommitDeleted tests ---

func TestEnsureCommitDeleted(t *testing.T) {
	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")
	commit.Finalizers = []string{agentsv1alpha1.CommitFinalizer}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(commit).Build()
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

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(commit).Build()
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
		name           string
		pods           []corev1.Pod
		expectNil      bool
		expectType     string
		expectStatus   metav1.ConditionStatus
		expectReason   string
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
			name: "exit code 2 - push failed",
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
			expectType:   "PushCommittedImage",
			expectStatus: metav1.ConditionFalse,
			expectReason: "PushCommittedImageFailed",
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
								ContainerID: "containerd://abc",
								State: corev1.ContainerState{
									Terminated: &corev1.ContainerStateTerminated{ExitCode: 6},
								},
							},
						},
					},
				},
			},
			expectNil:    false,
			expectType:   "",
			expectStatus: metav1.ConditionFalse,
			expectReason: "",
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
			builder := fake.NewClientBuilder().WithScheme(scheme)
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

// --- applyCommitJob error tests ---

func TestApplyCommitJob_EmptyContainerID(t *testing.T) {
	os.Setenv("AGENT_JOB_IMAGE", "test-image:latest")
	defer os.Unsetenv("AGENT_JOB_IMAGE")

	scheme := newTestScheme()
	commit := newTestCommit("test-commit", "default")
	// Pod with empty container ID should cause job generation to fail
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

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(commit, pod).Build()
	ctrl := newCommonControlForTest(fc)

	err := ctrl.applyCommitJob(context.TODO(), commit, pod)
	if err == nil {
		t.Fatal("expected error when container ID is empty")
	}
	if !contains(err.Error(), "failed to generate commit job") {
		t.Errorf("expected error to contain 'failed to generate commit job', got: %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
