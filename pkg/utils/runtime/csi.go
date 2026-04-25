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

package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/proto/envd/process"
)

var MountCommand = "/mnt/envd/sandbox-runtime-storage"

// CSIMount creates a dynamic mount point in Sandbox with `sandbox-storage` cli.
// It accepts the raw Sandbox API object to avoid circular dependency on the sandboxcr package.
//
// NOTE: `sandbox-storage` cli should be injected with `sandbox-runtime` and will be replaced by a built-in service of
// `sandbox-runtime`.
func CSIMount(ctx context.Context, sbx *agentsv1alpha1.Sandbox, driver string, request string) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
	startTime := time.Now()
	processConfig := &process.ProcessConfig{
		Cmd: MountCommand,
		Args: []string{
			"mount",
			"--driver", driver,
			"--config", request,
		},
		Cwd: nil,
		Envs: map[string]string{
			"POD_UID": string(sbx.Status.PodInfo.PodUID),
		},
	}

	result, err := RunCommandWithRuntime(ctx, RunCmdFuncArgs{
		Sbx:           sbx,
		ProcessConfig: processConfig,
		Timeout:       5 * time.Second,
	})
	if err != nil {
		log.Error(err, "failed to run command", "stdout", result.Stdout, "stderr", result.Stderr)
		return err
	}
	if result.ExitCode != 0 {
		err = fmt.Errorf("command failed: [%d] %s", result.ExitCode, result.Stderr)
		log.Error(err, "command failed", "exitCode", result.ExitCode)
		return err
	}
	log.Info("execute csi mount command", "driverName", driver, "mountCost", time.Since(startTime))
	return nil
}

// ProcessCSIMounts performs CSI volume mounting operations for all mount configurations concurrently.
// It uses opts.Concurrency to limit the number of concurrent mount goroutines.
// If Concurrency is 0 or negative, it defaults to config.DefaultCSIMountConcurrency.
// Returns the total duration spent on all mount operations and all encountered errors (joined via errors.Join).
func ProcessCSIMounts(ctx context.Context, sbx *agentsv1alpha1.Sandbox, opts config.CSIMountOptions) (time.Duration, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
	start := time.Now()

	var wg sync.WaitGroup
	errCh := make(chan error, len(opts.MountOptionList))

	// Use a semaphore channel to limit concurrency
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = config.DefaultCSIMountConcurrency
	}
	sem := make(chan struct{}, concurrency)

	for _, opt := range opts.MountOptionList {
		wg.Add(1)
		sem <- struct{}{}
		go func(opt config.MountConfig) {
			defer wg.Done()
			defer func() { <-sem }()
			mountDuration, err := doCSIMount(ctx, sbx, opt)
			if err != nil {
				log.Error(err, "failed to perform CSI mount", "mountOptionConfig", opt)
				errCh <- err
				return
			}
			log.Info("CSI mount completed successfully",
				"mountOptionConfig", opt,
				"duration", mountDuration)
		}(opt)
	}

	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	return time.Since(start), errors.Join(errs...)
}

func doCSIMount(ctx context.Context, sbx *agentsv1alpha1.Sandbox, opts config.MountConfig) (time.Duration, error) {
	ctx = logs.Extend(ctx, "action", "csiMount")
	start := time.Now()
	err := CSIMount(ctx, sbx, opts.Driver, opts.RequestRaw)
	return time.Since(start), err
}
