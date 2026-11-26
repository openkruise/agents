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
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/lifecycle/claim"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandboxid"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/tracing"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"github.com/openkruise/agents/pkg/utils/logs"
	"github.com/openkruise/agents/pkg/utils/proxyutils"
	"github.com/openkruise/agents/pkg/utils/runtime"
	"github.com/openkruise/agents/pkg/utils/timeout"

	"go.opentelemetry.io/otel/attribute"
)

// ModifierFunc mutates the sandbox and decides whether retryUpdate should persist it.
// It returns:
//   - changed: true when the provided sandbox was modified and should be updated;
//     false when no update should be issued.
//   - err:     non-nil to abort retryUpdate immediately.
type ModifierFunc = claim.ModifierFunc

type Sandbox struct {
	*agentsv1alpha1.Sandbox
	Cache           cache.Provider
	storageRegistry storages.VolumeMountProviderRegistry
	// trafficToken holds the access token response minted for accessing this
	// sandbox through the sandbox gateway. It is transient and per-operation: set
	// in memory during claim or clone and never persisted to the CR, so
	// InplaceRefresh / retryUpdate reassigning s.Sandbox do not clear it.
	trafficToken *identity.TokenResponse
}

func setMemoryPersistence(sbx *Sandbox, enabled *bool) {
	claim.SetMemoryPersistence(sbx.Sandbox, enabled)
}

func (s *Sandbox) GetIP() string {
	return s.Status.PodInfo.PodIP
}

func (s *Sandbox) GetSandboxID() string {
	return sandboxid.Resolve(s.Sandbox)
}

func (s *Sandbox) GetRoute() (sandboxroute.Route, error) {
	return sandboxroute.RouteFromSandbox(s.Sandbox)
}

func (s *Sandbox) GetTemplate() string {
	return utils.GetTemplateFromSandbox(s.Sandbox)
}

func (s *Sandbox) InplaceRefresh(ctx context.Context, deepcopy bool) error {
	cr, err := claim.InplaceRefresh(ctx, s.Cache, s.Sandbox, deepcopy)
	if err != nil {
		return err
	}
	s.Sandbox = cr
	return nil
}

// refreshFunc returns a RefreshFunc callback that refreshes this sandbox and returns the latest object.
// This allows InitRuntime in utils/runtime to refresh sandbox state without depending on the sandboxcr package.
func (s *Sandbox) refreshFunc() runtime.RefreshFunc {
	return func(ctx context.Context) (*agentsv1alpha1.Sandbox, error) {
		// Post-init steps may update annotations on this wrapper. Keep newer
		// informer state private instead of sharing its maps with those writes.
		if err := s.InplaceRefresh(ctx, true); err != nil {
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
	cr, updated, err := claim.RetryUpdate(ctx, s.Cache, s.Sandbox, modifier)
	if err != nil {
		return false, err
	}
	s.Sandbox = cr
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox))
	if updated {
		log.Info("sandbox updated successfully")
	} else {
		log.Info("sandbox update skipped")
	}
	return updated, nil
}

// retryPatch mirrors retryUpdate but persists the mutation with a
// MergeFromWithOptimisticLock patch instead of a full Update. The patch body
// only carries the fields the modifier changed plus the resourceVersion
// precondition, so callers backed by a field-stripping informer cache (the
// sandbox-gateway cache, see stripSandboxCacheFields) cannot erase stripped
// fields such as spec.embeddedSandboxTemplate on the API server.
//
// Returns the same contract as retryUpdate.
func (s *Sandbox) retryPatch(ctx context.Context, modifier ModifierFunc) (bool, error) {
	cr, updated, err := claim.RetryPatch(ctx, s.Cache, s.Sandbox, modifier)
	if err != nil {
		return false, err
	}
	s.Sandbox = cr
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox))
	if updated {
		log.Info("sandbox patched successfully")
	} else {
		log.Info("sandbox patch skipped")
	}
	return updated, nil
}

func (s *Sandbox) refreshFromAPIReader(ctx context.Context) error {
	latest, err := claim.RefreshFromAPIReader(ctx, s.Cache, s.Sandbox)
	if err != nil {
		return err
	}
	s.Sandbox = latest
	return nil
}

// Kill delegates to the neutral engine, whose DefaultDeleteSandbox hook tests
// override to intercept deletion.
func (s *Sandbox) Kill(ctx context.Context) error {
	return claim.Kill(ctx, s.Cache, s.Sandbox)
}

// GetTrafficAccessToken returns the transient access token minted during claim or clone
// for accessing this sandbox through the sandbox gateway. It is empty unless
// the sandbox opted in via AnnotationEnableJwtAuth.
func (s *Sandbox) GetTrafficAccessToken() string {
	if s.trafficToken == nil {
		return ""
	}
	return s.trafficToken.AccessToken
}

// GetTrafficAccessTokenExpiration returns the expiration time (RFC3339) of the
// transient traffic token minted during claim or clone. It is empty unless the sandbox
// opted in via AnnotationEnableJwtAuth.
func (s *Sandbox) GetTrafficAccessTokenExpiration() string {
	if s.trafficToken == nil {
		return ""
	}
	return s.trafficToken.AccessTokenExpiration
}

func setTimeout(sbx *agentsv1alpha1.Sandbox, opts timeout.Options) {
	claim.ApplyTimeout(sbx, opts)
}

func mergeExtraAnnotations(sbx *agentsv1alpha1.Sandbox, annotations map[string]string) {
	claim.MergeExtraAnnotations(sbx, annotations)
}

func (s *Sandbox) SetTimeout(opts timeout.Options) {
	setTimeout(s.Sandbox, opts)
}

// EnableWakeOnIngressTraffic arms the wake-on-ingress-traffic resume rule. A
// positive pauseTimeout becomes the rule's PauseTimeout so a traffic wake
// re-arms auto-pause with it; a non-positive value leaves PauseTimeout unset
// and the wake does not re-arm auto-pause.
func (s *Sandbox) EnableWakeOnIngressTraffic(pauseTimeout time.Duration) {
	claim.EnableWakeOnIngressTraffic(s.Sandbox, pauseTimeout)
}

// ClearWakeOnIngressTraffic removes the wake-on-ingress-traffic resume rule
// and prunes now-empty parent policy nodes so a pooled spec stays
// byte-identical to a fresh one.
func (s *Sandbox) ClearWakeOnIngressTraffic() {
	claim.ClearWakeOnIngressTraffic(s.Sandbox)
}

func (s *Sandbox) GetPodLabels() map[string]string { return claim.GetPodLabels(s.Sandbox) }

func (s *Sandbox) SetPodLabels(labels map[string]string) { claim.SetPodLabels(s.Sandbox, labels) }

func (s *Sandbox) GetPodAnnotations() map[string]string { return claim.GetPodAnnotations(s.Sandbox) }

func (s *Sandbox) SetPodAnnotations(annotations map[string]string) {
	claim.SetPodAnnotations(s.Sandbox, annotations)
}

// SetImage sets the image of the first container
func (s *Sandbox) SetImage(image string) { claim.SetImage(s.Sandbox, image) }

func (s *Sandbox) GetImage() string { return claim.GetImage(s.Sandbox) }

// SaveTimeoutWithPolicy updates timeout with given policy. Available timeout update policies:
//   - Always: overwrite timeout whenever the requested value differs from current.
//   - ExtendOnly: only extend to a later effective end time.
func (s *Sandbox) SaveTimeoutWithPolicy(ctx context.Context, opts infra.SaveTimeoutOptions, policy timeout.UpdatePolicy) (infra.TimeoutUpdateResult, error) {
	log := klog.FromContext(ctx).V(logs.DebugLogLevel).WithValues("sandbox", klog.KObj(s.Sandbox), "policy", policy)
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
	return claim.GetResource(s.Sandbox)
}

func (s *Sandbox) Request(ctx context.Context, method, path string, port int, body io.Reader) (*http.Response, error) {
	return proxyutils.DefaultRequestFunc(ctx, s.Sandbox, method, path, port, body)
}

func validatePause(sbx *agentsv1alpha1.Sandbox) error {
	if pausable, reason := utils.IsSandboxPausable(sbx); !pausable {
		return managererrors.NewError(managererrors.ErrorConflict, "sandbox is not pausable, reason: %s", reason)
	}
	return nil
}

func (s *Sandbox) Pause(ctx context.Context, opts infra.PauseOptions) (err error) {
	ctx, span := tracing.StartManagerSpan(ctx, tracing.SpanInfraPause)
	// Keep the closure: a direct defer tracing.EndSpan(ctx, span, err) would
	// evaluate err while still nil and record every failure as success.
	defer func() { tracing.EndSpan(ctx, span, err) }()
	log := klog.FromContext(ctx)
	if err := s.refreshFromAPIReader(ctx); err != nil {
		return err
	}
	if err := validatePause(s.Sandbox); err != nil {
		return err
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
		// A stale informer write conflicts on resourceVersion. Only a current
		// observation can supersede the admission decision from the API reader.
		if expectations.IsResourceVersionNewerOrEqual(s.ResourceVersion, sbx.ResourceVersion) {
			if err := validatePause(sbx); err != nil {
				return false, err
			}
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
	paused, err := pauseTask.WaitForObject(s.Sandbox, pauseWaitMaxTimeout)
	if err != nil {
		var conflict *cache.SandboxPauseConflictError
		if errors.As(err, &conflict) {
			err = managererrors.WrapError(managererrors.ErrorConflict, err, "%s", conflict)
		} else {
			err = sandboxWaitTimeoutError(err, "pause", sandboxid.Resolve(s.Sandbox))
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			log.Info("stop waiting sandbox pause: request canceled by client (disconnected or client-side timeout)",
				"err", err, "ctxErr", ctxErr)
			return err
		}
		log.Error(err, "failed to wait sandbox pause")
		return err
	}
	log.Info("sandbox paused", "cost", time.Since(start))
	// The waiter only accepts snapshots at or beyond our spec write.
	s.Sandbox = paused.DeepCopy()
	return nil
}

const postResumeOperationTimeout = 30 * time.Second

// pauseWaitMaxTimeout is a defensive upper bound for callers without a
// deadline. A PrePause hook adds up to its timeout (60 seconds by default)
// to the pause wait, so the previous one-minute limit could expire before
// an accepted pause completed. Request context cancellation remains the normal
// deadline for API callers.
const pauseWaitMaxTimeout = 10 * time.Minute

// resumeWaitMaxTimeout is a defensive upper bound for resumeTask.Wait. The real
// timeout is expected to come from the request ctx; this value only guards
// callers that pass a ctx without a deadline so Resume cannot block forever.
const resumeWaitMaxTimeout = 10 * time.Minute

func (s *Sandbox) Resume(ctx context.Context, opts infra.ResumeOptions) (err error) {
	ctx, span := tracing.StartManagerSpan(ctx, tracing.SpanInfraResume)
	// Keep the closure: a direct defer tracing.EndSpan(ctx, span, err) would
	// evaluate err while still nil and record every failure as success.
	defer func() { tracing.EndSpan(ctx, span, err) }()
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox))

	if err := s.refreshFromAPIReader(ctx); err != nil {
		return err
	}

	if resumable, reason := utils.IsSandboxResumable(s.Sandbox); !resumable {
		return managererrors.NewError(managererrors.ErrorConflict, "sandbox is not resumable, reason: %s", reason)
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
	resumeModifier := func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
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
	}
	resumeUpdated, err := s.retryPatch(ctx, resumeModifier)
	if err != nil {
		log.Error(err, "failed to update sandbox spec.paused")
		return err
	}
	log.Info("waiting sandbox resume")
	start := time.Now()
	if _, err = resumeTask.WaitForObject(s.Sandbox, resumeWaitMaxTimeout); err != nil {
		err = sandboxWaitTimeoutError(err, "resume", sandboxid.Resolve(s.Sandbox))
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
	// The request may expire after the waiter has already observed readiness.
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

func (s *Sandbox) CreateCheckpoint(ctx context.Context, opts infra.CreateCheckpointOptions) (checkpointID string, err error) {
	// Apply defaults before recording span attributes so the recorded timeout
	// reflects the effective value instead of 0 when the caller leaves it unset.
	opts = ValidateAndInitCheckpointOptions(opts)
	ctx, span := tracing.StartManagerSpan(ctx, tracing.SpanInfraCreateCheckpoint,
		attribute.Float64(tracing.AttrCheckpointDuration, opts.WaitSuccessTimeout.Seconds()),
	)
	// Keep the closure: a direct defer tracing.EndSpan(ctx, span, err) would
	// evaluate err while still nil and record every failure as success.
	defer func() { tracing.EndSpan(ctx, span, err) }()
	log := klog.FromContext(ctx)
	log.Info("create checkpoint options", "options", opts)
	return CreateCheckpoint(ctx, s.Sandbox, s.Cache, opts)
}

var _ infra.Sandbox = &Sandbox{}
var _ claim.Handle = &Sandbox{}

func AsSandbox(sbx *agentsv1alpha1.Sandbox, cache cache.Provider) *Sandbox {
	return &Sandbox{
		Cache:           cache,
		Sandbox:         sbx,
		storageRegistry: storages.NewStorageProvider(),
	}
}
