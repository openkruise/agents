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
	"os/exec"
	"strings"
	"testing"
)

func TestNerdctlExec_BinaryNotFound(t *testing.T) {
	// nerdctl binary doesn't exist in test env, should fail with "nerdctl binary not found"
	err := NerdctlExec(context.Background(), WithArgs("version"))
	if err == nil {
		t.Fatal("expected error when nerdctl binary not found")
	}
	if !strings.Contains(err.Error(), "nerdctl binary not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNerdctlExec_WaitFailed(t *testing.T) {
	// Use "false" binary which exits with code 1 to cover the Wait() error path.
	falseBin, err := exec.LookPath("false")
	if err != nil {
		t.Skip("false binary not available")
	}
	err = NerdctlExec(context.Background(), WithBinary(falseBin))
	if err == nil {
		t.Fatal("expected error from false binary")
	}
	if !strings.Contains(err.Error(), "nerdctl output") {
		t.Errorf("unexpected error format: %v", err)
	}
}

func TestNerdctlExec_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	// Should still fail with binary not found (cancellation is a secondary path)
	err := NerdctlExec(ctx, WithArgs("version"))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestWithArgs(t *testing.T) {
	cmd := exec.Command("echo")
	cmd.Args = []string{"nerdctl", "--debug"}
	opt := WithArgs("commit", "abc123", "image:v1")
	opt(cmd)
	want := []string{"nerdctl", "--debug", "commit", "abc123", "image:v1"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("Args length = %d, want %d", len(cmd.Args), len(want))
	}
	for i, arg := range cmd.Args {
		if arg != want[i] {
			t.Errorf("Args[%d] = %q, want %q", i, arg, want[i])
		}
	}
}

func TestWithBinary(t *testing.T) {
	cmd := exec.Command("nerdctl")
	opt := WithBinary("/usr/bin/false")
	opt(cmd)
	if cmd.Path != "/usr/bin/false" {
		t.Errorf("Path = %q, want /usr/bin/false", cmd.Path)
	}
}
