/*
Copyright 2025.

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

	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
)

// defaultSandboxReinitializer performs basic post-recreation initialization.
// It only re-initializes runtime. CSI re-mount is not supported in community deployments.
type defaultSandboxReinitializer struct {
	sandboxClient *clients.ClientSet
	cache         *sandboxcr.Cache
}

// Reinitialize performs post-recreation initialization for a sandbox.
// It sequentially executes:
//  1. Re-init runtime (if initRuntimeRequest annotation is set)
//  2. TODO Re-mount CSI storage concurrently (if CSI mount annotations are set)
//
// The controller only handles initialization for sandboxes claimed by a SandboxClaim
// (identified by the claim-name label). Non-claimed sandboxes are handled by E2B's Resume flow.
// Future post-recreation operations should be added here.
func (d *defaultSandboxReinitializer) Reinitialize(
	ctx context.Context,
	box *agentsv1alpha1.Sandbox,
	newStatus *agentsv1alpha1.SandboxStatus,
) error {
	if d.sandboxClient == nil || d.cache == nil {
		return nil
	}
	logger := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(box))

	// Only handle initialization for sandboxes claimed by a SandboxClaim.
	// Non-claimed sandboxes are managed by E2B's Resume flow which handles runtime re-init
	// and CSI re-mount independently.
	if box.Labels[agentsv1alpha1.LabelSandboxClaimName] == "" {
		logger.Info("sandbox is not claimed by SandboxClaim, skipping controller re-initialization")
		return nil
	}

	runtimeSandbox := sandboxcr.AsSandbox(&agentsv1alpha1.Sandbox{
		ObjectMeta: box.ObjectMeta,
		Status:     *newStatus,
	}, d.cache, d.sandboxClient)
	return reinitRuntime(ctx, logger, box, runtimeSandbox)
}

// reinitRuntime performs runtime re-initialization after pod recreation.
// This is shared by both internal (sandboxInfra) and community (defaultSandboxReinitializer) implementations.
func reinitRuntime(ctx context.Context, logger klog.Logger, box *agentsv1alpha1.Sandbox, runtimeSandbox *sandboxcr.Sandbox) error {
	logger.Info("start to decode init runtime request...")
	initRuntimeOpts, err := sandboxcr.GetInitRuntimeRequest(box)
	if err != nil {
		logger.Error(err, "failed to get init runtime request")
		return err
	}
	if initRuntimeOpts != nil {
		initRuntimeOpts.SkipRefresh = true
		logger.Info("will re-init runtime after resume")
		if _, err = sandboxcr.InitRuntime(ctx, runtimeSandbox, *initRuntimeOpts); err != nil {
			logger.Error(err, "failed to perform ReInit after resume")
			return err
		}
		logger.Info("re-init completed after resume")
	}
	return nil
}
