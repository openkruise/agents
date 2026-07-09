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

func TestDoCommitWith_Success(t *testing.T) {
	opts := CommitOptions{ContainerID: "test-container-id", Image: "registry.example.com/app:v1"}

	fakeExec := func(ctx context.Context, opts ...CmdOpt) error {
		return nil
	}
	code := doCommitWith(context.Background(), opts, fakeExec)
	if code != ExitCodeSuccess {
		t.Errorf("expected ExitCodeSuccess (%d), got %d", ExitCodeSuccess, code)
	}
}

func TestDoCommitWith_CommitFailed(t *testing.T) {
	opts := CommitOptions{ContainerID: "test-container-id", Image: "registry.example.com/app:v1"}

	callCount := 0
	fakeExec := func(ctx context.Context, opts ...CmdOpt) error {
		callCount++
		// First call is commit, make it fail
		if callCount == 1 {
			return fmt.Errorf("commit error")
		}
		return nil
	}
	code := doCommitWith(context.Background(), opts, fakeExec)
	if code != ExitCodeCommitFailed {
		t.Errorf("expected ExitCodeCommitFailed (%d), got %d", ExitCodeCommitFailed, code)
	}
}

func TestDoCommitWith_PushFailed(t *testing.T) {
	opts := CommitOptions{ContainerID: "test-container-id", Image: "registry.example.com/app:v1"}

	callCount := 0
	fakeExec := func(ctx context.Context, opts ...CmdOpt) error {
		callCount++
		// Second call is push, make it fail
		if callCount == 2 {
			return fmt.Errorf("push error")
		}
		return nil
	}
	code := doCommitWith(context.Background(), opts, fakeExec)
	if code != ExitCodePushFailed {
		t.Errorf("expected ExitCodePushFailed (%d), got %d", ExitCodePushFailed, code)
	}
}

func TestDoCommitWith_EmptyContainerID(t *testing.T) {
	opts := CommitOptions{Image: "registry.example.com/app:v1"}

	called := false
	fakeExec := func(ctx context.Context, opts ...CmdOpt) error {
		called = true
		return nil
	}
	code := doCommitWith(context.Background(), opts, fakeExec)
	if code != ExitCodeCommitFailed {
		t.Errorf("expected ExitCodeCommitFailed (%d), got %d", ExitCodeCommitFailed, code)
	}
	if called {
		t.Error("executor should not be called when container ID is empty")
	}
}

func TestDoCommitWith_EmptyImage(t *testing.T) {
	opts := CommitOptions{ContainerID: "test-container-id"}

	called := false
	fakeExec := func(ctx context.Context, opts ...CmdOpt) error {
		called = true
		return nil
	}
	code := doCommitWith(context.Background(), opts, fakeExec)
	if code != ExitCodeCommitFailed {
		t.Errorf("expected ExitCodeCommitFailed (%d), got %d", ExitCodeCommitFailed, code)
	}
	if called {
		t.Error("executor should not be called when image is empty")
	}
}

func TestDoCommitWith_ArgsPassedCorrectly(t *testing.T) {
	opts := CommitOptions{ContainerID: "ctr-123", Image: "reg.io/img:v2"}

	var capturedCalls [][]string
	fakeExec := func(ctx context.Context, opts ...CmdOpt) error {
		cmd := exec.Command("echo")
		cmd.Args = []string{"nerdctl"}
		for _, opt := range opts {
			opt(cmd)
		}
		capturedCalls = append(capturedCalls, cmd.Args)
		return nil
	}
	code := doCommitWith(context.Background(), opts, fakeExec)
	if code != ExitCodeSuccess {
		t.Fatalf("expected success, got %d", code)
	}
	if len(capturedCalls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(capturedCalls))
	}
	// Verify commit args
	commitArgs := capturedCalls[0]
	if commitArgs[len(commitArgs)-2] != "ctr-123" || commitArgs[len(commitArgs)-1] != "reg.io/img:v2" {
		t.Errorf("commit args = %v", commitArgs)
	}
	// Verify push args
	pushArgs := capturedCalls[1]
	if pushArgs[len(pushArgs)-1] != "reg.io/img:v2" {
		t.Errorf("push args = %v", pushArgs)
	}
}
