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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/proto/envd/process"
)

func TestInitRuntimePropagation(t *testing.T) {
	requestID := "test-propagation-id"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, requestID, r.Header.Get("X-Request-ID"))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sbx := newTestSandboxWithURL(server.URL)
	ctx := logs.WithRequestID(context.Background(), requestID)

	_, err := InitRuntime(ctx, sbx, config.InitRuntimeOptions{SkipRefresh: true}, nil)
	require.NoError(t, err)
}

func TestRunCommandWithRuntimePropagation(t *testing.T) {
	requestID := "test-grpc-propagation-id"

	handler := &mockProcessHandler{
		startFn: func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
			assert.Equal(t, requestID, req.Header().Get("X-Request-ID"))
			return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
				Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{ExitCode: 0, Exited: true}},
			}})
		},
	}
	_, sbx := newMockRuntimeServer(t, handler)

	ctx := logs.WithRequestID(context.Background(), requestID)

	_, err := RunCommandWithRuntime(ctx, RunCmdFuncArgs{
		Sbx:           sbx,
		ProcessConfig: &process.ProcessConfig{Cmd: "test"},
		Timeout:       5 * time.Second,
	})
	require.NoError(t, err)
}
