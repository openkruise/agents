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

package mountpath

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestValidate(t *testing.T) {
	tempDir := t.TempDir()
	customBin := filepath.Join(tempDir, "tools", "bin")
	if err := os.MkdirAll(customBin, 0o755); err != nil {
		t.Fatalf("create custom PATH directory: %v", err)
	}
	parentLink := filepath.Join(tempDir, "usr-link")
	if err := os.Symlink("/usr", parentLink); err != nil {
		t.Fatalf("create parent symlink: %v", err)
	}

	tests := []struct {
		name      string
		path      string
		pathEnv   string
		wantError bool
	}{
		{name: "safe existing parent", path: filepath.Join(tempDir, "workspace", "data"), pathEnv: customBin},
		{name: "safe missing parents", path: filepath.Join(tempDir, "missing", "nested", "data"), pathEnv: customBin},
		{name: "empty path", path: "", pathEnv: customBin, wantError: true},
		{name: "relative path", path: "workspace/data", pathEnv: customBin, wantError: true},
		{name: "filesystem root", path: "/", pathEnv: customBin, wantError: true},
		{name: "proc root", path: "/proc", pathEnv: customBin, wantError: true},
		{name: "proc child", path: "/proc/self/root", pathEnv: customBin, wantError: true},
		{name: "proc sibling prefix", path: "/proc-safe/data", pathEnv: customBin},
		{name: "standard executable directory", path: "/usr/local/sbin", pathEnv: customBin, wantError: true},
		{name: "below standard executable directory", path: "/usr/local/sbin/extensions", pathEnv: customBin, wantError: true},
		{name: "ancestor of standard executable directory", path: "/usr/local", pathEnv: customBin, wantError: true},
		{name: "standard directory sibling prefix", path: "/usr/bin-safe", pathEnv: customBin},
		{name: "custom PATH directory", path: customBin, pathEnv: customBin, wantError: true},
		{name: "below custom PATH directory", path: filepath.Join(customBin, "plugins"), pathEnv: customBin, wantError: true},
		{name: "ancestor of custom PATH directory", path: filepath.Dir(customBin), pathEnv: customBin, wantError: true},
		{name: "symlinked parent resolves into executable directory", path: filepath.Join(parentLink, "bin"), pathEnv: customBin, wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.path, tt.pathEnv)
			if tt.wantError {
				if !errors.Is(err, ErrUnsafeMountPath) {
					t.Fatalf("Validate(%q) error = %v, want ErrUnsafeMountPath", tt.path, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate(%q) unexpected error: %v", tt.path, err)
			}
		})
	}
}

func TestValidateRelativeAndEmptyPATHEntries(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}

	separator := string(os.PathListSeparator)
	tests := []struct {
		name    string
		path    string
		pathEnv string
	}{
		{name: "empty PATH entry means working directory", path: filepath.Join(cwd, "mount"), pathEnv: separator},
		{name: "relative PATH entry resolves from working directory", path: filepath.Join(cwd, "tools"), pathEnv: "tools"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Validate(tt.path, tt.pathEnv); !errors.Is(err, ErrUnsafeMountPath) {
				t.Fatalf("Validate(%q) error = %v, want ErrUnsafeMountPath", tt.path, err)
			}
		})
	}
}

func TestValidateRejectsProcMagicLinkChain(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs magic links are Linux-specific")
	}

	parentLink := filepath.Join(t.TempDir(), "container-root")
	if err := os.Symlink("/proc/self/root", parentLink); err != nil {
		t.Fatalf("create procfs symlink: %v", err)
	}
	if err := Validate(filepath.Join(parentLink, "var", "data"), "/usr/bin"); !errors.Is(err, ErrUnsafeMountPath) {
		t.Fatalf("Validate through /proc/self/root error = %v, want ErrUnsafeMountPath", err)
	}
}
