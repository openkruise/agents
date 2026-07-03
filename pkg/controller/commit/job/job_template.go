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
	"flag"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
)

var backoffLimit = 1

func init() {
	flag.IntVar(&backoffLimit, "job-backoff-limit", backoffLimit, "BackoffLimit for agent job.")
}

// JobGenerator generates K8s Job specs for commit operations.
type JobGenerator struct {
	Commit *v1alpha1.Commit
	Pod    *corev1.Pod
	// DockerConfigSecretName is the name of an existing Secret (same namespace as Pod)
	// to mount as registry auth for pushing. Empty means anonymous push.
	DockerConfigSecretName string
}

func (g *JobGenerator) commitContainerID() string {
	for _, status := range g.Pod.Status.ContainerStatuses {
		if status.Name == g.Commit.Spec.ContainerName {
			containerID := strings.TrimPrefix(status.ContainerID, "containerd://")
			if containerID == status.ContainerID {
				return ""
			}
			return containerID
		}
	}
	return ""
}

func (g *JobGenerator) commitLabels() map[string]string {
	return map[string]string{
		LabelCommitName: g.Commit.Name,
		LabelCommitUID:  string(g.Commit.UID),
	}
}

func (g *JobGenerator) commitEnvs() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: EnvContainerID, Value: g.commitContainerID()},
		{Name: EnvCommitNamespace, Value: g.Commit.Namespace},
		{Name: EnvCommitName, Value: g.Commit.Name},
		{Name: EnvCommitImage, Value: g.Commit.Spec.Image},
		{Name: EnvContainerName, Value: g.Commit.Spec.ContainerName},
		{Name: EnvAgentJobActionKey, Value: EnvAgentJobActionCommit},
		{Name: EnvCommitPodName, Value: g.Commit.Spec.PodName},
		{Name: EnvCommitPodNamespace, Value: g.Pod.Namespace},
		{Name: EnvCommitPodUID, Value: string(g.Pod.UID)},
	}
}

func (g *JobGenerator) volumes() ([]corev1.Volume, []corev1.VolumeMount) {
	directoryOrCreate := corev1.HostPathDirectoryOrCreate
	containerdPath := Config().ContainerdSockPath()
	volumes := []corev1.Volume{
		{
			Name: "host-containerd-run",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: containerdPath,
					Type: &directoryOrCreate,
				},
			},
		},
		{
			Name: "host-containerd-certs",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/etc/containerd/certs.d",
					Type: &directoryOrCreate,
				},
			},
		},
	}
	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "host-containerd-run",
			MountPath: containerdPath,
		},
		{
			Name:      "host-containerd-certs",
			MountPath: "/etc/containerd/certs.d",
			ReadOnly:  true,
		},
	}
	return volumes, volumeMounts
}

// GenerateCommitJob creates a K8s Job spec for the commit operation.
func (g *JobGenerator) GenerateCommitJob() (*batchv1.Job, error) {
	if g.Commit == nil {
		return nil, fmt.Errorf("commit is nil")
	}
	if g.Pod == nil {
		return nil, fmt.Errorf("pod is nil")
	}
	if g.commitContainerID() == "" {
		return nil, fmt.Errorf("containerd container not found in pod status")
	}
	if g.Pod.Spec.NodeName == "" {
		return nil, fmt.Errorf("pod node name is empty (unscheduled)")
	}

	jobImage := Config().AgentJobImage()
	if jobImage == "" {
		return nil, fmt.Errorf("env AGENT_JOB_IMAGE is empty")
	}

	volumes, volumeMounts := g.volumes()

	// Mount registry auth secret for pushing (nerdctl push)
	if g.DockerConfigSecretName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "docker-config",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: g.DockerConfigSecretName,
					Items: []corev1.KeyToPath{
						{Key: ".dockerconfigjson", Path: "config.json"},
					},
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "docker-config",
			MountPath: "/var/run/secrets/registry",
			ReadOnly:  true,
		})
	}

	rootUID := int64(0)
	trueVal := true
	backoff := int32(backoffLimit) // #nosec G115 -- backoffLimit is a small constant

	// Map CommitSpec.TimeoutSeconds to Job's ActiveDeadlineSeconds
	var activeDeadlineSeconds *int64
	if g.Commit.Spec.TimeoutSeconds > 0 {
		deadline := int64(g.Commit.Spec.TimeoutSeconds)
		activeDeadlineSeconds = &deadline
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: MakeJobName(g.Commit.Name),
			Namespace:    g.Pod.Namespace,
			Labels:       g.commitLabels(),
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         v1alpha1.SchemeGroupVersion.String(),
					Kind:               "Commit",
					Name:               g.Commit.Name,
					UID:                g.Commit.UID,
					Controller:         &trueVal,
					BlockOwnerDeletion: &trueVal,
				},
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: activeDeadlineSeconds,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: g.commitLabels(),
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchFields: []corev1.NodeSelectorRequirement{
											{
												Key:      "metadata.name",
												Operator: corev1.NodeSelectorOpIn,
												Values:   []string{g.Pod.Spec.NodeName},
											},
										},
									},
								},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
					HostNetwork:   true,
					Tolerations: []corev1.Toleration{
						{Operator: corev1.TolerationOpExists},
					},
					Volumes: volumes,
					Containers: []corev1.Container{
						{
							Name:            AgentJobContainerName,
							Image:           jobImage,
							VolumeMounts:    volumeMounts,
							ImagePullPolicy: Config().ImagePullPolicy(),
							Env:             g.commitEnvs(),
							SecurityContext: &corev1.SecurityContext{
								RunAsUser: &rootUID,
							},
						},
					},
				},
			},
		},
	}

	return job, nil
}
