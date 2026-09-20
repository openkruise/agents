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

// Package runtimeprobe checks the in-container data plane used by agent-runtime.
package runtimeprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

const (
	helperHealthPath          = "/health"
	helperHealthStatusServing = "SERVING"
	maxHelperHealthBodyBytes  = 4 * 1024
)

type helperHealthResponse struct {
	Status string `json:"status"`
}

// ProbeEnvd verifies that envd accepts TCP connections at address. A successful
// connection is the compatibility-safe liveness contract because not every envd
// version exposes an unauthenticated HTTP health endpoint.
func ProbeEnvd(ctx context.Context, address string, timeout time.Duration) (retErr error) {
	if address == "" {
		return fmt.Errorf("envd address must not be empty")
	}
	if timeout <= 0 {
		return fmt.Errorf("probe timeout must be positive")
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(probeCtx, "tcp", address)
	if err != nil {
		return fmt.Errorf("connect to envd at %q: %w", address, err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close envd probe connection: %w", closeErr))
		}
	}()
	return nil
}

// ProbeHelper verifies that agent-helper answers GET /health over its UNIX
// domain socket with HTTP 200 and the SERVING status.
func ProbeHelper(ctx context.Context, socketPath string, timeout time.Duration) (retErr error) {
	if socketPath == "" {
		return fmt.Errorf("helper socket path must not be empty")
	}
	if timeout <= 0 {
		return fmt.Errorf("probe timeout must be positive")
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://helper"+helperHealthPath, nil)
	if err != nil {
		return fmt.Errorf("build helper health request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("reach helper socket %q: %w", socketPath, err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close helper health response: %w", closeErr))
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("helper health returned status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxHelperHealthBodyBytes+1))
	if err != nil {
		return fmt.Errorf("read helper health response: %w", err)
	}
	if len(data) > maxHelperHealthBodyBytes {
		return fmt.Errorf("helper health response exceeds %d bytes", maxHelperHealthBodyBytes)
	}
	var body helperHealthResponse
	if err := json.Unmarshal(data, &body); err != nil {
		return fmt.Errorf("decode helper health response: %w", err)
	}
	if body.Status != helperHealthStatusServing {
		return fmt.Errorf("helper reported unhealthy status %q", body.Status)
	}
	return nil
}
