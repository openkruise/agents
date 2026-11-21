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

package sandboxtemplate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"connectrpc.com/connect"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/openkruise/agents/api/v1alpha1"
	template "github.com/openkruise/agents/pkg/sandboxtemplate"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
	runtimeclient "github.com/openkruise/agents/pkg/utils/runtime"
	runtimeconfig "github.com/openkruise/agents/pkg/utils/runtime/config"
	"github.com/openkruise/agents/proto/envd/process"
)

const (
	buildReadyCondition = api.SandboxTemplateConditionBuildReady
	buildContainerName  = template.BuildContainerName
	buildRootUser       = "root"
)

var errExecutionInterrupted = errors.New("build execution identity changed or is being deleted")

const executionIdentityAnnotation = template.ExecutionIdentityAnnotation

type executionIdentity struct {
	SandboxUID  types.UID `json:"sandboxUID,omitempty"`
	PodUID      types.UID `json:"podUID,omitempty"`
	ContainerID string    `json:"containerID,omitempty"`
}

func parseExecutionIdentity(value string) (executionIdentity, error) {
	identity := &executionIdentity{}
	if value != "" {
		if err := json.Unmarshal([]byte(value), &identity); err != nil {
			return executionIdentity{}, fmt.Errorf("decode template execution identity: %w", err)
		}
		if identity == nil {
			return executionIdentity{}, fmt.Errorf("template execution identity must be an object")
		}
	}
	return *identity, nil
}

type executionSession struct {
	versionName        string
	versionUID         types.UID
	namespace          string
	sandboxUID         types.UID
	podUID             types.UID
	containerID        string
	initialContainerID string
	imageChanged       bool
}

func buildContainer(template *corev1.PodTemplateSpec) *corev1.Container {
	if template != nil {
		for i := range template.Spec.Containers {
			if template.Spec.Containers[i].Name == buildContainerName {
				return &template.Spec.Containers[i]
			}
		}
	}
	return nil
}

func (r *Reconciler) reconcileExecution(ctx context.Context, version *api.SandboxTemplate) (ctrl.Result, error) {
	condition := meta.FindStatusCondition(version.Status.Conditions, buildReadyCondition)
	if condition != nil && condition.Reason == buildReasonProvisioned {
		return r.reconcileCapture(ctx, version)
	}
	if lease := r.confirmedBuildLease(version, nil); lease != nil {
		return ctrl.Result{RequeueAfter: r.buildRequeueAfter(version, lease)}, nil
	}
	if condition != nil {
		if !time.Now().Before(r.buildDeadline(condition)) {
			return ctrl.Result{RequeueAfter: retryInterval}, r.failRunningBuild(ctx, version, buildReasonTimeout, "The total build execution budget expired.")
		}
	} else if buildContainer(version.Spec.Template) == nil {
		return ctrl.Result{RequeueAfter: retryInterval}, r.executionStatus(ctx, version, api.SandboxTemplatePhaseFailed, metav1.ConditionFalse, buildReasonInvalidDefinition, "The business runtime container is missing.")
	}
	lease, acquired, err := r.acquireBuildLease(ctx, version, condition != nil)
	if err != nil {
		return ctrl.Result{RequeueAfter: retryInterval}, err
	}
	if !acquired {
		if lease != nil {
			return ctrl.Result{RequeueAfter: r.buildRequeueAfter(version, lease)}, nil
		}
		return ctrl.Result{RequeueAfter: retryInterval}, nil
	}
	if condition != nil {
		return ctrl.Result{RequeueAfter: retryInterval}, r.failRunningBuild(ctx, version, buildReasonInterrupted, "The build execution heartbeat expired; steps will not be replayed.")
	}
	if !r.startWorker(version, lease) {
		return ctrl.Result{RequeueAfter: retryInterval}, r.setPhase(ctx, version, api.SandboxTemplatePhaseBuilding)
	}
	return ctrl.Result{RequeueAfter: r.buildRequeueAfter(version, lease)}, nil
}

func (r *Reconciler) buildDeadline(started *metav1.Condition) time.Time {
	budget := r.Options.Timeout
	if budget <= 0 {
		budget = DefaultBuildTimeout
	}
	return started.LastTransitionTime.Add(budget)
}

func (r *Reconciler) buildRequeueAfter(version *api.SandboxTemplate, lease *coordinationv1.Lease) time.Duration {
	remaining := lease.Spec.RenewTime.Add(buildLeaseDuration).Sub(r.leaseClock.Now())
	if condition := meta.FindStatusCondition(version.Status.Conditions, buildReadyCondition); condition != nil {
		remaining = min(remaining, time.Until(r.buildDeadline(condition)))
	}
	if remaining <= 0 {
		return retryInterval
	}
	return remaining
}

func (r *Reconciler) failRunningBuild(ctx context.Context, version *api.SandboxTemplate, reason, message string) error {
	condition := meta.FindStatusCondition(version.Status.Conditions, buildReadyCondition)
	if condition == nil || condition.Reason != buildReasonRunning || version.Status.Phase == api.SandboxTemplatePhaseReady || version.Status.Phase == api.SandboxTemplatePhaseFailed {
		return nil
	}
	before := version.DeepCopy()
	version.Status.Phase = api.SandboxTemplatePhaseFailed
	version.Status.ObservedGeneration = version.Generation
	meta.SetStatusCondition(&version.Status.Conditions, metav1.Condition{Type: buildReadyCondition, Status: metav1.ConditionFalse, Reason: reason, Message: message, ObservedGeneration: version.Generation})
	return r.Status().Patch(ctx, version, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (r *Reconciler) runExecution(ctx context.Context, version *api.SandboxTemplate, lease *coordinationv1.Lease) error {
	if _, err := r.currentBuildLease(ctx, version, lease); err != nil {
		return err
	}
	if err := r.executionStatus(ctx, version, api.SandboxTemplatePhaseBuilding, metav1.ConditionFalse, buildReasonRunning, "Build execution started."); err != nil {
		if errors.Is(err, errExecutionInterrupted) {
			finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			return r.failRunningBuild(finishCtx, version, buildReasonInterrupted, "The build execution holder changed; steps will not be replayed.")
		}
		return err
	}
	started := meta.FindStatusCondition(version.Status.Conditions, buildReadyCondition)
	executionCtx, cancel := context.WithDeadline(ctx, r.buildDeadline(started))
	defer cancel()
	session := &executionSession{versionName: version.Name, versionUID: version.UID, namespace: version.Namespace}
	err := r.bindExecutionSandbox(executionCtx, session)
	if err == nil {
		err = r.execute(executionCtx, session, version, lease)
	}
	if executionCtx.Err() != nil {
		err = executionCtx.Err()
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer finishCancel()
	if err != nil {
		reason, message := buildReasonExecutionFailed, "A build operation failed."
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			reason, message = buildReasonTimeout, "The total build execution budget expired."
		case errors.Is(err, context.Canceled), errors.Is(err, errExecutionInterrupted):
			reason, message = buildReasonInterrupted, "Build execution was interrupted; steps will not be replayed."
		default:
			var stepError *buildStepError
			if errors.As(err, &stepError) {
				message = fmt.Sprintf("Build step %d failed.", stepError.index+1)
			}
		}
		return r.executionStatus(finishCtx, version, api.SandboxTemplatePhaseFailed, metav1.ConditionFalse, reason, message)
	}
	if _, err := r.renewBuildLease(finishCtx, version, lease); err != nil {
		if errors.Is(err, errExecutionInterrupted) {
			return r.executionStatus(finishCtx, version, api.SandboxTemplatePhaseFailed, metav1.ConditionFalse, buildReasonInterrupted, "The build execution holder changed before result publication.")
		}
		return err
	}
	// Pending stages stay False so capture retains the original build deadline.
	return r.executionStatus(finishCtx, version, api.SandboxTemplatePhaseBuilding, metav1.ConditionFalse, buildReasonProvisioned, "All build steps completed; artifact capture is pending.")
}

// executionStatus publishes one step of the build condition protocol and
// retries on conflict:
//
//	Pending -- Running (first write only) --> Building
//	Building -- Provisioned --> capture pending
//	any -- Interrupted | Timeout | CaptureConflict | CaptureFailed --> Failed (terminal)
//	capture done -- Captured --> Ready (terminal)
//
// A terminal phase at the observed generation is immutable: another write
// reports it back as done, except Running, which means another replica
// restarted a finished build and fails as Interrupted. A write whose last
// observed condition showed a running execution also accepts a Provisioned
// condition that another replica already published, so replicas converge
// instead of overwriting each other.
func (r *Reconciler) executionStatus(ctx context.Context, version *api.SandboxTemplate, phase api.SandboxTemplateBuildPhase, status metav1.ConditionStatus, reason, message string) error {
	expected := meta.FindStatusCondition(version.Status.Conditions, buildReadyCondition)
	pendingExecution := expected != nil && expected.Status == metav1.ConditionFalse && expected.Reason != buildReasonProvisioned
	return wait.ExponentialBackoffWithContext(ctx, retry.DefaultBackoff, func(ctx context.Context) (bool, error) {
		current := &api.SandboxTemplate{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(version), current); err != nil {
			return false, err
		}
		if current.UID != version.UID || current.Generation != version.Generation {
			return false, errExecutionInterrupted
		}
		if buildTerminal(current) {
			*version = *current
			if reason == buildReasonRunning {
				return false, errExecutionInterrupted
			}
			return true, nil
		}
		condition := meta.FindStatusCondition(current.Status.Conditions, buildReadyCondition)
		// Running is the first write only: an existing condition or a
		// deleting Version means another replica already owns this build.
		if reason == buildReasonRunning && (condition != nil || current.DeletionTimestamp != nil) {
			*version = *current
			return false, errExecutionInterrupted
		}
		if pendingExecution && supersededByProvisioned(condition) {
			*version = *current
			return true, nil
		}
		before := current.DeepCopy()
		current.Status.Phase = phase
		current.Status.ObservedGeneration = current.Generation
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{Type: buildReadyCondition, Status: status, Reason: reason, Message: message, ObservedGeneration: current.Generation})
		if err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		*version = *current
		return true, nil
	})
}

// buildTerminal reports whether the Version reached Ready or Failed at the
// observed generation, after which the build result is immutable.
func buildTerminal(version *api.SandboxTemplate) bool {
	return version.Status.ObservedGeneration == version.Generation &&
		(version.Status.Phase == api.SandboxTemplatePhaseReady || version.Status.Phase == api.SandboxTemplatePhaseFailed)
}

// supersededByProvisioned reports whether another replica already published
// the Provisioned step that this write was about to publish.
func supersededByProvisioned(condition *metav1.Condition) bool {
	return condition != nil && condition.Reason == buildReasonProvisioned
}

func (r *Reconciler) bindExecutionSandbox(ctx context.Context, session *executionSession) error {
	return wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		version := &api.SandboxTemplate{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: session.namespace, Name: session.versionName}, version); err != nil {
			if apierrors.IsNotFound(err) {
				return false, errors.Join(errExecutionInterrupted, err)
			}
			return false, err
		}
		if version.UID != session.versionUID || version.DeletionTimestamp != nil || version.Status.Phase == api.SandboxTemplatePhaseFailed || !template.IsBuildVersion(version) {
			return false, errExecutionInterrupted
		}
		identity, err := parseExecutionIdentity(version.Annotations[executionIdentityAnnotation])
		if err != nil {
			return false, err
		}
		box := &api.Sandbox{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: session.namespace, Name: session.versionName}, box); err != nil {
			if apierrors.IsNotFound(err) && identity.PodUID == "" {
				return false, nil
			}
			if apierrors.IsNotFound(err) {
				return false, errors.Join(errExecutionInterrupted, err)
			}
			return false, err
		}
		if !ownedByVersion(box, version) || box.DeletionTimestamp != nil || identity.SandboxUID != "" && identity.SandboxUID != box.UID {
			return false, errExecutionInterrupted
		}
		session.sandboxUID, session.podUID, session.containerID = box.UID, identity.PodUID, identity.ContainerID
		return true, r.recordExecutionIdentity(ctx, session)
	})
}

func (r *Reconciler) readExecutionSandbox(ctx context.Context, session *executionSession) (*api.Sandbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	version := &api.SandboxTemplate{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: session.namespace, Name: session.versionName}, version); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errors.Join(errExecutionInterrupted, err)
		}
		return nil, err
	}
	if version.UID != session.versionUID || version.DeletionTimestamp != nil || version.Status.Phase == api.SandboxTemplatePhaseFailed || !template.IsBuildVersion(version) {
		return nil, errExecutionInterrupted
	}
	identity, err := parseExecutionIdentity(version.Annotations[executionIdentityAnnotation])
	if err != nil {
		return nil, err
	}
	if (identity.SandboxUID != "" && identity.SandboxUID != session.sandboxUID) ||
		(session.podUID != "" && identity.PodUID != "" && identity.PodUID != session.podUID) ||
		(session.containerID != "" && identity.ContainerID != "" && identity.ContainerID != session.containerID) {
		return nil, errExecutionInterrupted
	}
	box := &api.Sandbox{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: session.namespace, Name: session.versionName}, box); err != nil {
		return nil, err
	}
	owner := template.VersionOwnerReference(box)
	if box.UID != session.sandboxUID || owner == nil || owner.UID != session.versionUID || owner.Name != session.versionName || box.DeletionTimestamp != nil || session.podUID != "" && box.Status.PodInfo.PodUID != "" && box.Status.PodInfo.PodUID != session.podUID {
		return nil, errExecutionInterrupted
	}
	if box.Status.Phase == api.SandboxFailed || box.Status.Phase == api.SandboxSucceeded || box.Spec.Paused {
		return nil, errExecutionInterrupted
	}
	return box, nil
}

// executionSnapshot reports the provisioned Sandbox once its business
// container runs the expected image revision; executionContainerReady
// defines when that readiness is reached.
func (r *Reconciler) executionSnapshot(ctx context.Context, session *executionSession, image, revision string) (*api.Sandbox, bool, error) {
	box, err := r.readExecutionSandbox(ctx, session)
	if err != nil {
		return nil, false, err
	}
	pod := &corev1.Pod{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: session.namespace, Name: session.versionName}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			if session.podUID != "" {
				return nil, false, errExecutionInterrupted
			}
			return nil, false, nil
		}
		return nil, false, err
	}
	owner := metav1.GetControllerOfNoCopy(pod)
	if owner == nil || owner.APIVersion != api.GroupVersion.String() || owner.Kind != "Sandbox" ||
		owner.Name != session.versionName || owner.UID != session.sandboxUID ||
		(session.podUID != "" && session.podUID != pod.UID) || pod.DeletionTimestamp != nil ||
		pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		return nil, false, errExecutionInterrupted
	}
	for _, container := range pod.Status.ContainerStatuses {
		if container.Name != buildContainerName {
			continue
		}
		ready, err := executionContainerReady(ctx, session, pod, box, &container, image, revision)
		if err != nil || !ready {
			return nil, false, err
		}
		return box, true, nil
	}
	return nil, false, nil
}

// executionContainerReady reports whether the business container is usable
// for the build and records the session identity. Readiness is reached in
// two distinct steps: the first observation records the baseline container
// identity that interrupt detection compares against, and after an
// in-place image update it waits for the update to complete and records
// the updated identity instead.
func executionContainerReady(ctx context.Context, session *executionSession, pod *corev1.Pod, box *api.Sandbox, container *corev1.ContainerStatus, image, revision string) (bool, error) {
	if container.State.Terminated != nil {
		if session.imageChanged && session.containerID == "" && container.ContainerID == session.initialContainerID {
			return false, nil
		}
		return false, errExecutionInterrupted
	}
	if session.containerID != "" && container.ContainerID != "" && session.containerID != container.ContainerID {
		return false, errExecutionInterrupted
	}
	if !container.Ready || container.State.Running == nil || container.ContainerID == "" || !utils.IsSandboxReady(box) || box.Status.PodInfo.PodUID != pod.UID || box.Status.PodInfo.PodIP == "" {
		return false, nil
	}
	// Readiness after an image update: the pod must run the requested
	// revision and finish any in-place update; the updated container
	// identity then replaces the baseline.
	if image != "" {
		found := false
		for _, spec := range pod.Spec.Containers {
			if spec.Name == container.Name && spec.Image == image {
				found = true
			}
		}
		if !found || pod.Labels[api.PodLabelTemplateHash] != revision || box.Status.ObservedGeneration < box.Generation || box.Status.UpdateRevision != revision {
			return false, nil
		}
		state, err := inplaceupdate.GetPodInPlaceUpdateState(pod)
		if err != nil {
			return false, errExecutionInterrupted
		}
		if session.imageChanged && (container.ContainerID == session.initialContainerID || state == nil || !state.UpdateImages) {
			return false, nil
		}
		if completed, err := inplaceupdate.IsInplaceUpdateCompleted(ctx, pod); err != nil || !completed {
			return false, err
		}
		session.containerID = container.ContainerID
	} else {
		// First readiness: record the baseline container identity that
		// later identity changes are detected against.
		session.initialContainerID = container.ContainerID
	}
	session.podUID = pod.UID
	return true, nil
}

func (r *Reconciler) waitExecutionSandbox(ctx context.Context, session *executionSession, image, revision string) (*api.Sandbox, error) {
	var box *api.Sandbox
	err := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		current, ready, err := r.executionSnapshot(ctx, session, image, revision)
		if ready {
			box = current
		}
		return ready, err
	})
	return box, err
}

func (r *Reconciler) recordExecutionIdentity(ctx context.Context, session *executionSession) error {
	return wait.ExponentialBackoffWithContext(ctx, retry.DefaultBackoff, func(ctx context.Context) (bool, error) {
		version := &api.SandboxTemplate{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: session.namespace, Name: session.versionName}, version); err != nil {
			return false, err
		}
		if version.UID != session.versionUID || version.DeletionTimestamp != nil || version.Status.Phase == api.SandboxTemplatePhaseFailed || !template.IsBuildVersion(version) {
			return false, errExecutionInterrupted
		}
		identity, err := parseExecutionIdentity(version.Annotations[executionIdentityAnnotation])
		if err != nil {
			return false, err
		}
		if (identity.SandboxUID != "" && identity.SandboxUID != session.sandboxUID) ||
			(identity.PodUID != "" && identity.PodUID != session.podUID) ||
			(identity.ContainerID != "" && identity.ContainerID != session.containerID) {
			return false, errExecutionInterrupted
		}
		desired := executionIdentity{SandboxUID: session.sandboxUID, PodUID: session.podUID, ContainerID: session.containerID}
		if identity == desired {
			return true, nil
		}
		before := version.DeepCopy()
		data, err := json.Marshal(desired)
		if err != nil {
			return false, err
		}
		if version.Annotations == nil {
			version.Annotations = map[string]string{}
		}
		version.Annotations[executionIdentityAnnotation] = string(data)
		if err := r.Patch(ctx, version, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
}

// bindBuildRuntime persists the runtime access token and the submitted
// business image on the build Sandbox, reusing an existing token and
// retrying on conflict. It returns the bound Sandbox and the effective
// token.
func (r *Reconciler) bindBuildRuntime(ctx context.Context, session *executionSession, version *api.SandboxTemplate, lease *coordinationv1.Lease, token string) (*api.Sandbox, string, error) {
	var box *api.Sandbox
	err := wait.ExponentialBackoffWithContext(ctx, retry.DefaultBackoff, func(ctx context.Context) (bool, error) {
		current, err := r.readExecutionSandbox(ctx, session)
		if err != nil {
			return false, err
		}
		if _, err := r.currentBuildLease(ctx, version, lease); err != nil {
			return false, err
		}
		container, definition := buildContainer(current.Spec.Template), buildContainer(version.Spec.Template)
		if container == nil || definition == nil {
			return false, errExecutionInterrupted
		}
		before := current.DeepCopy()
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		if existing := utils.GetAccessToken(current); existing != "" {
			token = existing
		}
		initData, err := json.Marshal(runtimeconfig.InitRuntimeOptions{AccessToken: token}) //nolint:gosec // the token must reach the build sandbox runtime
		if err != nil {
			return false, err
		}
		current.Annotations[api.AnnotationRuntimeAccessToken] = token
		current.Annotations[api.AnnotationInitRuntimeRequest] = string(initData)
		session.imageChanged = container.Image != definition.Image
		container.Image = definition.Image
		current.Spec.PersistentContents = append([]string(nil), version.Spec.PersistentContents...)
		if err := r.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		box = current
		return true, nil
	})
	if err != nil {
		return nil, token, err
	}
	return box, token, nil
}

func (r *Reconciler) execute(ctx context.Context, session *executionSession, version *api.SandboxTemplate, lease *coordinationv1.Lease) error {
	box, err := r.waitExecutionSandbox(ctx, session, "", "")
	if err != nil {
		return err
	}
	if _, err := r.currentBuildLease(ctx, version, lease); err != nil {
		return err
	}
	if err := r.recordExecutionIdentity(ctx, session); err != nil {
		return err
	}
	token := runtimeconfig.NewDefaultAccessToken()
	box, token, err = r.bindBuildRuntime(ctx, session, version, lease, token)
	if err != nil {
		return err
	}
	image := buildContainer(box.Spec.Template).Image
	revisionData, err := json.Marshal(*box.Spec.Template)
	if err != nil {
		return err
	}
	revision := utils.HashData(revisionData)
	box, err = r.waitExecutionSandbox(ctx, session, image, revision)
	if err != nil {
		return err
	}
	if err := r.recordExecutionIdentity(ctx, session); err != nil {
		return err
	}
	rtOpts, err := runtimeclient.TransportOptionsFor(box, r.Options.RuntimeTLSBundle)
	if err != nil {
		return err
	}
	refresh := func(ctx context.Context) (*api.Sandbox, error) {
		current, err := r.waitExecutionSandbox(ctx, session, image, revision)
		if err != nil {
			return nil, err
		}
		if _, err := r.currentBuildLease(ctx, version, lease); err != nil {
			return nil, err
		}
		return current, nil
	}
	if _, err := runtimeclient.InitRuntime(ctx, box, runtimeconfig.InitRuntimeOptions{AccessToken: token}, refresh, rtOpts...); err != nil {
		return err
	}
	for index, step := range version.Spec.BuildOptions.Steps {
		box, err = refresh(ctx)
		if err != nil {
			return err
		}
		command := step.Command
		if step.Copy != nil {
			command, err = materializeCommand(step.Copy)
			if err != nil {
				return &buildStepError{index: index, cause: err}
			}
		}
		if command == nil {
			return &buildStepError{index: index, cause: errors.New("build step is empty")}
		}
		user := command.User
		if user == "" {
			user = buildRootUser
		}
		// The execution context always carries the persisted build deadline.
		deadline, _ := ctx.Deadline()
		remaining := time.Until(deadline)
		config := &process.ProcessConfig{Cmd: "/bin/bash", Args: []string{"-l", "-c", command.Command}, Envs: maps.Clone(command.Env)}
		if command.Cwd != "" {
			config.Cwd = ptr.To(command.Cwd)
		}
		result, err := runtimeclient.RunCommandWithRuntime(ctx, runtimeclient.RunCmdFuncArgs{Sbx: box, ProcessConfig: config, Timeout: remaining, ProcessTimeout: remaining, AuthUser: user, OutputOptions: runtimeclient.ProcessOutputOptions{MaxBytes: 8192, SuppressResultLog: true}}, rtOpts...)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Terminal process errors can also be present in the RPC error.
		if result.Error != nil || result.Exited && result.ExitCode != 0 {
			return &buildStepError{index: index, cause: errors.New("build command did not exit successfully")}
		}
		if err != nil {
			switch connect.CodeOf(err) {
			case connect.CodeCanceled, connect.CodeDeadlineExceeded, connect.CodeUnavailable, connect.CodeInternal, connect.CodeDataLoss, connect.CodeUnknown:
				return errors.Join(errExecutionInterrupted, err)
			}
			return &buildStepError{index: index, cause: err}
		}
		if !result.Exited {
			return errExecutionInterrupted
		}
	}
	_, err = refresh(ctx)
	return err
}

type buildStepError struct {
	index int
	cause error
}

func (e *buildStepError) Error() string { return fmt.Sprintf("build step %d failed", e.index+1) }
func (e *buildStepError) Unwrap() error { return e.cause }

// reconcileCapture runs the artifact-capture tail of the build: it verifies
// the execution identity, snapshots the provisioned Sandbox, and either
// marks the Version Ready directly or creates and tracks the Version-owned
// Checkpoint that publishes the artifact.
func (r *Reconciler) reconcileCapture(ctx context.Context, version *api.SandboxTemplate) (ctrl.Result, error) {
	started := meta.FindStatusCondition(version.Status.Conditions, buildReadyCondition)
	if started == nil {
		return ctrl.Result{RequeueAfter: retryInterval}, r.executionStatus(ctx, version, api.SandboxTemplatePhaseFailed, metav1.ConditionFalse, buildReasonInterrupted, "Build execution progress is missing.")
	}
	deadline := r.buildDeadline(started)
	if !time.Now().Before(deadline) {
		return ctrl.Result{RequeueAfter: retryInterval}, r.executionStatus(ctx, version, api.SandboxTemplatePhaseFailed, metav1.ConditionFalse, buildReasonTimeout, "The total build and capture budget expired.")
	}
	captureCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	identity, err := parseExecutionIdentity(version.Annotations[executionIdentityAnnotation])
	if err != nil {
		return ctrl.Result{}, err
	}
	if identity.SandboxUID == "" || identity.PodUID == "" || identity.ContainerID == "" {
		return ctrl.Result{RequeueAfter: retryInterval}, nil
	}
	definition := buildContainer(version.Spec.Template)
	if definition == nil {
		return ctrl.Result{RequeueAfter: retryInterval}, r.executionStatus(ctx, version, api.SandboxTemplatePhaseFailed, metav1.ConditionFalse, buildReasonCaptureConflict, "The business runtime definition is missing.")
	}
	session := &executionSession{namespace: version.Namespace, versionName: version.Name, versionUID: version.UID, sandboxUID: identity.SandboxUID, podUID: identity.PodUID, containerID: identity.ContainerID}
	box, err := r.readExecutionSandbox(captureCtx, session)
	if err != nil {
		if errors.Is(err, errExecutionInterrupted) || apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: retryInterval}, r.executionStatus(ctx, version, api.SandboxTemplatePhaseFailed, metav1.ConditionFalse, buildReasonInterrupted, "The provisioned Sandbox was lost before capture.")
		}
		return ctrl.Result{}, err
	}
	if buildContainer(box.Spec.Template) == nil {
		return ctrl.Result{RequeueAfter: retryInterval}, r.executionStatus(ctx, version, api.SandboxTemplatePhaseFailed, metav1.ConditionFalse, buildReasonCaptureConflict, "The provisioned business runtime is missing.")
	}
	revisionData, err := json.Marshal(*box.Spec.Template)
	if err != nil {
		return ctrl.Result{}, err
	}
	box, ready, err := r.executionSnapshot(captureCtx, session, definition.Image, utils.HashData(revisionData))
	if err != nil {
		if errors.Is(err, errExecutionInterrupted) {
			return ctrl.Result{RequeueAfter: retryInterval}, r.executionStatus(ctx, version, api.SandboxTemplatePhaseFailed, metav1.ConditionFalse, buildReasonInterrupted, "The execution Pod or container was replaced before capture.")
		}
		return ctrl.Result{}, err
	}
	if !ready {
		return ctrl.Result{RequeueAfter: retryInterval}, nil
	}
	if version.Spec.BuildOptions == nil || version.Spec.BuildOptions.Checkpoint == nil || !version.Spec.BuildOptions.Checkpoint.Enable {
		return ctrl.Result{RequeueAfter: retryInterval}, r.executionStatus(ctx, version, api.SandboxTemplatePhaseReady, metav1.ConditionTrue, buildReasonProvisioned, "All declared build work completed.")
	}
	name := version.Name + "-checkpoint"
	key := client.ObjectKey{Namespace: version.Namespace, Name: name}
	expected := &api.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{Namespace: version.Namespace, Name: name, Labels: maps.Clone(version.Labels), Annotations: map[string]string{api.AnnotationInitRuntimeRequest: box.Annotations[api.AnnotationInitRuntimeRequest]},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplate", Name: version.Name, UID: version.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}},
		Spec: api.CheckpointSpec{SandboxName: ptr.To(box.Name), PodName: ptr.To(box.Name), Containers: []string{buildContainerName}, KeepRunning: ptr.To(true), PersistentContents: slices.Clone(version.Spec.PersistentContents)},
	}
	delete(expected.Labels, api.LabelTemplateName)
	checkpoint := &api.Checkpoint{}
	if err := r.Get(captureCtx, key, checkpoint); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		checkpoint = expected.DeepCopy()
		if err := r.Create(captureCtx, checkpoint); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return ctrl.Result{RequeueAfter: retryInterval}, nil
			}
			return ctrl.Result{}, err
		}
	}
	if !ownedByVersion(checkpoint, version) || checkpoint.DeletionTimestamp != nil || !equality.Semantic.DeepEqual(checkpoint.Spec, expected.Spec) || checkpoint.Annotations[api.AnnotationInitRuntimeRequest] != expected.Annotations[api.AnnotationInitRuntimeRequest] {
		return ctrl.Result{RequeueAfter: retryInterval}, r.executionStatus(ctx, version, api.SandboxTemplatePhaseFailed, metav1.ConditionFalse, buildReasonCaptureConflict, "The capture resource identity or scope changed.")
	}
	if checkpoint.Status.ObservedGeneration != checkpoint.Generation {
		return ctrl.Result{RequeueAfter: retryInterval}, nil
	}
	switch checkpoint.Status.Phase {
	case api.CheckpointFailed:
		return ctrl.Result{RequeueAfter: retryInterval}, r.executionStatus(ctx, version, api.SandboxTemplatePhaseFailed, metav1.ConditionFalse, buildReasonCaptureFailed, "The backend could not capture build artifacts.")
	case api.CheckpointSucceeded:
		if checkpoint.Status.CheckpointId == "" {
			return ctrl.Result{RequeueAfter: retryInterval}, r.executionStatus(ctx, version, api.SandboxTemplatePhaseFailed, metav1.ConditionFalse, buildReasonCaptureFailed, "The completed checkpoint has no backend identity.")
		}
		before := version.DeepCopy()
		version.Status.Artifacts = []api.SandboxTemplateArtifact{{Type: api.SandboxTemplateArtifactCheckpoint, Name: checkpoint.Name, Value: checkpoint.Status.CheckpointId}}
		version.Status.Phase = api.SandboxTemplatePhaseReady
		version.Status.ObservedGeneration = version.Generation
		meta.SetStatusCondition(&version.Status.Conditions, metav1.Condition{Type: buildReadyCondition, Status: metav1.ConditionTrue, Reason: buildReasonCaptured, Message: "Build execution and artifact capture completed.", ObservedGeneration: version.Generation})
		if err := r.Status().Patch(captureCtx, version, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: retryInterval}, nil
	default:
		return ctrl.Result{RequeueAfter: retryInterval}, nil
	}
}
