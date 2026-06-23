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
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	jobutil "github.com/openkruise/agents/pkg/controller/commit/job"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

// defaultCommitRequeueDuration is the requeue interval used when a commit Job is still running.
const defaultCommitRequeueDuration = 30 * time.Second

func init() {
	RegisterCommitControl(CommitControlFactory{
		Name:     CommonControlName,
		Required: true,
		New:      newCommonControl,
	})
}

type commonControl struct {
	client.Client
	Recorder record.EventRecorder
}

func newCommonControl(c client.Client, recorder record.EventRecorder) (CommitControl, error) {
	return &commonControl{Client: c, Recorder: recorder}, nil
}

func (r *commonControl) EnsureCommitRunning(ctx context.Context, args *EnsureFuncArgs) (time.Duration, error) {
	log := log.FromContext(ctx)
	pod, commit := args.Pod, args.Commit

	log.Info("EnsureCommitRunning", "name", commit.Name, "namespace", commit.Namespace, "phase", commit.Status.Phase)

	if _, ok := commit.Annotations[utils.CommitAnnotationModeKey]; !ok {
		patch := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, utils.CommitAnnotationModeKey, CommonControlName)
		rcvObject := &agentsv1alpha1.Commit{ObjectMeta: metav1.ObjectMeta{Namespace: commit.Namespace, Name: commit.Name}}
		if err := r.Patch(ctx, rcvObject, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
			log.Error(err, "patch annotations failed", "commit", klog.KObj(commit))
			return 0, err
		}
	}

	// If a Job already exists for this commit, do not create a duplicate.
	jobList := &batchv1.JobList{}
	if err := r.Client.List(ctx, jobList, client.InNamespace(commit.Namespace), client.MatchingFields{jobutil.IndexFieldCommitUID: string(commit.UID)}); err != nil {
		return 0, fmt.Errorf("failed to list commit jobs: %w", err)
	}
	if len(jobList.Items) > 0 {
		log.Info("commit job already exists, transitioning to Running", "commit", klog.KObj(commit))
		setCommitRunning(args.NewStatus, commit)
		return 0, nil
	}

	// Generate Job spec — permanent errors (bad input, missing config) mark Failed.
	g := &jobutil.JobGenerator{Commit: commit, Pod: pod}
	g.DockerConfigSecretName = r.resolveRegistrySecretName(ctx, commit)
	job, err := g.GenerateCommitJob()
	if err != nil {
		log.Error(err, "failed to generate commit job", "commit", klog.KObj(commit))
		r.Recorder.Eventf(commit, corev1.EventTypeWarning, "JobGenerationFailed", "Failed to generate commit job: %v", err)
		now := metav1.Now()
		args.NewStatus.StartTime = &now
		args.NewStatus.Phase = agentsv1alpha1.CommitPhaseFailed
		args.NewStatus.CompletionTime = &now
		return 0, nil
	}

	// Create Job — transient errors return error for backoff retry, Commit stays Pending.
	if err := r.Client.Create(ctx, job); err != nil {
		if errors.IsAlreadyExists(err) {
			log.Info("Job already exists, ignore", "job", klog.KObj(job), "commit", klog.KObj(commit))
			setCommitRunning(args.NewStatus, commit)
			return 0, nil
		}
		return 0, fmt.Errorf("failed to create job: %w", err)
	}

	// Set expectation to prevent duplicated reconcile from stale cache.
	ScaleExpectations.ExpectScale(utils.GetControllerKey(commit), expectations.Create, job.Name)
	log.Info("created Job", "job", klog.KObj(job), "commit", klog.KObj(commit))

	setCommitRunning(args.NewStatus, commit)
	return 0, nil
}

func setCommitRunning(status *agentsv1alpha1.CommitStatus, commit *agentsv1alpha1.Commit) {
	if status.StartTime == nil {
		now := metav1.Now()
		status.StartTime = &now
	}
	status.Phase = agentsv1alpha1.CommitPhaseRunning
	status.CommitID = commit.Name
}

func (r *commonControl) EnsureCommitUpdated(ctx context.Context, args *EnsureFuncArgs) (time.Duration, error) {
	log := log.FromContext(ctx)
	commit := args.Commit
	log.Info("EnsureCommitUpdated", "commit", klog.KObj(commit), "commitID", commit.Status.CommitID)

	// List Jobs by LabelCommitUID since GenerateName produces a non-deterministic name.
	jobList := &batchv1.JobList{}
	if err := r.Client.List(ctx, jobList, client.InNamespace(commit.Namespace), client.MatchingFields{jobutil.IndexFieldCommitUID: string(commit.UID)}); err != nil {
		return 0, fmt.Errorf("failed to list jobs: %w", err)
	}
	if len(jobList.Items) == 0 {
		log.Info("Job not found, marking commit as failed", "commit", klog.KObj(commit))
		r.Recorder.Eventf(commit, corev1.EventTypeWarning, "JobNotFound", "Commit job not found for commit %s", commit.Name)
		now := metav1.Now()
		args.NewStatus.Phase = agentsv1alpha1.CommitPhaseFailed
		args.NewStatus.CompletionTime = &now
		return 0, nil
	}
	if len(jobList.Items) > 1 {
		log.Info("Multiple jobs found for commit, using the latest one", "commit", klog.KObj(commit), "count", len(jobList.Items))
		sort.Slice(jobList.Items, func(i, j int) bool {
			return jobList.Items[i].CreationTimestamp.After(jobList.Items[j].CreationTimestamp.Time)
		})
	}
	job := &jobList.Items[0]

	done, success := jobutil.IsJobCompleted(job)
	if !done {
		log.Info("Job still running, will requeue", "job", klog.KObj(job), "commit", klog.KObj(commit))
		return defaultCommitRequeueDuration, nil
	}

	log.Info("Job completed", "job", klog.KObj(job), "commit", klog.KObj(commit), "success", success)
	phase := agentsv1alpha1.CommitPhaseSucceeded
	if !success {
		phase = agentsv1alpha1.CommitPhaseFailed
	}
	if condition := r.getLatestJobPodExitCode(ctx, commit); condition != nil {
		args.NewStatus.Conditions = append(args.NewStatus.Conditions, *condition)
	}
	args.NewStatus.Phase = phase
	now := metav1.Now()
	args.NewStatus.CompletionTime = &now
	return 0, nil
}

func (r *commonControl) EnsureCommitDeleted(ctx context.Context, args *EnsureFuncArgs) (time.Duration, error) {
	log := log.FromContext(ctx)
	commit := args.Commit

	_, err := utils.PatchFinalizer(ctx, r.Client, commit, utils.RemoveFinalizerOpType, agentsv1alpha1.CommitFinalizer)
	if err != nil {
		log.Error(err, "remove commit finalizer failed", "commit", klog.KObj(commit))
		return 0, err
	}
	log.Info("remove commit finalizer success", "commit", klog.KObj(commit))
	return 0, nil
}

// resolveRegistrySecretName resolves the registry auth secret from user-specified
// spec.registryAuth.secrets. Returns the secret name, or empty string if none found.
func (r *commonControl) resolveRegistrySecretName(ctx context.Context, commit *agentsv1alpha1.Commit) string {
	log := log.FromContext(ctx)
	ns := commit.Namespace

	if commit.Spec.RegistryAuth != nil {
		for _, name := range commit.Spec.RegistryAuth.Secrets {
			secret := &corev1.Secret{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, secret); err != nil {
				log.V(4).Info("Failed to get registryAuth secret, trying next", "namespace", ns, "name", name, "err", err)
				continue
			}
			if secret.Type == corev1.SecretTypeDockerConfigJson {
				log.Info("Using registryAuth secret for registry auth", "namespace", ns, "name", name)
				return name
			}
		}
	}

	log.Info("No registry secret found, commit will attempt anonymous push", "commit", klog.KObj(commit))
	return ""
}

func (r *commonControl) listCRJobPods(ctx context.Context, commit *agentsv1alpha1.Commit) (*corev1.PodList, error) {
	jobPods := &corev1.PodList{}
	// Filter by the commit-uid label set on the Job pod template. This works
	// with GenerateName (where the Job name is not deterministic) and avoids
	// relying on the batch.kubernetes.io/job-name label which requires knowing
	// the generated name.
	matchingFields := client.MatchingFields{
		jobutil.IndexFieldCommitUID: string(commit.UID),
	}
	if err := r.Client.List(ctx, jobPods, client.InNamespace(commit.Namespace), matchingFields); err != nil {
		return nil, err
	}
	return jobPods, nil
}

func (r *commonControl) getLatestJobPodExitCode(ctx context.Context, commit *agentsv1alpha1.Commit) *metav1.Condition {
	log := log.FromContext(ctx)
	jobPods, err := r.listCRJobPods(ctx, commit)
	if err != nil {
		log.Error(err, "list job pods failed", "commit", klog.KObj(commit))
		return nil
	}
	if jobPods == nil || len(jobPods.Items) == 0 {
		log.Info("job pods not found", "commit", klog.KObj(commit))
		return nil
	}
	sort.Slice(jobPods.Items, func(i, j int) bool {
		return jobPods.Items[i].CreationTimestamp.After(jobPods.Items[j].CreationTimestamp.Time)
	})
	return jobutil.GetCommitCondition(ctx, &jobPods.Items[0])
}
