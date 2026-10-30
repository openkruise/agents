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

package sandboxcr

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/tracing"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

// submitPauseResume persists lifecycle intent without owning its completion.
// Reads stay informer-backed; even a no-op uses an optimistic patch to validate
// its observed resource version on the server before acknowledging the intent.
func (s *Sandbox) submitPauseResume(ctx context.Context, paused bool, timeoutOpts *timeout.Options, annotations map[string]string) error {
	key := client.ObjectKeyFromObject(s.Sandbox)
	var attemptErr error
	err := retry.RetryOnConflict(retry.DefaultRetry, func() (err error) {
		defer func() { attemptErr = err }()
		if err := ctx.Err(); err != nil {
			return err
		}
		latest := &agentsv1alpha1.Sandbox{}
		if err := s.Cache.GetClient().Get(ctx, key, latest); err != nil {
			return err
		}
		// A previous acknowledged write may be ahead of the informer.
		if expectations.IsResourceVersionNewer(latest.ResourceVersion, s.ResourceVersion) {
			latest = s.Sandbox
		}
		transitionErr := pauseResumeTransitionError(latest, paused)
		if transitionErr == nil {
			// Keep the native protection against an opposite synchronous waiter,
			// but release it on every exit since this path never calls Wait.
			if paused {
				task, err := s.Cache.NewSandboxPauseTask(ctx, latest)
				if err != nil {
					return err
				}
				defer task.Release()
			} else {
				task, err := s.Cache.NewSandboxResumeTask(ctx, latest)
				if err != nil {
					return err
				}
				defer task.Release()
			}
		}
		copied := latest.DeepCopy()
		changed := transitionErr == nil && latest.Spec.Paused != paused
		if changed {
			copied.Spec.Paused = paused
			if timeoutOpts != nil {
				setTimeout(copied, *timeoutOpts)
			}
			if paused {
				mergeExtraAnnotations(copied, annotations)
			}
			copied.Annotations = tracing.InjectTraceContext(ctx, copied.Annotations)
		}
		// An unchanged patch carries only the resourceVersion precondition.
		// Validate rejected transitions too: a stale phase must not cause a
		// false conflict when the controller has already completed the action.
		patch := client.MergeFromWithOptions(latest, client.MergeFromWithOptimisticLock{})
		if err := s.Cache.GetClient().Patch(ctx, copied, patch); err != nil {
			return err
		}
		s.Sandbox = copied
		if changed {
			expectations.ResourceVersionExpectationExpect(copied)
		}
		return transitionErr
	})
	if err == nil && attemptErr != nil {
		// RetryOnConflict can replace an interrupted first attempt with nil.
		err = attemptErr
	}
	if apierrors.IsConflict(err) {
		return errors.WrapError(errors.ErrorConflict, err, "sandbox changed while submitting paused=%t", paused)
	}
	if err != nil {
		return fmt.Errorf("submit sandbox paused=%t: %w", paused, err)
	}
	klog.FromContext(ctx).Info("sandbox lifecycle intent accepted", "sandbox", klog.KObj(s.Sandbox), "paused", paused)
	return nil
}

func pauseResumeTransitionError(sbx *agentsv1alpha1.Sandbox, paused bool) error {
	if sbx.DeletionTimestamp != nil || (sbx.Spec.ShutdownTime != nil && time.Now().After(sbx.Spec.ShutdownTime.Time)) {
		return errors.NewError(errors.ErrorConflict, "sandbox is terminating or its shutdown time has elapsed")
	}
	if paused {
		// The controller may still report Paused after accepting resume intent.
		// Reversing it here would overwrite a winning operation across Managers.
		if sbx.Status.Phase == agentsv1alpha1.SandboxPaused && !sbx.Spec.Paused {
			return errors.NewError(errors.ErrorConflict, "sandbox is resuming")
		}
		if ok, reason := utils.IsSandboxPausable(sbx); !ok {
			return errors.NewError(errors.ErrorConflict, "sandbox is not pausable, reason: %s", reason)
		}
	} else if ok, reason := utils.IsSandboxResumable(sbx); !ok {
		return errors.NewError(errors.ErrorConflict, "sandbox is not resumable, reason: %s", reason)
	}
	return nil
}

// SubmitPause persists pause intent using informer observations and native coordination.
func (s *Sandbox) SubmitPause(ctx context.Context, opts infra.PauseOptions) error {
	return s.submitPauseResume(ctx, true, opts.Timeout, opts.ExtraAnnotations)
}

// SubmitResume persists resume intent using informer observations and native coordination.
func (s *Sandbox) SubmitResume(ctx context.Context, opts infra.ResumeOptions) error {
	return s.submitPauseResume(ctx, false, opts.Timeout, nil)
}
