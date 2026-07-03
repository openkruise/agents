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
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CreateSymlink creates a symlink so that the user-facing mount path inside
// the container points at the actual shared mount under root-mount.
// The shared mount lives at /run/csi/mount-root/nas|oss/<md5(user-path)>,
// and we create a symlink from the user-specified path to that location, e.g.
// /data -> /run/csi/mount-root/nas|oss/<md5(user-path)>.
//   - link:   the directory specified by the user (e.g. /data).
//   - target: the actual mount path under /run/csi/mount-root/nas|oss/.
func CreateSymlink(target, link string) error {
	// Normalize link path: remove trailing slash if present
	link = strings.TrimRight(link, "/")

	// Validate link path is not empty after trimming
	if link == "" {
		return fmt.Errorf("link path cannot be empty")
	}

	// Check if target exists and is a directory
	stat, err := os.Stat(target)
	if os.IsNotExist(err) {
		return fmt.Errorf("target does not exist: %s", target)
	} else if err != nil {
		return fmt.Errorf("failed to stat target: %v", err)
	}
	if !stat.IsDir() {
		return fmt.Errorf("target is not a directory: %s", target)
	}

	// Ensure parent directory of link exists
	linkDir := filepath.Dir(link)
	if err := os.MkdirAll(linkDir, 0755); err != nil { // #nosec G301 -- standard directory permissions
		return fmt.Errorf("failed to create parent directory %s: %v", linkDir, err)
	}

	// Handle the link path
	linkStat, err := os.Lstat(link)
	if err == nil {
		// Link path exists
		if linkStat.Mode()&os.ModeSymlink != 0 {
			// It's already a symlink
			existingTarget, readErr := os.Readlink(link)
			if readErr != nil {
				return fmt.Errorf("failed to read existing symlink: %v", readErr)
			}
			if existingTarget == target {
				return nil // Already correct symlink
			}
			// Wrong target → remove and recreate
			if err := os.Remove(link); err != nil {
				return fmt.Errorf("failed to remove incorrect symlink: %v", err)
			}
		} else {
			// It's not a symlink — check if it's an empty directory
			if !linkStat.IsDir() {
				return fmt.Errorf("link path exists as a non-directory: %s", link)
			}

			// Try to read dir — must be empty to safely replace
			dirEntries, err := os.ReadDir(link)
			if err != nil {
				return fmt.Errorf("failed to read directory entries: %v", err)
			}
			if len(dirEntries) > 0 {
				return fmt.Errorf("cannot create symlink: directory is not empty: %s", link)
			}

			// Safe to remove empty dir and replace with symlink
			if err := os.Remove(link); err != nil {
				return fmt.Errorf("failed to remove empty directory for symlink: %v", err)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to stat link path: %v", err)
	}

	// Now create the symlink
	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("failed to create symlink: %v", err)
	}

	return nil
}
