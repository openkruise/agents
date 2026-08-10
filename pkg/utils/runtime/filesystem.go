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
// API, and the directory operations exposed by the envd Filesystem service
// (ListDir, Remove). These map to the standard envd/e2b protocol rather than to
// a KruiseAgents-specific extended route.

import (
	"bytes"
	"context"
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
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/proto/envd/filesystem"
	"github.com/openkruise/agents/proto/envd/filesystem/filesystemconnect"
)

// FilesystemAPI is the runtime filesystem capability group. It covers the
// E2B-compatible multipart /files route (Write) and the envd Filesystem service
// (ListDir, Remove).
//
// Every method is addressed through the transport resolved by the owning
// runtimeClient, so a TLS-capable sandbox is reached over HTTPS with the same
// forced resolution the JSON routes use, while a legacy sandbox keeps the
// plaintext runtime URL. The group is bound to one sandbox at construction
// time, so no method takes a *Sandbox argument.
type FilesystemAPI interface {
	// Write pushes a single file into the sandbox, unconditionally overwriting
	// any pre-existing file at the same path.
	Write(ctx context.Context, req WriteFileRequest) (WriteFileResult, error)
	// ListDir lists the entries of a directory inside the sandbox.
	ListDir(ctx context.Context, req ListDirRequest) ([]*filesystem.EntryInfo, error)
	// Remove deletes a file or directory inside the sandbox. Directory removal is
	// recursive.
	Remove(ctx context.Context, req RemovePathRequest) error
}

// WriteFileRequest describes a single file write. It is WriteFileArgs without
// the sandbox, which the capability group already carries.
type WriteFileRequest struct {
	// FilePath is the absolute file path inside the sandbox runtime where Content
	// will be materialized. The parent directory must already exist on the
	// runtime side.
	FilePath string
	// Content is the raw file body, uploaded as-is via multipart/form-data.
	Content []byte
	// Username is the OS user the runtime should use when writing the file.
	// Defaults to defaultRuntimeFilesUsername ("root") when empty.
	Username string
	// AuthUser is the user identity sent as an HTTP Basic Authorization header
	// (empty password). Empty means no Authorization header is sent.
	AuthUser string
	// Timeout bounds the duration of a single HTTP write request. Defaults to
	// defaultRuntimeWriteTimeout when zero or negative.
	Timeout time.Duration
	// Permissions is the intended UNIX file mode of the written file. It is
	// currently NOT transmitted: the runtime applies its default permissions
	// (typically 0644 derived from umask), and a caller that needs an exact mode
	// must follow the write with ProcessAPI.Chmod. The field is retained because
	// it records the caller's intent and becomes effective without a call-site
	// change once the runtime honors an explicit file-mode header.
	Permissions os.FileMode
}

// ListDirRequest describes a directory listing. It is ListDirArgs without the
// sandbox, which the capability group already carries.
type ListDirRequest struct {
	// Path is the directory to list inside the sandbox.
	Path string
	// Depth bounds how deep the listing descends. Zero means the runtime default
	// of 1, i.e. the direct children of Path.
	Depth uint32
	// AuthUser is the user identity sent as an HTTP Basic Authorization header
	// (empty password). It is required: the Filesystem service resolves every
	// path relative to an authenticated user and rejects requests without one.
	AuthUser string
	// Timeout bounds the RPC. Defaults to defaultRuntimeFilesystemTimeout when
	// zero or negative.
	Timeout time.Duration
}

// RemovePathRequest describes a removal. It is RemovePathArgs without the
// sandbox, which the capability group already carries.
type RemovePathRequest struct {
	// Path is the file or directory to remove inside the sandbox.
	Path string
	// AuthUser is the user identity sent as an HTTP Basic Authorization header
	// (empty password), required for the same reason as in ListDirRequest.
	AuthUser string
	// Timeout bounds the RPC. Defaults to defaultRuntimeFilesystemTimeout when
	// zero or negative.
	Timeout time.Duration
}

// filesystemAPI is the default FilesystemAPI implementation. It delegates
// transport to the owning runtimeClient and carries no domain logic of its own.
type filesystemAPI struct {
	r *runtimeClient
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
	// AuthUser is the user identity sent as an HTTP Basic Authorization header
	// (empty password). Empty means no Authorization header is sent; callers
	// that need the runtime to resolve a user identity (e.g. "root") must set
	// it explicitly.
	AuthUser string
	// Timeout bounds the duration of a single HTTP write request. Defaults to
	// defaultRuntimeWriteTimeout when zero or negative.
	Timeout time.Duration
	// Permissions is the intended UNIX file mode of the written file (e.g. 0600
	// for credential files). See WriteFileRequest.Permissions: the mode is not
	// transmitted today, so a caller needing an exact mode must follow the write
	// with ChmodFileOnRuntime.
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
//
// rtOpts selects the transport for this sandbox: non-empty (typically the TLS
// options resolved by TransportOptionsFor) routes the write over HTTPS to the
// agent-runtime, empty keeps the legacy plaintext runtime URL. The call is
// delegated to FilesystemAPI.Write, which owns the implementation.
func WriteFileWithRuntime(ctx context.Context, args WriteFileArgs, rtOpts ...Option) (WriteFileResult, error) {
	// Keep the nil-sandbox guard here: NewRuntime binds the sandbox at
	// construction time, so a nil one would otherwise surface later as an
	// unresolved endpoint instead of naming the actual programming error.
	if args.Sbx == nil {
		return WriteFileResult{}, fmt.Errorf("sandbox is nil")
	}
	return NewRuntime(args.Sbx, rtOpts...).Filesystem().Write(ctx, WriteFileRequest{
		FilePath:    args.FilePath,
		Content:     args.Content,
		Username:    args.Username,
		AuthUser:    args.AuthUser,
		Timeout:     args.Timeout,
		Permissions: args.Permissions,
	})
}

// Write implements FilesystemAPI by posting the file content to the runtime
// files API. It is the single implementation of the write path, shared by the
// capability group and the WriteFileWithRuntime convenience wrapper.
func (f *filesystemAPI) Write(ctx context.Context, req WriteFileRequest) (WriteFileResult, error) {
	if req.FilePath == "" {
		return WriteFileResult{}, fmt.Errorf("filePath is required")
	}
	sbx := f.r.sbx
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx)).V(utils.DebugLogLevel)

	// The multipart write shares the transport decision with every other
	// capability group; runtimeFilesHTTPClient stays the plaintext client so the
	// existing test seam keeps working.
	base, httpClient, err := f.r.resolveTransport(sbx, runtimeFilesHTTPClient)
	if err != nil {
		return WriteFileResult{}, err
	}

	username := req.Username
	if username == "" {
		username = defaultRuntimeFilesUsername
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultRuntimeWriteTimeout
	}

	body, contentType, err := buildRuntimeFilesMultipartBody(req.FilePath, req.Content)
	if err != nil {
		return WriteFileResult{}, err
	}

	endpoint := buildRuntimeFilesEndpoint(base, req.FilePath, username)
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, body)
	if err != nil {
		return WriteFileResult{}, fmt.Errorf("failed to build runtime files write request: %w", err)
	}
	httpReq.Header.Set("Content-Type", contentType)
	if accessToken := utils.GetAccessToken(sbx); accessToken != "" {
		httpReq.Header.Set(accessTokenHeader, accessToken)
	}
	// The Basic user credential is caller-supplied: it is sent only when
	// req.AuthUser expresses a user identity for the runtime to resolve.
	if req.AuthUser != "" {
		httpReq.Header.Set("Authorization", basicAuthHeader(req.AuthUser))
	}

	start := time.Now()
	log.Info("writing file to runtime via files API",
		"filePath", req.FilePath,
		"endpoint", endpoint)

	resp, err := httpClient.Do(httpReq)
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
		"filePath", req.FilePath,
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

// Sentinel errors returned by the Filesystem service helpers below, so callers
// can classify a failure with errors.Is instead of matching message text or
// re-deriving connect codes themselves.
var (
	// ErrRuntimePathNotFound reports that the target path does not exist inside
	// the sandbox (connect code NotFound). It lets callers treat "nothing there"
	// as a benign outcome without swallowing genuine failures.
	ErrRuntimePathNotFound = errors.New("path not found in sandbox runtime")
	// ErrRuntimeFilesystemUnsupported reports that the sandbox runtime does not
	// serve the Filesystem service (connect code Unimplemented), which is how an
	// agent-runtime predating the service answers. Callers that treat the
	// capability as optional degrade on this error instead of failing.
	ErrRuntimeFilesystemUnsupported = errors.New("filesystem service unsupported by sandbox runtime")
)

// defaultRuntimeFilesystemTimeout bounds a single Filesystem RPC when the caller
// does not set one. Both operations are metadata-only, but each call pays for a
// connection setup, so the budget is sized for the round trip.
const defaultRuntimeFilesystemTimeout = 10 * time.Second

// ListDirArgs are the arguments accepted by ListDirWithRuntime.
type ListDirArgs struct {
	// Sbx is the target sandbox. Its annotations supply the runtime URL and
	// access token, resolved via GetRuntimeURL / GetAccessToken.
	Sbx *agentsv1alpha1.Sandbox
	// Path is the directory to list inside the sandbox.
	Path string
	// Depth bounds how deep the listing descends. Zero means the runtime default
	// of 1, i.e. the direct children of Path.
	Depth uint32
	// AuthUser is the user identity sent as an HTTP Basic Authorization header
	// (empty password). It is required rather than optional: the Filesystem
	// service resolves every path relative to an authenticated user and rejects
	// requests without one.
	AuthUser string
	// Timeout bounds the RPC. Defaults to defaultRuntimeFilesystemTimeout when
	// zero or negative.
	Timeout time.Duration
}

// ListDirWithRuntime lists the entries of a directory inside the sandbox via the
// envd Filesystem service.
//
// The returned entries describe the direct children of args.Path by default (see
// Depth); the directory itself is never included. Each entry carries its type, so
// callers can tell a regular file from a directory or a symlink without a second
// call. An empty directory yields no entries and no error.
//
// Errors are classified so callers can act on them:
//   - a missing directory wraps ErrRuntimePathNotFound;
//   - a runtime that does not serve the Filesystem service wraps
//     ErrRuntimeFilesystemUnsupported;
//   - everything else (unauthenticated, path is not a directory, transport
//     failure) is returned as a plain wrapped error.
//
// rtOpts selects the transport for this sandbox exactly as in
// WriteFileWithRuntime. The call is delegated to FilesystemAPI.ListDir.
func ListDirWithRuntime(ctx context.Context, args ListDirArgs, rtOpts ...Option) ([]*filesystem.EntryInfo, error) {
	if args.Sbx == nil {
		return nil, fmt.Errorf("sandbox is nil")
	}
	return NewRuntime(args.Sbx, rtOpts...).Filesystem().ListDir(ctx, ListDirRequest{
		Path:     args.Path,
		Depth:    args.Depth,
		AuthUser: args.AuthUser,
		Timeout:  args.Timeout,
	})
}

// ListDir implements FilesystemAPI. It is the single implementation of the
// listing path, shared by the capability group and the ListDirWithRuntime
// convenience wrapper.
func (f *filesystemAPI) ListDir(ctx context.Context, req ListDirRequest) ([]*filesystem.EntryInfo, error) {
	client, callCtx, cancel, err := f.newCall(ctx, req.Path, req.AuthUser, req.Timeout)
	if err != nil {
		return nil, err
	}
	defer cancel()

	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(f.r.sbx)).V(utils.DebugLogLevel)
	start := time.Now()

	resp, err := client.ListDir(callCtx, connect.NewRequest(&filesystem.ListDirRequest{
		Path:  req.Path,
		Depth: req.Depth,
	}))
	if err != nil {
		return nil, classifyFilesystemError("ListDir", req.Path, err)
	}

	entries := resp.Msg.GetEntries()
	log.Info("listed sandbox runtime directory", "path", req.Path,
		"entries", len(entries), "cost", time.Since(start))
	return entries, nil
}

// RemovePathArgs are the arguments accepted by RemovePathWithRuntime.
type RemovePathArgs struct {
	// Sbx is the target sandbox, used exactly as in ListDirArgs.
	Sbx *agentsv1alpha1.Sandbox
	// Path is the file or directory to remove inside the sandbox.
	Path string
	// AuthUser is the user identity sent as an HTTP Basic Authorization header
	// (empty password), required for the same reason as in ListDirArgs.
	AuthUser string
	// Timeout bounds the RPC. Defaults to defaultRuntimeFilesystemTimeout when
	// zero or negative.
	Timeout time.Duration
}

// RemovePathWithRuntime removes a single file or directory inside the sandbox via
// the envd Filesystem service.
//
// One property of the underlying service is load-bearing for callers:
//
//   - Removal is RECURSIVE. Passing a directory deletes its whole subtree, so a
//     caller that means to delete only files must check the entry type (which
//     ListDirWithRuntime reports) before calling this.
//
// Errors are classified so callers can act on them:
//   - a missing path wraps ErrRuntimePathNotFound: removal is NOT idempotent, so
//     a caller that treats "already gone" as success must ignore this error with
//     errors.Is;
//   - a runtime that does not serve the Filesystem service wraps
//     ErrRuntimeFilesystemUnsupported;
//   - everything else (unauthenticated, transport failure) is returned as a
//     plain wrapped error.
//
// rtOpts selects the transport for this sandbox exactly as in
// WriteFileWithRuntime. The call is delegated to FilesystemAPI.Remove.
func RemovePathWithRuntime(ctx context.Context, args RemovePathArgs, rtOpts ...Option) error {
	if args.Sbx == nil {
		return fmt.Errorf("sandbox is nil")
	}
	return NewRuntime(args.Sbx, rtOpts...).Filesystem().Remove(ctx, RemovePathRequest{
		Path:     args.Path,
		AuthUser: args.AuthUser,
		Timeout:  args.Timeout,
	})
}

// Remove implements FilesystemAPI. It is the single implementation of the
// removal path, shared by the capability group and the RemovePathWithRuntime
// convenience wrapper.
func (f *filesystemAPI) Remove(ctx context.Context, req RemovePathRequest) error {
	client, callCtx, cancel, err := f.newCall(ctx, req.Path, req.AuthUser, req.Timeout)
	if err != nil {
		return err
	}
	defer cancel()

	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(f.r.sbx)).V(utils.DebugLogLevel)
	start := time.Now()

	if _, err := client.Remove(callCtx, connect.NewRequest(&filesystem.RemoveRequest{
		Path: req.Path,
	})); err != nil {
		return classifyFilesystemError("Remove", req.Path, err)
	}

	log.Info("removed sandbox runtime path", "path", req.Path, "cost", time.Since(start))
	return nil
}

// newCall validates the shared arguments of the Filesystem RPCs and builds the
// connect client plus the per-call context carrying the timeout and the
// authentication headers. The returned cancel func must always be called.
//
// It mirrors the Process capability group's transport choices (connect over
// gRPC, the X-Access-Token header, Basic user identity) so the process and
// filesystem capabilities authenticate identically against the same runtime
// endpoint, and it resolves that endpoint through resolveTransport so a
// TLS-capable sandbox is reached over HTTPS.
func (f *filesystemAPI) newCall(ctx context.Context, path, authUser string,
	timeout time.Duration) (filesystemconnect.FilesystemClient, context.Context, context.CancelFunc, error) {
	if path == "" {
		return nil, nil, nil, fmt.Errorf("path is required")
	}
	// The service answers CodeUnauthenticated without a user identity, so an
	// empty AuthUser is rejected here rather than spending a round trip on a
	// request that cannot succeed.
	if authUser == "" {
		return nil, nil, nil, fmt.Errorf("authUser is required by the runtime filesystem service")
	}
	sbx := f.r.sbx
	base, httpClient, err := f.r.resolveTransport(sbx, runtimeGRPCHTTPClient)
	if err != nil {
		return nil, nil, nil, err
	}
	if timeout <= 0 {
		timeout = defaultRuntimeFilesystemTimeout
	}

	client := filesystemconnect.NewFilesystemClient(httpClient, base, connect.WithGRPC())

	ctxWithTimeout, cancel := context.WithTimeout(ctx, timeout)
	callCtx, callInfo := connect.NewClientContext(ctxWithTimeout)
	if token := utils.GetAccessToken(sbx); token != "" {
		callInfo.RequestHeader().Set(accessTokenHeader, token)
	}
	callInfo.RequestHeader().Set("Authorization", basicAuthHeader(authUser))
	return client, callCtx, cancel, nil
}

// classifyFilesystemError maps a Filesystem RPC failure onto the package
// sentinels, keeping the connect code out of the callers.
func classifyFilesystemError(rpcName, path string, err error) error {
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		switch connectErr.Code() {
		case connect.CodeNotFound:
			return fmt.Errorf("%w: %s: %v", ErrRuntimePathNotFound, path, err)
		case connect.CodeUnimplemented:
			return fmt.Errorf("%w: %s: %v", ErrRuntimeFilesystemUnsupported, rpcName, err)
		}
	}
	return fmt.Errorf("runtime filesystem %s on %s failed: %w", rpcName, path, err)
}
