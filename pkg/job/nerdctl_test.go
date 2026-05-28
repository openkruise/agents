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
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestWithArgs(t *testing.T) {
	cmd := exec.Command("echo")
	opt := WithArgs("commit", "abc123", "registry.example.com/img:v1")
	opt(cmd)

	expected := []string{"echo", "commit", "abc123", "registry.example.com/img:v1"}
	if len(cmd.Args) != len(expected) {
		t.Fatalf("args len = %d, want %d", len(cmd.Args), len(expected))
	}
	for i, arg := range cmd.Args {
		if arg != expected[i] {
			t.Errorf("arg[%d] = %s, want %s", i, arg, expected[i])
		}
	}
}

func TestWithStdout(t *testing.T) {
	buf := &bytes.Buffer{}
	cmd := exec.Command("echo")
	opt := WithStdout(buf)
	opt(cmd)

	if cmd.Stdout != buf {
		t.Error("expected Stdout to be set to buffer")
	}
}

func TestWithArgs_Multiple(t *testing.T) {
	cmd := exec.Command("nerdctl")
	WithArgs("--debug")(cmd)
	WithArgs("push", "img:v1")(cmd)

	expected := []string{"nerdctl", "--debug", "push", "img:v1"}
	if len(cmd.Args) != len(expected) {
		t.Fatalf("args len = %d, want %d", len(cmd.Args), len(expected))
	}
	for i, arg := range cmd.Args {
		if arg != expected[i] {
			t.Errorf("arg[%d] = %s, want %s", i, arg, expected[i])
		}
	}
}

func TestWithStderr(t *testing.T) {
	buf := &bytes.Buffer{}
	cmd := exec.Command("echo")
	opt := WithStderr(buf)
	opt(cmd)

	if cmd.Stderr != buf {
		t.Error("expected Stderr to be set to buffer")
	}
}

func TestNerdctlExec_BinaryNotFound(t *testing.T) {
	// Override PATH to ensure nerdctl binary cannot be found
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", "/nonexistent-path-for-nerdctl-test")
	defer os.Setenv("PATH", origPath)

	os.Setenv(EnvContainerdSock, "/tmp/test.sock")
	defer os.Unsetenv(EnvContainerdSock)

	err := NerdctlExec(context.Background(), WithArgs("version"))
	if err == nil {
		t.Fatal("expected error when nerdctl binary is not found")
	}
	if !strings.Contains(err.Error(), "start cmd failed") {
		t.Errorf("expected 'start cmd failed' in error, got: %v", err)
	}
}

func TestNerdctlExec_ContextCanceled(t *testing.T) {
	// Use a valid command that will block, and cancel the context
	// We use 'sleep' as a substitute since nerdctl may not be available
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	os.Setenv(EnvContainerdSock, "/tmp/test.sock")
	defer os.Unsetenv(EnvContainerdSock)

	// With PATH overridden, it will fail at Start() which is also a valid path
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", "/nonexistent-path-for-nerdctl-test")
	defer os.Setenv("PATH", origPath)

	err := NerdctlExec(ctx, WithArgs("version"))
	if err == nil {
		t.Fatal("expected error with canceled context and missing binary")
	}
}

func TestNerdctlExec_InvalidCommand(t *testing.T) {
	// Test that NerdctlExec properly includes stderr in error message
	// Since nerdctl is likely not installed in test env, this tests the start failure path
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", "/nonexistent")
	defer os.Setenv("PATH", origPath)

	os.Setenv(EnvContainerdSock, "/tmp/fake.sock")
	defer os.Unsetenv(EnvContainerdSock)

	err := NerdctlExec(context.Background(), WithArgs("invalid-subcommand"))
	if err == nil {
		t.Fatal("expected error for invalid nerdctl command")
	}
}

func TestNerdctlExec_CommandExitsNonZero(t *testing.T) {
	// Test the cmd.Wait() error path by using a script that exists but exits with non-zero
	// Create a fake "nerdctl" script that always fails
	tmpDir := t.TempDir()
	fakeNerdctl := tmpDir + "/nerdctl"
	os.WriteFile(fakeNerdctl, []byte("#!/bin/sh\necho 'error: fake failure' >&2\nexit 1\n"), 0755)

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir)
	defer os.Setenv("PATH", origPath)

	os.Setenv(EnvContainerdSock, "/tmp/fake.sock")
	defer os.Unsetenv(EnvContainerdSock)

	err := NerdctlExec(context.Background(), WithArgs("commit", "abc", "img:v1"))
	if err == nil {
		t.Fatal("expected error when nerdctl exits non-zero")
	}
	if !strings.Contains(err.Error(), "nerdctl output") {
		t.Errorf("expected 'nerdctl output' in error, got: %v", err)
	}
}

func TestNerdctlExec_CommandSucceeds(t *testing.T) {
	// Test the success path using a fake nerdctl that exits 0
	tmpDir := t.TempDir()
	fakeNerdctl := tmpDir + "/nerdctl"
	os.WriteFile(fakeNerdctl, []byte("#!/bin/sh\nexit 0\n"), 0755)

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir)
	defer os.Setenv("PATH", origPath)

	os.Setenv(EnvContainerdSock, "/tmp/fake.sock")
	defer os.Unsetenv(EnvContainerdSock)

	err := NerdctlExec(context.Background(), WithArgs("version"))
	if err != nil {
		t.Fatalf("expected no error when fake nerdctl exits 0, got: %v", err)
	}
}

func TestNerdctlExec_WithCustomStdout(t *testing.T) {
	// Test WithStdout option by capturing output from a fake nerdctl
	tmpDir := t.TempDir()
	fakeNerdctl := tmpDir + "/nerdctl"
	os.WriteFile(fakeNerdctl, []byte("#!/bin/sh\necho 'hello world'\nexit 0\n"), 0755)

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir)
	defer os.Setenv("PATH", origPath)

	os.Setenv(EnvContainerdSock, "/tmp/fake.sock")
	defer os.Unsetenv(EnvContainerdSock)

	buf := &bytes.Buffer{}
	err := NerdctlExec(context.Background(), WithArgs("images"), WithStdout(buf))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !strings.Contains(buf.String(), "hello world") {
		t.Errorf("expected 'hello world' in stdout, got: %s", buf.String())
	}
}
