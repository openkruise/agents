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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/klog/v2"
)

// DoCommit is the main entry point for the commit-job binary.
// It performs: setup registry auth → nerdctl commit → nerdctl push.
func DoCommit(ctx context.Context) int {
	containerID := Config().ContainerID()
	image := Config().CommitImage()

	klog.InfoS("Start commit", "containerID", containerID, "image", image)

	// Dry-run mode: only check prerequisites, then exit
	if Config().DryRun() {
		klog.InfoS("Dry-run mode: prerequisites check passed, exiting without commit")
		return ExitCodeSuccess
	}

	// 1. Setup registry authentication
	if err := setupRegistryAuth(); err != nil {
		klog.ErrorS(err, "Failed to setup registry authentication, push may fail")
	}

	// 2. nerdctl commit
	start := time.Now()
	if err := NerdctlExec(ctx, WithArgs("commit", containerID, image)); err != nil {
		klog.ErrorS(err, "Commit failed", "containerID", containerID, "image", image)
		return ExitCodeCommitFailed
	}
	klog.InfoS("Commit succeeded", "elapsed", time.Since(start))

	// 3. Get image size info (non-fatal, for logging only)
	stdoutBuf := &bytes.Buffer{}
	if err := NerdctlExec(ctx, WithArgs("images", "--format", "json"), WithStdout(stdoutBuf)); err != nil {
		klog.ErrorS(err, "Get image size failed (non-fatal, continuing to push)")
	} else if imageInfo, err := getImageSizeInfo(image, stdoutBuf.String()); err != nil {
		klog.ErrorS(err, "Parse image size failed (non-fatal)")
	} else {
		klog.InfoS("Commit image size", "size", imageInfo.Size, "blobSize", imageInfo.BlobSize)
	}

	// 4. nerdctl push
	klog.InfoS(fmt.Sprintf("Start to push image %s", image))
	start = time.Now()
	if err := NerdctlExec(ctx, WithArgs("push", image)); err != nil {
		klog.ErrorS(err, "Push failed", "image", image)
		return ExitCodePushFailed
	}
	klog.InfoS("Push succeeded", "elapsed", time.Since(start))

	return ExitCodeSuccess
}

type imageInfo struct {
	Name     string `json:"Name"`
	Size     string `json:"Size"`
	BlobSize string `json:"BlobSize"`
}

func getImageSizeInfo(imageRef string, output string) (*imageInfo, error) {
	// Simple parsing - nerdctl images --format json outputs one JSON per line
	lines := bytes.Split([]byte(output), []byte("\n"))
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var img imageInfo
		if err := json.Unmarshal(line, &img); err != nil {
			continue
		}
		if img.Name == imageRef {
			return &img, nil
		}
	}
	return nil, fmt.Errorf("image not found in output: %s", imageRef)
}
