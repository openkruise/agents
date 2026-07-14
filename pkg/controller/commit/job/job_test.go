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

package job

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
)

func TestDoCommitWith(t *testing.T) {
	tests := []struct {
		name      string
		opts      CommitOptions
		executor  Executor
		wantCode  int
		wantCalls int // expected number of executor invocations; -1 means skip check
		// verifyArgs is called after a successful run to inspect the captured args.
		verifyArgs func(t *testing.T, calls [][]string)
	}{
		{
			name: "success",
			opts: CommitOptions{ContainerID: "test-container-id", Image: "registry.example.com/app:v1"},
			executor: func(ctx context.Context, opts ...CmdOpt) error {
				return nil
			},
			wantCode:  ExitCodeSuccess,
			wantCalls: 2,
		},
		{
			name: "commit fails",
			opts: CommitOptions{ContainerID: "test-container-id", Image: "registry.example.com/app:v1"},
			executor: func() Executor {
				callCount := 0
				return func(ctx context.Context, opts ...CmdOpt) error {
					callCount++
					if callCount == 1 {
						return fmt.Errorf("commit error")
					}
					return nil
				}
			}(),
			wantCode:  ExitCodeCommitFailed,
			wantCalls: 1,
		},
		{
			name: "push fails",
			opts: CommitOptions{ContainerID: "test-container-id", Image: "registry.example.com/app:v1"},
			executor: func() Executor {
				callCount := 0
				return func(ctx context.Context, opts ...CmdOpt) error {
					callCount++
					if callCount == 2 {
						return fmt.Errorf("push error")
					}
					return nil
				}
			}(),
			wantCode:  ExitCodePushFailed,
			wantCalls: 2,
		},
		{
			name:      "empty container ID",
			opts:      CommitOptions{Image: "registry.example.com/app:v1"},
			executor:  func(ctx context.Context, opts ...CmdOpt) error { return nil },
			wantCode:  ExitCodeCommitFailed,
			wantCalls: 0,
		},
		{
			name:      "empty image",
			opts:      CommitOptions{ContainerID: "test-container-id"},
			executor:  func(ctx context.Context, opts ...CmdOpt) error { return nil },
			wantCode:  ExitCodeCommitFailed,
			wantCalls: 0,
		},
		{
			name: "args passed correctly",
			opts: CommitOptions{ContainerID: "ctr-123", Image: "reg.io/img:v2"},
			executor: func(ctx context.Context, opts ...CmdOpt) error {
				return nil
			},
			wantCode:  ExitCodeSuccess,
			wantCalls: 2,
			verifyArgs: func(t *testing.T, calls [][]string) {
				t.Helper()
				if len(calls) != 2 {
					t.Fatalf("expected 2 calls, got %d", len(calls))
				}
				commitArgs := calls[0]
				if commitArgs[len(commitArgs)-2] != "ctr-123" || commitArgs[len(commitArgs)-1] != "reg.io/img:v2" {
					t.Errorf("commit args = %v", commitArgs)
				}
				pushArgs := calls[1]
				if pushArgs[len(pushArgs)-1] != "reg.io/img:v2" {
					t.Errorf("push args = %v", pushArgs)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			callCount := 0
			var capturedCalls [][]string
			wrappedExec := func(ctx context.Context, opts ...CmdOpt) error {
				callCount++
				// Capture args by applying opts to a dummy cmd.
				cmd := exec.Command("echo")
				cmd.Args = []string{"nerdctl"}
				for _, opt := range opts {
					opt(cmd)
				}
				capturedCalls = append(capturedCalls, cmd.Args)
				return tt.executor(ctx, opts...)
			}

			code := doCommitWith(context.Background(), tt.opts, wrappedExec)
			if code != tt.wantCode {
				t.Errorf("expected exit code %d, got %d", tt.wantCode, code)
			}
			if tt.wantCalls >= 0 && callCount != tt.wantCalls {
				t.Errorf("expected %d executor calls, got %d", tt.wantCalls, callCount)
			}
			if tt.verifyArgs != nil {
				tt.verifyArgs(t, capturedCalls)
			}
		})
	}
}
