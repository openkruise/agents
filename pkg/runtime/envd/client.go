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

// Package envd provides native execution transport without Kubernetes or
// sandbox-manager dependencies. Callers own protocol adaptation and policy.
package envd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
)

// Client is a concrete transport to one workload's envd process.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// Request adds the native runtime credential and execution-user identity.
func (c Client) Request(ctx context.Context, method, path, token, user string, body io.Reader) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	r.Header.Set("X-Access-Token", token)
	if user != "" {
		r.SetBasicAuth(user, "")
	}
	return r, nil
}

func authorize[T any](req *connect.Request[T], token, user string) {
	req.Header().Set("X-Access-Token", token)
	if user != "" {
		r := &http.Request{Header: req.Header()}
		r.SetBasicAuth(user, "")
	}
}

// Start exposes the native stream and keeps its execution timeout separate from
// the caller's transport deadline. A non-positive timeout adds no process limit.
func (c Client) Start(ctx context.Context, token, user string, cfg *process.ProcessConfig, executionTimeout time.Duration) (*connect.ServerStreamForClient[process.StartResponse], error) {
	req := connect.NewRequest(&process.StartRequest{Process: cfg})
	authorize(req, token, user)
	// Match the command stream keepalive interval in E2B SDK 2.51.0.
	req.Header().Set("Keepalive-Ping-Interval", "50")
	if executionTimeout > 0 {
		req.Header().Set("Connect-Timeout-Ms", strconv.FormatInt(max(int64(1), executionTimeout.Milliseconds()), 10))
	}
	return processconnect.NewProcessClient(c.HTTP, c.BaseURL, connect.WithGRPC()).Start(ctx, req)
}

// Kill uses the native PID selector and reports the signal RPC's result.
func (c Client) Kill(ctx context.Context, token, user string, pid uint32) error {
	req := connect.NewRequest(&process.SendSignalRequest{
		Process: &process.ProcessSelector{Selector: &process.ProcessSelector_Pid{Pid: pid}},
		Signal:  process.Signal_SIGNAL_SIGKILL,
	})
	authorize(req, token, user)
	_, err := processconnect.NewProcessClient(c.HTTP, c.BaseURL, connect.WithGRPC()).SendSignal(ctx, req)
	return err
}

// FilesRequest addresses envd's binary file endpoint using its native query.
func (c Client) FilesRequest(ctx context.Context, method, token, user, path string, body io.Reader) (*http.Request, error) {
	query := url.Values{"username": {user}}
	if path != "" {
		query.Set("path", path)
	}
	return c.Request(ctx, method, "/files?"+query.Encode(), token, user, body)
}

// ProbeList checks the authenticated native process endpoint without decoding
// or retaining process metadata. HTTP status is preserved for readiness checks.
func (c Client) ProbeList(ctx context.Context, token string) (int, error) {
	r, err := c.Request(ctx, http.MethodPost, processconnect.ProcessListProcedure, token, "", strings.NewReader("{}"))
	if err != nil {
		return 0, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Connect-Protocol-Version", "1")
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return 0, err
	}
	if err := resp.Body.Close(); err != nil {
		return resp.StatusCode, fmt.Errorf("close runtime List response: %w", err)
	}
	return resp.StatusCode, nil
}
