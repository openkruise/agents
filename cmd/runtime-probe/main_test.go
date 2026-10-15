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

package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunArgumentErrors(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantOutput string
	}{
		{name: "missing mode", wantCode: 2, wantOutput: "Usage:"},
		{name: "unknown mode", args: []string{"unknown"}, wantCode: 2, wantOutput: "Usage:"},
		{name: "bad duration", args: []string{"envd", "--timeout=bad"}, wantCode: 1, wantOutput: "invalid value"},
		{name: "extra argument", args: []string{"helper", "extra"}, wantCode: 1, wantOutput: "does not accept positional arguments"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := run(tt.args, &stderr); got != tt.wantCode {
				t.Fatalf("run() = %d, want %d; stderr=%q", got, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantOutput) {
				t.Fatalf("stderr = %q, want marker %q", stderr.String(), tt.wantOutput)
			}
		})
	}
}

func TestRunEnvdAndAuto(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	modeFile := filepath.Join(t.TempDir(), "mode")
	if err := os.WriteFile(modeFile, []byte("envd\n"+listener.Addr().String()+"\n"), 0o600); err != nil {
		t.Fatalf("write mode file: %v", err)
	}

	tests := []struct {
		name string
		args []string
	}{
		{name: "explicit envd", args: []string{"envd", "--address=" + listener.Addr().String()}},
		{name: "explicit local uses envd probe", args: []string{"local", "--address=" + listener.Addr().String()}},
		{name: "auto reads endpoint", args: []string{"auto", "--mode-file=" + modeFile}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := run(tt.args, &stderr); got != 0 {
				t.Fatalf("run() = %d, want 0; stderr=%q", got, stderr.String())
			}
		})
	}
}

func TestRunAutoRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		content string
		marker  string
	}{
		{name: "missing endpoint", content: "envd\n", marker: "invalid data-plane probe configuration"},
		{name: "unknown mode", content: "unknown\nendpoint\n", marker: "unsupported data-plane mode"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			modeFile := filepath.Join(t.TempDir(), "mode")
			if err := os.WriteFile(modeFile, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write mode file: %v", err)
			}
			var stderr bytes.Buffer
			if got := run([]string{"auto", "--mode-file=" + modeFile}, &stderr); got != 1 {
				t.Fatalf("run() = %d, want 1", got)
			}
			if !strings.Contains(stderr.String(), tt.marker) {
				t.Fatalf("stderr = %q, want marker %q", stderr.String(), tt.marker)
			}
		})
	}
}

func TestDefaultModeFilePath(t *testing.T) {
	t.Setenv("RUNTIME_STATE_DIR", "/custom/state")
	t.Setenv("TMPDIR", "/ignored")
	if got := defaultModeFilePath(); got != "/custom/state/runtime-probe-mode" {
		t.Fatalf("defaultModeFilePath() = %q", got)
	}
}
