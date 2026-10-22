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
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/proto/envd/process"
)

func TestRunCommandWithRuntimeProcessTimeoutHeader(t *testing.T) {
	tests := []struct {
		name           string
		processTimeout time.Duration
		wantHeader     string
	}{
		{name: "unset preserves historical behavior"},
		{name: "server timeout is sent", processTimeout: 1500 * time.Millisecond, wantHeader: "1500"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &mockProcessHandler{startFn: func(_ context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				assert.Equal(t, tt.wantHeader, req.Header().Get("Connect-Timeout-Ms"))
				return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{
					End: &process.ProcessEvent_EndEvent{ExitCode: 0, Exited: true},
				}}})
			}}
			_, sbx := newMockRuntimeServer(t, handler)

			_, err := RunCommandWithRuntime(t.Context(), RunCmdFuncArgs{
				Sbx:            sbx,
				ProcessConfig:  &process.ProcessConfig{Cmd: "true"},
				Timeout:        time.Second,
				ProcessTimeout: tt.processTimeout,
			})
			require.NoError(t, err)
		})
	}
}
