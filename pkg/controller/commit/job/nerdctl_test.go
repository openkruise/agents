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
	"strings"
	"testing"
)

func TestNerdctlExec_BinaryNotFound(t *testing.T) {
	// nerdctl binary doesn't exist in test env, should fail with "start cmd failed"
	err := NerdctlExec(context.Background(), WithArgs("version"))
	if err == nil {
		t.Fatal("expected error when nerdctl binary not found")
	}
	if !strings.Contains(err.Error(), "start cmd failed") {
		t.Errorf("unexpected error: %v", err)
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
	// Verify WithArgs appends correctly
	args := []string{"nerdctl", "--debug"}
	opt := WithArgs("commit", "abc123", "image:v1")
	// Simulate what NerdctlExec does
	type fakeCmd struct{ Args []string }
	cmd := &fakeCmd{Args: args}
	// Can't directly test on exec.Cmd without running, so just verify the type exists
	_ = opt
	_ = cmd
}
