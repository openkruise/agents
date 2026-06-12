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
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/distribution/reference"
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

	klog.InfoS("EnsureCommitRunning", "name", commit.Name, "namespace", commit.Namespace, "phase", commit.Status.Phase)

	if _, ok := commit.Annotations[utils.CommitAnnotationModeKey]; !ok {
		patch := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, utils.CommitAnnotationModeKey, CommonControlName)
		rcvObject := &agentsv1alpha1.Commit{ObjectMeta: metav1.ObjectMeta{Namespace: commit.Namespace, Name: commit.Name}}
		if err := r.Patch(ctx, rcvObject, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
			klog.ErrorS(err, "patch annotations failed", "commit", klog.KObj(commit))
			return 0, err
		}
	}

	jobPods, err := r.listCRJobPods(ctx, commit)
	if err != nil {
		return 0, fmt.Errorf("failed to list commit job pods: %w", err)
	}
	if len(jobPods.Items) > 0 {
		klog.InfoS("commit job pod already exists, transitioning to Running", "commit", klog.KObj(commit))
		now := metav1.Now()
		args.NewStatus.StartTime = &now
		args.NewStatus.Phase = agentsv1alpha1.CommitPhaseRunning
		return 0, nil
	}

	now := metav1.Now()
	if err = r.applyCommitJob(ctx, commit, pod); err != nil {
		klog.ErrorS(err, "EnsureCommitRunning failed", "commit", klog.KObj(commit))
		args.NewStatus.StartTime = &now
		args.NewStatus.Phase = agentsv1alpha1.CommitPhaseFailed
		args.NewStatus.CompletionTime = &now
		return 0, err
	}

	args.NewStatus.StartTime = &now
	args.NewStatus.Phase = agentsv1alpha1.CommitPhaseRunning
	args.NewStatus.CommitID = commit.Name
	return 0, nil
}

func (r *commonControl) EnsureCommitUpdated(ctx context.Context, args *EnsureFuncArgs) (time.Duration, error) {
	commit := args.Commit
	klog.InfoS("EnsureCommitUpdated", "commit", klog.KObj(commit), "commitID", commit.Status.CommitID)

	job := new(batchv1.Job)
	jobKey := client.ObjectKey{Namespace: commit.Namespace, Name: jobutil.MakeJobName(string(commit.UID))}
	if err := r.Client.Get(ctx, jobKey, job); err != nil {
		if errors.IsNotFound(err) {
			klog.InfoS("Job not found, marking commit as failed", "commit", klog.KObj(commit))
			now := metav1.Now()
			args.NewStatus.Phase = agentsv1alpha1.CommitPhaseFailed
			args.NewStatus.CompletionTime = &now
			return 0, nil
		}
		return 0, fmt.Errorf("failed to get job: %w", err)
	}

	done, success := jobutil.IsJobCompleted(job)
	if !done {
		klog.InfoS("Job still running, will requeue", "job", klog.KObj(job), "commit", klog.KObj(commit))
		return 30 * time.Second, nil
	}

	klog.InfoS("Job completed", "job", klog.KObj(job), "commit", klog.KObj(commit), "success", success)
	phase := agentsv1alpha1.CommitPhaseSucceeded
	if !success {
		phase = agentsv1alpha1.CommitPhaseFailed
	}
	if condition := r.getLatestJobPodExitCode(ctx, commit); condition != nil {
		args.NewStatus.Conditions = append(args.NewStatus.Conditions, *condition)
	}
	args.NewStatus.Phase = phase
	now := metav1.NewTime(time.Now())
	args.NewStatus.CompletionTime = &now
	return 0, nil
}

func (r *commonControl) EnsureCommitDeleted(ctx context.Context, args *EnsureFuncArgs) (time.Duration, error) {
	commit := args.Commit

	_, err := utils.PatchFinalizer(ctx, r.Client, commit, utils.RemoveFinalizerOpType, agentsv1alpha1.CommitFinalizer)
	if err != nil {
		klog.ErrorS(err, "remove commit finalizer failed", "commit", klog.KObj(commit))
		return 0, err
	}
	klog.InfoS("remove commit finalizer success", "commit", klog.KObj(commit))
	return 0, nil
}

func (r *commonControl) applyCommitJob(ctx context.Context, commit *agentsv1alpha1.Commit, pod *corev1.Pod) error {
	g := &jobutil.JobGenerator{Commit: commit, Pod: pod}

	// Resolve registry auth secret (same namespace, mounted directly by name)
	g.DockerConfigSecretName = r.resolveRegistrySecretName(ctx, commit, pod)

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

// resolveRegistrySecretName implements the three-tier fallback for registry authentication.
// All secrets are resolved within the Commit's namespace (same as Pod/Job namespace).
// Tier 1: spec.registryAuth.secrets — explicitly specified secret names
// Tier 2: DockerKeyring lookup across all dockerconfigjson secrets in namespace
// Tier 3: Pod's ServiceAccount imagePullSecrets (best-effort)
// Returns the secret name, or empty string if no matching secret is found.
func (r *commonControl) resolveRegistrySecretName(ctx context.Context, commit *agentsv1alpha1.Commit, pod *corev1.Pod) string {
	ns := commit.Namespace

	// Tier 1: Explicit registryAuth.secrets
	if commit.Spec.RegistryAuth != nil {
		for _, name := range commit.Spec.RegistryAuth.Secrets {
			secret := &corev1.Secret{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, secret); err != nil {
				klog.V(4).InfoS("Failed to get registryAuth secret, trying next", "namespace", ns, "name", name, "err", err)
				continue
			}
			if secret.Type == corev1.SecretTypeDockerConfigJson {
				klog.InfoS("Using registryAuth secret for registry auth", "namespace", ns, "name", name)
				return name
			}
		}
	}

	// Tier 2: DockerKeyring lookup across all dockerconfigjson secrets in namespace
	if name := r.resolveRegistrySecretByKeyring(ctx, commit); name != "" {
		return name
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
						return ref.Name
					}
				}
			}
		}
	}

	klog.InfoS("No registry secret found, commit will attempt anonymous push", "commit", klog.KObj(commit))
	return ""
}

// resolveRegistrySecretByKeyring lists all dockerconfigjson secrets in the Commit's namespace,
// parses their auth configs, and returns the name of the first secret that contains
// credentials matching the target registry host.
func (r *commonControl) resolveRegistrySecretByKeyring(ctx context.Context, commit *agentsv1alpha1.Commit) string {
	targetRegistry := extractRegistryHost(commit.Spec.Image)
	if targetRegistry == "" {
		return ""
	}

	secretList := &corev1.SecretList{}
	if err := r.Client.List(ctx, secretList, client.InNamespace(commit.Namespace)); err != nil {
		klog.V(4).InfoS("Failed to list secrets in namespace for Tier 2 lookup", "namespace", commit.Namespace, "err", err)
		return ""
	}

	for i := range secretList.Items {
		secret := &secretList.Items[i]
		if secret.Type != corev1.SecretTypeDockerConfigJson && secret.Type != corev1.SecretTypeDockercfg {
			continue
		}
		if matchRegistryInDockerConfig(secret, targetRegistry) {
			klog.InfoS("Using namespace secret for registry auth (Tier 2)",
				"namespace", secret.Namespace, "name", secret.Name, "registry", targetRegistry)
			return secret.Name
		}
	}

	return ""
}

// dockerConfigJSON represents the structure of a .dockerconfigjson secret.
type dockerConfigJSON struct {
	Auths map[string]dockerConfigEntry `json:"auths"`
}

type dockerConfigEntry struct {
	Auth string `json:"auth,omitempty"`
}

// matchRegistryInDockerConfig checks whether the given secret contains credentials
// for the target registry host by parsing its .dockerconfigjson data.
func matchRegistryInDockerConfig(secret *corev1.Secret, targetRegistry string) bool {
	var raw []byte
	if secret.Type == corev1.SecretTypeDockerConfigJson {
		raw = secret.Data[".dockerconfigjson"]
	} else {
		raw = secret.Data[".dockercfg"]
	}
	if len(raw) == 0 {
		return false
	}

	var cfg dockerConfigJSON
	if secret.Type == corev1.SecretTypeDockercfg {
		// .dockercfg is a flat map: {"registry": {"auth": "..."}}
		var flat map[string]dockerConfigEntry
		if err := json.Unmarshal(raw, &flat); err != nil {
			return false
		}
		cfg.Auths = flat
	} else {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return false
		}
	}

	for registryKey := range cfg.Auths {
		if registryHostMatches(registryKey, targetRegistry) {
			return true
		}
	}
	return false
}

// registryHostMatches checks if a registry key from dockerconfigjson matches the target host.
// Registry keys can be full URLs (https://registry.example.com) or plain hosts (registry.example.com).
func registryHostMatches(registryKey, targetHost string) bool {
	// Normalize: strip scheme prefix if present
	host := registryKey
	if idx := strings.Index(host, "://"); idx >= 0 {
		host = host[idx+3:]
	}
	// Strip path suffix (e.g. "https://index.docker.io/v1/" -> "index.docker.io")
	if idx := strings.Index(host, "/"); idx >= 0 {
		host = host[:idx]
	}

	// docker.io normalization: "index.docker.io" and "docker.io" are equivalent
	host = normalizeDockerHost(host)
	target := normalizeDockerHost(targetHost)

	return strings.EqualFold(host, target)
}

func normalizeDockerHost(host string) string {
	switch strings.ToLower(host) {
	case "index.docker.io", "docker.io", "registry-1.docker.io":
		return "docker.io"
	}
	return host
}

// extractRegistryHost extracts the registry host from an image reference.
func extractRegistryHost(image string) string {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return ""
	}
	return reference.Domain(named)
}

func (r *commonControl) listCRJobPods(ctx context.Context, commit *agentsv1alpha1.Commit) (*corev1.PodList, error) {
	jobPods := &corev1.PodList{}
	// Filter by the standard label set automatically by the Kubernetes Job
	// controller (stable since K8s 1.27). This avoids relying on a custom label
	// being correctly propagated from the Job spec to its child Pods.
	matchingLabels := client.MatchingLabels{
		"batch.kubernetes.io/job-name": jobutil.MakeJobName(string(commit.UID)),
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
