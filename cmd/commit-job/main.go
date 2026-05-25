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
	"os"
	"os/signal"
	"syscall"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/job"
)

func main() {
	klog.InitFlags(nil)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	action := os.Getenv(job.EnvAgentJobActionKey)
	klog.InfoS("Agent job starting", "action", action)

	var exitCode int
	switch action {
	case job.EnvAgentJobActionCommit:
		exitCode = job.DoCommit(ctx)
	default:
		klog.Errorf("Unknown action: %s", action)
		exitCode = 1
	}

	klog.InfoS("Agent job finished", "exitCode", exitCode)
	klog.Flush()
	os.Exit(exitCode)
}
