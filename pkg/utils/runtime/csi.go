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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/utils"
	csimountutils "github.com/openkruise/agents/pkg/utils/csiutils"
	"github.com/openkruise/agents/pkg/utils/logs"
	"github.com/openkruise/agents/pkg/utils/runtime/config"
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
		Timeout:       30 * time.Second,
		// The mount CLI manipulates mounts inside the sandbox and must run as root.
		AuthUser: "root",
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

// CSIUnmount reverses one dynamic mount by running the sandbox-storage CLI
// inside the Sandbox, with the same --config contract as CSIMount: the
// base64-encoded NodePublishVolumeRequest the mount was issued with.
//
// Unlike CSIMount, the rtOpts are forwarded to RunCommandWithRuntime instead of
// being dropped: even a TLS-capable sandbox takes the CLI path for unmount
// (the runtime storage API has no unmount endpoint yet), and the transport
// decision still has to reach the process call.
func CSIUnmount(ctx context.Context, sbx *agentsv1alpha1.Sandbox, driver string, request string, rtOpts ...Option) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
	startTime := time.Now()
	processConfig := &process.ProcessConfig{
		Cmd: MountCommand,
		Args: []string{
			"unmount",
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
		Timeout:       30 * time.Second,
		// The unmount CLI manipulates mounts inside the sandbox and must run as root.
		AuthUser: "root",
	}, rtOpts...)
	if err != nil {
		log.Error(err, "failed to run command", "stdout", result.Stdout, "stderr", result.Stderr)
		return err
	}
	if result.ExitCode != 0 {
		err = fmt.Errorf("command failed: [%d] %s", result.ExitCode, result.Stderr)
		log.Error(err, "command failed", "exitCode", result.ExitCode)
		return err
	}
	log.Info("execute csi unmount command", "driverName", driver, "unmountCost", time.Since(startTime))
	return nil
}

// runConcurrentCSI is the shared scheduling skeleton of ProcessCSIMounts and
// ProcessCSIUnmounts: it applies fn to every entry concurrently, bounded by a
// semaphore, and joins all errors (via errors.Join) with the total wall-clock
// duration. The concurrency scaffolding lives in exactly this one place; the
// per-entry semantics (transport dispatch, driver-scoped logging) stay with
// the caller's fn.
// If concurrency is 0 or negative, config.DefaultCSIMountConcurrency applies.
func runConcurrentCSI[T any](ctx context.Context, sbx *agentsv1alpha1.Sandbox, list []T, concurrency int,
	fn func(ctx context.Context, sbx *agentsv1alpha1.Sandbox, index int, entry T) (time.Duration, error),
) (time.Duration, error) {
	start := time.Now()

	var wg sync.WaitGroup
	errCh := make(chan error, len(list))

	// Use a semaphore channel to limit concurrency
	if concurrency <= 0 {
		concurrency = config.DefaultCSIMountConcurrency
	}
	sem := make(chan struct{}, concurrency)

	for i, entry := range list {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, entry T) {
			defer wg.Done()
			defer func() { <-sem }()
			if _, err := fn(ctx, sbx, i, entry); err != nil {
				errCh <- err
			}
		}(i, entry)
	}

	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	return time.Since(start), errors.Join(errs...)
}

// ProcessCSIMounts performs CSI volume mounting operations for all mount configurations concurrently.
// It uses opts.Concurrency to limit the number of concurrent mount goroutines.
// If Concurrency is 0 or negative, it defaults to config.DefaultCSIMountConcurrency.
// Returns the total duration spent on all mount operations and all encountered errors (joined via errors.Join).
//
// rtOpts selects the mount transport per sandbox: when non-empty (typically the
// TLS options resolved by TransportOptionsFor for a sandbox advertising
// AnnotationRuntimeTLSPort), every mount goes through the runtime storage API
// (POST /v1/storage/mounts over HTTPS with forced resolution). When empty, the
// legacy sandbox-storage CLI path over the envd process protocol is used, so
// pre-TLS sandboxes keep their existing behavior untouched.
func ProcessCSIMounts(ctx context.Context, sbx *agentsv1alpha1.Sandbox, opts config.CSIMountOptions, rtOpts ...Option) (time.Duration, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
	return runConcurrentCSI(ctx, sbx, opts.MountOptionList, opts.Concurrency,
		func(ctx context.Context, sbx *agentsv1alpha1.Sandbox, i int, opt config.MountConfig) (time.Duration, error) {
			// Never log the MountConfig itself: PublishRequest carries the volume
			// Secrets and PublishContext (see MountConfig.PublishRequest). The driver
			// plus the position in the list is enough to correlate an entry with
			// its per-mount logs, which add the target path.
			mountDuration, err := doCSIMount(ctx, sbx, opt, rtOpts...)
			if err != nil {
				log.Error(err, "failed to perform CSI mount", "driver", opt.Driver, "mountIndex", i)
				return mountDuration, err
			}
			log.Info("CSI mount completed successfully",
				"driver", opt.Driver,
				"mountIndex", i,
				"duration", mountDuration)
			return mountDuration, nil
		})
}

// doCSIMount performs a single CSI mount, dispatching between the two coexisting
// transports: the runtime storage API (HTTPS, when rtOpts carries the TLS options
// for a TLS-capable sandbox) and the legacy sandbox-storage CLI (plaintext envd
// process protocol) for everything else. The dispatch key is intentionally just
// "rtOpts non-empty": TransportOptionsFor is the single oracle deciding whether a
// sandbox is served over TLS, so this function stays free of annotation parsing.
func doCSIMount(ctx context.Context, sbx *agentsv1alpha1.Sandbox, opts config.MountConfig, rtOpts ...Option) (time.Duration, error) {
	ctx = logs.Extend(ctx, "action", "csiMount")
	start := time.Now()
	if len(rtOpts) > 0 {
		// New path: the runtime storage API carries the typed CSI message, so the
		// request travels as-is and protojson owns its encoding.
		_, err := NewRuntime(sbx, rtOpts...).Storage().Mount(ctx, CreateMountRequest{
			Driver:         opts.Driver,
			PublishRequest: opts.PublishRequest,
		})
		if err != nil {
			// The transport error names the endpoint, not the mount: add the
			// driver so both paths fail with the same amount of context.
			return time.Since(start), fmt.Errorf("failed to mount via runtime storage API for driver %q: %w", opts.Driver, err)
		}
		return time.Since(start), nil
	}
	// Legacy path: the CLI consumes an opaque blob, so encode the message here —
	// the last step before it becomes a command-line argument.
	requestRaw, err := encodePublishRequest(opts.PublishRequest)
	if err != nil {
		return time.Since(start), fmt.Errorf("failed to encode csi publish request for driver %q: %w", opts.Driver, err)
	}
	// Keep the mount and the elapsed time in separate statements: in a return
	// statement time.Since would be evaluated before the call it is meant to
	// measure, reporting a duration that excludes the mount itself.
	err = CSIMount(ctx, sbx, opts.Driver, requestRaw)
	return time.Since(start), err
}

// encodePublishRequest renders the typed CSI request as the base64 protobuf blob
// the sandbox-storage CLI expects on its --config flag. It is the only place the
// control plane still pre-encodes a mount request, and it disappears with the
// CLI transport.
func encodePublishRequest(publishRequest *csi.NodePublishVolumeRequest) (string, error) {
	// An empty --config would only fail inside the sandbox: reject it here, where
	// the error can still name the mount that is misconfigured.
	if publishRequest == nil {
		return "", fmt.Errorf("csi publish request is required")
	}
	data, err := proto.Marshal(protoadapt.MessageV2Of(publishRequest))
	if err != nil {
		return "", fmt.Errorf("failed to marshal csi publish request: %w", err)
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// ProcessCSIUnmounts unmounts all CSI mounts of a sandbox concurrently, mirroring
// ProcessCSIMounts. It consumes CSIUnmountOptions: the resolved UnmountConfig
// entries carry a NodeUnpublishVolumeRequest each (see MountConfig.ToUnmountConfig
// for how one is derived from a MountConfig).
// It uses opts.Concurrency to limit the number of concurrent unmount goroutines;
// if Concurrency is 0 or negative, config.DefaultCSIMountConcurrency applies.
// Returns the total duration spent on all unmount operations and all encountered
// errors (joined via errors.Join).
func ProcessCSIUnmounts(ctx context.Context, sbx *agentsv1alpha1.Sandbox, opts config.CSIUnmountOptions, rtOpts ...Option) (time.Duration, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
	return runConcurrentCSI(ctx, sbx, opts.UnmountOptionList, opts.Concurrency,
		func(ctx context.Context, sbx *agentsv1alpha1.Sandbox, i int, opt config.UnmountConfig) (time.Duration, error) {
			// The unpublish request carries no credentials by construction, but
			// keep the mount-side discipline of never logging the config itself;
			// the driver plus the position in the list is enough to correlate an
			// entry with its per-mount logs, which add the target path.
			unmountDuration, err := doCSIUnmount(ctx, sbx, opt, rtOpts...)
			if err != nil {
				log.Error(err, "failed to perform CSI unmount", "driver", opt.Driver, "mountIndex", i)
				return unmountDuration, err
			}
			log.Info("CSI unmount completed successfully",
				"driver", opt.Driver,
				"mountIndex", i,
				"duration", unmountDuration)
			return unmountDuration, nil
		})
}

// doCSIUnmount performs a single CSI unmount. Unlike doCSIMount there is no
// runtime-storage-API branch: the /v1/storage endpoint has no unmount operation
// yet, so the unmount always travels through the sandbox-storage CLI over the
// envd process protocol. rtOpts is still forwarded so a TLS-capable sandbox
// reaches the process endpoint over HTTPS.
func doCSIUnmount(ctx context.Context, sbx *agentsv1alpha1.Sandbox, opts config.UnmountConfig, rtOpts ...Option) (time.Duration, error) {
	ctx = logs.Extend(ctx, "action", "csiUnmount")
	start := time.Now()
	// The CLI consumes an opaque blob, so encode the message here — the last
	// step before it becomes a command-line argument.
	requestRaw, err := encodeUnpublishRequest(opts.UnpublishRequest)
	if err != nil {
		return time.Since(start), fmt.Errorf("failed to encode csi unpublish request for driver %q: %w", opts.Driver, err)
	}
	// Keep the unmount and the elapsed time in separate statements: in a return
	// statement time.Since would be evaluated before the call it is meant to
	// measure, reporting a duration that excludes the unmount itself.
	err = CSIUnmount(ctx, sbx, opts.Driver, requestRaw, rtOpts...)
	return time.Since(start), err
}

// encodeUnpublishRequest renders the typed CSI unpublish request as the
// base64 protobuf blob the sandbox-storage CLI expects on its --config flag.
// An empty request would only fail inside the sandbox: reject it here, where
// the error can still name the mount that is misconfigured.
func encodeUnpublishRequest(unpublishRequest *csi.NodeUnpublishVolumeRequest) (string, error) {
	if unpublishRequest == nil {
		return "", fmt.Errorf("csi unpublish request is required")
	}
	data, err := proto.Marshal(protoadapt.MessageV2Of(unpublishRequest))
	if err != nil {
		return "", fmt.Errorf("failed to marshal csi unpublish request: %w", err)
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// GetCsiMountExtensionRequest parses the CSI mount extension request from object annotations.
func GetCsiMountExtensionRequest(s metav1.Object) ([]agentsv1alpha1.CSIMountConfig, error) {
	var csiMountRequests []agentsv1alpha1.CSIMountConfig
	csiMountRequestsRaw := s.GetAnnotations()[agentsv1alpha1.AnnotationCSIVolumeConfig]
	if csiMountRequestsRaw == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(csiMountRequestsRaw), &csiMountRequests); err != nil {
		return nil, fmt.Errorf("failed to unmarshal csi mount options: %v", err)
	}
	return csiMountRequests, nil
}

// ResolveCSIMountFromAnnotation parses CSI mount config from sandbox annotation and resolves it into MountOptionList.
// Returns nil if no CSI mount annotation is present.
func ResolveCSIMountFromAnnotation(ctx context.Context, obj metav1.Object, client client.Client, cache cache.Provider, storageRegistry storages.VolumeMountProviderRegistry) (*config.CSIMountOptions, error) {
	log := klog.FromContext(ctx)
	csiMountConfigs, err := GetCsiMountExtensionRequest(obj)
	if err != nil {
		log.Error(err, "failed to parse csi mount config from annotation")
		return nil, fmt.Errorf("failed to parse csi mount config from annotation: %w", err)
	}
	if len(csiMountConfigs) == 0 {
		return nil, nil
	}
	csiClient := csimountutils.NewCSIMountHandler(cache.GetClient(), cache.GetAPIReader(), storageRegistry, utils.DefaultSandboxDeployNamespace)
	mountOptionList := make([]config.MountConfig, 0, len(csiMountConfigs))
	for _, cfg := range csiMountConfigs {
		driverName, publishRequest, genErr := csiClient.GenerateNodePublishVolumeRequest(ctx, cfg)
		if genErr != nil {
			log.Error(genErr, "failed to generate csi mount options config", "mountConfig", cfg)
			return nil, fmt.Errorf("failed to generate csi mount options config: %w", genErr)
		}
		mountOptionList = append(mountOptionList, config.MountConfig{Driver: driverName, PublishRequest: publishRequest})
	}
	return &config.CSIMountOptions{MountOptionList: mountOptionList}, nil
}
