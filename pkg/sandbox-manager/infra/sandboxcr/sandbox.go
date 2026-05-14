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
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
	csimountutils "github.com/openkruise/agents/pkg/utils/csiutils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"github.com/openkruise/agents/pkg/utils/runtime"
	sandboxManagerUtils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager/expectationutils"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager/proxyutils"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
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
}

var DefaultDeleteSandbox = deleteSandbox

func deleteSandbox(ctx context.Context, sbx *agentsv1alpha1.Sandbox, client client.Client) error {
	return client.Delete(ctx, sbx)
}

func (s *Sandbox) GetTemplate() string {
	return utils.GetTemplateFromSandbox(s.Sandbox)
}

func (s *Sandbox) InplaceRefresh(ctx context.Context, deepcopy bool) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox)).V(consts.DebugLogLevel)
	fetchFromApiServer := false
	objectKey := client.ObjectKeyFromObject(s.Sandbox)
	newSbx := &agentsv1alpha1.Sandbox{}
	err := s.Cache.GetClient().Get(ctx, objectKey, newSbx)
	if err != nil {
		log.Info("failed to get claimed sandbox from cache, fetch from api-server", "reason", err.Error())
		fetchFromApiServer = true
	} else if !expectationutils.ResourceVersionExpectationSatisfied(newSbx) {
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
// Conflict retries refresh from APIReader to avoid reusing stale informer data.
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
		expectationutils.ResourceVersionExpectationExpect(copied)
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

func (s *Sandbox) GetSandboxID() string {
	return stateutils.GetSandboxID(s.Sandbox)
}

func (s *Sandbox) GetRoute() proxy.Route {
	return proxyutils.DefaultGetRouteFunc(s.Sandbox)
}

func setTimeout(s *agentsv1alpha1.Sandbox, opts timeout.Options) {
	if !opts.PauseTime.IsZero() {
		s.Spec.PauseTime = ptr.To(metav1.NewTime(timeout.NormalizeTime(opts.PauseTime)))
	} else {
		s.Spec.PauseTime = nil
	}
	if !opts.ShutdownTime.IsZero() {
		s.Spec.ShutdownTime = ptr.To(metav1.NewTime(timeout.NormalizeTime(opts.ShutdownTime)))
	} else {
		s.Spec.ShutdownTime = nil
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
//   - BaselineAware: the caller provides the timeout data it observed before issuing
//     this update. When current timeout matches baseline, it is treated as
//     overwrite-allowed; when current timeout differs from baseline, only extension
//     is allowed.
func (s *Sandbox) SaveTimeoutWithPolicy(ctx context.Context, opts timeout.Options, policy timeout.UpdatePolicy) (infra.TimeoutUpdateResult, error) {
	log := klog.FromContext(ctx).V(consts.DebugLogLevel).WithValues("sandbox", klog.KObj(s.Sandbox), "policy", policy)
	result := infra.TimeoutUpdateResult{}

	updated, err := s.retryUpdate(ctx, func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
		current := timeout.GetTimeoutFromSandbox(sbx)
		log.Info("data fetched before saving timeout", "current", current)

		shouldUpdate := false
		switch policy {
		case timeout.UpdatePolicyAlways:
			shouldUpdate = !timeout.Equal(current, opts)
		case timeout.UpdatePolicyExtendOnly:
			shouldUpdate = timeout.ShouldExtendTimeout(current, opts)
		case timeout.UpdatePolicyBaselineAware:
			if opts.Baseline == nil {
				return false, fmt.Errorf("BaselineAware policy requires opts.Baseline to be set")
			}
			if timeout.Equal(current, *opts.Baseline) {
				// No concurrent writer has changed the timeout since the caller's observation.
				// Treat as Always: overwrite if different.
				shouldUpdate = !timeout.Equal(current, opts)
				log.Info("baseline matches current, using Always semantics", "shouldUpdate", shouldUpdate)
			} else {
				// Some other request has already written a new timeout in this cycle.
				// Degrade to ExtendOnly so we never shrink an already-extended timeout.
				shouldUpdate = timeout.ShouldExtendTimeout(current, opts)
				log.Info("baseline differs from current, using ExtendOnly semantics", "shouldUpdate", shouldUpdate)
			}
		default:
			return false, fmt.Errorf("unsupported timeout update policy %q", policy)
		}

		if !shouldUpdate {
			return false, nil
		}
		setTimeout(sbx, opts)
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
	return sandboxManagerUtils.CalculateResourceFromContainers(s.Spec.Template.Spec.Containers)
}

func (s *Sandbox) Request(ctx context.Context, method, path string, port int, body io.Reader) (*http.Response, error) {
	return proxyutils.DefaultRequestFunc(ctx, s.Sandbox, method, path, port, body)
}

func (s *Sandbox) Pause(ctx context.Context, opts infra.PauseOptions) error {
	log := klog.FromContext(ctx)
	if err := s.refreshFromAPIReader(ctx); err != nil {
		return err
	}
	if pausable, reason := stateutils.IsSandboxPausable(s.Sandbox); !pausable {
		return errors.NewError(errors.ErrorConflict, "sandbox is not pausable, reason: %s", reason)
	}
	pauseTask, err := s.Cache.NewSandboxPauseTask(ctx, s.Sandbox)
	if err != nil {
		return err
	}
	defer pauseTask.Release()

	cond := GetSandboxCondition(s.Sandbox, agentsv1alpha1.SandboxConditionPaused)
	if s.Status.Phase == agentsv1alpha1.SandboxPaused {
		if cond.Status == metav1.ConditionTrue {
			log.Info("sandbox is already paused")
			return nil
		}
	}
	updated, err := s.retryUpdate(ctx, func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
		if sbx.Spec.Paused {
			// Pause is first-writer-wins: only the request that flips
			// No need to update if spec.paused is already true.
			return false, nil
		}
		sbx.Spec.Paused = true
		if opts.Timeout != nil {
			current := timeout.GetTimeoutFromSandbox(sbx)
			if !timeout.Equal(current, *opts.Timeout) {
				setTimeout(sbx, *opts.Timeout)
			}
		}
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

func (s *Sandbox) Resume(ctx context.Context, _ infra.ResumeOptions) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox))

	if err := s.refreshFromAPIReader(ctx); err != nil {
		return err
	}

	if resumable, reason := stateutils.IsSandboxResumable(s.Sandbox); !resumable {
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
	initRuntimeOpts, err := runtime.GetInitRuntimeRequest(s.Sandbox)
	if err != nil {
		log.Error(err, "failed to get init runtime request")
		return fmt.Errorf("failed to get init runtime request: %w", err)
	}
	resumeUpdated, err := s.retryUpdate(ctx, func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
		if !sbx.Spec.Paused {
			// Resume is first-writer-wins: only the request that flips
			// No need to update if spec.paused is already false.
			return false, nil
		}
		sbx.Spec.Paused = false
		return true, nil
	})
	if err != nil {
		log.Error(err, "failed to update sandbox spec.paused")
		return err
	}
	log.Info("waiting sandbox resume")
	start := time.Now()
	if err = resumeTask.Wait(time.Minute); err != nil {
		log.Error(err, "failed to wait sandbox resume")
		return err
	}
	log.Info("sandbox resumed", "cost", time.Since(start))

	// If the original context deadline was consumed by the wait, create a fresh
	// context for post-resume operations (ReInit, CSI mount, inplace refresh).
	// This can happen when the wait succeeds via double-check right at the deadline boundary.
	postCtx := ctx
	if ctx.Err() != nil {
		var postCancel context.CancelFunc
		postCtx, postCancel = context.WithTimeout(context.Background(), postResumeOperationTimeout)
		defer postCancel()
		log.Info("original context expired after wait, using fresh context for post-resume operations")
	}
	if err = s.InplaceRefresh(postCtx, false); err != nil {
		log.Error(err, "failed to refresh sandbox after resume")
		return err
	}
	expectationutils.ResourceVersionExpectationExpect(s.Sandbox) // expect Running

	if !resumeUpdated {
		// Concurrent same-action Resume callers share the Sandbox resume wait.
		// Once the Sandbox reaches Ready, losing callers return success without
		// running or waiting for the transitional E2B post-resume initialization
		// below. ReInit and CSI remount are not part of the loser success contract.
		log.Info("sandbox resume already won by another request, skipping post-resume operations")
		return nil
	}

	// E2B only handles post-resume initialization for non-claimed sandboxes.
	// Claimed sandboxes (with claim-name label) are handled by the controller's Initialize function.
	if s.Labels[agentsv1alpha1.LabelSandboxClaimName] == "" {
		// Perform ReInit if initRuntimeOpts is set
		if initRuntimeOpts != nil {
			log.Info("will re-init runtime after resume")
			if _, err := runtime.InitRuntime(postCtx, s.Sandbox, *initRuntimeOpts, s.refreshFunc()); err != nil {
				log.Error(err, "failed to perform ReInit after resume")
				return fmt.Errorf("failed to perform ReInit after resume: %w", err)
			}
			log.Info("ReInit completed after resume")
		}

		// Perform csi mount after resume
		csiMountConfigRequests, err := runtime.GetCsiMountExtensionRequest(s.Sandbox)
		if err != nil {
			log.Error(err, "failed to get csi mount request")
			return fmt.Errorf("failed to get csi mount request: %w", err)
		}

		if len(csiMountConfigRequests) != 0 {
			log.Info("will re-mount csi storage after resume")
			startTime := time.Now()
			csiClient := csimountutils.NewCSIMountHandler(s.Cache.GetClient(), s.Cache.GetAPIReader(), s.storageRegistry, utils.DefaultSandboxDeployNamespace)
			mountConfigs, resolveErr := resolveCSIMountConfigs(postCtx, csiClient, csiMountConfigRequests)
			if resolveErr != nil {
				return resolveErr
			}
			opts := config.CSIMountOptions{MountOptionList: mountConfigs}
			if _, mountErr := runtime.ProcessCSIMounts(postCtx, s.Sandbox, opts); mountErr != nil {
				log.Error(mountErr, "failed to remount csi storage after resume")
				return fmt.Errorf("failed to remount csi storage after resume: %v", mountErr)
			}
			log.Info("remount csi storage completed after resume", "costTime", time.Since(startTime))
		}
	} else {
		log.Info("sandbox is claimed by SandboxClaim, skipping E2B post-resume initialization",
			"claimName", s.Labels[agentsv1alpha1.LabelSandboxClaimName])
	}

	return nil
}

func (s *Sandbox) GetState() (string, string) {
	return stateutils.GetSandboxState(s.Sandbox)
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

// resolveCSIMountConfigs converts CSIMountConfig requests into MountConfig
// by calling CSIMountOptionsConfig for each request sequentially.
func resolveCSIMountConfigs(ctx context.Context, csiClient *csimountutils.CSIMountHandler, requests []agentsv1alpha1.CSIMountConfig) ([]config.MountConfig, error) {
	log := klog.FromContext(ctx)
	results := make([]config.MountConfig, 0, len(requests))
	for _, req := range requests {
		driverName, csiReqConfigRaw, err := csiClient.CSIMountOptionsConfig(ctx, req)
		if err != nil {
			log.Error(err, "failed to generate csi mount options config", "mountConfigRequest", req)
			return nil, fmt.Errorf("failed to generate csi mount options config, err: %v", err)
		}
		results = append(results, config.MountConfig{Driver: driverName, RequestRaw: csiReqConfigRaw})
	}
	return results, nil
}

func AsSandbox(sbx *agentsv1alpha1.Sandbox, cache cache.Provider) *Sandbox {
	return &Sandbox{
		Cache:           cache,
		Sandbox:         sbx,
		storageRegistry: storages.NewStorageProvider(),
	}
}
