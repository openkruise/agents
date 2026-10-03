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
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

// AgentJobContainerName is the name of the single container inside the commit
// Job pod. Both the Job spec generator and downstream container-status readers
// must reference this constant so that injected sidecars (e.g. service mesh
// proxies) cannot accidentally pollute exit-code lookups.
const AgentJobContainerName = "commit-job"

const (
	ExitCodeSuccess              = 0
	ExitCodeCommitFailed         = 1
	ExitCodeGetImageSizeFailed   = 2
	ExitCodeParseImageSizeFailed = 3
	ExitCodePushFailed           = 4
	ExitCodeGetSandboxIDFailed   = 5
)

func MakeJobName(commitName string) string {
	const maxPrefix = 50
	name := commitName
	if len(name) > maxPrefix {
		name = name[:maxPrefix]
	}
	return fmt.Sprintf("commit-%s-", name)
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

type commitConditionValue struct {
	conditionType   string
	conditionReason string
}

var commitJobExitCodeMap = map[int32]commitConditionValue{
	ExitCodeSuccess:              {string(v1alpha1.CommitConditionTypePushCommittedImage), "PushCommittedImageSuccess"},
	ExitCodeCommitFailed:         {string(v1alpha1.CommitConditionTypeCommitContainer), "CommitContainerFailed"},
	ExitCodeGetImageSizeFailed:   {string(v1alpha1.CommitConditionTypeCommitContainer), "GetImageSizeFailed"},
	ExitCodeParseImageSizeFailed: {string(v1alpha1.CommitConditionTypeCommitContainer), "ParseImageSizeFailed"},
	ExitCodePushFailed:           {string(v1alpha1.CommitConditionTypePushCommittedImage), "PushCommittedImageFailed"},
	ExitCodeGetSandboxIDFailed:   {string(v1alpha1.CommitConditionTypeCommitContainer), "GetSandboxIDFailed"},
}

func GetCommitCondition(ctx context.Context, pod *corev1.Pod) *metav1.Condition {
	log := log.FromContext(ctx)
	for _, cs := range pod.Status.ContainerStatuses {
		// Only consider the canonical commit-job container; ignore any sidecar or
		// init container that may have been injected by webhooks.
		if cs.Name != AgentJobContainerName {
			continue
		}
		if cs.State.Terminated != nil {
			conditionValue, ok := commitJobExitCodeMap[cs.State.Terminated.ExitCode]
			if !ok {
				// An unmapped exit code (e.g. 137 OOM-kill, 143 SIGTERM) still means
				// the commit job failed; record it instead of dropping the condition.
				log.Info("Unknown exit code, recording generic condition", "containerID", cs.ContainerID, "exitCode", cs.State.Terminated.ExitCode)
				return &metav1.Condition{
					Type:               string(v1alpha1.CommitConditionTypeCommitJob),
					Status:             metav1.ConditionFalse,
					Reason:             "UnknownExitCode",
					Message:            utils.TruncateConditionMessage(fmt.Sprintf("Commit job container exited with unknown code %d", cs.State.Terminated.ExitCode)),
					LastTransitionTime: metav1.Now(),
				}
			}
			log.Info("Commit job container terminated",
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
				Message:            utils.TruncateConditionMessage(cs.State.Terminated.Message),
				LastTransitionTime: metav1.Now(),
			}
			return cond
		}
	}
	return nil
}

// JobTerminalTime returns when the Job reached its terminal state: the
// CompletionTime for succeeded jobs, or the JobComplete/JobFailed condition's
// LastTransitionTime otherwise. Returns the zero time when no terminal
// timestamp is set.
func JobTerminalTime(job *batchv1.Job) time.Time {
	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime.Time
	}
	for _, cond := range job.Status.Conditions {
		if (cond.Type == batchv1.JobComplete || cond.Type == batchv1.JobFailed) &&
			cond.Status == corev1.ConditionTrue && !cond.LastTransitionTime.IsZero() {
			return cond.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

// FallbackCommitCondition derives a Commit condition from the terminal Job
// status when the Job pod's exit code cannot be observed, because the pod is
// missing from the informer cache or has already been deleted. It must only be
// called for jobs where IsJobCompleted reports done.
func FallbackCommitCondition(job *batchv1.Job) *metav1.Condition {
	_, success := IsJobCompleted(job)
	if success {
		// A Complete Job with completions=1 implies the container exited 0, so
		// the committed image push succeeded.
		return &metav1.Condition{
			Type:               string(v1alpha1.CommitConditionTypePushCommittedImage),
			Status:             metav1.ConditionTrue,
			Reason:             "PushCommittedImageSuccess",
			Message:            "Commit job completed; container exit code was not observable",
			LastTransitionTime: metav1.Now(),
		}
	}
	message := "Commit job failed; container exit code was not observable"
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue && cond.Message != "" {
			message = cond.Message
			break
		}
	}
	return &metav1.Condition{
		Type:               string(v1alpha1.CommitConditionTypeCommitJob),
		Status:             metav1.ConditionFalse,
		Reason:             "CommitJobFailed",
		Message:            utils.TruncateConditionMessage(message),
		LastTransitionTime: metav1.Now(),
	}
}
