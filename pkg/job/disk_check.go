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

package job

import (
	"context"
	"fmt"
	"syscall"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
)

// CheckDiskSpace checks whether there is sufficient disk space on the containerd root
// filesystem before performing a commit. It estimates the required space by querying
// the container's writable layer size via the CRI API and multiplying by the configured
// safety factor. Returns ExitCodeSuccess (0) to proceed, ExitCodeDiskSpaceCheckFailed (6)
// to abort. On any estimation error, it logs a warning and returns ExitCodeSuccess to
// avoid blocking the commit.
func CheckDiskSpace(ctx context.Context, containerID string) int {
	if !Config().DiskSpaceCheckEnabled() {
		return ExitCodeSuccess
	}

	klog.InfoS("Disk space check enabled, starting pre-commit check", "containerID", containerID)

	writableLayerSize, err := getWritableLayerSize(ctx, containerID)
	if err != nil {
		klog.InfoS("WARNING: Failed to estimate writable layer size, skipping disk space check",
			"containerID", containerID, "err", err)
		return ExitCodeSuccess
	}

	safetyFactor := Config().DiskSpaceSafetyFactor()
	requiredBytes := int64(float64(writableLayerSize) * safetyFactor)

	containerdRootPath := Config().ContainerdRootPath()
	availableBytes, err := getAvailableBytes(containerdRootPath)
	if err != nil {
		klog.InfoS("WARNING: Failed to get available disk space, skipping disk space check",
			"path", containerdRootPath, "err", err)
		return ExitCodeSuccess
	}

	klog.InfoS("Disk space check result",
		"containerID", containerID,
		"writableLayerSize", writableLayerSize,
		"safetyFactor", safetyFactor,
		"requiredBytes", requiredBytes,
		"availableBytes", availableBytes,
		"containerdRootPath", containerdRootPath,
	)

	if availableBytes < requiredBytes {
		klog.ErrorS(nil, "Disk space insufficient for commit",
			"availableBytes", availableBytes,
			"requiredBytes", requiredBytes,
		)
		return ExitCodeDiskSpaceCheckFailed
	}

	klog.InfoS("Disk space check passed")
	return ExitCodeSuccess
}

func getWritableLayerSize(ctx context.Context, containerID string) (int64, error) {
	client, err := NewCRIClient(Config().ContainerdSock())
	if err != nil {
		return 0, fmt.Errorf("connect to containerd CRI: %w", err)
	}
	defer client.Close()

	resp, err := client.runtimeClient.ContainerStats(ctx, &runtimeapi.ContainerStatsRequest{
		ContainerId: containerID,
	})
	if err != nil {
		return 0, fmt.Errorf("get container stats for %s: %w", containerID, err)
	}

	stats := resp.GetStats()
	if stats == nil {
		return 0, fmt.Errorf("container stats response is nil for %s", containerID)
	}

	writableLayer := stats.GetWritableLayer()
	if writableLayer == nil {
		return 0, fmt.Errorf("writable layer stats not available for %s", containerID)
	}

	usedBytes := writableLayer.GetUsedBytes()
	if usedBytes == nil {
		return 0, fmt.Errorf("writable layer used_bytes not available for %s", containerID)
	}

	return int64(usedBytes.GetValue()), nil
}

func getAvailableBytes(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}
