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

package runtimeprovider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func init() {
	register(KindExecd, newExecdProvider)
}

// execd daemon endpoints, per specs/execd-api.yaml in the OpenSandbox project
// (https://github.com/alibaba/OpenSandbox/blob/main/specs/execd-api.yaml).
// execd listens on a fixed in-pod port (44772 by default) and authenticates
// every call via the X-EXECD-ACCESS-TOKEN header.
const (
	execdDefaultPort    = "44772"
	execdAccessTokenHdr = "X-EXECD-ACCESS-TOKEN"
	execdCommandPath    = "/command"
	execdUploadPath     = "/files/upload"
	execdDownloadPath   = "/files/download"
)

// execdProvider speaks the OpenSandbox execd REST/SSE protocol directly: no
// generated client exists for it in this repository yet, so requests are
// built and parsed by hand the same way pkg/utils/runtime/filesystem.go talks
// to envd's multipart /files route.
type execdProvider struct {
	baseURL     string
	accessToken string
	client      *http.Client
	// timeout bounds a call whose context carries no deadline of its own,
	// mirroring pkg/utils/runtime's defaultRuntimeTimeout. It is not applied
	// via http.Client.Timeout because command execution can legitimately run
	// long; WriteFile/ReadFile still benefit from a bound.
	timeout time.Duration
}

func newExecdProvider(endpoint string, opts ...Option) (Provider, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("runtimeprovider: execd: empty endpoint")
	}
	s := newSettings(opts)
	base := endpoint
	if !strings.Contains(base, "://") {
		host := base
		port := execdDefaultPort
		if h, p, ok := strings.Cut(base, ":"); ok {
			host, port = h, p
		}
		base = fmt.Sprintf("http://%s:%s", host, port)
	}
	return &execdProvider{baseURL: strings.TrimSuffix(base, "/"), client: s.httpClient, timeout: s.timeout}, nil
}

// withTimeout applies the provider's default deadline when ctx does not
// already carry one, so a caller that forgets to bound Exec or WriteFile
// cannot hang forever on a stuck daemon. It is not used by ReadFile: that call
// returns its response body for the caller to stream after this function
// returns, so imposing a deadline here would cancel a read still in progress.
func (p *execdProvider) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, p.timeout)
}

func (p *execdProvider) Kind() Kind { return KindExecd }

// Init stores the access token used to authenticate subsequent calls. execd
// (unlike envd) does not have a separate handshake endpoint in the lifecycle
// spec: the token is minted by the sandbox lifecycle API at creation time and
// simply presented on every execd call, so Init is a local, non-networked
// assignment.
func (p *execdProvider) Init(_ context.Context, opts InitOptions) error {
	if opts.AccessToken == "" {
		return fmt.Errorf("runtimeprovider: execd: access token is required")
	}
	p.accessToken = opts.AccessToken
	return nil
}

// execdCommandRequest mirrors the POST /command request body in
// specs/execd-api.yaml.
type execdCommandRequest struct {
	Cmd  string            `json:"cmd"`
	Args []string          `json:"args,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
	Cwd  string            `json:"cwd,omitempty"`
	User string            `json:"user,omitempty"`
}

// execdEventData is the JSON payload of one Server-Sent Event frame emitted
// by POST /command: "stdout"/"stderr" frames carry output chunks, a "status"
// frame carries the final exit status (and marks the stream complete).
type execdEventData struct {
	Chunk    string `json:"chunk"`
	ExitCode *int32 `json:"exitCode"`
	Exited   *bool  `json:"exited"`
}

func (p *execdProvider) Exec(ctx context.Context, opts ExecOptions) (ExecResult, error) {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	body, err := json.Marshal(execdCommandRequest{
		Cmd:  opts.Cmd,
		Args: opts.Args,
		Env:  opts.Envs,
		Cwd:  opts.Cwd,
		User: opts.AuthUser,
	})
	if err != nil {
		return ExecResult{}, fmt.Errorf("runtimeprovider: execd: marshal command: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+execdCommandPath, bytes.NewReader(body))
	if err != nil {
		return ExecResult{}, fmt.Errorf("runtimeprovider: execd: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(execdAccessTokenHdr, p.accessToken)

	resp, err := p.client.Do(req)
	if err != nil {
		return ExecResult{}, fmt.Errorf("runtimeprovider: execd: exec %q: %w", opts.Cmd, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ExecResult{}, execdStatusError(resp)
	}
	return parseExecdCommandStream(resp.Body)
}

// parseExecdCommandStream reads the SSE-formatted /command response and
// accumulates it into a single ExecResult. It implements just enough of SSE
// (event:/data: lines, blank-line-terminated frames) to consume execd's
// command stream; it is not a general-purpose SSE client.
func parseExecdCommandStream(r io.Reader) (ExecResult, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var result ExecResult
	var stdout, stderr strings.Builder
	var event, data string
	flush := func() error {
		if event == "" {
			return nil
		}
		defer func() { event, data = "", "" }()
		var payload execdEventData
		if data != "" {
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				return fmt.Errorf("runtimeprovider: execd: decode %s event: %w", event, err)
			}
		}
		switch event {
		case "stdout":
			stdout.WriteString(payload.Chunk)
		case "stderr":
			stderr.WriteString(payload.Chunk)
		case "status":
			if payload.ExitCode != nil {
				result.ExitCode = *payload.ExitCode
			}
			if payload.Exited != nil {
				result.Exited = *payload.Exited
			}
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return ExecResult{}, err
			}
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	if err := flush(); err != nil {
		return ExecResult{}, err
	}
	if err := scanner.Err(); err != nil {
		return ExecResult{}, fmt.Errorf("runtimeprovider: execd: read command stream: %w", err)
	}
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	return result, nil
}

func (p *execdProvider) WriteFile(ctx context.Context, opts WriteFileOptions) error {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", opts.Path)
	if err != nil {
		return fmt.Errorf("runtimeprovider: execd: build upload form: %w", err)
	}
	if _, err := io.Copy(part, opts.Content); err != nil {
		return fmt.Errorf("runtimeprovider: execd: read content: %w", err)
	}
	if opts.AuthUser != "" {
		if err := writer.WriteField("user", opts.AuthUser); err != nil {
			return fmt.Errorf("runtimeprovider: execd: write user field: %w", err)
		}
	}
	if err := writer.WriteField("path", opts.Path); err != nil {
		return fmt.Errorf("runtimeprovider: execd: write path field: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("runtimeprovider: execd: close upload form: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+execdUploadPath, &buf)
	if err != nil {
		return fmt.Errorf("runtimeprovider: execd: build request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set(execdAccessTokenHdr, p.accessToken)

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("runtimeprovider: execd: upload %q: %w", opts.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return execdStatusError(resp)
	}
	return nil
}

func (p *execdProvider) ReadFile(ctx context.Context, opts ReadFileOptions) (ReadFileResult, error) {
	q := url.Values{"path": []string{opts.Path}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+execdDownloadPath+"?"+q.Encode(), nil)
	if err != nil {
		return ReadFileResult{}, fmt.Errorf("runtimeprovider: execd: build request: %w", err)
	}
	if opts.AuthUser != "" {
		req.Header.Set("X-EXECD-USER", opts.AuthUser)
	}
	req.Header.Set(execdAccessTokenHdr, p.accessToken)

	resp, err := p.client.Do(req)
	if err != nil {
		return ReadFileResult{}, fmt.Errorf("runtimeprovider: execd: download %q: %w", opts.Path, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return ReadFileResult{}, execdStatusError(resp)
	}
	size := int64(-1)
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			size = n
		}
	}
	return ReadFileResult{Content: resp.Body, Size: size}, nil
}

// execdStatusError reads a bounded amount of the error body and wraps it with
// the HTTP status so callers can tell a daemon-reported failure from a
// transport failure.
func execdStatusError(resp *http.Response) error {
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("runtimeprovider: execd: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}
