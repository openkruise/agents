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

// ErrMountPathUnverifiable marks a failure to evaluate the policy against the
// local filesystem. The requested path may be safe, but the sandbox environment
// prevented a decision, so the mount is refused.
var ErrMountPathUnverifiable = errors.New("mount path could not be verified")

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

// protectedSystemDirs contains root-owned paths that must not be shadowed by a
// user-visible mount. Keep the CSI runtime paths and mount root reserved here.
var protectedSystemDirs = []string{
	"/mnt/envd",
	"/run/csi", // Includes the CSI sockets and the default mount-root.
	"/var/run/csi",
	"/etc",
	"/lib",
	"/lib64",
	"/usr/lib",
	"/usr/lib64",
	"/dev",
	"/sys",
	"/root",
	"/home",
	"/boot",
	"/var/run",
}

// Validate rejects a user-visible mount path that overlaps a protected system
// path or a directory used for executable lookup. pathEnv must come from the
// process that runs in the target container, never from the request being
// validated. The deployment policy from MountPathPolicyEnv is applied.
func Validate(pathValue, pathEnv string) error {
	return ValidateWithPolicy(pathValue, pathEnv, DefaultPolicy())
}

// ValidateWithPolicy is Validate with an explicit policy. A nil policy keeps
// the built-in baseline, so callers that do not participate in the deployment
// policy keep the strictest behavior.
func ValidateWithPolicy(pathValue, pathEnv string, policy *Policy) error {
	if pathValue == "" {
		return unsafePathError(pathValue, "path must not be empty")
	}
	if strings.IndexByte(pathValue, 0) >= 0 {
		return unsafePathError(pathValue, "path must not contain NUL")
	}
	for _, segment := range strings.Split(pathValue, string(filepath.Separator)) {
		if segment == ".." {
			return unsafePathError(pathValue, "path must not contain '..' segments")
		}
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

	protectedDirs, resolveErr := protectedDirectories(pathEnv, policy)
	for _, protectedDir := range protectedDirs {
		if pathsOverlap(cleanPath, protectedDir) || pathsOverlap(resolvedPath, protectedDir) {
			return unsafePathError(pathValue, fmt.Sprintf("path overlaps protected directory %q", protectedDir))
		}
	}
	if resolveErr != nil {
		// Do not retain ErrUnsafeMountPath from the shared path resolver here.
		// A protected directory belongs to the sandbox environment, not the request.
		return fmt.Errorf("%w: failed to resolve protected directories: %v", ErrMountPathUnverifiable, resolveErr)
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
	if info, statErr := os.Lstat(resolvedParent); statErr == nil {
		if !info.IsDir() {
			return "", traversedProc, unsafePathError(pathValue,
				fmt.Sprintf("parent path %q is not a directory", resolvedParent))
		}
	} else if !os.IsNotExist(statErr) {
		return "", traversedProc, statErr
	}
	return filepath.Join(resolvedParent, base), traversedProc, nil
}

// resolveExistingPrefix follows every symlink in the existing portion of an
// absolute path. Missing trailing components are appended unchanged so callers
// can validate paths whose parents will be created later. filepath.EvalSymlinks
// cannot handle those missing components, and its final path alone cannot show
// whether resolution passed through procfs (for example, /proc/self/root may
// resolve outside /proc). The bool reports whether any symlink target traversed
// procfs.
func resolveExistingPrefix(pathValue string) (string, bool, error) {
	pending := splitPathComponents(filepath.Clean(pathValue))
	resolved := string(filepath.Separator)
	traversedProc := false
	followedLinks := 0

	for len(pending) > 0 {
		segment := pending[0]
		pending = pending[1:]
		switch segment {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, segment)
		if isWithin(next, "/proc") {
			traversedProc = true
		}
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
			if len(pending) > 0 && !info.IsDir() {
				return "", traversedProc, unsafePathError(pathValue,
					fmt.Sprintf("path component %q is not a directory", next))
			}
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
		if filepath.IsAbs(target) {
			resolved = string(filepath.Separator)
		}
		// Preserve dot segments until after preceding symlinks are followed. Cleaning
		// target here would not match kernel path-walk semantics for "link/..".
		pending = append(splitPathComponents(target), pending...)
	}
	return resolved, traversedProc, nil
}

func protectedDirectories(pathEnv string, policy *Policy) ([]string, error) {
	entries := baselineProtectedDirs()
	if policy != nil {
		entries = append(entries, policy.Add...)
	}
	entries = append(entries, filepath.SplitList(pathEnv)...)

	cwd, cwdErr := os.Getwd()

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
	var firstResolveErr error
	recordResolveErr := func(err error) {
		if firstResolveErr == nil {
			firstResolveErr = err
		}
	}

	for _, entry := range entries {
		originalEntry := entry
		if entry == "" || !filepath.IsAbs(entry) {
			if cwdErr != nil {
				recordResolveErr(fmt.Errorf("resolve relative protected directory %q: %w", originalEntry, cwdErr))
				continue
			}
			if entry == "" {
				entry = cwd
			} else {
				entry = filepath.Join(cwd, entry)
			}
		}
		entry = filepath.Clean(entry)
		// Protecting the filesystem root would overlap every absolute mount path.
		// Other PATH entries, including a non-root working directory, remain protected.
		if entry == string(filepath.Separator) {
			continue
		}
		if policy.Allows(entry) {
			continue
		}

		resolved, _, resolveErr := resolveExistingPrefix(entry)
		if resolveErr != nil {
			// Fail closed, but retain the lexical entry and continue so a known
			// request violation takes precedence over an unrelated environment error.
			add(entry)
			recordResolveErr(fmt.Errorf("resolve protected directory %q: %w", entry, resolveErr))
			continue
		}
		// An allowed path covers the location it resolves to as well, so a symlink
		// alias cannot keep protecting the path the policy removed.
		if resolved != string(filepath.Separator) && policy.Allows(resolved) {
			continue
		}
		add(entry)
		if resolved != string(filepath.Separator) {
			add(resolved)
		}
	}
	return protected, firstResolveErr
}

func splitPathComponents(pathValue string) []string {
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
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func unsafePathError(pathValue, reason string) error {
	return fmt.Errorf("%w: %q: %s", ErrUnsafeMountPath, pathValue, reason)
}
