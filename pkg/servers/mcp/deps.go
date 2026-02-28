/*
Copyright 2026 The Kruise Authors.

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

package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	sandbox_manager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
)

// defaultSessionDeps is the production implementation of SessionDependencies
type defaultSessionDeps struct {
	manager         *sandbox_manager.SandboxManager
	sessionSyncPort int
}

// NewDefaultSessionDeps creates the default production dependencies
func NewDefaultSessionDeps(manager *sandbox_manager.SandboxManager, sessionSyncPort int) SessionDependencies {
	return &defaultSessionDeps{
		manager:         manager,
		sessionSyncPort: sessionSyncPort,
	}
}

// CreateSandbox creates a new sandbox for the user
func (d *defaultSessionDeps) CreateSandbox(ctx context.Context, userID, sessionID, templateID string, sandboxTTL time.Duration) (*SandboxInfo, error) {
	return CreateSandbox(ctx, d.manager, userID, sessionID, templateID, sandboxTTL)
}

// RequestPeer sends an HTTP request to a peer node
func (d *defaultSessionDeps) RequestPeer(method, ip, path string, body []byte) error {
	var buf io.Reader
	if len(body) > 0 {
		buf = bytes.NewReader(body)
	}
	request, err := http.NewRequest(method, fmt.Sprintf("http://%s:%d%s", ip, d.sessionSyncPort, path), buf)
	if err != nil {
		return err
	}

	client := &http.Client{
		Timeout: consts.RequestPeerTimeout,
	}
	resp, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)

	return nil
}
