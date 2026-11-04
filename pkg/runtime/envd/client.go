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

// Start exposes the native stream rather than buffering the completed command.
func (c Client) Start(ctx context.Context, token, user, tag string, cfg *process.ProcessConfig) (*connect.ServerStreamForClient[process.StartResponse], error) {
	req := connect.NewRequest(&process.StartRequest{Process: cfg, Tag: &tag})
	authorize(req, token, user)
	if deadline, ok := ctx.Deadline(); ok {
		remaining := max(int64(1), time.Until(deadline).Milliseconds())
		req.Header().Set("Connect-Timeout-Ms", strconv.FormatInt(remaining, 10))
	}
	return processconnect.NewProcessClient(c.HTTP, c.BaseURL, connect.WithGRPC()).Start(ctx, req)
}

// Kill selects the unique execution tag, avoiding accidental PID reuse.
func (c Client) Kill(ctx context.Context, token, user, tag string) error {
	req := connect.NewRequest(&process.SendSignalRequest{
		Process: &process.ProcessSelector{Selector: &process.ProcessSelector_Tag{Tag: tag}},
		Signal:  process.Signal_SIGNAL_SIGKILL,
	})
	authorize(req, token, user)
	_, err := processconnect.NewProcessClient(c.HTTP, c.BaseURL, connect.WithGRPC()).SendSignal(ctx, req)
	return err
}

// FilesRequest addresses envd's binary file endpoint using its native query.
func (c Client) FilesRequest(ctx context.Context, method, token, user, path string, body io.Reader) (*http.Request, error) {
	query := url.Values{"username": {user}, "path": {path}}
	return c.Request(ctx, method, "/files?"+query.Encode(), token, user, body)
}

// Probe returns only the status, discarding environment or other sensitive data.
func (c Client) Probe(ctx context.Context, path, token string) (int, error) {
	r, err := c.Request(ctx, http.MethodGet, path, token, "", nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return 0, err
	}
	if err := resp.Body.Close(); err != nil {
		return 0, fmt.Errorf("close runtime probe: %w", err)
	}
	return resp.StatusCode, nil
}
