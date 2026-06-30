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
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
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

	"github.com/openkruise/agents/pkg/utils"
	csimountutils "github.com/openkruise/agents/pkg/utils/csiutils"
	"github.com/openkruise/agents/pkg/utils/runtime/config"

	"github.com/openkruise/agents/pkg/utils/logs"
	"github.com/openkruise/agents/pkg/utils/proxyutils"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
)

var AccessToken = "access-token"

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
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx)).V(utils.DebugLogLevel)
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
	callInfo.RequestHeader().Set("X-Access-Token", utils.GetAccessToken(sbx))
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

// ChmodFileOnRuntime executes `chmod <mode> <filePath>` inside the sandbox runtime
// via RunCommandWithRuntime. This is a temporary measure to enforce file permissions
// until the agent-runtime (envd) natively honors the X-File-Mode header.
func ChmodFileOnRuntime(ctx context.Context, sbx *agentsv1alpha1.Sandbox, filePath, mode string) error {
	result, err := RunCommandWithRuntime(ctx, RunCmdFuncArgs{
		Sbx: sbx,
		ProcessConfig: &process.ProcessConfig{
			Cmd:  "chmod",
			Args: []string{mode, filePath},
		},
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("chmod command failed: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("chmod exited with code %d: stderr=%v", result.ExitCode, result.Stderr)
	}
	return nil
}

// WriteFileArgs are the arguments accepted by WriteFileWithRuntime.
type WriteFileArgs struct {
	// Sbx is the target sandbox. Its annotations supply the runtime URL and access token,
	// resolved via GetRuntimeURL / GetAccessToken.
	Sbx *agentsv1alpha1.Sandbox
	// FilePath is the absolute file path inside the sandbox runtime where Content will be
	// materialized. The parent directory must already exist on the runtime side.
	FilePath string
	// Content is the raw file body. It is uploaded as-is via multipart/form-data and is not
	// interpreted by this function.
	Content []byte
	// Username is the OS user the runtime should use when writing the file. Defaults to
	// defaultRuntimeFilesUsername ("root") when empty.
	Username string
	// Timeout bounds the duration of a single HTTP write request. Defaults to
	// defaultRuntimeWriteTimeout when zero or negative.
	Timeout time.Duration
	// Permissions is the UNIX file mode applied to the written file by the runtime after
	// creation (e.g. 0600 for credential files, 0644 for non-sensitive files). When zero,
	// the runtime applies its default permissions (typically 0644 derived from umask).
	// Transmitted to the agent-runtime via the X-File-Mode HTTP header as an octal string.
	Permissions os.FileMode
}

// WriteFileResult carries metadata about a write call. The HTTP response body is drained
// internally so callers do not need to handle it.
type WriteFileResult struct {
	StatusCode int
}

// runtime files API constants. Kept package-private since the public surface is the typed
// WriteFileArgs struct.
const (
	defaultRuntimeWriteTimeout  = 10 * time.Second
	defaultRuntimeFilesUsername = "root"
	runtimeFilesFieldName       = "file"
)

// runtimeFilesHTTPClient is the package-level HTTP client used by WriteFileWithRuntime.
// It is a variable rather than a constant so tests can substitute their own transport
// without monkey-patching net/http.DefaultClient.
//
// Intentionally no http.Client.Timeout is set: WriteFileWithRuntime always wraps the
// caller's context with context.WithTimeout(ctx, args.Timeout), which becomes the
// single source of truth for request deadlines. Setting a client-level timeout here
// would silently cap any args.Timeout above that value (the effective timeout is
// min(client.Timeout, ctx deadline)). Mirrors RunCommandWithRuntime above which also
// relies solely on the per-call context for cancellation.
var runtimeFilesHTTPClient = &http.Client{}

// WriteFileWithRuntime writes a single file into the sandbox runtime by calling the
// E2B-compatible files API exposed by the agent-runtime sidecar:
//
//	POST <runtimeURL>/files?path=<filePath>&username=<username>
//	Content-Type: multipart/form-data; boundary=...
//	form field "file": <args.Content>
//
// The behavior is unconditional overwrite: any pre-existing file at the same path is
// replaced, mirroring the upstream E2B `sbx.files.write(path, content)` semantics.
//
// On success the function returns WriteFileResult with the HTTP status code. On HTTP-level
// failure (transport error or status >= 400) it returns a non-nil error that wraps the
// underlying cause (or the truncated runtime error body for HTTP errors).
//
// This function is intended as the standard counterpart to RunCommandWithRuntime: any
// caller that needs to push a file into the sandbox runtime should use it instead of
// rolling its own HTTP client.
func WriteFileWithRuntime(ctx context.Context, args WriteFileArgs) (WriteFileResult, error) {
	sbx := args.Sbx
	if sbx == nil {
		return WriteFileResult{}, fmt.Errorf("sandbox is nil")
	}
	if args.FilePath == "" {
		return WriteFileResult{}, fmt.Errorf("filePath is required")
	}
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx)).V(utils.DebugLogLevel)

	rtURL := GetRuntimeURL(sbx)
	if rtURL == "" {
		return WriteFileResult{}, fmt.Errorf("runtime url not found on sandbox")
	}

	username := args.Username
	if username == "" {
		username = defaultRuntimeFilesUsername
	}
	timeout := args.Timeout
	if timeout <= 0 {
		timeout = defaultRuntimeWriteTimeout
	}

	body, contentType, err := buildRuntimeFilesMultipartBody(args.FilePath, args.Content)
	if err != nil {
		return WriteFileResult{}, err
	}

	endpoint := buildRuntimeFilesEndpoint(rtURL, args.FilePath, username)
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, body)
	if err != nil {
		return WriteFileResult{}, fmt.Errorf("failed to build runtime files write request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	if accessToken := utils.GetAccessToken(sbx); accessToken != "" {
		req.Header.Set("X-Access-Token", accessToken)
	}
	// Basic auth header mirrors the agent-runtime expectation (root user, empty password)
	// and matches the value used by RunCommandWithRuntime above.
	req.Header.Set("Authorization", "Basic cm9vdDo=") // Basic root:

	start := time.Now()
	log.Info("writing file to runtime via files API",
		"filePath", args.FilePath,
		"endpoint", endpoint)

	resp, err := runtimeFilesHTTPClient.Do(req)
	if err != nil {
		return WriteFileResult{}, fmt.Errorf("failed to call runtime files API: %w", err)
	}
	defer func() {
		// Drain and close to enable connection reuse.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	result := WriteFileResult{StatusCode: resp.StatusCode}
	if resp.StatusCode >= http.StatusBadRequest {
		// Read up to 1 KiB of the response body to surface the runtime-side error reason
		// without unbounded memory usage.
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return result, fmt.Errorf("runtime files API returned status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	log.Info("file written to runtime successfully (overwrite)",
		"filePath", args.FilePath,
		"statusCode", resp.StatusCode,
		"cost", time.Since(start))
	return result, nil
}

// buildRuntimeFilesMultipartBody assembles a multipart/form-data body whose only field is
// the file content. The returned contentType already carries the boundary produced by
// multipart.Writer, so the caller can set it as Content-Type as-is.
func buildRuntimeFilesMultipartBody(filePath string, content []byte) (*bytes.Buffer, string, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile(runtimeFilesFieldName, path.Base(filePath))
	if err != nil {
		return nil, "", fmt.Errorf("failed to create multipart form file: %w", err)
	}
	if _, err := part.Write(content); err != nil {
		return nil, "", fmt.Errorf("failed to write multipart form file content: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("failed to close multipart writer: %w", err)
	}
	return body, writer.FormDataContentType(), nil
}

// buildRuntimeFilesEndpoint composes the absolute URL of the agent-runtime files API for
// the given runtime base URL, target file path and runtime username. Trailing slashes on
// runtimeURL are tolerated.
func buildRuntimeFilesEndpoint(runtimeURL, filePath, username string) string {
	base := strings.TrimRight(runtimeURL, "/")
	q := url.Values{}
	q.Set("path", filePath)
	q.Set("username", username)
	return fmt.Sprintf("%s/files?%s", base, q.Encode())
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
	initBody, err := json.Marshal(opts) // #nosec G117 -- AccessToken intentionally serialized for runtime init
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
	}, utils.RetryIfContextNotCanceled(ctx), func() error {
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
	csiMountRequestsRaw := s.GetAnnotations()[agentsv1alpha1.AnnotationCSIVolumeConfig]
	if csiMountRequestsRaw == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(csiMountRequestsRaw), &csiMountRequests); err != nil {
		return nil, fmt.Errorf("failed to unmarshal csi mount options: %v", err)
	}
	return csiMountRequests, nil
}

// GetRuntimeURL resolves the agent-runtime endpoint for a Sandbox.
//
// Lookup order:
//  1. AnnotationRuntimeURL on the Sandbox object.
//  2. AnnotationEnvdURL on the Sandbox object (legacy key, kept for backwards compatibility).
//  3. Pod IP from the cached route plus the well-known utils.RuntimePort, used as a fallback
//     while the controller has not yet stamped the URL annotation.
//
// Returns an empty string when none of the sources is usable (e.g. the pod has not been scheduled
// yet). Callers must treat an empty result as "not ready" and either skip or retry.
func GetRuntimeURL(sbx *agentsv1alpha1.Sandbox) string {
	if sbx == nil {
		return ""
	}
	annotations := sbx.GetAnnotations()
	if u := annotations[agentsv1alpha1.AnnotationRuntimeURL]; u != "" {
		return u
	}
	if u := annotations[agentsv1alpha1.AnnotationEnvdURL]; u != "" { // legacy
		return u
	}
	ip := sbx.Status.PodInfo.PodIP
	if ip == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", ip, utils.RuntimePort)
}
