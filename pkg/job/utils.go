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
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

const (
	LabelCommitName = "agents.kruise.io/commit-name"
	LabelCommitUID  = "agents.kruise.io/commit-uid"
)

// AgentJobContainerName is the name of the single container inside the commit
// Job pod. Both the Job spec generator and downstream container-status readers
// must reference this constant so that injected sidecars (e.g. service mesh
// proxies) cannot accidentally pollute exit-code lookups.
const AgentJobContainerName = "agent-job"

const (
	ExitCodeSuccess              = 0
	ExitCodeCommitFailed         = 1
	ExitCodeGetImageSizeFailed   = 2
	ExitCodeParseImageSizeFailed = 3
	ExitCodePushFailed           = 4
	ExitCodeGetSandboxIDFailed   = 5
)

func MakeJobName(uid string) string {
	return fmt.Sprintf("agent-job-%s", strings.ReplaceAll(uid, "-", ""))
}

func IsJobCompleted(job *batchv1.Job) (bool, bool) {
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			return true, true
		}
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			return true, false
		}
	}
	return false, false
}

type CommitConditionValue struct {
	conditionType   string
	conditionReason string
}

var CommitJobExitCodeMap = map[int32]CommitConditionValue{
	ExitCodeSuccess:              {"PushCommittedImage", "PushCommittedImageSuccess"},
	ExitCodeCommitFailed:         {"CommitContainer", "CommitContainerFailed"},
	ExitCodeGetImageSizeFailed:   {"CommitContainer", "GetImageSizeFailed"},
	ExitCodeParseImageSizeFailed: {"CommitContainer", "ParseImageSizeFailed"},
	ExitCodePushFailed:           {"PushCommittedImage", "PushCommittedImageFailed"},
	ExitCodeGetSandboxIDFailed:   {"CommitContainer", "GetSandboxIDFailed"},
}

func GetCommitCondition(pod *corev1.Pod) *metav1.Condition {
	for _, cs := range pod.Status.ContainerStatuses {
		// Only consider the canonical commit-job container; ignore any sidecar or
		// init container that may have been injected by webhooks.
		if cs.Name != AgentJobContainerName {
			continue
		}
		if cs.State.Terminated != nil {
			conditionValue, ok := CommitJobExitCodeMap[cs.State.Terminated.ExitCode]
			if !ok {
				klog.InfoS("Unknown exit code, skipping condition", "containerID", cs.ContainerID, "exitCode", cs.State.Terminated.ExitCode)
				return nil
			}
			klog.InfoS("Commit job container terminated",
				"containerID", cs.ContainerID,
				"exitCode", cs.State.Terminated.ExitCode,
				"type", conditionValue.conditionType,
				"reason", conditionValue.conditionReason)
			status := metav1.ConditionTrue
			if cs.State.Terminated.ExitCode != 0 {
				status = metav1.ConditionFalse
			}
			cond := &metav1.Condition{
				Type:               conditionValue.conditionType,
				Status:             status,
				Reason:             conditionValue.conditionReason,
				LastTransitionTime: metav1.Now(),
			}
			return cond
		}
	}
	return nil
}
