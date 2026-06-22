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

package link

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCreateSymlinkBranches enumerates every reachable control-flow branch
// of CreateSymlink using table-driven cases. Each case prepares a unique
// scratch sub-directory and reports the expected outcome (success or a
// substring of the error message). Covers: empty link, target-missing,
// target-not-directory, MkdirAll failure (parent is a file), trailing
// slash trim, idempotent existing-symlink, wrong-target replacement,
// link-is-regular-file rejection, link-is-empty-dir replacement,
// link-is-non-empty-dir rejection.
func TestCreateSymlinkBranches(t *testing.T) {
	type assertion struct {
		wantSymlink bool   // link must end up as a symlink pointing at target
		wantTarget  string // explicit target to validate; empty means "target arg"
	}

	tests := []struct {
		name        string
		// setup returns (target, link). If empty, the field is taken as-is.
		setup       func(t *testing.T, dir string) (target, link string)
		expectError string
		assert      assertion
	}{
		{
			name: "empty link path after trim returns error",
			setup: func(t *testing.T, dir string) (string, string) {
				tgt := filepath.Join(dir, "tgt")
				if err := os.Mkdir(tgt, 0o755); err != nil {
					t.Fatalf("mkdir tgt: %v", err)
				}
				// trailing slash will be trimmed to empty string.
				return tgt, "/"
			},
			expectError: "link path cannot be empty",
		},
		{
			name: "target does not exist returns error",
			setup: func(t *testing.T, dir string) (string, string) {
				return filepath.Join(dir, "missing-target"), filepath.Join(dir, "link")
			},
			expectError: "target does not exist:",
		},
		{
			name: "target exists but is not a directory returns error",
			setup: func(t *testing.T, dir string) (string, string) {
				tgt := filepath.Join(dir, "file.txt")
				if err := os.WriteFile(tgt, []byte("x"), 0o644); err != nil {
					t.Fatalf("write file: %v", err)
				}
				return tgt, filepath.Join(dir, "link")
			},
			expectError: "target is not a directory:",
		},
		{
			name: "MkdirAll fails because link parent is a regular file",
			setup: func(t *testing.T, dir string) (string, string) {
				tgt := filepath.Join(dir, "tgt")
				if err := os.Mkdir(tgt, 0o755); err != nil {
					t.Fatalf("mkdir tgt: %v", err)
				}
				// Make a *file* and ask CreateSymlink to put the link
				// inside it. MkdirAll on "<file>/sub" must fail.
				parentFile := filepath.Join(dir, "not-a-dir")
				if err := os.WriteFile(parentFile, []byte("x"), 0o644); err != nil {
					t.Fatalf("write parentFile: %v", err)
				}
				return tgt, filepath.Join(parentFile, "sub", "link")
			},
			expectError: "failed to create parent directory",
		},
		{
			name: "link path with trailing slash succeeds (trimmed)",
			setup: func(t *testing.T, dir string) (string, string) {
				tgt := filepath.Join(dir, "tgt")
				if err := os.Mkdir(tgt, 0o755); err != nil {
					t.Fatalf("mkdir tgt: %v", err)
				}
				return tgt, filepath.Join(dir, "link") + "/"
			},
			assert: assertion{wantSymlink: true},
		},
		{
			name: "existing symlink with same target is idempotent",
			setup: func(t *testing.T, dir string) (string, string) {
				tgt := filepath.Join(dir, "tgt")
				if err := os.Mkdir(tgt, 0o755); err != nil {
					t.Fatalf("mkdir tgt: %v", err)
				}
				link := filepath.Join(dir, "link")
				if err := os.Symlink(tgt, link); err != nil {
					t.Fatalf("prep symlink: %v", err)
				}
				return tgt, link
			},
			assert: assertion{wantSymlink: true},
		},
		{
			name: "existing symlink with wrong target is replaced",
			setup: func(t *testing.T, dir string) (string, string) {
				wrongTgt := filepath.Join(dir, "wrong")
				if err := os.Mkdir(wrongTgt, 0o755); err != nil {
					t.Fatalf("mkdir wrong: %v", err)
				}
				tgt := filepath.Join(dir, "tgt")
				if err := os.Mkdir(tgt, 0o755); err != nil {
					t.Fatalf("mkdir tgt: %v", err)
				}
				link := filepath.Join(dir, "link")
				if err := os.Symlink(wrongTgt, link); err != nil {
					t.Fatalf("prep wrong symlink: %v", err)
				}
				return tgt, link
			},
			assert: assertion{wantSymlink: true},
		},
		{
			name: "link path exists as regular file returns non-directory error",
			setup: func(t *testing.T, dir string) (string, string) {
				tgt := filepath.Join(dir, "tgt")
				if err := os.Mkdir(tgt, 0o755); err != nil {
					t.Fatalf("mkdir tgt: %v", err)
				}
				link := filepath.Join(dir, "link")
				if err := os.WriteFile(link, []byte("x"), 0o644); err != nil {
					t.Fatalf("write link file: %v", err)
				}
				return tgt, link
			},
			expectError: "link path exists as a non-directory:",
		},
		{
			name: "link path is empty directory and gets replaced",
			setup: func(t *testing.T, dir string) (string, string) {
				tgt := filepath.Join(dir, "tgt")
				if err := os.Mkdir(tgt, 0o755); err != nil {
					t.Fatalf("mkdir tgt: %v", err)
				}
				link := filepath.Join(dir, "link")
				if err := os.Mkdir(link, 0o755); err != nil {
					t.Fatalf("mkdir link dir: %v", err)
				}
				return tgt, link
			},
			assert: assertion{wantSymlink: true},
		},
		{
			name: "link path is non-empty directory returns not-empty error",
			setup: func(t *testing.T, dir string) (string, string) {
				tgt := filepath.Join(dir, "tgt")
				if err := os.Mkdir(tgt, 0o755); err != nil {
					t.Fatalf("mkdir tgt: %v", err)
				}
				link := filepath.Join(dir, "link")
				if err := os.Mkdir(link, 0o755); err != nil {
					t.Fatalf("mkdir link dir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(link, "child"), []byte("x"), 0o644); err != nil {
					t.Fatalf("write child: %v", err)
				}
				return tgt, link
			},
			expectError: "directory is not empty:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			target, link := tt.setup(t, dir)

			err := CreateSymlink(target, link)

			if tt.expectError == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if tt.assert.wantSymlink {
					assertSymlink(t, link, target)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.expectError)
			}
			if !strings.Contains(err.Error(), tt.expectError) {
				t.Errorf("want error containing %q, got %q", tt.expectError, err.Error())
			}
		})
	}
}

// assertSymlink fails the test if link is not a symlink whose target is
// exactly wantTarget. The caller must have already established that
// CreateSymlink returned no error.
func assertSymlink(t *testing.T, link, wantTarget string) {
	t.Helper()
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link %q: %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		// Trailing-slash case: when link arg has a trailing slash, it is
		// trimmed inside CreateSymlink, so the final path on disk is
		// without the slash. Recompute and re-check.
		trimmed := strings.TrimRight(link, "/")
		if trimmed == link {
			t.Fatalf("path %q is not a symlink: mode=%v", link, info.Mode())
		}
		info, err = os.Lstat(trimmed)
		if err != nil {
			t.Fatalf("lstat trimmed link %q: %v", trimmed, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("trimmed path %q is not a symlink", trimmed)
		}
		link = trimmed
	}
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink %q: %v", link, err)
	}
	if got != wantTarget {
		t.Errorf("symlink target: want %q, got %q", wantTarget, got)
	}
}
