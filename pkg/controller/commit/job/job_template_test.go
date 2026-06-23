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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
)

func newTestJobGenerator() *JobGenerator {
	return &JobGenerator{
		Commit: &v1alpha1.Commit{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-commit",
				Namespace: "test-ns",
				UID:       "test-uid",
			},
			Spec: v1alpha1.CommitSpec{
				PodName:       "test-pod",
				ContainerName: "test-container",
				Image:         "registry.example.com/app:v1",
			},
		},
		Pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "test-ns",
				UID:       "pod-uid",
			},
			Spec: corev1.PodSpec{NodeName: "test-node"},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{Name: "test-container", ContainerID: "containerd://abc123"},
				},
			},
		},
	}
}

func TestJobGenerator_commitContainerID(t *testing.T) {
	g := newTestJobGenerator()
	if got := g.commitContainerID(); got != "abc123" {
		t.Errorf("commitContainerID()=%q, want %q", got, "abc123")
	}

	// Container name does not match any container in the pod's status.
	g.Commit.Spec.ContainerName = "missing"
	if got := g.commitContainerID(); got != "" {
		t.Errorf("commitContainerID() with non-matching name=%q, want empty", got)
	}

	// Matching container name but the runtime prefix is not "containerd://".
	g.Commit.Spec.ContainerName = "test-container"
	g.Pod.Status.ContainerStatuses[0].ContainerID = "dockerd://def456"
	if got := g.commitContainerID(); got != "" {
		t.Errorf("commitContainerID() with non-containerd prefix=%q, want empty", got)
	}

	// Pod has no container statuses at all.
	g.Pod.Status.ContainerStatuses = nil
	if got := g.commitContainerID(); got != "" {
		t.Errorf("commitContainerID() with empty statuses=%q, want empty", got)
	}
}

func TestJobGenerator_commitLabels(t *testing.T) {
	g := newTestJobGenerator()
	labels := g.commitLabels()
	if labels[LabelCommitName] != "test-commit" {
		t.Errorf("LabelCommitName=%q, want %q", labels[LabelCommitName], "test-commit")
	}
	if labels[LabelCommitUID] != "test-uid" {
		t.Errorf("LabelCommitUID=%q, want %q", labels[LabelCommitUID], "test-uid")
	}
}

func TestJobGenerator_commitEnvs(t *testing.T) {
	g := newTestJobGenerator()
	envs := g.commitEnvs()
	envMap := make(map[string]string, len(envs))
	for _, e := range envs {
		envMap[e.Name] = e.Value
	}

	expected := map[string]string{
		EnvContainerID:        "abc123",
		EnvCommitNamespace:    "test-ns",
		EnvCommitName:         "test-commit",
		EnvCommitImage:        "registry.example.com/app:v1",
		EnvContainerName:      "test-container",
		EnvAgentJobActionKey:  EnvAgentJobActionCommit,
		EnvCommitPodName:      "test-pod",
		EnvCommitPodNamespace: "test-ns",
		EnvCommitPodUID:       "pod-uid",
	}
	for key, want := range expected {
		if got := envMap[key]; got != want {
			t.Errorf("env[%s]=%q, want %q", key, got, want)
		}
	}
}

func TestJobGenerator_volumes(t *testing.T) {
	setEnv(t, EnvContainerdSockPath, "")
	g := newTestJobGenerator()
	volumes, mounts := g.volumes()
	if len(volumes) != 2 || len(mounts) != 2 {
		t.Fatalf("expected 2 volumes and 2 mounts, got %d volumes / %d mounts", len(volumes), len(mounts))
	}
	if volumes[0].Name != "host-containerd-run" || mounts[0].Name != "host-containerd-run" {
		t.Errorf("first volume/mount must be host-containerd-run, got %q / %q", volumes[0].Name, mounts[0].Name)
	}
	if volumes[1].Name != "host-containerd-certs" || mounts[1].Name != "host-containerd-certs" {
		t.Errorf("second volume/mount must be host-containerd-certs, got %q / %q", volumes[1].Name, mounts[1].Name)
	}
	if !mounts[1].ReadOnly {
		t.Error("host-containerd-certs mount must be read-only")
	}
}

func TestGenerateCommitJob_NilCommit(t *testing.T) {
	g := &JobGenerator{Commit: nil, Pod: &corev1.Pod{}}
	if _, err := g.GenerateCommitJob(); err == nil {
		t.Error("expected error for nil commit, got nil")
	}
}

func TestGenerateCommitJob_NilPod(t *testing.T) {
	g := &JobGenerator{Commit: &v1alpha1.Commit{}, Pod: nil}
	if _, err := g.GenerateCommitJob(); err == nil {
		t.Error("expected error for nil pod, got nil")
	}
}

func TestGenerateCommitJob_NoMatchingContainer(t *testing.T) {
	g := newTestJobGenerator()
	g.Commit.Spec.ContainerName = "missing"
	if _, err := g.GenerateCommitJob(); err == nil {
		t.Error("expected error when commit container is not found in pod status, got nil")
	}
}

func TestGenerateCommitJob_EmptyNodeName(t *testing.T) {
	g := newTestJobGenerator()
	g.Pod.Spec.NodeName = ""
	if _, err := g.GenerateCommitJob(); err == nil {
		t.Error("expected error for empty node name, got nil")
	}
}

func TestGenerateCommitJob_EmptyAgentJobImage(t *testing.T) {
	setEnv(t, EnvAgentJobImage, "")
	g := newTestJobGenerator()
	if _, err := g.GenerateCommitJob(); err == nil {
		t.Error("expected error when AGENT_JOB_IMAGE is empty, got nil")
	}
}

func TestGenerateCommitJob_Success(t *testing.T) {
	setEnv(t, EnvAgentJobImage, "agent-job:latest")
	g := newTestJobGenerator()

	job, err := g.GenerateCommitJob()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := job.GenerateName; got != MakeJobName(g.Commit.Name) {
		t.Errorf("job.GenerateName=%q, want %q", got, MakeJobName(g.Commit.Name))
	}
	if job.Namespace != "test-ns" {
		t.Errorf("job.Namespace=%q, want test-ns", job.Namespace)
	}

	// OwnerReference must point to the Commit and be a controller reference with BlockOwnerDeletion.
	if len(job.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(job.OwnerReferences))
	}
	owner := job.OwnerReferences[0]
	if owner.Kind != "Commit" || owner.Name != "test-commit" {
		t.Errorf("owner reference unexpected: %+v", owner)
	}
	if owner.Controller == nil || !*owner.Controller {
		t.Error("owner reference must be a controller reference")
	}
	if owner.BlockOwnerDeletion == nil || !*owner.BlockOwnerDeletion {
		t.Error("owner reference must set BlockOwnerDeletion=true")
	}

	// Affinity must restrict the job pod to the source pod's node.
	podSpec := job.Spec.Template.Spec
	if podSpec.Affinity == nil || podSpec.Affinity.NodeAffinity == nil {
		t.Fatal("expected node affinity to be set")
	}
	terms := podSpec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchFields) != 1 {
		t.Fatalf("unexpected node selector terms: %+v", terms)
	}
	mf := terms[0].MatchFields[0]
	if mf.Key != "metadata.name" || mf.Operator != corev1.NodeSelectorOpIn ||
		len(mf.Values) != 1 || mf.Values[0] != "test-node" {
		t.Errorf("node selector field unexpected: %+v", mf)
	}

	// Tolerations: a single Exists toleration that matches every taint.
	if len(podSpec.Tolerations) != 1 || podSpec.Tolerations[0].Operator != corev1.TolerationOpExists {
		t.Errorf("tolerations unexpected: %+v", podSpec.Tolerations)
	}

	// Without DockerConfigSecretName the job must not mount the registry secret.
	for _, v := range podSpec.Volumes {
		if v.Name == "docker-config" {
			t.Error("docker-config volume should not exist when DockerConfigSecretName is empty")
		}
	}

	// Without TimeoutSeconds the job must not set ActiveDeadlineSeconds.
	if job.Spec.ActiveDeadlineSeconds != nil {
		t.Errorf("ActiveDeadlineSeconds should be nil when TimeoutSeconds is 0, got %d", *job.Spec.ActiveDeadlineSeconds)
	}

	// Container basics.
	if len(podSpec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(podSpec.Containers))
	}
	c := podSpec.Containers[0]
	if c.Name != "agent-job" || c.Image != "agent-job:latest" {
		t.Errorf("unexpected container basics: name=%q image=%q", c.Name, c.Image)
	}
	if c.SecurityContext == nil || c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 0 {
		t.Error("container must run as uid 0")
	}
}

func TestGenerateCommitJob_WithDockerConfigSecret(t *testing.T) {
	setEnv(t, EnvAgentJobImage, "agent-job:latest")
	g := newTestJobGenerator()
	g.DockerConfigSecretName = "registry-cred"

	job, err := g.GenerateCommitJob()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	podSpec := job.Spec.Template.Spec
	var dockerVol *corev1.Volume
	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].Name == "docker-config" {
			dockerVol = &podSpec.Volumes[i]
			break
		}
	}
	if dockerVol == nil {
		t.Fatal("docker-config volume should be added when DockerConfigSecretName is set")
	}
	if dockerVol.Secret == nil || dockerVol.Secret.SecretName != "registry-cred" {
		t.Errorf("docker-config volume should reference secret %q, got %+v", "registry-cred", dockerVol.Secret)
	}

	var dockerMount *corev1.VolumeMount
	for i := range podSpec.Containers[0].VolumeMounts {
		if podSpec.Containers[0].VolumeMounts[i].Name == "docker-config" {
			dockerMount = &podSpec.Containers[0].VolumeMounts[i]
			break
		}
	}
	if dockerMount == nil {
		t.Fatal("docker-config volume mount should be added")
	}
	if dockerMount.MountPath != "/var/run/secrets/registry" || !dockerMount.ReadOnly {
		t.Errorf("docker-config mount unexpected: %+v", dockerMount)
	}
}

func TestGenerateCommitJob_WithTimeoutSeconds(t *testing.T) {
	setEnv(t, EnvAgentJobImage, "agent-job:latest")
	g := newTestJobGenerator()
	g.Commit.Spec.TimeoutSeconds = 600

	job, err := g.GenerateCommitJob()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if job.Spec.ActiveDeadlineSeconds == nil {
		t.Fatal("ActiveDeadlineSeconds should be set when TimeoutSeconds > 0")
	}
	if *job.Spec.ActiveDeadlineSeconds != 600 {
		t.Errorf("ActiveDeadlineSeconds=%d, want 600", *job.Spec.ActiveDeadlineSeconds)
	}
}
