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

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	jobutil "github.com/openkruise/agents/pkg/job"
	"github.com/openkruise/agents/pkg/utils"
)

const CommonControlName = "common"

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
	pod, commit := args.Pod, args.Commit
	var requeueAfter time.Duration

	klog.InfoS("EnsureCommitRunning", "name", commit.Name, "namespace", commit.Namespace, "phase", commit.Status.Phase)

	if _, ok := commit.Annotations[utils.CommitAnnotationModeKey]; !ok {
		patch := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, utils.CommitAnnotationModeKey, CommonControlName)
		rcvObject := &agentsv1alpha1.Commit{ObjectMeta: metav1.ObjectMeta{Namespace: commit.Namespace, Name: commit.Name}}
		if err := r.Patch(ctx, rcvObject, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
			klog.ErrorS(err, "patch annotations failed", "commit", klog.KObj(commit))
			return requeueAfter, err
		}
	}

	jobPods, err := r.listCRJobPods(ctx, commit)
	if err != nil {
		return requeueAfter, fmt.Errorf("failed to list commit job pods: %w", err)
	}
	if len(jobPods.Items) > 0 {
		klog.InfoS("commit job pod already exists, ignore", "commit", klog.KObj(commit))
		return requeueAfter, nil
	}

	if err = r.applyCommitJob(ctx, commit, pod); err != nil {
		klog.ErrorS(err, "EnsureCommitRunning failed", "commit", klog.KObj(commit))
		now := metav1.Now()
		args.NewStatus.StartTime = &now
		args.NewStatus.Phase = agentsv1alpha1.CommitFailed
		args.NewStatus.CompletionTime = &now
		return requeueAfter, err
	}

	now := metav1.Now()
	args.NewStatus.StartTime = &now
	args.NewStatus.Phase = agentsv1alpha1.CommitRunning
	args.NewStatus.CommitID = commit.Name
	return requeueAfter, nil
}

func (r *commonControl) EnsureCommitUpdated(ctx context.Context, args *EnsureFuncArgs) (time.Duration, error) {
	commit := args.Commit
	var requeueAfter time.Duration
	klog.InfoS("EnsureCommitUpdated", "commit", klog.KObj(commit), "commitID", commit.Status.CommitID)

	job := new(batchv1.Job)
	jobKey := client.ObjectKey{Namespace: commit.Namespace, Name: jobutil.MakeJobName(string(commit.UID))}
	if err := r.Client.Get(ctx, jobKey, job); err != nil {
		if errors.IsNotFound(err) {
			klog.InfoS("Job not found", "commit", klog.KObj(commit))
			now := metav1.Now()
			args.NewStatus.Phase = agentsv1alpha1.CommitFailed
			args.NewStatus.CompletionTime = &now
			return requeueAfter, err
		}
		return requeueAfter, fmt.Errorf("failed to get job: %w", err)
	}

	done, success := jobutil.IsJobCompleted(job)
	klog.InfoS("job status", "job", klog.KObj(job), "commit", klog.KObj(commit), "success", success)
	if done {
		phase := agentsv1alpha1.CommitSucceeded
		if !success {
			phase = agentsv1alpha1.CommitFailed
		}
		if condition := r.getLatestJobPodExitCode(ctx, commit); condition != nil {
			args.NewStatus.Conditions = append(args.NewStatus.Conditions, *condition)
		}
		args.NewStatus.Phase = phase
		now := metav1.NewTime(time.Now())
		args.NewStatus.CompletionTime = &now
	}
	return requeueAfter, nil
}

func (r *commonControl) EnsureCommitDeleted(ctx context.Context, args *EnsureFuncArgs) (time.Duration, error) {
	commit := args.Commit
	var requeueAfter time.Duration

	_, err := utils.PatchFinalizer(ctx, r.Client, commit, utils.RemoveFinalizerOpType, agentsv1alpha1.CommitFinalizer)
	if err != nil {
		klog.ErrorS(err, "remove commit finalizer failed", "commit", klog.KObj(commit))
		return requeueAfter, err
	}
	klog.InfoS("remove commit finalizer success", "commit", klog.KObj(commit))
	return requeueAfter, nil
}

func (r *commonControl) applyCommitJob(ctx context.Context, commit *agentsv1alpha1.Commit, pod *corev1.Pod) error {
	g := &jobutil.JobGenerator{Commit: commit, Pod: pod}

	// Resolve registry auth secret using three-tier fallback
	resolvedSecret := r.resolveRegistrySecret(ctx, commit, pod)
	if resolvedSecret != nil {
		if resolvedSecret.Namespace != pod.Namespace {
			// Cross-namespace secret: create a mirror in the Job namespace so it can be mounted
			mirror, err := r.ensureMirrorSecret(ctx, commit, resolvedSecret, pod.Namespace)
			if err != nil {
				return fmt.Errorf("failed to create mirror secret for cross-namespace registry auth: %w", err)
			}
			g.DockerConfigSecretName = mirror.Name
		} else {
			g.DockerConfigSecretName = resolvedSecret.Name
		}
	}

	job, err := g.GenerateCommitJob()
	if err != nil {
		return fmt.Errorf("failed to generate commit job: %v", err)
	}
	if err := r.Client.Create(ctx, job); err != nil {
		if errors.IsAlreadyExists(err) {
			klog.InfoS("Job already exists, ignore", "job", klog.KObj(job), "commit", klog.KObj(commit))
			return nil
		}
		return fmt.Errorf("failed to create job: %w", err)
	}
	klog.InfoS("created Job", "job", klog.KObj(job), "commit", klog.KObj(commit))
	return nil
}

// ensureMirrorSecret creates a copy of the source Secret in the target namespace,
// owned by the Commit CR so that Kubernetes GC cleans it up on Commit deletion.
func (r *commonControl) ensureMirrorSecret(ctx context.Context, commit *agentsv1alpha1.Commit, source *corev1.Secret, targetNamespace string) (*corev1.Secret, error) {
	mirrorName := fmt.Sprintf("commit-%s-registry-auth", commit.Name)
	trueVal := true
	mirror := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mirrorName,
			Namespace: targetNamespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         agentsv1alpha1.SchemeGroupVersion.String(),
					Kind:               "Commit",
					Name:               commit.Name,
					UID:                commit.UID,
					BlockOwnerDeletion: &trueVal,
				},
			},
		},
		Type: source.Type,
		Data: source.Data,
	}
	if err := r.Create(ctx, mirror); err != nil {
		if errors.IsAlreadyExists(err) {
			klog.InfoS("Mirror secret already exists", "name", mirrorName, "namespace", targetNamespace)
			return mirror, nil
		}
		return nil, err
	}
	klog.InfoS("Created mirror secret for cross-namespace registry auth",
		"source", fmt.Sprintf("%s/%s", source.Namespace, source.Name),
		"mirror", fmt.Sprintf("%s/%s", targetNamespace, mirrorName))
	return mirror, nil
}

// resolveRegistrySecret implements the three-tier fallback for registry authentication:
// Tier 1: spec.pushSecrets — explicitly specified secrets
// Tier 2: DockerKeyring Lookup across namespace secrets (future enhancement)
// Tier 3: Pod's ServiceAccount imagePullSecrets (best-effort)
// Returns the resolved Secret object, or nil if no matching secret is found.
func (r *commonControl) resolveRegistrySecret(ctx context.Context, commit *agentsv1alpha1.Commit, pod *corev1.Pod) *corev1.Secret {
	// Tier 1: Explicit pushSecrets
	for _, ref := range commit.Spec.PushSecrets {
		ns := ref.Namespace
		if ns == "" {
			ns = commit.Namespace
		}
		secret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, secret); err != nil {
			klog.V(4).InfoS("Failed to get pushSecret, trying next", "namespace", ns, "name", ref.Name, "err", err)
			continue
		}
		if secret.Type == corev1.SecretTypeDockerConfigJson {
			klog.InfoS("Using pushSecret for registry auth", "namespace", ns, "name", ref.Name)
			return secret
		}
	}

	// Tier 3: ServiceAccount imagePullSecrets (best-effort)
	if pod != nil {
		sa := &corev1.ServiceAccount{}
		saName := pod.Spec.ServiceAccountName
		if saName == "" {
			saName = "default"
		}
		if err := r.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: saName}, sa); err == nil {
			for _, ref := range sa.ImagePullSecrets {
				secret := &corev1.Secret{}
				if err := r.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: ref.Name}, secret); err == nil {
					if secret.Type == corev1.SecretTypeDockerConfigJson {
						klog.InfoS("Using SA imagePullSecret for registry auth (best-effort)", "namespace", pod.Namespace, "name", ref.Name)
						return secret
					}
				}
			}
		}
	}

	klog.InfoS("No registry secret found, commit will attempt anonymous push", "commit", klog.KObj(commit))
	return nil
}

func (r *commonControl) listCRJobPods(ctx context.Context, commit *agentsv1alpha1.Commit) (*corev1.PodList, error) {
	jobPods := &corev1.PodList{}
	matchingLabels := client.MatchingLabels{
		jobutil.LabelCommitUID: string(commit.UID),
	}
	if err := r.Client.List(ctx, jobPods, client.InNamespace(commit.Namespace), matchingLabels); err != nil {
		return nil, err
	}
	return jobPods, nil
}

func (r *commonControl) getLatestJobPodExitCode(ctx context.Context, commit *agentsv1alpha1.Commit) *metav1.Condition {
	jobPods, err := r.listCRJobPods(ctx, commit)
	if err != nil {
		klog.ErrorS(err, "list job pods failed", "commit", klog.KObj(commit))
		return nil
	}
	if jobPods == nil || len(jobPods.Items) == 0 {
		klog.InfoS("job pods not found", "commit", klog.KObj(commit))
		return nil
	}
	sort.Slice(jobPods.Items, func(i, j int) bool {
		return jobPods.Items[i].CreationTimestamp.After(jobPods.Items[j].CreationTimestamp.Time)
	})
	return jobutil.GetCommitCondition(&jobPods.Items[0])
}
