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
	"sync/atomic"
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

func TestRunCommandWithRuntimeOutputOptions(t *testing.T) {
	sendChunks := func(stream *connect.ServerStream[process.StartResponse]) error {
		for i := 0; i < 5; i++ {
			if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Data{
				Data: &process.ProcessEvent_DataEvent{Output: &process.ProcessEvent_DataEvent_Stdout{Stdout: []byte("abcdefgh")}},
			}}}); err != nil {
				return err
			}
		}
		return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{
			End: &process.ProcessEvent_EndEvent{ExitCode: 0, Exited: true},
		}}})
	}

	t.Run("default captures all output", func(t *testing.T) {
		handler := &mockProcessHandler{startFn: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
			return sendChunks(stream)
		}}
		_, sbx := newMockRuntimeServer(t, handler)
		result, err := RunCommandWithRuntime(t.Context(), RunCmdFuncArgs{
			Sbx:           sbx,
			ProcessConfig: &process.ProcessConfig{Cmd: "true"},
			Timeout:       time.Second,
		})
		require.NoError(t, err)
		assert.Len(t, result.Stdout, 5)
	})

	t.Run("max bytes truncates capture", func(t *testing.T) {
		handler := &mockProcessHandler{startFn: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
			return sendChunks(stream)
		}}
		_, sbx := newMockRuntimeServer(t, handler)
		result, err := RunCommandWithRuntime(t.Context(), RunCmdFuncArgs{
			Sbx:           sbx,
			ProcessConfig: &process.ProcessConfig{Cmd: "true"},
			Timeout:       time.Second,
			OutputOptions: ProcessOutputOptions{MaxBytes: 20},
		})
		require.NoError(t, err)
		// Two full chunks plus a truncated third: 8+8+4 = 20 bytes.
		var captured int
		for _, s := range result.Stdout {
			captured += len(s)
		}
		assert.Equal(t, 20, captured)
		assert.Len(t, result.Stdout, 3)
		assert.Equal(t, "abcd", result.Stdout[2])
	})
}

func TestRunCommandWithRuntimeOutputOptionsEdgeCases(t *testing.T) {
	sendStdout := func(stream *connect.ServerStream[process.StartResponse], chunks ...string) error {
		for _, chunk := range chunks {
			event := &process.ProcessEvent{
				Event: &process.ProcessEvent_Data{
					Data: &process.ProcessEvent_DataEvent{
						Output: &process.ProcessEvent_DataEvent_Stdout{Stdout: []byte(chunk)},
					},
				},
			}
			if err := stream.Send(&process.StartResponse{Event: event}); err != nil {
				return err
			}
		}
		end := &process.ProcessEvent{
			Event: &process.ProcessEvent_End{
				End: &process.ProcessEvent_EndEvent{ExitCode: 0, Exited: true},
			},
		}
		return stream.Send(&process.StartResponse{Event: end})
	}

	t.Run("chunks past the bound are dropped entirely", func(t *testing.T) {
		handler := &mockProcessHandler{startFn: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
			// First chunk fills the cap exactly; the overflow chunks must be
			// dropped without appending even an empty string.
			return sendStdout(stream, "abcd", "overflow", "overflow")
		}}
		_, sbx := newMockRuntimeServer(t, handler)
		result, err := RunCommandWithRuntime(t.Context(), RunCmdFuncArgs{
			Sbx:           sbx,
			ProcessConfig: &process.ProcessConfig{Cmd: "true"},
			Timeout:       time.Second,
			OutputOptions: ProcessOutputOptions{MaxBytes: 4, SuppressResultLog: true},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"abcd"}, result.Stdout)
	})

	t.Run("suppressed result log still captures full output", func(t *testing.T) {
		handler := &mockProcessHandler{startFn: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
			return sendStdout(stream, "x", "x", "x")
		}}
		_, sbx := newMockRuntimeServer(t, handler)
		result, err := RunCommandWithRuntime(t.Context(), RunCmdFuncArgs{
			Sbx:           sbx,
			ProcessConfig: &process.ProcessConfig{Cmd: "true"},
			Timeout:       time.Second,
			OutputOptions: ProcessOutputOptions{SuppressResultLog: true},
		})
		require.NoError(t, err)
		// Suppression only affects logging; capture is unchanged when MaxBytes
		// is unset.
		assert.Len(t, result.Stdout, 3)
	})
}

// TestProcessRunClientTimeout covers the client-stream deadline on Process.Run.
// A non-positive Timeout must still deliver the RPC: context.WithTimeout treats
// 0 as already elapsed, and an E2B command timeout of 0 means no extra deadline.
func TestProcessRunClientTimeout(t *testing.T) {
	tests := []struct {
		name           string
		timeout        time.Duration
		parent         time.Duration
		blockUntilDone bool
		wantReached    bool
		wantErr        bool
	}{
		{
			name:        "zero timeout reaches the server",
			timeout:     0,
			wantReached: true,
		},
		{
			name:        "negative timeout reaches the server",
			timeout:     -time.Second,
			wantReached: true,
		},
		{
			name:        "positive timeout reaches the server",
			timeout:     time.Second,
			wantReached: true,
		},
		{
			name:           "positive timeout still expires",
			timeout:        200 * time.Millisecond,
			blockUntilDone: true,
			wantReached:    true,
			wantErr:        true,
		},
		{
			name:           "zero timeout still honors the parent deadline",
			timeout:        0,
			parent:         200 * time.Millisecond,
			blockUntilDone: true,
			wantReached:    true,
			wantErr:        true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reached atomic.Bool
			handler := &mockProcessHandler{startFn: func(ctx context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				reached.Store(true)
				if tt.blockUntilDone {
					<-ctx.Done()
					return ctx.Err()
				}
				return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{
					End: &process.ProcessEvent_EndEvent{ExitCode: 0, Exited: true},
				}}})
			}}
			_, sbx := newMockRuntimeServer(t, handler)

			ctx := t.Context()
			if tt.parent > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.parent)
				t.Cleanup(cancel)
			}
			result, err := NewRuntime(sbx).Process().Run(ctx, RunCommandRequest{
				ProcessConfig: &process.ProcessConfig{Cmd: "true"},
				Timeout:       tt.timeout,
			})
			assert.Equal(t, tt.wantReached, reached.Load(), "request reached server")
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.True(t, result.Exited)
			assert.Equal(t, int32(0), result.ExitCode)
		})
	}
}
