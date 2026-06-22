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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/utils"
	csimountutils "github.com/openkruise/agents/pkg/utils/csiutils"
	utilruntime "github.com/openkruise/agents/pkg/utils/runtime"
)

// defaultSandboxInitializer wraps the package-level Initialize function to implement SandboxInitializer.
type defaultSandboxInitializer struct {
	client          client.Client
	apiReader       client.Reader
	storageRegistry storages.VolumeMountProviderRegistry
	recorder        record.EventRecorder
}

func (d *defaultSandboxInitializer) Initialize(ctx context.Context, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus) error {
	if err := Initialize(ctx, box, newStatus, d.client, d.apiReader, d.storageRegistry); err != nil {
		klog.ErrorS(err, "post-resume/upgrade initialization failed", "sandbox", klog.KObj(box))
		d.recorder.Event(box, corev1.EventTypeWarning, string(agentsv1alpha1.RuntimeInitialized),
			fmt.Sprintf("Failed to perform initialization: %v", err))
		utils.SetSandboxCondition(newStatus, metav1.Condition{
			Type:               string(agentsv1alpha1.RuntimeInitialized),
			Status:             metav1.ConditionFalse,
			Reason:             agentsv1alpha1.SandboxConditionRuntimeInitReasonFailed,
			Message:            utils.TruncateConditionMessage(fmt.Sprintf("Runtime initialization failed: %v", err)),
			LastTransitionTime: metav1.Now(),
		})
		return err
	}
	d.recorder.Event(box, corev1.EventTypeNormal, string(agentsv1alpha1.RuntimeInitialized),
		"Initialization completed successfully")
	utils.SetSandboxCondition(newStatus, metav1.Condition{
		Type:               string(agentsv1alpha1.RuntimeInitialized),
		Status:             metav1.ConditionTrue,
		Reason:             agentsv1alpha1.SandboxConditionRuntimeInitReasonSucceeded,
		Message:            "Runtime initialization completed",
		LastTransitionTime: metav1.Now(),
	})
	return nil
}

// Initialize performs post-recreation initialization for a sandbox.
// It sequentially executes:
//  1. Re-init runtime (if initRuntimeRequest annotation is set)
//  2. Re-mount CSI storage concurrently (if CSI mount annotations are set)
//
// This is the unified initialization logic for all sandboxes after resume or recreate upgrade.
// Both E2B and SandboxClaim paths rely on this to re-initialize runtime and CSI mounts.
func Initialize(ctx context.Context, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus,
	client client.Client, apiReader client.Reader, storageRegistry storages.VolumeMountProviderRegistry) error {
	if client == nil || apiReader == nil {
		return nil
	}
	logger := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(box))

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
		csiMountHandler := csimountutils.NewCSIMountHandler(client, apiReader, storageRegistry, utils.DefaultSandboxDeployNamespace)

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
