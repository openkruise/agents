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
	"errors"
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

var (
	errSandboxAlreadyPaused  = errors.New("sandbox is already paused")
	errSandboxAlreadyRunning = errors.New("sandbox is already running")
)

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

// setTimeout overwrites Spec.PauseTime / Spec.ShutdownTime from opts.
//
// Contract (relied upon by callers such as buildResumeTimeoutOptions and
// SaveTimeout): a zero time.Time in opts is treated as "clear this field" and
// will set the corresponding Spec.*Time pointer back to nil. This is the
// intended way for upper layers to express "this sandbox should be
// never-timeout"; do not change this to skip-on-zero, otherwise callers that
// pass infra.TimeoutOptions{} expecting the underlying fields to be cleared
// will silently retain stale values.
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

// SaveTimeout persists opts to Spec.PauseTime / Spec.ShutdownTime via the
// API server. Per setTimeout's contract, a zero time.Time in opts clears the
// corresponding spec field; callers that want to mark a sandbox as
// never-timeout should pass infra.TimeoutOptions{} (or a struct with both
// times zero).
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

// SingleflightDo executes a distributed single-flight operation using this Sandbox
// as the coordination group. It delegates to cache.DistributedSingleFlightDo.
func (s *Sandbox) SingleflightDo(
	ctx context.Context,
	key string,
	precheck func(*agentsv1alpha1.Sandbox) error,
	modifier func(*agentsv1alpha1.Sandbox),
	function func(*agentsv1alpha1.Sandbox) error,
) (*agentsv1alpha1.Sandbox, error) {
	return cache.DistributedSingleFlightDo(
		ctx,
		s.Cache,
		s.Sandbox,
		key,
		precheck,
		modifier,
		function,
		cache.DefaultSingleflightPreemptionThreshold,
	)
}

func (s *Sandbox) Pause(ctx context.Context, opts infra.PauseOptions) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox))

	pausePrecheck := func(sbx *agentsv1alpha1.Sandbox) error {
		cond := GetSandboxCondition(sbx, agentsv1alpha1.SandboxConditionPaused)
		if cond.Type != "" && cond.Status == metav1.ConditionTrue {
			return errSandboxAlreadyPaused
		}

		state, _ := stateutils.GetSandboxState(sbx)
		if state == agentsv1alpha1.SandboxStateDead {
			return fmt.Errorf("sandbox is dead, cannot pause")
		}
		return nil
	}

	pauseModifier := func(sbx *agentsv1alpha1.Sandbox) {
		sbx.Spec.Paused = true
		if opts.Timeout != nil {
			setTimeout(sbx, *opts.Timeout)
		}
	}

	pauseFunction := func(sbx *agentsv1alpha1.Sandbox) error {
		expectationutils.ResourceVersionExpectationExpect(sbx)
		log.Info("waiting sandbox pause")
		start := time.Now()
		if err := s.Cache.NewSandboxPauseTask(ctx, sbx).Wait(time.Minute); err != nil {
			log.Error(err, "failed to wait sandbox pause")
			return err
		}
		log.Info("sandbox paused", "cost", time.Since(start))
		s.Sandbox = sbx
		if err := s.InplaceRefresh(ctx, false); err != nil {
			return err
		}
		*sbx = *s.Sandbox.DeepCopy()
		return nil
	}

	for {
		latest, err := s.SingleflightDo(ctx, "pause-resume", pausePrecheck, pauseModifier, pauseFunction)
		if err != nil {
			if errors.Is(err, errSandboxAlreadyPaused) && s.refreshSingleflightState(ctx) == nil &&
				hasSingleflightAnnotation(s.Sandbox, "pause-resume") {
				cond := GetSandboxCondition(s.Sandbox, agentsv1alpha1.SandboxConditionPaused)
				if cond.Type != "" && cond.Status == metav1.ConditionTrue {
					return nil
				}
			}
			return err
		}
		s.Sandbox = latest

		cond := GetSandboxCondition(s.Sandbox, agentsv1alpha1.SandboxConditionPaused)
		if cond.Type != "" && cond.Status == metav1.ConditionTrue {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}

func bumpResumeTimeoutProtection(sbx *agentsv1alpha1.Sandbox, protectUntil time.Time) {
	if sbx.Spec.PauseTime != nil && sbx.Spec.PauseTime.Time.Before(protectUntil) {
		sbx.Spec.PauseTime = ptr.To(metav1.NewTime(protectUntil))
	}
	if sbx.Spec.ShutdownTime != nil && sbx.Spec.ShutdownTime.Time.Before(protectUntil) {
		sbx.Spec.ShutdownTime = ptr.To(metav1.NewTime(protectUntil))
	}
}

const postResumeOperationTimeout = 30 * time.Second

func postResumeContext(ctx context.Context) (context.Context, context.CancelFunc, bool) {
	if ctx.Err() == nil {
		return ctx, func() {}, false
	}
	postCtx, cancel := context.WithTimeout(context.Background(), postResumeOperationTimeout)
	return postCtx, cancel, true
}

func (s *Sandbox) Resume(ctx context.Context, opts infra.ResumeOptions) (retErr error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox))
	resumeStartedAt := time.Now()
	resumeExecuted := false
	timeoutPersisted := false

	saveFinalResumeTimeout := func() {
		if opts.Timeout == nil || timeoutPersisted {
			return
		}
		timeoutPersisted = true
		if !opts.DisablePushTimeout {
			infra.PushTimeout(opts.Timeout, time.Since(resumeStartedAt))
		}
		saveCtx, cancel, freshSaveCtx := postResumeContext(ctx)
		defer cancel()
		if freshSaveCtx {
			log.Info("original context expired after wait, using fresh context for saving final resume timeout")
		}
		log.Info("saving final resume timeout", "timeout", *opts.Timeout)
		if err := s.SaveTimeout(saveCtx, *opts.Timeout); err != nil {
			if retErr != nil {
				log.Error(err, "failed to save final resume timeout", "originalError", retErr)
				return
			}
			log.Error(err, "failed to save final resume timeout")
			retErr = fmt.Errorf("failed to save final resume timeout: %w", err)
		}
	}

	resumePrecheck := func(sbx *agentsv1alpha1.Sandbox) error {
		state, reason := stateutils.GetSandboxState(sbx)
		if state == agentsv1alpha1.SandboxStateRunning {
			return errSandboxAlreadyRunning
		}
		if state == agentsv1alpha1.SandboxStateDead {
			if reason == "ShutdownTimeReached" {
				return fmt.Errorf("ShutdownTimeReached")
			}
			return fmt.Errorf("sandbox is dead, cannot resume")
		}
		return nil
	}

	resumeModifier := func(sbx *agentsv1alpha1.Sandbox) {
		sbx.Spec.Paused = false
		bumpResumeTimeoutProtection(sbx, time.Now().Add(time.Hour))
	}

	resumeFunction := func(sbx *agentsv1alpha1.Sandbox) error {
		resumeExecuted = true
		expectationutils.ResourceVersionExpectationExpect(sbx)
		log.Info("waiting sandbox resume")
		start := time.Now()
		if opts.Timeout != nil {
			defer saveFinalResumeTimeout()
		}
		if err := s.Cache.NewSandboxResumeTask(ctx, sbx).Wait(time.Minute); err != nil {
			log.Error(err, "failed to wait sandbox resume")
			return err
		}
		log.Info("sandbox resumed", "cost", time.Since(start))

		postCtx, postCancel, freshPostCtx := postResumeContext(ctx)
		defer postCancel()
		if freshPostCtx {
			log.Info("original context expired after wait, using fresh context for post-resume operations")
		}
		s.Sandbox = sbx
		if err := s.InplaceRefresh(postCtx, false); err != nil {
			log.Error(err, "failed to refresh sandbox after resume")
			return err
		}
		expectationutils.ResourceVersionExpectationExpect(s.Sandbox)
		*sbx = *s.Sandbox.DeepCopy()

		if s.Labels[agentsv1alpha1.LabelSandboxClaimName] == "" {
			initRuntimeOpts, err := runtime.GetInitRuntimeRequest(s.Sandbox)
			if err != nil {
				log.Error(err, "failed to get init runtime request")
				return fmt.Errorf("failed to get init runtime request: %w", err)
			}
			if initRuntimeOpts != nil {
				log.Info("will re-init runtime after resume")
				if _, err := runtime.InitRuntime(postCtx, s.Sandbox, *initRuntimeOpts, s.refreshFunc()); err != nil {
					log.Error(err, "failed to perform ReInit after resume")
					return fmt.Errorf("failed to perform ReInit after resume: %w", err)
				}
				log.Info("ReInit completed after resume")
			}

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

	for {
		latest, err := s.SingleflightDo(ctx, "pause-resume", resumePrecheck, resumeModifier, resumeFunction)
		if err != nil {
			if errors.Is(err, errSandboxAlreadyRunning) && s.refreshSingleflightState(ctx) == nil &&
				hasSingleflightAnnotation(s.Sandbox, "pause-resume") {
				state, _ := stateutils.GetSandboxState(s.Sandbox)
				if state == agentsv1alpha1.SandboxStateRunning {
					if !resumeExecuted {
						saveFinalResumeTimeout()
					}
					return retErr
				}
			}
			return err
		}
		s.Sandbox = latest

		state, _ := stateutils.GetSandboxState(s.Sandbox)
		if state == agentsv1alpha1.SandboxStateRunning {
			if !resumeExecuted {
				saveFinalResumeTimeout()
			}
			return retErr
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}

func (s *Sandbox) refreshSingleflightState(ctx context.Context) error {
	fresh := s.Sandbox.DeepCopy()
	if err := s.Cache.GetAPIReader().Get(ctx, client.ObjectKeyFromObject(s.Sandbox), fresh); err != nil {
		return err
	}
	s.Sandbox = fresh
	return nil
}

func hasSingleflightAnnotation(sbx *agentsv1alpha1.Sandbox, key string) bool {
	if sbx == nil || sbx.Annotations == nil {
		return false
	}
	_, ok := sbx.Annotations[cache.SingleflightAnnotationPrefix+key]
	return ok
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
