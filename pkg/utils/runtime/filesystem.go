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

// filesystem.go groups the native e2b-compatible files capability of the
// runtime: pushing a file into the sandbox via the upstream multipart /files
// API. This maps to the standard envd/e2b protocol rather than to a
// KruiseAgents-specific extended route.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

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
	// AuthUser is the user identity sent as an HTTP Basic Authorization header
	// (empty password). Empty means no Authorization header is sent; callers
	// that need the runtime to resolve a user identity (e.g. "root") must set
	// it explicitly.
	AuthUser string
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
	// The Basic user credential is caller-supplied: it is sent only when
	// args.AuthUser expresses a user identity for the runtime to resolve.
	if args.AuthUser != "" {
		req.Header.Set("Authorization", basicAuthHeader(args.AuthUser))
	}

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
		// without unbounded memory usage. The status code alone already classifies the
		// failure, so a mid-body read error only degrades the diagnostic message.
		bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		reason := strings.TrimSpace(string(bodyBytes))
		if reason == "" && readErr != nil {
			reason = fmt.Sprintf("failed to read error response body: %v", readErr)
		}
		return result, fmt.Errorf("runtime files API returned status %d: %s",
			resp.StatusCode, reason)
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
