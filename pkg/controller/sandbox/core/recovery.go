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
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

const recoveryAttemptAnnotation = agentsv1alpha1.InternalPrefix + "recovery-attempt"

// RecoveryEnabled requires both operator and per-Sandbox opt-in.
func RecoveryEnabled(box *agentsv1alpha1.Sandbox) bool {
	return utilfeature.DefaultFeatureGate.Enabled(features.SandboxEvictionRecoveryGate) &&
		box.Spec.RecoveryPolicy != nil && box.Spec.RecoveryPolicy.Type == "OnEvicted"
}

// RecoveryPolicy applies defaults even for objects created without CRD defaulting.
func RecoveryPolicy(box *agentsv1alpha1.Sandbox) agentsv1alpha1.SandboxRecoveryPolicy {
	p := agentsv1alpha1.SandboxRecoveryPolicy{MaxAttempts: 3, BackoffSeconds: 30, TimeoutSeconds: 600, HealthyResetSeconds: 600}
	if box.Spec.RecoveryPolicy != nil {
		if box.Spec.RecoveryPolicy.MaxAttempts > 0 {
			p.MaxAttempts = box.Spec.RecoveryPolicy.MaxAttempts
		}
		if box.Spec.RecoveryPolicy.BackoffSeconds > 0 {
			p.BackoffSeconds = box.Spec.RecoveryPolicy.BackoffSeconds
		}
		if box.Spec.RecoveryPolicy.TimeoutSeconds > 0 {
			p.TimeoutSeconds = box.Spec.RecoveryPolicy.TimeoutSeconds
		}
		if box.Spec.RecoveryPolicy.HealthyResetSeconds > 0 {
			p.HealthyResetSeconds = box.Spec.RecoveryPolicy.HealthyResetSeconds
		}
	}
	return p
}

// IsEvictedPod uses kubelet's structured terminal reason, not exit codes or messages.
func IsEvictedPod(pod *corev1.Pod) bool {
	return pod != nil && pod.Status.Phase == corev1.PodFailed && pod.Status.Reason == "Evicted"
}

// RecoveryTemplateHash rejects unsupported cold-recovery inputs and fingerprints
// all declarative Pod construction inputs. A mutable templateRef is not a snapshot.
func RecoveryTemplateHash(box *agentsv1alpha1.Sandbox) (string, error) {
	if box.Spec.Template == nil || box.Spec.TemplateRef != nil {
		return "", fmt.Errorf("eviction recovery requires an inline pod template")
	}
	if box.Spec.StartupMode == agentsv1alpha1.StartupModeDeferred {
		return "", fmt.Errorf("eviction recovery does not support Deferred startup")
	}
	if box.Spec.PauseStrategy == nil || box.Spec.PauseStrategy.Type != agentsv1alpha1.PauseStrategyStop {
		return "", fmt.Errorf("eviction recovery requires the Stop pause strategy")
	}
	if slices.Contains(box.Spec.PersistentContents, "memory") || slices.Contains(box.Spec.PersistentContents, "ip") {
		return "", fmt.Errorf("cold recovery cannot preserve memory or IP")
	}
	data, err := json.Marshal(struct {
		Template    agentsv1alpha1.EmbeddedSandboxTemplate
		Runtimes    []agentsv1alpha1.RuntimeConfig
		StartupMode agentsv1alpha1.StartupMode
	}{box.Spec.EmbeddedSandboxTemplate, box.Spec.Runtimes, box.Spec.StartupMode})
	if err != nil {
		return "", fmt.Errorf("hash recovery template: %w", err)
	}
	return utils.HashData(data), nil
}

// RecoveryVolumes records or checks the original binding before any PVC ensure.
// Reads use the informer-backed client; transient misses fail closed and are retried.
func RecoveryVolumes(ctx context.Context, cli client.Client, box *agentsv1alpha1.Sandbox) ([]agentsv1alpha1.SandboxRecoveryVolume, error) {
	claims := map[string]bool{}
	for _, tmpl := range box.Spec.VolumeClaimTemplates {
		name, err := GeneratePVCName(tmpl.Name, box.Name)
		if err != nil {
			return nil, err
		}
		claims[name] = true
	}
	if box.Spec.Template != nil {
		for _, vol := range box.Spec.Template.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil {
				if _, exists := claims[vol.PersistentVolumeClaim.ClaimName]; !exists {
					claims[vol.PersistentVolumeClaim.ClaimName] = false
				}
			}
		}
	}
	names := make([]string, 0, len(claims))
	for name := range claims {
		names = append(names, name)
	}
	slices.Sort(names)
	var volumes []agentsv1alpha1.SandboxRecoveryVolume
	for _, name := range names {
		pvc := &corev1.PersistentVolumeClaim{}
		if err := cli.Get(ctx, client.ObjectKey{Namespace: box.Namespace, Name: name}, pvc); err != nil {
			return nil, fmt.Errorf("read original PVC %s: %w", name, err)
		}
		if pvc.UID == "" || !pvc.DeletionTimestamp.IsZero() || pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "" {
			return nil, fmt.Errorf("original PVC %s is not bound or is being deleted", name)
		}
		if claims[name] && !metav1.IsControlledBy(pvc, box) {
			return nil, fmt.Errorf("original PVC %s is not owned by this Sandbox", name)
		}
		pv := &corev1.PersistentVolume{}
		if err := cli.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv); err != nil {
			return nil, fmt.Errorf("read original PV %s: %w", pvc.Spec.VolumeName, err)
		}
		ref := pv.Spec.ClaimRef
		if pv.UID == "" || !pv.DeletionTimestamp.IsZero() || ref == nil || ref.UID != pvc.UID || ref.Name != pvc.Name || ref.Namespace != pvc.Namespace {
			return nil, fmt.Errorf("PV %s no longer binds the original PVC %s", pv.Name, name)
		}
		volumes = append(volumes, agentsv1alpha1.SandboxRecoveryVolume{Name: name, UID: pvc.UID, VolumeName: pv.Name, VolumeUID: pv.UID})
	}
	if box.Status.Recovery != nil && !reflect.DeepEqual(volumes, box.Status.Recovery.Volumes) {
		return nil, fmt.Errorf("original PVC/PV identities changed; refusing to provision or use replacement storage")
	}
	return volumes, nil
}

// SetRecoveryNotReady revokes stale service addresses and readiness together.
func SetRecoveryNotReady(status *agentsv1alpha1.SandboxStatus, reason, message string) {
	status.PodInfo.PodIP = ""
	status.SandboxIp = ""
	utils.SetSandboxCondition(status, metav1.Condition{
		Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse,
		Reason: reason, Message: utils.TruncateConditionMessage(message), LastTransitionTime: metav1.Now(),
	})
}

func failRecovery(status *agentsv1alpha1.SandboxStatus, message string) {
	status.Phase = agentsv1alpha1.SandboxFailed
	status.Message = message
	SetRecoveryNotReady(status, "RecoveryFailed", message)
}

func recoveryAttemptID(status *agentsv1alpha1.SandboxStatus) string {
	return fmt.Sprintf("%s/%d", status.Recovery.PodUID, status.Recovery.Attempts)
}

// RecoveryImages resolves the immutable image references observed on a healthy
// Pod. A mutable image tag must not turn recovery into an implicit upgrade.
func RecoveryImages(pod *corev1.Pod) (map[string]string, error) {
	images := map[string]string{}
	for _, group := range []struct {
		prefix     string
		containers []corev1.Container
		statuses   []corev1.ContainerStatus
	}{{"containers/", pod.Spec.Containers, pod.Status.ContainerStatuses}, {"initContainers/", pod.Spec.InitContainers, pod.Status.InitContainerStatuses}} {
		for _, container := range group.containers {
			var imageID string
			for _, status := range group.statuses {
				if status.Name == container.Name {
					imageID = status.ImageID
					break
				}
			}
			if _, value, found := strings.Cut(imageID, "://"); found {
				imageID = value
			}
			if _, value, found := strings.Cut(imageID, "@"); found {
				imageID = value
			}
			d, err := digest.Parse(imageID)
			if err != nil {
				return nil, fmt.Errorf("container %s has no usable image digest: %w", container.Name, err)
			}
			name, err := reference.ParseNormalizedNamed(container.Image)
			if err != nil {
				return nil, fmt.Errorf("parse image for container %s: %w", container.Name, err)
			}
			pinned, err := reference.WithDigest(reference.TrimNamed(name), d)
			if err != nil {
				return nil, fmt.Errorf("pin image for container %s: %w", container.Name, err)
			}
			images[group.prefix+container.Name] = pinned.String()
		}
	}
	return images, nil
}

func pinRecoveryImages(pod *corev1.Pod, recovery *agentsv1alpha1.SandboxRecoveryStatus) error {
	for _, group := range []struct {
		prefix     string
		containers []corev1.Container
	}{
		{"containers/", pod.Spec.Containers}, {"initContainers/", pod.Spec.InitContainers},
	} {
		for i := range group.containers {
			container := &group.containers[i]
			image := recovery.Images[group.prefix+container.Name]
			if image == "" {
				return fmt.Errorf("recovery has no original image for container %s", container.Name)
			}
			container.Image = image
		}
	}
	return nil
}

// EnsureSandboxRecovered never deletes a Pod until the recovery intent has
// been persisted by the reconciler. All retries are bounded by StartedAt.
func (r *commonControl) EnsureSandboxRecovered(ctx context.Context, args EnsureFuncArgs) (time.Duration, error) {
	pod, box, status := args.Pod, args.Box, args.NewStatus
	recovery := status.Recovery
	SetRecoveryNotReady(status, "Recovering", "Waiting for a replacement Pod and runtime initialization")
	if recovery == nil || recovery.StartedAt == nil {
		failRecovery(status, "Recovery has no persisted baseline")
		return 0, nil
	}
	if pod != nil && (!metav1.IsControlledBy(pod, box) || pod.UID == "") {
		failRecovery(status, "Recovery Pod is not owned by this Sandbox")
		return 0, nil
	}
	// A pause stops recovery even if its policy has subsequently been disabled.
	// Wait for actual Pod absence, not just a satisfied delete expectation.
	if box.Spec.Paused {
		if pod != nil {
			return 5 * time.Second, r.deleteRecoveryPod(ctx, box, pod)
		}
		status.Phase = agentsv1alpha1.SandboxPaused
		recovery.StartedAt, recovery.RetryAfter, recovery.HealthySince = nil, nil, nil
		utils.SetSandboxCondition(status, metav1.Condition{
			Type: string(agentsv1alpha1.SandboxConditionPaused), Status: metav1.ConditionTrue,
			Reason: agentsv1alpha1.SandboxPausedReasonStopPauseSucceed, LastTransitionTime: metav1.Now(),
		})
		return 0, nil
	}
	policy := RecoveryPolicy(box)
	remaining := time.Until(recovery.StartedAt.Add(time.Duration(policy.TimeoutSeconds) * time.Second))
	if !RecoveryEnabled(box) || remaining <= 0 {
		failRecovery(status, "Recovery is disabled or its timeout has expired")
		return 0, nil
	}
	requeue := min(5*time.Second, remaining)
	hash, err := RecoveryTemplateHash(box)
	if err != nil || hash != recovery.TemplateHash {
		failRecovery(status, "Recovery template changed or is unsupported")
		return 0, nil
	}
	if _, err := RecoveryVolumes(ctx, r.Client, box); err != nil {
		status.Message = err.Error()
		SetRecoveryNotReady(status, "RecoveryStorageUnavailable", err.Error())
		return requeue, nil
	}
	if pod != nil && pod.UID == recovery.PodUID {
		if !IsEvictedPod(pod) {
			failRecovery(status, "Recorded source Pod is no longer an evicted Pod")
			return 0, nil
		}
		return requeue, r.deleteRecoveryPod(ctx, box, pod)
	}
	if pod == nil {
		if recovery.ReplacementPodUID != "" {
			failRecovery(status, "Replacement Pod disappeared without eviction evidence")
			return 0, nil
		}
		return r.createRecoveryPod(ctx, args, requeue)
	}
	if pod.Annotations[recoveryAttemptAnnotation] != recoveryAttemptID(status) ||
		(recovery.ReplacementPodUID != "" && recovery.ReplacementPodUID != pod.UID) {
		failRecovery(status, "Unexpected replacement Pod identity")
		return 0, nil
	}
	if recovery.ReplacementPodUID == "" {
		recovery.ReplacementPodUID = pod.UID
		return requeue, nil
	}
	if IsEvictedPod(pod) {
		// Persist the next source UID before deleting it; retain the original
		// timeout and attempt budget across repeated evictions.
		recovery.PodUID, recovery.NodeName = pod.UID, pod.Spec.NodeName
		recovery.ReplacementPodUID, recovery.RetryAfter = "", nil
		return requeue, nil
	}
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded || !pod.DeletionTimestamp.IsZero() {
		failRecovery(status, "Replacement Pod terminated without recoverable eviction")
		return 0, nil
	}
	ready := utils.GetPodCondition(&pod.Status, corev1.PodReady)
	if pod.Status.Phase != corev1.PodRunning || ready == nil || ready.Status != corev1.ConditionTrue {
		return requeue, nil
	}
	images, err := RecoveryImages(pod)
	if err != nil || !reflect.DeepEqual(images, recovery.Images) {
		failRecovery(status, "Replacement container images do not match the healthy baseline")
		return 0, nil
	}
	// Supply the new IP only to the initializer. Do not publish it until init
	// succeeds, including when Initialize modifies conditions and returns an error.
	initStatus := status.DeepCopy()
	r.syncStatusFromPod(pod, initStatus, false)
	initCtx, cancel := context.WithTimeout(ctx, min(30*time.Second, remaining))
	defer cancel()
	if err := r.initializer.Initialize(initCtx, box, initStatus); err != nil {
		status.Conditions = initStatus.Conditions
		SetRecoveryNotReady(status, "RecoveryInitializing", "Replacement runtime initialization has not succeeded")
		return requeue, err
	}
	*status = *initStatus
	r.syncStatusFromPod(pod, status, true)
	status.Phase, status.Message = agentsv1alpha1.SandboxRunning, ""
	status.Recovery.PodUID, status.Recovery.NodeName = pod.UID, pod.Spec.NodeName
	status.Recovery.ReplacementPodUID = ""
	status.Recovery.StartedAt, status.Recovery.RetryAfter = nil, nil
	now := metav1.Now()
	status.Recovery.HealthySince = &now
	return time.Duration(policy.HealthyResetSeconds) * time.Second, nil
}

// createRecoveryPod reserves one generation before any create, then waits for
// backoff and source-node health. Replays reuse the persisted reservation.
func (r *commonControl) createRecoveryPod(ctx context.Context, args EnsureFuncArgs, requeue time.Duration) (time.Duration, error) {
	box, status := args.Box, args.NewStatus
	recovery, policy := status.Recovery, RecoveryPolicy(box)
	if recovery.RetryAfter == nil {
		if recovery.Attempts >= policy.MaxAttempts {
			failRecovery(status, "Eviction recovery attempt budget exhausted")
			return 0, nil
		}
		recovery.Attempts++
		retry := metav1.NewTime(time.Now().Add(time.Duration(policy.BackoffSeconds) * time.Second))
		recovery.RetryAfter = &retry
		return requeue, nil
	}
	if wait := time.Until(recovery.RetryAfter.Time); wait > 0 {
		return min(wait, requeue), nil
	}
	// A failed node is not sufficient fencing for cross-node recovery. Wait
	// for the source node to acknowledge health and clear resource pressure.
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: recovery.NodeName}, node); err != nil {
		return requeue, fmt.Errorf("check recovery source node: %w", err)
	}
	ready := false
	for _, condition := range node.Status.Conditions {
		switch condition.Type {
		case corev1.NodeReady:
			ready = condition.Status == corev1.ConditionTrue
		case corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure:
			if condition.Status != corev1.ConditionFalse {
				return requeue, nil
			}
		}
	}
	if !ready || node.Spec.Unschedulable || !node.DeletionTimestamp.IsZero() {
		return requeue, nil
	}
	_, err := r.podControl.CreatePod(ctx, CreatePodArgs{Box: box, NewStatus: status, AdvertiseRuntimeTLS: true})
	return requeue, err
}

func (r *commonControl) deleteRecoveryPod(ctx context.Context, box *agentsv1alpha1.Sandbox, pod *corev1.Pod) error {
	if !pod.DeletionTimestamp.IsZero() {
		return nil
	}
	ScaleExpectation.ExpectScale(GetControllerKey(box), expectations.Delete, pod.Name)
	err := r.Delete(ctx, pod, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pod.UID}})
	if err != nil {
		ScaleExpectation.ObserveScale(GetControllerKey(box), expectations.Delete, pod.Name)
	}
	return client.IgnoreNotFound(err)
}
