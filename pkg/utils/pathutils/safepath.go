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

package pathutils

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// ValidateSafePath validates an OS-local path, rejecting empty paths and paths
// whose cleaned form retains ".." segments. It intentionally permits relative
// paths and the filesystem root for existing filesystem-writer callers. Use
// NormalizeAbsoluteNonRootPath for container mount destinations.
func ValidateSafePath(p string) error {
	if p == "" {
		return fmt.Errorf("path must not be empty")
	}
	clean := filepath.Clean(p)
	for _, seg := range strings.Split(clean, string(filepath.Separator)) {
		if seg == ".." {
			return fmt.Errorf("path must not contain '..' segments: %s", p)
		}
	}
	return nil
}

// NormalizeAbsoluteNonRootPath validates and canonicalizes an absolute,
// non-root POSIX path. Unlike ValidateSafePath, it rejects NUL bytes and
// parent-directory segments before cleaning so normalization cannot hide
// ambiguous traversal supplied by a caller. POSIX path semantics are
// intentional: these paths name locations inside Linux containers even when
// validation runs on another host OS.
func NormalizeAbsoluteNonRootPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	if strings.IndexByte(p, 0) >= 0 {
		return "", fmt.Errorf("path must not contain NUL bytes")
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == ".." {
			return "", fmt.Errorf("path must not contain '..' segments: %s", p)
		}
	}

	clean := path.Clean(p)
	if !path.IsAbs(clean) {
		return "", fmt.Errorf("path must be absolute: %s", p)
	}
	if clean == "/" {
		return "", fmt.Errorf("path must not be the filesystem root")
	}
	return clean, nil
}
