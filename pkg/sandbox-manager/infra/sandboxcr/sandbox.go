/*
Copyright 2025 The Kruise Authors.

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

package sandboxcr

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"github.com/openkruise/agents/pkg/utils/proxyutils"
	"github.com/openkruise/agents/pkg/utils/runtime"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

// ModifierFunc mutates the sandbox and decides whether retryUpdate should persist it.
// It returns:
//   - changed: true when the provided sandbox was modified and should be updated;
//     false when no update should be issued.
//   - err:     non-nil to abort retryUpdate immediately.
type ModifierFunc func(sbx *agentsv1alpha1.Sandbox) (bool, error)

type Sandbox struct {
	*agentsv1alpha1.Sandbox
	Cache           cache.Provider
	storageRegistry storages.VolumeMountProviderRegistry
	// trafficToken holds the access token response minted for accessing this
	// sandbox through the sandbox gateway. It is transient and per-claim: set in
	// memory during runClaimPostProcesses and never persisted to the CR, so
	// InplaceRefresh / retryUpdate reassigning s.Sandbox do not clear it.
	trafficToken *identity.TokenResponse
}

var DefaultDeleteSandbox = deleteSandbox

func deleteSandbox(ctx context.Context, sbx *agentsv1alpha1.Sandbox, client client.Client) error {
	return client.Delete(ctx, sbx)
}

func (s *Sandbox) GetTemplate() string {
	return utils.GetTemplateFromSandbox(s.Sandbox)
}

func (s *Sandbox) InplaceRefresh(ctx context.Context, deepcopy bool) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox)).V(utils.DebugLogLevel)
	fetchFromApiServer := false
	objectKey := client.ObjectKeyFromObject(s.Sandbox)
	newSbx := &agentsv1alpha1.Sandbox{}
	err := s.Cache.GetClient().Get(ctx, objectKey, newSbx)
	if err != nil {
		log.Info("failed to get claimed sandbox from cache, fetch from api-server", "reason", err.Error())
		fetchFromApiServer = true
	} else if !expectations.ResourceVersionExpectationSatisfied(newSbx) {
		log.Info("sandbox cache is out-dated, fetch from api-server")
		fetchFromApiServer = true
	}
	if fetchFromApiServer {
		if err = s.Cache.GetAPIReader().Get(ctx, objectKey, newSbx); err != nil {
			return err
		}
	}
	if expectations.IsResourceVersionReallyNewer(s.Sandbox.GetResourceVersion(), newSbx.GetResourceVersion()) {
		if deepcopy {
			s.Sandbox = newSbx.DeepCopy()
		} else {
			s.Sandbox = newSbx
		}
	}
	return nil
}

// refreshFunc returns a RefreshFunc callback that refreshes this sandbox and returns the latest object.
// This allows InitRuntime in utils/runtime to refresh sandbox state without depending on the sandboxcr package.
func (s *Sandbox) refreshFunc() runtime.RefreshFunc {
	return func(ctx context.Context) (*agentsv1alpha1.Sandbox, error) {
		if err := s.InplaceRefresh(ctx, false); err != nil {
			return nil, err
		}
		return s.Sandbox, nil
	}
}

// retryUpdate loads the latest sandbox from informer first, applies modifier, and retries on conflict.
// Conflict retries refresh from APIReader to avoid cleaning stale informer data.
//
// Returns:
//   - updated: true if a real Update was issued and the sandbox was written back; false if no update was needed.
//   - err:     non-nil when either refresh/update failed or modifier/Update returned an error.
func (s *Sandbox) retryUpdate(ctx context.Context, modifier ModifierFunc) (bool, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox))
	objectKey := client.ObjectKeyFromObject(s.Sandbox)
	updated := false
	first := true
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &agentsv1alpha1.Sandbox{}
		var err error
		if first {
			err = s.Cache.GetClient().Get(ctx, objectKey, latest)
		} else {
			err = s.Cache.GetAPIReader().Get(ctx, objectKey, latest)
		}
		first = false
		if err != nil {
			return err
		}

		copied := latest.DeepCopy()
		shouldUpdate, err := modifier(copied)
		if err != nil {
			return err
		}
		if !shouldUpdate {
			s.Sandbox = latest
			updated = false
			return nil
		}
		if err = s.Cache.GetClient().Update(ctx, copied); err != nil {
			return err
		}
		s.Sandbox = copied
		expectations.ResourceVersionExpectationExpect(copied)
		updated = true
		return nil
	})
	if err != nil {
		log.Error(err, "failed to update sandbox after retries")
		return false, err
	}
	if updated {
		log.Info("sandbox updated successfully")
	} else {
		log.Info("sandbox update skipped")
	}
	return updated, nil
}

func (s *Sandbox) refreshFromAPIReader(ctx context.Context) error {
	latest := &agentsv1alpha1.Sandbox{}
	if err := s.Cache.GetAPIReader().Get(ctx, client.ObjectKeyFromObject(s.Sandbox), latest); err != nil {
		return err
	}
	s.Sandbox = latest
	return nil
}

func (s *Sandbox) Kill(ctx context.Context) error {
	if s.GetDeletionTimestamp() != nil {
		return nil
	}
	return DefaultDeleteSandbox(ctx, s.Sandbox, s.Cache.GetClient())
}

func (s *Sandbox) TriggerRecycle(ctx context.Context) error {
	patch := client.MergeFrom(s.Sandbox.DeepCopy())
	if s.Sandbox.Annotations == nil {
		s.Sandbox.Annotations = make(map[string]string, 1)
	}
	s.Sandbox.Annotations[agentsv1alpha1.AnnotationCleanup] = agentsv1alpha1.True
	return s.Cache.GetClient().Patch(ctx, s.Sandbox, patch)
}

func (s *Sandbox) IsRecycleEnabled() bool {
	return s.Sandbox.Annotations[agentsv1alpha1.AnnotationCleanupEnabled] == agentsv1alpha1.True
}

func (s *Sandbox) Phase() string {
	return string(s.Sandbox.Status.Phase)
}

func (s *Sandbox) GetSandboxID() string {
	return utils.GetSandboxID(s.Sandbox)
}

// GetTrafficAccessToken returns the transient access token minted during claim
// for accessing this sandbox through the sandbox gateway. It is empty unless
// the sandbox opted in via AnnotationEnableJwtAuth.
func (s *Sandbox) GetTrafficAccessToken() string {
	if s.trafficToken == nil {
		return ""
	}
	return s.trafficToken.AccessToken
}

// GetTrafficAccessTokenExpiration returns the expiration time (RFC3339) of the
// transient traffic token minted during claim. It is empty unless the sandbox
// opted in via AnnotationEnableJwtAuth.
func (s *Sandbox) GetTrafficAccessTokenExpiration() string {
	if s.trafficToken == nil {
		return ""
	}
	return s.trafficToken.AccessTokenExpiration
}

func (s *Sandbox) GetRoute() proxy.Route {
	return proxyutils.DefaultGetRouteFunc(s.Sandbox)
}

func setTimeout(sbx *agentsv1alpha1.Sandbox, opts timeout.Options) {
	if !opts.PauseTime.IsZero() {
		sbx.Spec.PauseTime = ptr.To(metav1.NewTime(timeout.NormalizeTime(opts.PauseTime)))
	} else {
		sbx.Spec.PauseTime = nil
	}
	if !opts.ShutdownTime.IsZero() {
		sbx.Spec.ShutdownTime = ptr.To(metav1.NewTime(timeout.NormalizeTime(opts.ShutdownTime)))
	} else {
		sbx.Spec.ShutdownTime = nil
	}
}

func mergeExtraAnnotations(sbx *agentsv1alpha1.Sandbox, annotations map[string]string) {
	if len(annotations) == 0 {
		return
	}
	if sbx.Annotations == nil {
		sbx.Annotations = map[string]string{}
	}
	for key, value := range annotations {
		sbx.Annotations[key] = value
	}
}

func (s *Sandbox) SetTimeout(opts timeout.Options) {
	setTimeout(s.Sandbox, opts)
}

func (s *Sandbox) GetPodLabels() map[string]string {
	if s.Spec.Template != nil {
		return s.Spec.Template.Labels
	}
	return nil
}

func (s *Sandbox) SetPodLabels(labels map[string]string) {
	if s.Spec.Template != nil {
		s.Spec.Template.Labels = labels
	}
}

// SetImage sets the image of the first container
func (s *Sandbox) SetImage(image string) {
	if s.Spec.Template != nil {
		s.Spec.Template.Spec.Containers[0].Image = image
	}
}

func (s *Sandbox) GetImage() string {
	if s.Spec.Template != nil {
		return s.Spec.Template.Spec.Containers[0].Image
	}
	return ""
}

// SaveTimeoutWithPolicy updates timeout with given policy. Available timeout update policies:
//   - Always: overwrite timeout whenever the requested value differs from current.
//   - ExtendOnly: only extend to a later effective end time.
func (s *Sandbox) SaveTimeoutWithPolicy(ctx context.Context, opts infra.SaveTimeoutOptions, policy timeout.UpdatePolicy) (infra.TimeoutUpdateResult, error) {
	log := klog.FromContext(ctx).V(utils.DebugLogLevel).WithValues("sandbox", klog.KObj(s.Sandbox), "policy", policy)
	result := infra.TimeoutUpdateResult{}

	updated, err := s.retryUpdate(ctx, func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
		current := timeout.GetTimeoutFromSandbox(sbx)
		log.Info("data fetched before saving timeout", "current", current)

		shouldUpdate := false
		switch policy {
		case timeout.UpdatePolicyAlways:
			shouldUpdate = !timeout.Equal(current, opts.Timeout)
		case timeout.UpdatePolicyExtendOnly:
			shouldUpdate = timeout.ShouldExtendTimeout(current, opts.Timeout)
		default:
			return false, fmt.Errorf("unsupported timeout update policy %q", policy)
		}

		if !shouldUpdate {
			return false, nil
		}
		setTimeout(sbx, opts.Timeout)
		mergeExtraAnnotations(sbx, opts.ExtraAnnotations)
		return true, nil
	})
	if err != nil {
		log.Error(err, "failed to update sandbox timeout after retries")
		return infra.TimeoutUpdateResult{}, err
	}
	result.Updated = updated

	log.Info("sandbox timeout updated successfully", "updated", result.Updated, "timeout", s.GetTimeout())
	return result, nil
}

func (s *Sandbox) GetTimeout() timeout.Options {
	return timeout.GetTimeoutFromSandbox(s.Sandbox)
}

func (s *Sandbox) GetResource() infra.SandboxResource {
	if s.Spec.Template == nil {
		return infra.SandboxResource{}
	}
	return infra.CalculateResourceFromContainers(s.Spec.Template.Spec.Containers)
}

func (s *Sandbox) Request(ctx context.Context, method, path string, port int, body io.Reader) (*http.Response, error) {
	return proxyutils.DefaultRequestFunc(ctx, s.Sandbox, method, path, port, body)
}

func (s *Sandbox) Pause(ctx context.Context, opts infra.PauseOptions) error {
	log := klog.FromContext(ctx)
	if err := s.refreshFromAPIReader(ctx); err != nil {
		return err
	}
	if pausable, reason := utils.IsSandboxPausable(s.Sandbox); !pausable {
		return errors.NewError(errors.ErrorConflict, "sandbox is not pausable, reason: %s", reason)
	}

	cond := GetSandboxCondition(s.Sandbox, agentsv1alpha1.SandboxConditionPaused)
	if s.Status.Phase == agentsv1alpha1.SandboxPaused {
		if cond.Status == metav1.ConditionTrue {
			log.Info("sandbox is already paused")
			return nil
		}
	}

	pauseTask, err := s.Cache.NewSandboxPauseTask(ctx, s.Sandbox)
	if err != nil {
		return err
	}
	defer pauseTask.Release()
	updated, err := s.retryUpdate(ctx, func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
		if sbx.Spec.Paused {
			// Pause is first-writer-wins: only the request that flips Spec.Paused
			// from false to true may update timeout fields or annotations.
			return false, nil
		}
		sbx.Spec.Paused = true
		if opts.Timeout != nil {
			current := timeout.GetTimeoutFromSandbox(sbx)
			if !timeout.Equal(current, *opts.Timeout) {
				setTimeout(sbx, *opts.Timeout)
			}
		}
		mergeExtraAnnotations(sbx, opts.ExtraAnnotations)
		return true, nil
	})
	if err != nil {
		log.Error(err, "failed to update sandbox")
		return err
	}
	if !updated {
		log.Info("skip update sandbox as it is already set to paused")
	}
	log.Info("waiting sandbox pause")
	start := time.Now()
	if err = pauseTask.Wait(time.Minute); err != nil {
		log.Error(err, "failed to wait sandbox pause")
		return err
	}
	log.Info("sandbox paused", "cost", time.Since(start))
	return s.InplaceRefresh(ctx, false)
}

const postResumeOperationTimeout = 30 * time.Second

// resumeWaitMaxTimeout is a defensive upper bound for resumeTask.Wait. The real
// timeout is expected to come from the request ctx; this value only guards
// callers that pass a ctx without a deadline so Resume cannot block forever.
const resumeWaitMaxTimeout = 10 * time.Minute

func (s *Sandbox) Resume(ctx context.Context, opts infra.ResumeOptions) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox))

	if err := s.refreshFromAPIReader(ctx); err != nil {
		return err
	}

	if resumable, reason := utils.IsSandboxResumable(s.Sandbox); !resumable {
		return errors.NewError(errors.ErrorConflict, "sandbox is not resumable, reason: %s", reason)
	}
	resumeTask, err := s.Cache.NewSandboxResumeTask(ctx, s.Sandbox)
	if err != nil {
		return err
	}
	defer resumeTask.Release()

	cond := GetSandboxCondition(s.Sandbox, agentsv1alpha1.SandboxConditionReady)
	if cond.Status == metav1.ConditionTrue {
		log.Info("sandbox is already resumed")
		return nil
	}

	state, reason := s.GetState()
	log.Info("try to resume sandbox", "state", state, "reason", reason)
	resumeUpdated, err := s.retryUpdate(ctx, func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
		if !sbx.Spec.Paused {
			// First-writer-wins: the loser must not overwrite the winner's
			// fresh PauseTime, otherwise the controller would auto-pause.
			return false, nil
		}
		sbx.Spec.Paused = false
		if opts.Timeout != nil {
			setTimeout(sbx, *opts.Timeout)
		}
		return true, nil
	})
	if err != nil {
		log.Error(err, "failed to update sandbox spec.paused")
		return err
	}
	log.Info("waiting sandbox resume")
	start := time.Now()
	if err = resumeTask.Wait(resumeWaitMaxTimeout); err != nil {
		// A canceled request context surfaces here as "client rate limiter Wait
		// returned an error: context canceled" from client-go, which is misleading:
		// it is not a throttling failure but a propagation of ctx.Err(). Log it at
		// Info level so it is not mistaken for a server-side error in metrics or
		// alerting pipelines that watch klog ERROR.
		if ctxErr := ctx.Err(); ctxErr != nil {
			log.Info("stop waiting sandbox resume: request canceled by client (disconnected or client-side timeout)",
				"err", err, "ctxErr", ctxErr)
			return err
		}
		log.Error(err, "failed to wait sandbox resume")
		return err
	}
	log.Info("sandbox resumed", "cost", time.Since(start))

	// If the original context deadline was consumed by the wait, create a fresh
	// context for post-resume operations (inplace refresh).
	// This can happen when the wait succeeds via double-check right at the deadline boundary.
	postCtx := ctx
	if ctx.Err() != nil {
		var postCancel context.CancelFunc
		postCtx, postCancel = context.WithTimeout(context.Background(), postResumeOperationTimeout)
		defer postCancel()
		log.Info("original context expired after wait, using fresh context for post-resume operations")
	}
	if err := s.InplaceRefresh(postCtx, false); err != nil {
		log.Error(err, "failed to refresh sandbox after resume")
		return err
	}
	expectations.ResourceVersionExpectationExpect(s.Sandbox) // expect Running

	if !resumeUpdated {
		// Concurrent same-action Resume callers share the Sandbox resume wait.
		// Once the Sandbox reaches Ready, losing callers return success without
		// running or waiting for the transitional E2B post-resume initialization
		// below. ReInit and CSI remount are not part of the loser success contract.
		log.Info("sandbox resume already won by another request, skipping post-resume operations")
		return nil
	}

	// Post-resume initialization (ReInit + CSI mount) is now handled by the sandbox-controller's
	// Initialize function. The controller gates the Running state on successful initialization,
	// so by the time NewSandboxResumeTask completes, all mounts are already done.

	return nil
}

func (s *Sandbox) GetState() (string, string) {
	return utils.GetSandboxState(s.Sandbox)
}

func (s *Sandbox) GetClaimTime() (time.Time, error) {
	claimTimestamp := s.GetAnnotations()[agentsv1alpha1.AnnotationClaimTime]
	return time.Parse(time.RFC3339, claimTimestamp)
}

// CSIMount creates a dynamic mount point in Sandbox with `sandbox-storage` cli.
// It delegates to the runtime package's CSIMount function to avoid circular dependencies.
func (s *Sandbox) CSIMount(ctx context.Context, driver string, request string) error {
	return runtime.CSIMount(ctx, s.Sandbox, driver, request)
}

func (s *Sandbox) CreateCheckpoint(ctx context.Context, opts infra.CreateCheckpointOptions) (string, error) {
	log := klog.FromContext(ctx)
	opts = ValidateAndInitCheckpointOptions(opts)
	log.Info("create checkpoint options", "options", opts)
	return CreateCheckpoint(ctx, s.Sandbox, s.Cache, opts)
}

var _ infra.Sandbox = &Sandbox{}

func AsSandbox(sbx *agentsv1alpha1.Sandbox, cache cache.Provider) *Sandbox {
	return &Sandbox{
		Cache:           cache,
		Sandbox:         sbx,
		storageRegistry: storages.NewStorageProvider(),
	}
}
