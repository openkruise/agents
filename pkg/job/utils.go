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

const (
	ExitCodeSuccess      = 0
	ExitCodeCommitFailed = 1
	ExitCodePushFailed   = 2
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
	ExitCodeSuccess:      {"PushCommittedImage", "PushCommittedImageSuccess"},
	ExitCodeCommitFailed: {"CommitContainer", "CommitContainerFailed"},
	ExitCodePushFailed:   {"PushCommittedImage", "PushCommittedImageFailed"},
}

func GetCommitCondition(pod *corev1.Pod) *metav1.Condition {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != "agent-job" {
			continue
		}
		if cs.State.Terminated != nil {
			exitCode := cs.State.Terminated.ExitCode
			conditionValue, ok := CommitJobExitCodeMap[exitCode]
			if !ok {
				conditionValue = CommitConditionValue{"CommitContainer", "UnknownExitCode"}
			}
			klog.InfoS("Commit job exit", "containerID", cs.ContainerID, "exitCode", exitCode)
			status := metav1.ConditionTrue
			if exitCode != 0 {
				status = metav1.ConditionFalse
			}
			return &metav1.Condition{
				Type:               conditionValue.conditionType,
				Status:             status,
				Reason:             conditionValue.conditionReason,
				LastTransitionTime: metav1.Now(),
			}
		}
	}
	return nil
}
