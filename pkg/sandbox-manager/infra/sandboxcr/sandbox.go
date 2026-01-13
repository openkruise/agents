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
	"net/http"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	sandboxclient "github.com/openkruise/agents/client/clientset/versioned"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager/proxyutils"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
	"github.com/openkruise/agents/proto/envd/process"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

type SandboxCR interface {
	*agentsv1alpha1.Sandbox
	metav1.Object
}

type PatchFunc[T SandboxCR] func(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subResources ...string) (result T, err error)
type UpdateFunc[T SandboxCR] func(ctx context.Context, sbx T, opts metav1.UpdateOptions) (T, error)
type DeleteFunc func(ctx context.Context, name string, opts metav1.DeleteOptions) error
type ModifierFunc[T SandboxCR] func(sbx T)
type SetConditionFunc[T SandboxCR] func(sbx T, tp string, status metav1.ConditionStatus, reason, message string)
type GetConditionsFunc[T SandboxCR] func(sbx T) []metav1.Condition
type DeepCopyFunc[T any] func(src T) T

type BaseSandbox[T SandboxCR] struct {
	Sandbox T
	Cache   *Cache
	Client  clients.SandboxClient

	PatchSandbox  PatchFunc[T]
	UpdateStatus  UpdateFunc[T]
	Update        UpdateFunc[T]
	DeleteFunc    DeleteFunc
	SetCondition  SetConditionFunc[T]
	GetConditions GetConditionsFunc[T]
	DeepCopy      DeepCopyFunc[T]
}

func (s *BaseSandbox[T]) GetTemplate() string {
	return s.Sandbox.GetLabels()[agentsv1alpha1.LabelSandboxPool]
}

func (s *BaseSandbox[T]) InplaceRefresh(ctx context.Context, deepcopy bool) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox)).V(consts.DebugLogLevel)
	sbx, err := s.Cache.GetSandbox(stateutils.GetSandboxID(s.Sandbox))
	if err != nil {
		return err
	}
	if !utils.ResourceVersionExpectationSatisfied(sbx) {
		log.Info("sandbox cache is out-dated, fetch from api-server")
		sbx, err = s.Client.ApiV1alpha1().Sandboxes(s.Sandbox.GetNamespace()).Get(ctx, s.Sandbox.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}
	}
	if deepcopy {
		s.Sandbox = s.DeepCopy(sbx)
	} else {
		s.Sandbox = sbx
	}
	return nil
}

func (s *BaseSandbox[T]) retryUpdate(ctx context.Context, updateFunc UpdateFunc[T], modifier func(sbx T)) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox))
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// get the latest sandbox
		sbx, err := s.Cache.GetSandbox(stateutils.GetSandboxID(s.Sandbox))
		if err != nil {
			return err
		}
		copied := sbx.DeepCopy()
		modifier(copied)
		updated, err := updateFunc(ctx, copied, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		s.Sandbox = updated
		utils.ResourceVersionExpectationExpect(updated)
		return nil
	})
	if err != nil {
		log.Error(err, "failed to update sandbox after retries")
	} else {
		log.Info("sandbox updated successfully")
	}
	return err
}

func (s *BaseSandbox[T]) Kill(ctx context.Context) error {
	if s.Sandbox.GetDeletionTimestamp() != nil {
		return nil
	}
	return s.DeleteFunc(ctx, s.Sandbox.GetName(), metav1.DeleteOptions{})
}

type Sandbox struct {
	BaseSandbox[*agentsv1alpha1.Sandbox]
	*agentsv1alpha1.Sandbox
}

func (s *Sandbox) GetSandboxID() string {
	return stateutils.GetSandboxID(s.Sandbox)
}

func (s *Sandbox) GetRoute() proxy.Route {
	state, _ := s.GetState()
	return proxy.Route{
		IP:    s.Status.PodInfo.PodIP,
		ID:    s.GetSandboxID(),
		Owner: s.GetAnnotations()[agentsv1alpha1.AnnotationOwner],
		State: state,
	}
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
	return s.retryUpdate(ctx, s.Update, func(sbx *agentsv1alpha1.Sandbox) {
		setTimeout(sbx, opts)
	})
}

func (s *Sandbox) GetTimeout() infra.TimeoutOptions {
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
	return utils.CalculateResourceFromContainers(s.Spec.Template.Spec.Containers)
}

func (s *Sandbox) Request(r *http.Request, path string, port int) (*http.Response, error) {
	if s.Status.Phase != agentsv1alpha1.SandboxRunning {
		return nil, errors.New("sandbox is not running")
	}
	return proxyutils.ProxyRequest(r, path, port, s.Status.PodInfo.PodIP)
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
	err := s.retryUpdate(ctx, s.Update, func(sbx *agentsv1alpha1.Sandbox) {
		sbx.Spec.Paused = true
		if opts.Timeout != nil {
			setTimeout(sbx, *opts.Timeout)
		}
	})
	if err != nil {
		log.Error(err, "failed to update sandbox spec.paused")
		return err
	}
	utils.ResourceVersionExpectationExpect(s.Sandbox)
	return nil
}

func (s *Sandbox) Resume(ctx context.Context) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox))
	state, reason := s.GetState()
	log.Info("try to resume sandbox", "state", state, "reason", reason)
	if state != agentsv1alpha1.SandboxStatePaused {
		err := fmt.Errorf("resuming is only available for paused state, current state: %s", state)
		log.Error(err, "sandbox is not paused", "state", state, "reason", reason)
		return err
	}
	cond := GetSandboxCondition(s.Sandbox, agentsv1alpha1.SandboxConditionPaused)
	if cond.Status == metav1.ConditionFalse {
		return fmt.Errorf("sandbox is pausing, please wait a moment and try again")
	}
	if s.Sandbox.Spec.Paused {
		if err := s.retryUpdate(ctx, s.Update, func(sbx *agentsv1alpha1.Sandbox) {
			sbx.Spec.Paused = false
			setTimeout(sbx, infra.TimeoutOptions{}) // remove all timeout options
		}); err != nil {
			log.Error(err, "failed to update sandbox spec.paused")
			return err
		}
	}
	utils.ResourceVersionExpectationExpect(s.Sandbox) // expect Resuming
	log.Info("waiting sandbox resume")
	start := time.Now()
	err := s.Cache.WaitForSandboxSatisfied(ctx, s.Sandbox, WaitActionResume, func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
		state, reason := stateutils.GetSandboxState(sbx)
		log.V(consts.DebugLogLevel).Info("checking sandbox state", "state", state, "reason", reason)
		return state == agentsv1alpha1.SandboxStateRunning, nil
	}, time.Minute)
	if err != nil {
		log.Error(err, "failed to wait sandbox resume")
		return err
	}
	log.Info("sandbox resumed", "cost", time.Since(start))
	return s.InplaceRefresh(ctx, false)
}

func (s *Sandbox) InplaceRefresh(ctx context.Context, deepcopy bool) error {
	err := s.BaseSandbox.InplaceRefresh(ctx, deepcopy)
	if err != nil {
		return err
	}
	s.Sandbox = s.BaseSandbox.Sandbox
	return nil
}

func (s *Sandbox) GetState() (string, string) {
	return stateutils.GetSandboxState(s.Sandbox)
}

func (s *Sandbox) GetClaimTime() (time.Time, error) {
	claimTimestamp := s.GetAnnotations()[agentsv1alpha1.AnnotationClaimTime]
	return time.Parse(time.RFC3339, claimTimestamp)
}

var MountCommand = "/mnt/envd/sandbox-runtime-storage"

// CSIMount creates a dynamic mount point in Sandbox with `sandbox-storage` cli
//
// NOTE: `sandbox-storage` cli should be injected with `sandbox-runtime` and will be replaced by a built-in service of
// `sandbox-runtime`.
func (s *Sandbox) CSIMount(ctx context.Context, driver string, request string) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox))
	processConfig := &process.ProcessConfig{
		Cmd: MountCommand,
		Args: []string{
			"mount",
			"--driver", driver,
			"--config", request,
		},
		Cwd: nil,
		Envs: map[string]string{
			"POD_UID": string(s.Status.PodInfo.PodUID),
		},
	}
	result, err := s.runCommandWithEnvd(ctx, processConfig, 5*time.Second)
	if err != nil {
		log.Error(err, "failed to run command")
		return err
	}
	if result.ExitCode != 0 {
		err = fmt.Errorf("command failed: [%d] %s", result.ExitCode, result.Stderr)
		log.Error(err, "command failed", "exitCode", result.ExitCode)
		return err
	}
	return nil
}

func DeepCopy(sbx *agentsv1alpha1.Sandbox) *agentsv1alpha1.Sandbox {
	return sbx.DeepCopy()
}

var _ infra.Sandbox = &Sandbox{}

func AsSandbox(sbx *agentsv1alpha1.Sandbox, cache *Cache, client sandboxclient.Interface) *Sandbox {
	return &Sandbox{
		BaseSandbox: BaseSandbox[*agentsv1alpha1.Sandbox]{
			Sandbox:       sbx,
			Cache:         cache,
			Client:        client,
			PatchSandbox:  client.ApiV1alpha1().Sandboxes(sbx.Namespace).Patch,
			UpdateStatus:  client.ApiV1alpha1().Sandboxes(sbx.Namespace).UpdateStatus,
			Update:        client.ApiV1alpha1().Sandboxes(sbx.Namespace).Update,
			DeleteFunc:    client.ApiV1alpha1().Sandboxes(sbx.Namespace).Delete,
			SetCondition:  SetSandboxCondition,
			GetConditions: ListSandboxConditions,
			DeepCopy:      DeepCopy,
		},
		Sandbox: sbx,
	}
}
