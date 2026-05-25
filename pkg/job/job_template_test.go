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

package job

import (
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/api/v1alpha1"
)

func newTestGenerator() *JobGenerator {
	return &JobGenerator{
		Commit: &v1alpha1.Commit{
			TypeMeta: metav1.TypeMeta{
				APIVersion: v1alpha1.SchemeGroupVersion.String(),
				Kind:       "Commit",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-commit",
				Namespace: "default",
				UID:       types.UID("uid-1234"),
			},
			Spec: v1alpha1.CommitSpec{
				PodName:       "test-pod",
				ContainerName: "workspace",
				Image:         "registry.example.com/team/env:v1",
			},
		},
		Pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
				UID:       types.UID("pod-uid-5678"),
			},
			Spec: corev1.PodSpec{
				NodeName: "node-1",
			},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:        "workspace",
						ContainerID: "containerd://abc123def",
					},
				},
			},
		},
	}
}

func TestGenerateCommitJob_Success(t *testing.T) {
	os.Setenv(EnvAgentJobImage, "agent-job:latest")
	defer os.Unsetenv(EnvAgentJobImage)

	g := newTestGenerator()
	job, err := g.GenerateCommitJob()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if job.Name != MakeJobName("uid-1234") {
		t.Errorf("job name = %s, want %s", job.Name, MakeJobName("uid-1234"))
	}
	if job.Namespace != "default" {
		t.Errorf("namespace = %s, want default", job.Namespace)
	}

	// Check node affinity
	terms := job.Spec.Template.Spec.Affinity.NodeAffinity.
		RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || terms[0].MatchFields[0].Values[0] != "node-1" {
		t.Error("expected node affinity targeting node-1")
	}

	// Check HostNetwork
	if !job.Spec.Template.Spec.HostNetwork {
		t.Error("expected HostNetwork=true")
	}

	// Check security context
	sc := job.Spec.Template.Spec.Containers[0].SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 0 {
		t.Error("expected RunAsUser=0")
	}

	// Check labels
	if job.Labels[LabelCommitName] != "test-commit" {
		t.Errorf("expected commit-name label, got %v", job.Labels)
	}
	if job.Labels[LabelCommitUID] != "uid-1234" {
		t.Errorf("expected commit-uid label, got %v", job.Labels)
	}

	// Check OwnerReference
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].Name != "test-commit" {
		t.Error("expected OwnerReference to test-commit")
	}
}

func TestGenerateCommitJob_WithDockerConfigSecret(t *testing.T) {
	os.Setenv(EnvAgentJobImage, "agent-job:latest")
	defer os.Unsetenv(EnvAgentJobImage)

	g := newTestGenerator()
	g.DockerConfigSecretName = "my-push-secret"

	job, err := g.GenerateCommitJob()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check docker-config volume exists
	foundVol := false
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "docker-config" {
			foundVol = true
			if v.Secret.SecretName != "my-push-secret" {
				t.Errorf("expected secret 'my-push-secret', got %s", v.Secret.SecretName)
			}
		}
	}
	if !foundVol {
		t.Error("expected docker-config volume")
	}

	// Check volume mount
	foundMount := false
	for _, vm := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
		if vm.Name == "docker-config" {
			foundMount = true
			if !vm.ReadOnly {
				t.Error("expected docker-config mount to be ReadOnly")
			}
		}
	}
	if !foundMount {
		t.Error("expected docker-config volume mount")
	}
}

func TestGenerateCommitJob_WithoutDockerConfigSecret(t *testing.T) {
	os.Setenv(EnvAgentJobImage, "agent-job:latest")
	defer os.Unsetenv(EnvAgentJobImage)

	g := newTestGenerator()
	g.DockerConfigSecretName = ""

	job, err := g.GenerateCommitJob()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "docker-config" {
			t.Error("should not have docker-config volume when no secret specified")
		}
	}
}

func TestGenerateCommitJob_DryRunEnv(t *testing.T) {
	os.Setenv(EnvAgentJobImage, "agent-job:latest")
	defer os.Unsetenv(EnvAgentJobImage)

	g := newTestGenerator()
	g.Commit.Spec.DryRun = true

	job, err := g.GenerateCommitJob()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, env := range job.Spec.Template.Spec.Containers[0].Env {
		if env.Name == EnvDryRun && env.Value == "true" {
			found = true
		}
	}
	if !found {
		t.Error("expected DRY_RUN=true env when DryRun is set")
	}
}

func TestGenerateCommitJob_NilCommit(t *testing.T) {
	g := &JobGenerator{Pod: &corev1.Pod{}}
	_, err := g.GenerateCommitJob()
	if err == nil {
		t.Error("expected error for nil commit")
	}
}

func TestGenerateCommitJob_NilPod(t *testing.T) {
	g := &JobGenerator{Commit: &v1alpha1.Commit{}}
	_, err := g.GenerateCommitJob()
	if err == nil {
		t.Error("expected error for nil pod")
	}
}

func TestGenerateCommitJob_NoContainerID(t *testing.T) {
	os.Setenv(EnvAgentJobImage, "agent-job:latest")
	defer os.Unsetenv(EnvAgentJobImage)

	g := newTestGenerator()
	g.Pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "workspace", ContainerID: "docker://abc123"}, // not containerd
	}

	_, err := g.GenerateCommitJob()
	if err == nil {
		t.Error("expected error for non-containerd runtime")
	}
}

func TestGenerateCommitJob_EmptyNodeName(t *testing.T) {
	os.Setenv(EnvAgentJobImage, "agent-job:latest")
	defer os.Unsetenv(EnvAgentJobImage)

	g := newTestGenerator()
	g.Pod.Spec.NodeName = ""

	_, err := g.GenerateCommitJob()
	if err == nil {
		t.Error("expected error for empty node name")
	}
}

func TestGenerateCommitJob_NoJobImage(t *testing.T) {
	os.Unsetenv(EnvAgentJobImage)

	g := newTestGenerator()
	_, err := g.GenerateCommitJob()
	if err == nil {
		t.Error("expected error when AGENT_JOB_IMAGE is not set")
	}
}

func TestCommitContainerID(t *testing.T) {
	g := newTestGenerator()

	tests := []struct {
		name     string
		statuses []corev1.ContainerStatus
		expected string
	}{
		{
			name: "valid containerd ID",
			statuses: []corev1.ContainerStatus{
				{Name: "workspace", ContainerID: "containerd://abc123"},
			},
			expected: "abc123",
		},
		{
			name: "docker runtime (unsupported)",
			statuses: []corev1.ContainerStatus{
				{Name: "workspace", ContainerID: "docker://abc123"},
			},
			expected: "",
		},
		{
			name: "container not found",
			statuses: []corev1.ContainerStatus{
				{Name: "other", ContainerID: "containerd://abc123"},
			},
			expected: "",
		},
		{
			name:     "empty statuses",
			statuses: nil,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g.Pod.Status.ContainerStatuses = tt.statuses
			got := g.commitContainerID()
			if got != tt.expected {
				t.Errorf("commitContainerID() = %q, want %q", got, tt.expected)
			}
		})
	}
}
