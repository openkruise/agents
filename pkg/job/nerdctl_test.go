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
	"os/exec"
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
