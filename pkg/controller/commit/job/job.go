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
	"time"

	"k8s.io/klog/v2"
)

// Executor defines the interface for running nerdctl commands.
type Executor func(ctx context.Context, opts ...CmdOpt) error

// defaultExecutor is the production executor.
var defaultExecutor Executor = NerdctlExec

// CommitOptions contains the explicit runtime inputs for a commit job.
type CommitOptions struct {
	ContainerID string
	Image       string
}

// DoCommit is the main entry point for the commit-job binary.
// It performs: setup registry auth → nerdctl commit → nerdctl push.
func DoCommit(ctx context.Context, opts CommitOptions) int {
	return doCommitWith(ctx, opts, defaultExecutor)
}

func doCommitWith(ctx context.Context, opts CommitOptions, executor Executor) int {
	containerID := opts.ContainerID
	image := opts.Image

	if containerID == "" {
		klog.ErrorS(nil, "Commit container ID is empty", "arg", ArgContainerID)
		return ExitCodeCommitFailed
	}
	if image == "" {
		klog.ErrorS(nil, "Commit image is empty", "arg", ArgImage)
		return ExitCodeCommitFailed
	}

	klog.InfoS("Start commit", "containerID", containerID, "image", image)

	// 1. Setup registry authentication
	if err := setupRegistryAuth(); err != nil {
		klog.ErrorS(err, "Failed to setup registry authentication, push may fail")
	}

	// 2. nerdctl commit
	start := time.Now()
	if err := executor(ctx, WithArgs("commit", containerID, image)); err != nil {
		klog.ErrorS(err, "Commit failed", "containerID", containerID, "image", image)
		return ExitCodeCommitFailed
	}
	klog.InfoS("Commit succeeded", "elapsed", time.Since(start))

	// 3. nerdctl push
	klog.InfoS("Start to push image", "image", image)
	start = time.Now()
	if err := executor(ctx, WithArgs("push", image)); err != nil {
		klog.ErrorS(err, "Push failed", "image", image)
		return ExitCodePushFailed
	}
	klog.InfoS("Push succeeded", "elapsed", time.Since(start))

	return ExitCodeSuccess
}
