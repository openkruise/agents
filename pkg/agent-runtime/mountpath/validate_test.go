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
	"strings"
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
	relativeParentLink := filepath.Join(tempDir, "tools-link")
	if err := os.Symlink("tools", relativeParentLink); err != nil {
		t.Fatalf("create relative parent symlink: %v", err)
	}
	protectedChild := filepath.Join(customBin, "child")
	if err := os.MkdirAll(protectedChild, 0o755); err != nil {
		t.Fatalf("create protected child directory: %v", err)
	}
	parentTraversalLink := filepath.Join(tempDir, "parent-traversal-link")
	if err := os.Symlink(protectedChild, parentTraversalLink); err != nil {
		t.Fatalf("create parent traversal symlink: %v", err)
	}
	parentTraversalPath := parentTraversalLink + string(filepath.Separator) + ".." + string(filepath.Separator) + "payload"
	symlinkTargetParentLink := filepath.Join(tempDir, "symlink-target-parent-link")
	symlinkTargetWithParent := filepath.Base(parentTraversalLink) + string(filepath.Separator) + ".."
	if err := os.Symlink(symlinkTargetWithParent, symlinkTargetParentLink); err != nil {
		t.Fatalf("create symlink target parent traversal: %v", err)
	}
	rootLink := filepath.Join(tempDir, "root-link")
	if err := os.Symlink(string(filepath.Separator), rootLink); err != nil {
		t.Fatalf("create root symlink: %v", err)
	}
	fileParent := filepath.Join(tempDir, "file-parent")
	if err := os.WriteFile(fileParent, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("create parent file: %v", err)
	}
	relativeFileParentLink := filepath.Join(tempDir, "relative-file-parent-link")
	relativeFileParentTarget := filepath.Base(fileParent) + string(filepath.Separator) + ".." + string(filepath.Separator) + "workspace"
	if err := os.Symlink(relativeFileParentTarget, relativeFileParentLink); err != nil {
		t.Fatalf("create relative file parent symlink: %v", err)
	}
	absoluteFileParentLink := filepath.Join(tempDir, "absolute-file-parent-link")
	absoluteFileParentTarget := fileParent + string(filepath.Separator) + ".." + string(filepath.Separator) + "workspace"
	if err := os.Symlink(absoluteFileParentTarget, absoluteFileParentLink); err != nil {
		t.Fatalf("create absolute file parent symlink: %v", err)
	}
	dotFileParentLink := filepath.Join(tempDir, "dot-file-parent-link")
	dotFileParentTarget := filepath.Base(fileParent) + string(filepath.Separator) + "." + string(filepath.Separator) + "workspace"
	if err := os.Symlink(dotFileParentTarget, dotFileParentLink); err != nil {
		t.Fatalf("create dot file parent symlink: %v", err)
	}

	tests := []struct {
		name      string
		path      string
		pathEnv   string
		wantError bool
	}{
		{name: "safe existing parent", path: filepath.Join(tempDir, "workspace", "data"), pathEnv: customBin},
		{name: "safe missing parents", path: filepath.Join(tempDir, "missing", "nested", "data"), pathEnv: customBin},
		{name: "PATH entry resolving to root does not reject safe path", path: filepath.Join(tempDir, "workspace", "data"), pathEnv: rootLink},
		{name: "parent path is a file", path: filepath.Join(fileParent, "data"), pathEnv: customBin, wantError: true},
		{name: "empty path", path: "", pathEnv: customBin, wantError: true},
		{name: "path containing NUL", path: filepath.Join(tempDir, "workspace") + "\x00data", pathEnv: customBin, wantError: true},
		{name: "path containing parent segment", path: parentTraversalPath, pathEnv: customBin, wantError: true},
		{name: "symlink target parent segment resolves into PATH directory", path: filepath.Join(symlinkTargetParentLink, "payload"), pathEnv: customBin, wantError: true},
		{name: "relative symlink target cannot traverse through file", path: filepath.Join(relativeFileParentLink, "payload"), pathEnv: customBin, wantError: true},
		{name: "absolute symlink target cannot traverse through file", path: filepath.Join(absoluteFileParentLink, "payload"), pathEnv: customBin, wantError: true},
		{name: "symlink target dot segment cannot traverse through file", path: filepath.Join(dotFileParentLink, "payload"), pathEnv: customBin, wantError: true},
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
		{name: "relative symlinked parent resolves into PATH directory", path: filepath.Join(relativeParentLink, "bin", "data"), pathEnv: customBin, wantError: true},
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

func TestResolveExistingPrefixRejectsSymlinkLoop(t *testing.T) {
	tempDir := t.TempDir()
	first := filepath.Join(tempDir, "first")
	second := filepath.Join(tempDir, "second")
	if err := os.Symlink("second", first); err != nil {
		t.Fatalf("create first symlink: %v", err)
	}
	if err := os.Symlink("first", second); err != nil {
		t.Fatalf("create second symlink: %v", err)
	}

	_, _, err := resolveExistingPrefix(first)
	if !errors.Is(err, ErrUnsafeMountPath) {
		t.Fatalf("resolveExistingPrefix(%q) error = %v, want ErrUnsafeMountPath", first, err)
	}
	if !strings.Contains(err.Error(), "too many symbolic links") {
		t.Fatalf("resolveExistingPrefix(%q) error = %v, want symbolic link limit reason", first, err)
	}

	safeTarget := filepath.Join(tempDir, "safe", "mount")
	if err := Validate(safeTarget, first); !errors.Is(err, ErrUnsafeMountPath) {
		t.Fatalf("Validate(%q, %q) error = %v, want fail-closed ErrUnsafeMountPath", safeTarget, first, err)
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

func TestValidateEmptyPATHEntryDoesNotProtectFilesystemRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("filesystem root semantics are Unix-specific")
	}

	originalCWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalCWD); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	target := filepath.Join(t.TempDir(), "mount")
	if err := os.Chdir(string(filepath.Separator)); err != nil {
		t.Fatalf("change to filesystem root: %v", err)
	}

	if err := Validate(target, string(os.PathListSeparator)); err != nil {
		t.Fatalf("Validate(%q) with an empty PATH entry at filesystem root: %v", target, err)
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
