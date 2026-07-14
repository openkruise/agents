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

package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/klog/v2"

	jobutil "github.com/openkruise/agents/pkg/controller/commit/job"
)

func main() {
	klog.InitFlags(nil)
	containerID := flag.String(jobutil.ArgContainerID, "", "Target container ID to commit.")
	image := flag.String(jobutil.ArgImage, "", "Target image to commit and push.")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	klog.InfoS("Commit job starting")
	exitCode := jobutil.DoCommit(ctx, jobutil.CommitOptions{ContainerID: *containerID, Image: *image})

	klog.InfoS("Commit job finished", "exitCode", exitCode)
	klog.Flush()
	os.Exit(exitCode)
}
