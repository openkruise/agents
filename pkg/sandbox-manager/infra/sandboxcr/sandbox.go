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

	"github.com/openkruise/agents/pkg/utils/runtime"

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
)

type ModifierFunc func(sbx *agentsv1alpha1.Sandbox)

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

func (s *Sandbox) retryUpdate(ctx context.Context, modifier ModifierFunc) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox))
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// get the latest sandbox from cache
		sbx, err := s.Cache.GetClaimedSandbox(ctx, stateutils.GetSandboxID(s.Sandbox))
		if err != nil {
			return err
		}

		copied := sbx.DeepCopy()
		modifier(copied)
		if err = s.Cache.GetClient().Update(ctx, copied); err != nil {
			return err
		}
		s.Sandbox = copied
		expectationutils.ResourceVersionExpectationExpect(copied)
		return nil
	})
	if err != nil {
		log.Error(err, "failed to update sandbox after retries")
	} else {
		log.Info("sandbox updated successfully")
	}
	return err
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

func setTimeout(s *agentsv1alpha1.Sandbox, opts infra.TimeoutOptions) {
	if !opts.PauseTime.IsZero() {
		s.Spec.PauseTime = ptr.To(metav1.NewTime(opts.PauseTime))
	} else {
		s.Spec.PauseTime = nil
	}
	if !opts.ShutdownTime.IsZero() {
		s.Spec.ShutdownTime = ptr.To(metav1.NewTime(opts.ShutdownTime))
	} else {
		s.Spec.ShutdownTime = nil
	}
}

func (s *Sandbox) SetTimeout(opts infra.TimeoutOptions) {
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

func (s *Sandbox) SaveTimeout(ctx context.Context, opts infra.TimeoutOptions) error {
	return s.retryUpdate(ctx, func(sbx *agentsv1alpha1.Sandbox) {
		setTimeout(sbx, opts)
	})
}

func (s *Sandbox) GetTimeout() infra.TimeoutOptions {
	return getTimeoutFromSandbox(s.Sandbox)
}

func getTimeoutFromSandbox(s *agentsv1alpha1.Sandbox) infra.TimeoutOptions {
	opts := infra.TimeoutOptions{}
	if s.Spec.ShutdownTime != nil {
		opts.ShutdownTime = s.Spec.ShutdownTime.Time
	}
	if s.Spec.PauseTime != nil {
		opts.PauseTime = s.Spec.PauseTime.Time
	}
	return opts
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
	if s.Status.Phase != agentsv1alpha1.SandboxRunning {
		return fmt.Errorf("sandbox is not in running phase")
	}
	state, reason := s.GetState()
	if state != agentsv1alpha1.SandboxStateRunning {
		err := fmt.Errorf("pausing is only available for running state, current state: %s", state)
		log.Error(err, "sandbox is not running", "state", state, "reason", reason)
		return err
	}
	err := s.retryUpdate(ctx, func(sbx *agentsv1alpha1.Sandbox) {
		sbx.Spec.Paused = true
		if opts.Timeout != nil {
			setTimeout(sbx, *opts.Timeout)
		}
	})
	if err != nil {
		log.Error(err, "failed to update sandbox spec.paused")
		return err
	}
	expectationutils.ResourceVersionExpectationExpect(s.Sandbox)
	log.Info("waiting sandbox pause")
	start := time.Now()
	if err = s.Cache.NewSandboxPauseTask(ctx, s.Sandbox).Wait(time.Minute); err != nil {
		log.Error(err, "failed to wait sandbox pause")
		return err
	}
	log.Info("sandbox paused", "cost", time.Since(start))
	return s.InplaceRefresh(ctx, false)
}

const postResumeOperationTimeout = 30 * time.Second

func (s *Sandbox) Resume(ctx context.Context) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox))

	initRuntimeOpts, err := runtime.GetInitRuntimeRequest(s.Sandbox)
	if err != nil {
		log.Error(err, "failed to get init runtime request")
		return fmt.Errorf("failed to get init runtime request: %w", err)
	}

	state, reason := s.GetState()
	log.Info("try to resume sandbox", "state", state, "reason", reason)
	if state != agentsv1alpha1.SandboxStatePaused {
		err := fmt.Errorf("resuming is only available for paused state, current state: %s", state)
		log.Error(err, "sandbox is not paused", "state", state, "reason", reason)
		return err
	}
	cond := GetSandboxCondition(s.Sandbox, agentsv1alpha1.SandboxConditionPaused)
	if s.Spec.Paused && cond.Status == metav1.ConditionFalse {
		return errors.NewError(errors.ErrorBadRequest, "sandbox is pausing, please wait a moment and try again")
	}
	if s.Sandbox.Spec.Paused {
		if err := s.retryUpdate(ctx, func(sbx *agentsv1alpha1.Sandbox) {
			sbx.Spec.Paused = false
			setTimeout(sbx, infra.TimeoutOptions{}) // remove all timeout options
		}); err != nil {
			log.Error(err, "failed to update sandbox spec.paused")
			return err
		}
	}
	expectationutils.ResourceVersionExpectationExpect(s.Sandbox) // expect Resuming
	log.Info("waiting sandbox resume")
	start := time.Now()
	if err = s.Cache.NewSandboxResumeTask(ctx, s.Sandbox).Wait(time.Minute); err != nil {
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


	// E2B only handles post-resume initialization for non-claimed sandboxes.
	// Claimed sandboxes (with claim-name label) are handled by the controller's Initialize function.
	if s.Labels[agentsv1alpha1.LabelSandboxClaimName] == "" {
		// Perform ReInit if initRuntimeOpts is set
		if initRuntimeOpts != nil {
			log.Info("will re-init runtime after resume")
			if _, err := runtime.InitRuntime(ctx, s.Sandbox, *initRuntimeOpts, s.refreshFunc()); err != nil {
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
			csiClient := csimountutils.NewCSIMountHandler(s.Cache.GetClient(), s.Cache, s.storageRegistry, utils.DefaultSandboxDeployNamespace)
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
