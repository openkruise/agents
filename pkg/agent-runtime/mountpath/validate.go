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

// Package mountpath validates user-visible paths before storage is mounted or
// exposed there.
package mountpath

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ErrUnsafeMountPath marks a deterministic policy violation that retrying
// cannot change.
var ErrUnsafeMountPath = errors.New("unsafe mount path")

// UnsafePathExitCode is emitted by sandbox-runtime-storage when a command is
// rejected by this policy. It lets callers classify the result without parsing
// stderr that may contain user-controlled paths.
const UnsafePathExitCode = 64

var standardExecutableDirs = []string{
	"/bin",
	"/sbin",
	"/usr/bin",
	"/usr/sbin",
	"/usr/local/bin",
	"/usr/local/sbin",
}

// Validate rejects a user-visible mount path that can replace procfs or a
// directory used for executable lookup. pathEnv must come from the process that
// runs in the target container, never from the request being validated.
func Validate(pathValue, pathEnv string) error {
	if pathValue == "" {
		return unsafePathError(pathValue, "path must not be empty")
	}
	if strings.IndexByte(pathValue, 0) >= 0 {
		return unsafePathError(pathValue, "path must not contain NUL")
	}
	if !filepath.IsAbs(pathValue) {
		return unsafePathError(pathValue, "path must be absolute")
	}

	cleanPath := filepath.Clean(pathValue)
	if cleanPath == string(filepath.Separator) {
		return unsafePathError(pathValue, "filesystem root cannot be used as a mount path")
	}
	if isWithin(cleanPath, "/proc") {
		return unsafePathError(pathValue, "path is under /proc")
	}

	resolvedPath, traversedProc, err := resolveDestination(cleanPath)
	if err != nil {
		return fmt.Errorf("failed to resolve mount path %q: %w", pathValue, err)
	}
	if traversedProc || isWithin(resolvedPath, "/proc") {
		return unsafePathError(pathValue, "path resolves through /proc")
	}

	protectedDirs, err := executableDirs(pathEnv)
	if err != nil {
		return fmt.Errorf("failed to resolve executable directories: %w", err)
	}
	for _, protectedDir := range protectedDirs {
		if pathsOverlap(cleanPath, protectedDir) || pathsOverlap(resolvedPath, protectedDir) {
			return unsafePathError(pathValue, fmt.Sprintf("path overlaps executable directory %q", protectedDir))
		}
	}
	return nil
}

// resolveDestination resolves symlinks in the destination's parent while
// deliberately leaving the final component unresolved. The final component is
// the directory entry CreateSymlink replaces, so following an existing link at
// that position would validate its old target rather than the location being
// modified.
func resolveDestination(pathValue string) (string, bool, error) {
	parent, base := filepath.Dir(pathValue), filepath.Base(pathValue)
	resolvedParent, traversedProc, err := resolveExistingPrefix(parent)
	if err != nil {
		return "", false, err
	}
	return filepath.Join(resolvedParent, base), traversedProc, nil
}

// resolveExistingPrefix follows every symlink in the existing portion of an
// absolute path. Missing trailing components are appended unchanged so callers
// can validate paths whose parents will be created later. The bool reports
// whether any symlink target traversed procfs, including magic-link forms such
// as /proc/self/root that may resolve to a final path outside /proc.
func resolveExistingPrefix(pathValue string) (string, bool, error) {
	pending := splitAbsolutePath(filepath.Clean(pathValue))
	resolved := string(filepath.Separator)
	traversedProc := false
	followedLinks := 0

	for len(pending) > 0 {
		segment := pending[0]
		pending = pending[1:]
		next := filepath.Join(resolved, segment)
		info, err := os.Lstat(next)
		if err != nil {
			if os.IsNotExist(err) {
				parts := append([]string{resolved, segment}, pending...)
				return filepath.Join(parts...), traversedProc, nil
			}
			if errors.Is(err, os.ErrInvalid) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
				return "", traversedProc, unsafePathError(pathValue, err.Error())
			}
			return "", traversedProc, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved = next
			continue
		}

		followedLinks++
		if followedLinks > 255 {
			return "", traversedProc, unsafePathError(pathValue, "too many symbolic links")
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", traversedProc, err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(resolved, target)
		}
		target = filepath.Clean(target)
		if isWithin(target, "/proc") {
			traversedProc = true
		}
		pending = append(splitAbsolutePath(target), pending...)
		resolved = string(filepath.Separator)
	}
	return resolved, traversedProc, nil
}

func executableDirs(pathEnv string) ([]string, error) {
	entries := append([]string(nil), standardExecutableDirs...)
	entries = append(entries, filepath.SplitList(pathEnv)...)

	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(entries)*2)
	protected := make([]string, 0, len(entries)*2)
	add := func(pathValue string) {
		pathValue = filepath.Clean(pathValue)
		if _, ok := seen[pathValue]; ok {
			return
		}
		seen[pathValue] = struct{}{}
		protected = append(protected, pathValue)
	}

	for _, entry := range entries {
		if entry == "" {
			entry = cwd
		} else if !filepath.IsAbs(entry) {
			entry = filepath.Join(cwd, entry)
		}
		entry = filepath.Clean(entry)
		add(entry)

		resolved, _, resolveErr := resolveExistingPrefix(entry)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve PATH entry %q: %w", entry, resolveErr)
		}
		add(resolved)
	}
	return protected, nil
}

func splitAbsolutePath(pathValue string) []string {
	trimmed := strings.TrimPrefix(pathValue, string(filepath.Separator))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, string(filepath.Separator))
}

func pathsOverlap(first, second string) bool {
	return isWithin(first, second) || isWithin(second, first)
}

// isWithin reports whether pathValue is root or one of its descendants. It uses
// filepath.Rel so sibling names such as /proc-safe do not match /proc.
func isWithin(pathValue, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(pathValue))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

func unsafePathError(pathValue, reason string) error {
	return fmt.Errorf("%w: %q: %s", ErrUnsafeMountPath, pathValue, reason)
}
