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

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"connectrpc.com/connect"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/pkg/cache"

	"github.com/openkruise/agents/api/v1alpha1"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"

	"github.com/openkruise/agents/pkg/agent-runtime/storages"

	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/utils"
	csimountutils "github.com/openkruise/agents/pkg/utils/csiutils"

	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	commonutils "github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager/proxyutils"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
)

var AccessToken = "access-token"

func GetRuntimeURL(sbx *agentsv1alpha1.Sandbox) string {
	// firstly, get runtime url from the annotation
	url := sbx.GetAnnotations()[agentsv1alpha1.AnnotationRuntimeURL]
	if url == "" {
		url = sbx.GetAnnotations()[agentsv1alpha1.AnnotationEnvdURL] // legacy
	}
	if url != "" {
		return url
	}
	// secondly, calculate runtime url from the route
	route := sandboxutils.GetRouteFromSandbox(sbx)
	if route.IP == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", route.IP, consts.RuntimePort)
}

func GetAccessToken(sbx metav1.Object) string {
	token := sbx.GetAnnotations()[agentsv1alpha1.AnnotationRuntimeAccessToken]
	if token == "" {
		token = sbx.GetAnnotations()[agentsv1alpha1.AnnotationEnvdAccessToken] // legacy
	}
	return token
}

type RunCommandResult struct {
	PID      uint32
	Stdout   []string
	Stderr   []string
	ExitCode int32
	Exited   bool
	Error    error
}

type RunCmdFuncArgs struct {
	Sbx           *agentsv1alpha1.Sandbox
	ProcessConfig *process.ProcessConfig
	Timeout       time.Duration
}

func RunCommandWithRuntime(ctx context.Context, args RunCmdFuncArgs) (RunCommandResult, error) {
	sbx, processConfig, timeout := args.Sbx, args.ProcessConfig, args.Timeout
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx)).V(consts.DebugLogLevel)
	url := GetRuntimeURL(sbx)
	if url == "" {
		return RunCommandResult{}, fmt.Errorf("runtime url not found on sandbox")
	}
	client := processconnect.NewProcessClient(
		http.DefaultClient,
		url,
		connect.WithGRPC(),
	)

	ctxWithTimeout, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	clientContext, callInfo := connect.NewClientContext(ctxWithTimeout)
	callInfo.RequestHeader().Set("X-Access-Token", GetAccessToken(sbx))
	callInfo.RequestHeader().Set("Authorization", "Basic cm9vdDo=") // Basic root:

	req := connect.NewRequest(&process.StartRequest{
		Process: processConfig,
		Tag:     nil,
		Pty:     nil,
		Stdin:   nil,
	})
	stream, err := client.Start(clientContext, req)
	if err != nil {
		return RunCommandResult{}, err
	}
	defer func() {
		if err := stream.Close(); err != nil {
			log.Error(err, "failed to close stream")
		} else {
			log.Info("stream closed")
		}
	}()

	var result RunCommandResult
	start := time.Now()
	log.Info("receiving messages", "timeout", timeout)
	for stream.Receive() {
		event := stream.Msg().Event
		switch evt := event.Event.(type) {
		case *process.ProcessEvent_Start:
			pid := evt.Start.Pid
			result.PID = pid
		case *process.ProcessEvent_Data:
			switch data := evt.Data.Output.(type) {
			case *process.ProcessEvent_DataEvent_Stdout:
				result.Stdout = append(result.Stdout, string(data.Stdout))
			case *process.ProcessEvent_DataEvent_Stderr:
				result.Stderr = append(result.Stderr, string(data.Stderr))
			}

		case *process.ProcessEvent_End:
			result.ExitCode = evt.End.ExitCode
			result.Exited = evt.End.Exited
			if evt.End.Error != nil {
				result.Error = fmt.Errorf("process error: %s", *evt.End.Error)
			}

		default: // ProcessEvent_Keepalive
			continue
		}
	}
	log.Info("all messages are received", "cost", time.Since(start), "result", result)
	return result, errors.Join(result.Error, stream.Err())
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
		driverName, csiReqConfigRaw, genErr := csiClient.CSIMountOptionsConfig(ctx, cfg)
		if genErr != nil {
			log.Error(genErr, "failed to generate csi mount options config", "mountConfig", cfg)
			return nil, fmt.Errorf("failed to generate csi mount options config: %w", genErr)
		}
		mountOptionList = append(mountOptionList, config.MountConfig{Driver: driverName, RequestRaw: csiReqConfigRaw})
	}
	return &config.CSIMountOptions{MountOptionList: mountOptionList}, nil
}

// GetInitRuntimeRequest parses init runtime configuration from object annotations.
func GetInitRuntimeRequest(s metav1.Object) (*config.InitRuntimeOptions, error) {
	// Build initRuntimeOpts from annotation at the beginning
	var initRuntimeOpts *config.InitRuntimeOptions
	if initRuntimeRequest := s.GetAnnotations()[agentsv1alpha1.AnnotationInitRuntimeRequest]; initRuntimeRequest != "" {
		var opts config.InitRuntimeOptions
		if err := json.Unmarshal([]byte(initRuntimeRequest), &opts); err != nil {
			return nil, fmt.Errorf("failed to unmarshal init runtime request: %w", err)
		}
		opts.ReInit = true
		initRuntimeOpts = &opts
	}
	return initRuntimeOpts, nil
}

// RefreshFunc is a callback that refreshes the sandbox object to its latest state.
// It returns the updated sandbox object, allowing InitRuntime to use the latest
// annotations (e.g., runtime URL) without depending on the sandboxcr package.
type RefreshFunc func(ctx context.Context) (*agentsv1alpha1.Sandbox, error)

// InitRuntime sends an init request to the sandbox runtime sidecar.
// The sbx parameter is the raw Sandbox API object. When opts.SkipRefresh is false
// and refreshFn is provided, it will be called to get the latest sandbox state
// before each retry attempt.
func InitRuntime(ctx context.Context, sbx *agentsv1alpha1.Sandbox, opts config.InitRuntimeOptions, refreshFn RefreshFunc) (time.Duration, error) {
	ctx = logs.Extend(ctx, "action", "initRuntime")
	log := klog.FromContext(ctx).WithValues("sandboxID", sbx.GetName(), "resourceVersion", sbx.GetResourceVersion())
	start := time.Now()
	initBody, err := json.Marshal(opts)
	if err != nil {
		log.Error(err, "failed to marshal initBody")
		return 0, err
	}
	retries := -1
	currentSbx := sbx
	err = retry.OnError(wait.Backoff{
		// about retry 20s
		Duration: 200 * time.Millisecond,
		Factor:   2.0,
		Steps:    5,
		Cap:      10 * time.Second,
	}, commonutils.RetryIfContextNotCanceled(ctx), func() error {
		var initErr error
		retries++
		requestCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer func() {
			cancel()
			if initErr != nil {
				log.Error(initErr, "init runtime request failed", "retries", retries)
			}
		}()
		if !opts.SkipRefresh && refreshFn != nil {
			updated, refreshErr := refreshFn(ctx)
			if refreshErr != nil {
				log.Error(refreshErr, "failed to refresh sandbox")
				initErr = refreshErr
				return initErr
			}
			currentSbx = updated
		}
		runtimeURL := GetRuntimeURL(currentSbx)
		if runtimeURL == "" {
			log.Error(nil, "runtimeURL is empty")
			return fmt.Errorf("runtimeURL is empty")
		}
		url := runtimeURL + "/init"
		log.Info("sending request to runtime", "resourceVersion", sbx.GetResourceVersion(),
			"url", url, "params", opts, "retries", retries)

		// Create a new request for each retry to avoid Body reuse issue
		r, initErr := http.NewRequestWithContext(requestCtx, http.MethodPost, url, bytes.NewBuffer(initBody))
		if initErr != nil {
			log.Error(initErr, "failed to create request")
			return initErr
		}
		resp, initErr := proxyutils.ProxyRequest(r)
		defer func() {
			// Discard response body to allow connection reuse
			if resp != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}()
		// When ReInit is true, treat 401 as success (sandbox already initialized)
		if resp != nil && resp.StatusCode == http.StatusUnauthorized && opts.ReInit {
			log.Info("init runtime returned 401, treated as success because ReInit is true")
			return nil
		}
		if initErr != nil {
			return initErr
		}
		return nil
	})
	return time.Since(start), err
}

func GetCsiMountExtensionRequest(s metav1.Object) ([]v1alpha1.CSIMountConfig, error) {
	var csiMountRequests []v1alpha1.CSIMountConfig
	csiMountRequestsRaw := s.GetAnnotations()[models.ExtensionKeyClaimWithCSIMount_MountConfig]
	if csiMountRequestsRaw == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(csiMountRequestsRaw), &csiMountRequests); err != nil {
		return nil, fmt.Errorf("failed to unmarshal csi mount options: %v", err)
	}
	return csiMountRequests, nil
}
