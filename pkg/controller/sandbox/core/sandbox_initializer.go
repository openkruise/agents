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
	"fmt"

	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
	csimountutils "github.com/openkruise/agents/pkg/utils/csiutils"
	utilruntime "github.com/openkruise/agents/pkg/utils/runtime"
)

// defaultSandboxInitializer wraps the package-level Initialize function to implement SandboxInitializer.
type defaultSandboxInitializer struct {
	sandboxClient   *clients.ClientSet
	cache           infra.CacheProvider
	storageRegistry storages.VolumeMountProviderRegistry
}

func (d *defaultSandboxInitializer) Initialize(ctx context.Context, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus) error {
	return Initialize(ctx, box, newStatus, d.sandboxClient, d.cache, d.storageRegistry)
}

// Initialize performs post-recreation initialization for a sandbox.
// It sequentially executes:
//  1. Re-init runtime (if initRuntimeRequest annotation is set)
//  2. Re-mount CSI storage concurrently (if CSI mount annotations are set)
//
// The controller only handles initialization for sandboxes claimed by a SandboxClaim
// (identified by the claim-name label). Non-claimed sandboxes are handled by E2B's Resume flow.
func Initialize(
	ctx context.Context,
	box *agentsv1alpha1.Sandbox,
	newStatus *agentsv1alpha1.SandboxStatus,
	sandboxClient *clients.ClientSet,
	cache infra.CacheProvider,
	storageRegistry storages.VolumeMountProviderRegistry,
) error {
	if sandboxClient == nil || cache == nil {
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

	// build a lightweight sandbox object with the latest status for runtime operations
	sbxForInit := &agentsv1alpha1.Sandbox{
		ObjectMeta: box.ObjectMeta,
		Status:     *newStatus,
	}

	// Re-init runtime
	// TODO: check whether agent-runtime is available in sandbox, if not, we should just return and skip the initialization
	if err := reinitRuntime(ctx, logger, box, sbxForInit); err != nil {
		return err
	}

	// Re-mount CSI storage (concurrent)
	csiMountConfigRequests, err := utilruntime.GetCsiMountExtensionRequest(box)
	if err != nil {
		logger.Error(err, "failed to get csi mount request")
		return fmt.Errorf("failed to get csi mount request: %w", err)
	}

	if len(csiMountConfigRequests) != 0 {
		logger.Info("will re-mount csi storage after resume or upgrade", "count", len(csiMountConfigRequests))
		csiMountHandler := csimountutils.NewCSIMountHandler(sandboxClient, cache, storageRegistry, utils.DefaultSandboxDeployNamespace)

		// Resolve all CSIMountConfig annotations into MountConfig (driver + requestRaw)
		var mountOptionList []config.MountConfig
		for _, req := range csiMountConfigRequests {
			driverName, csiReqConfigRaw, genErr := csiMountHandler.CSIMountOptionsConfig(ctx, req)
			if genErr != nil {
				return fmt.Errorf("failed to generate csi mount options config for sandbox, err: %v", genErr)
			}
			mountOptionList = append(mountOptionList, config.MountConfig{
				Driver:     driverName,
				RequestRaw: csiReqConfigRaw,
			})
		}

		// Reuse ProcessCSIMounts for concurrent mount execution
		duration, mountErr := utilruntime.ProcessCSIMounts(ctx, sbxForInit, config.CSIMountOptions{
			MountOptionList: mountOptionList,
		})
		if mountErr != nil {
			return fmt.Errorf("failed to perform ReCSIMount after resume: %w", mountErr)
		}
		logger.Info("ReCSIMount completed after resume or upgrade", "costTime", duration)
	}
	return nil
}

// reinitRuntime performs runtime re-initialization after pod recreation.
// This is the common runtime re-initialization logic used by Initialize.
func reinitRuntime(ctx context.Context, logger klog.Logger, box *agentsv1alpha1.Sandbox, sbxForInit *agentsv1alpha1.Sandbox) error {
	logger.Info("start to decode init runtime request...")
	initRuntimeOpts, err := utilruntime.GetInitRuntimeRequest(box)
	if err != nil {
		logger.Error(err, "failed to get init runtime request")
		return err
	}
	if initRuntimeOpts != nil {
		initRuntimeOpts.SkipRefresh = true
		logger.Info("will re-init runtime after resume")
		if _, err = utilruntime.InitRuntime(ctx, sbxForInit, *initRuntimeOpts, nil); err != nil {
			logger.Error(err, "failed to perform ReInit after resume")
			return err
		}
		logger.Info("re-init completed after resume")
	}
	return nil
}
