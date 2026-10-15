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
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRemoveSymlink enumerates the control-flow branches of RemoveSymlink:
// removing a real symlink, tolerating a missing path (idempotent unmount),
// refusing a real directory or file, and rejecting unsafe paths.
func TestRemoveSymlink(t *testing.T) {
	dir := t.TempDir()

	// Real symlink pointing at an existing directory.
	realTarget := filepath.Join(dir, "real-target")
	assert.NoError(t, os.MkdirAll(realTarget, 0o755))
	symlinkPath := filepath.Join(dir, "data")
	assert.NoError(t, os.Symlink(realTarget, symlinkPath))

	// Regular file at the link path — must be refused.
	regularFile := filepath.Join(dir, "regular-file")
	assert.NoError(t, os.WriteFile(regularFile, []byte("user data"), 0o644))

	// Real directory at the link path — must be refused.
	realDir := filepath.Join(dir, "real-dir")
	assert.NoError(t, os.MkdirAll(realDir, 0o755))

	tests := []struct {
		name        string
		link        string
		expectError string
	}{
		{
			name:        "removes an existing symlink",
			link:        symlinkPath,
			expectError: "",
		},
		{
			name:        "missing path is idempotent success",
			link:        filepath.Join(dir, "does-not-exist"),
			expectError: "",
		},
		{
			name:        "refuses a regular file",
			link:        regularFile,
			expectError: "is not a symlink, refusing to remove",
		},
		{
			name:        "refuses a real directory",
			link:        realDir,
			expectError: "is not a symlink, refusing to remove",
		},
		{
			name:        "rejects unsafe path",
			link:        "../escape",
			expectError: "invalid link path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RemoveSymlink(tt.link)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
		})
	}

	// The symlink case must actually have removed the link, leaving the target intact.
	_, err := os.Lstat(symlinkPath)
	assert.True(t, os.IsNotExist(err), "symlink must be gone after RemoveSymlink")
	_, err = os.Stat(realTarget)
	assert.NoError(t, err, "symlink target must be left intact")
}
