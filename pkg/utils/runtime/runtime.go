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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/api/v1alpha1"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/utils"
	csimountutils "github.com/openkruise/agents/pkg/utils/csiutils"
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

// sidecar runtime 提供的 run command 能力
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
func ResolveCSIMountFromAnnotation(ctx context.Context, obj metav1.Object, client *clients.ClientSet, cache infra.CacheProvider, storageRegistry storages.VolumeMountProviderRegistry) (*config.CSIMountOptions, error) {
	log := klog.FromContext(ctx)
	csiMountConfigs, err := GetCsiMountExtensionRequest(obj)
	if err != nil {
		log.Error(err, "failed to parse csi mount config from annotation")
		return nil, fmt.Errorf("failed to parse csi mount config from annotation: %w", err)
	}
	if len(csiMountConfigs) == 0 {
		return nil, nil
	}
	csiClient := csimountutils.NewCSIMountHandler(client, cache, storageRegistry, utils.DefaultSandboxDeployNamespace)
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

// GetCsiMountExtensionRequest parses CSI mount config from object annotations.
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
