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

package core

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	csimountutils "github.com/openkruise/agents/pkg/utils/csiutils"
	"github.com/openkruise/agents/pkg/utils/logs"
	utilruntime "github.com/openkruise/agents/pkg/utils/runtime"
	"github.com/openkruise/agents/pkg/utils/runtime/config"
)

const (
	// csiUnmountMaxAttempts bounds the best-effort unmount retries on the
	// delete path before the pod deletion proceeds anyway.
	csiUnmountMaxAttempts = 3
	// csiUnmountRetryDelay is the pause between unmount attempts.
	csiUnmountRetryDelay = time.Second
	// eventReasonCSIUnmountFailed is emitted when dynamic CSI mounts could not
	// be released before pod deletion.
	eventReasonCSIUnmountFailed = "CSIUnmountFailed"
)

// CSIUnmounter is an optional capability of a SandboxInitializer: releasing
// the sandbox's dynamic CSI mounts while the runtime is still reachable, i.e.
// strictly before pod deletion. Callers must tolerate its absence — the
// cleanup is best-effort and must never block deletion.
type CSIUnmounter interface {
	UnmountCSIVolumes(ctx context.Context, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus)
}

// UnmountCSIVolumesBeforePodDeletion is the single entry point every pod
// deletion path must call right before issuing the delete: terminal deletion
// (EnsureSandboxTerminated), pause (Stop and Checkpoint strategies) and
// recreate upgrade. Without it the FUSE daemon dies with the pod and the
// mount propagated to the host stays behind as a stale entry.
//
// It is a no-op when the pod is already gone or being deleted (the runtime is
// no longer reachable, and the vendor prestop path owns the cleanup then), and
// when the initializer does not carry the CSIUnmounter capability (mocks in
// tests).
func UnmountCSIVolumesBeforePodDeletion(ctx context.Context, initializer SandboxInitializer,
	box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus, pod *corev1.Pod) {
	if pod == nil || !pod.DeletionTimestamp.IsZero() {
		return
	}
	if unmounter, ok := initializer.(CSIUnmounter); ok {
		unmounter.UnmountCSIVolumes(ctx, box, newStatus)
	}
}

// UnmountCSIVolumes best-effort unmounts all dynamic CSI mounts declared on the
// sandbox. It never returns an error: after bounded retries a Warning event is
// recorded and pod deletion proceeds regardless (best-effort policy — a stale
// mount that can be cleaned up manually is preferable to a sandbox stuck in
// deletion).
//
// Failures that make the unmount pointless (unparsable annotation, publish
// request generation failure, unreachable transport) also only log and record
// an event; none of them may block the deletion flow.
//
// It reuses the initializer's own dependencies (client, API reader, storage
// registry, TLS bundle and event recorder) — the same objects that resolved
// the mount in the first place, so the unmount consumes the identical publish
// request the mount was issued with.
func (d *defaultSandboxInitializer) UnmountCSIVolumes(ctx context.Context, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(box))

	// No dynamic mounts declared — nothing to do, and no log noise.
	csiMountConfigs, err := utilruntime.GetCsiMountExtensionRequest(box)
	if err != nil {
		log.Error(err, "failed to parse csi mount config annotation, skipping unmount")
		d.recorder.Event(box, corev1.EventTypeWarning, eventReasonCSIUnmountFailed,
			fmt.Sprintf("failed to parse csi mount config, skipping unmount: %s", logs.SanitizeValue(err.Error())))
		return
	}
	if len(csiMountConfigs) == 0 {
		return
	}

	// Resolve the mount annotations back into publish requests, exactly the way
	// the mount path does, then convert each into its unmount counterpart so
	// that what travels to the sandbox-storage CLI is a NodeUnpublishVolume
	// request addressed by volume id and container mount path.
	csiMountHandler := csimountutils.NewCSIMountHandler(d.client, d.apiReader, d.storageRegistry, utils.DefaultSandboxDeployNamespace)
	unmountOptionList := make([]config.UnmountConfig, 0, len(csiMountConfigs))
	for _, cfg := range csiMountConfigs {
		driverName, publishRequest, genErr := csiMountHandler.GenerateNodePublishVolumeRequest(ctx, cfg)
		if genErr != nil {
			log.Error(genErr, "failed to generate csi publish request for unmount, skipping unmount", "mountConfig", cfg)
			d.recorder.Event(box, corev1.EventTypeWarning, eventReasonCSIUnmountFailed,
				fmt.Sprintf("failed to generate csi publish request for unmount, skipping unmount: %s", logs.SanitizeValue(genErr.Error())))
			return
		}
		unmountOptionList = append(unmountOptionList, *config.MountConfig{
			Driver:         driverName,
			PublishRequest: publishRequest,
		}.ToUnmountConfig())
	}

	// Build a lightweight sandbox object with the latest status so
	// Status.PodInfo.PodUID is populated for the POD_UID env of the unmount CLI.
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: box.ObjectMeta,
		Status:     *newStatus,
	}

	rtOpts, err := utilruntime.TransportOptionsFor(sbx, d.tlsBundle)
	if err != nil {
		log.Error(err, "failed to resolve runtime transport options, skipping unmount")
		d.recorder.Event(box, corev1.EventTypeWarning, eventReasonCSIUnmountFailed,
			fmt.Sprintf("failed to resolve runtime transport options, skipping unmount: %s", logs.SanitizeValue(err.Error())))
		return
	}

	var lastErr error
	for attempt := 1; attempt <= csiUnmountMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return
		}
		_, lastErr = utilruntime.ProcessCSIUnmounts(ctx, sbx, config.CSIUnmountOptions{UnmountOptionList: unmountOptionList}, rtOpts...)
		if lastErr == nil {
			log.Info("Unmounted dynamic CSI volumes before pod deletion", "count", len(unmountOptionList), "attempt", attempt)
			return
		}
		if attempt < csiUnmountMaxAttempts {
			log.Info("Failed to unmount dynamic CSI volumes, will retry", "attempt", attempt, "error", lastErr)
			select {
			case <-ctx.Done():
				return
			case <-time.After(csiUnmountRetryDelay):
			}
		}
	}

	// Bounded retries exhausted: surface the failure and let pod deletion
	// proceed. The leftover mount (if any) has to be cleaned up manually.
	log.Error(lastErr, "csi unmount failed after retries, proceeding with pod deletion",
		"attempts", csiUnmountMaxAttempts)
	d.recorder.Event(box, corev1.EventTypeWarning, eventReasonCSIUnmountFailed,
		fmt.Sprintf("failed to unmount %d dynamic CSI volume(s) after %d attempts, proceeding with pod deletion: %s",
			len(unmountOptionList), csiUnmountMaxAttempts, logs.SanitizeValue(lastErr.Error())))
}
