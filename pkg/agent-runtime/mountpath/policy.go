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
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// MountPathPolicyAddEnv lists extra paths a deployment protects on top of the
// built-in baseline. Entries only raise the security boundary: a mount over one
// of them is refused. The value is a ';' separated list of absolute paths, for
// example "/src;/data". An empty value adds nothing.
const MountPathPolicyAddEnv = "AGENT_RUNTIME_MOUNT_EXTRA_BLACK_LIST"

// MountPathPolicyAllowEnv lists protected paths this deployment removes from the
// protected set. The value is a ';' separated list, for example "/home;/opt".
//
// This is an exemption list, not an admission allow-list: paths absent from it
// stay protected. Removing a path lowers the security boundary, so it is refused
// for coreProtectedDirs, which the mount attaches depend on.
const MountPathPolicyAllowEnv = "AGENT_RUNTIME_MOUNT_WHITE_LIST"

// Both values are read once per process, so pin them to the runtime image
// version they were verified against: an exemption is only as safe as the
// filesystem layout of the version that receives it. An invalid value fails
// closed to the built-in baseline instead of applying a partial policy.

// Policy is a deployment-level adjustment layered over the built-in protected
// path lists.
type Policy struct {
	// Add lists extra protected paths. It can only raise the security boundary
	// and is applied to every caller, including the in-container storage CLI.
	Add []string

	// Allow lists protected paths this deployment tolerates mounting over. An
	// entry names the protected path itself, not a descendant: protection is
	// checked with overlap, so mounting /home/user remains refused while /home
	// is still protected. Symlink aliases are matched by their resolved path.
	Allow []string

	// allowSet holds the lexical and resolved forms of Allow. It is built by
	// NewPolicy; a nil set falls back to a lexical scan.
	allowSet map[string]struct{}
}

// coreProtectedDirs can never be removed by a policy. They are either runtime
// plumbing the mount attaches depend on, kernel interfaces where a mount would
// break the sandbox, executable/library locations where a mount would allow
// binary or library replacement, or the root and boot directories a workload
// must not be able to swap under the view of another workload.
//
// /home is deliberately absent: it stays in protectedSystemDirs, so it is
// protected by default, but a deployment may allow it away. An image that keeps
// its working data under /home rather than /opt is the usual reason.
var coreProtectedDirs = []string{
	"/mnt/envd",
	"/run/csi",
	"/var/run/csi",
	"/run",
	"/var/run",
	"/proc",
	"/sys",
	"/dev",
	"/etc",
	"/root",
	"/boot",
	"/bin",
	"/sbin",
	"/usr/bin",
	"/usr/sbin",
	"/usr/local/bin",
	"/usr/local/sbin",
	"/lib",
	"/lib64",
	"/usr/lib",
	"/usr/lib64",
}

// additionalCoreProtectedDirs lets a downstream build protect more paths than
// this baseline while keeping them non-removable. It is empty in this tree; a
// same-package file added by that build appends its entries from init, so this
// tree stays self-contained.
var additionalCoreProtectedDirs []string

// additionalBaselineProtectedDirs lets a downstream build extend the baseline
// itself, for paths that must also be refused by callers running Validate
// directly — the symlink path and the CLI check — which layer policy additions
// on top of the baseline without building the union in common. Entries here are
// non-removable for the same reason as additionalCoreProtectedDirs:
// protectedDirectories never consults the allow list for baseline entries. Empty
// in this tree.
var additionalBaselineProtectedDirs []string

var (
	defaultPolicyOnce sync.Once
	defaultPolicy     *Policy
)

// DefaultPolicy returns the policy from MountPathPolicyAddEnv and
// MountPathPolicyAllowEnv. The result is cached because this is deployment
// configuration, fixed for the life of the process; callers that need a
// different policy use ValidateWithPolicy.
func DefaultPolicy() *Policy {
	defaultPolicyOnce.Do(func() {
		policy, err := policyFromEnv()
		if err != nil {
			log.Printf("ignoring invalid mount path policy: %v; keeping the built-in baseline", err)
			return
		}
		defaultPolicy = policy
		if policy != nil {
			log.Printf("loaded mount path policy: add=%v allow=%v", policy.Add, policy.Allow)
		}
	})
	return defaultPolicy
}

// policyFromEnv builds the policy from the two list variables. Both unset returns
// a nil policy, which is the built-in baseline.
func policyFromEnv() (*Policy, error) {
	add := splitPolicyPaths(os.Getenv(MountPathPolicyAddEnv))
	allow := splitPolicyPaths(os.Getenv(MountPathPolicyAllowEnv))
	return NewPolicy(add, allow)
}

// splitPolicyPaths splits a ';' separated list and drops blank entries, so
// "/src; /data" and a trailing separator both read the same. filepath.SplitList
// is not used on purpose: on Unix it separates on ':', which a path may contain.
func splitPolicyPaths(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ";")
	entries := make([]string, 0, len(parts))
	for _, part := range parts {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

// NewPolicy normalizes and validates the two lists. Empty lists return a nil
// policy, which is the built-in baseline. Any malformed path rejects the whole
// policy rather than applying part of a relaxation.
func NewPolicy(add, allow []string) (*Policy, error) {
	normalizedAdd, err := normalizePolicyPaths(add)
	if err != nil {
		return nil, fmt.Errorf("invalid add list: %w", err)
	}
	normalizedAllow, err := normalizePolicyPaths(allow)
	if err != nil {
		return nil, fmt.Errorf("invalid allow list: %w", err)
	}
	allowSet := make(map[string]struct{}, len(normalizedAllow)*2)
	for _, pathValue := range normalizedAllow {
		if isCoreProtected(pathValue) {
			return nil, fmt.Errorf("allow list: %q is a non-removable protected path", pathValue)
		}
		allowSet[pathValue] = struct{}{}
		if resolved, _, resolveErr := resolveExistingPrefix(pathValue); resolveErr == nil && resolved != string(filepath.Separator) {
			if isCoreProtected(resolved) {
				return nil, fmt.Errorf("allow list: %q resolves to non-removable protected path %q", pathValue, resolved)
			}
			allowSet[resolved] = struct{}{}
		}
	}
	if len(normalizedAdd) == 0 && len(normalizedAllow) == 0 {
		return nil, nil
	}
	return &Policy{Add: normalizedAdd, Allow: normalizedAllow, allowSet: allowSet}, nil
}

// Allows reports whether pathValue is explicitly removed from protection. Core
// paths always report false, so a programmatically built Policy cannot weaken
// them either.
func (p *Policy) Allows(pathValue string) bool {
	if p == nil || len(p.Allow) == 0 {
		return false
	}
	cleaned := filepath.Clean(pathValue)
	if isCoreProtected(cleaned) {
		return false
	}
	// Compare both the lexical path and the location it resolves to. The allow
	// set already holds both forms of each entry, so a symlinked protected path
	// is removed in either direction.
	matches := func(candidate string) bool {
		if p.allowSet != nil {
			_, ok := p.allowSet[candidate]
			return ok
		}
		// Entries from NewPolicy are already normalized, so the fallback only sees
		// a programmatically built Policy and compares it as given.
		for _, allowed := range p.Allow {
			if allowed == candidate {
				return true
			}
		}
		return false
	}
	if matches(cleaned) {
		return true
	}
	resolved, _, err := resolveExistingPrefix(cleaned)
	return err == nil && resolved != cleaned && matches(resolved)
}

// ProtectedDirs returns the community baseline plus the deployment additions,
// minus allowed paths, with the policy from the deployment environment. The
// result is a fresh slice, so an appending caller cannot reach the package-level
// slices and change the next caller's policy.
//
// Callers outside this package need the list because their preflight runs where
// Validate is not reachable — in front of CSI NodePublishVolume, or before the
// path crosses the helper boundary — and a second transcription of the paths is
// how the two sides drift apart. Applying the deployment policy here as well
// keeps the baseline side of the preflight and Validate on the same answer.
func ProtectedDirs() []string {
	return ProtectedDirsWithPolicy(DefaultPolicy())
}

// ProtectedDirsWithPolicy returns the built-in and deployment-added protected
// directories minus the allowed ones. The result is a fresh slice; core paths
// are always retained because Allows refuses to match them.
func ProtectedDirsWithPolicy(policy *Policy) []string {
	entries := baselineProtectedDirs()
	capacity := len(entries)
	if policy != nil {
		capacity += len(policy.Add)
	}
	dirs := make([]string, 0, capacity)
	seen := make(map[string]struct{}, capacity)
	add := func(pathValue string) {
		if policy.Allows(pathValue) {
			return
		}
		if _, ok := seen[pathValue]; ok {
			return
		}
		seen[pathValue] = struct{}{}
		dirs = append(dirs, pathValue)
	}
	for _, entry := range entries {
		add(entry)
	}
	if policy != nil {
		for _, entry := range policy.Add {
			add(entry)
		}
	}
	return dirs
}

// baselineProtectedDirs returns a fresh copy of the community baseline plus the
// downstream-added entries that every caller must see. The deployment additions
// in common are layered on top by that package.
func baselineProtectedDirs() []string {
	entries := make([]string, 0,
		len(standardExecutableDirs)+len(protectedSystemDirs)+len(additionalBaselineProtectedDirs))
	entries = append(entries, standardExecutableDirs...)
	entries = append(entries, protectedSystemDirs...)
	entries = append(entries, additionalBaselineProtectedDirs...)
	return entries
}

func normalizePolicyPaths(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	normalized := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, pathValue := range paths {
		cleaned, err := normalizePolicyPath(pathValue)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[cleaned]; ok {
			continue
		}
		seen[cleaned] = struct{}{}
		normalized = append(normalized, cleaned)
	}
	return normalized, nil
}

func normalizePolicyPath(pathValue string) (string, error) {
	if pathValue == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	if strings.IndexByte(pathValue, 0) >= 0 {
		return "", fmt.Errorf("path %q must not contain NUL", pathValue)
	}
	for _, segment := range strings.Split(pathValue, string(filepath.Separator)) {
		if segment == ".." {
			return "", fmt.Errorf("path %q must not contain '..' segments", pathValue)
		}
	}
	if !filepath.IsAbs(pathValue) {
		return "", fmt.Errorf("path %q must be absolute", pathValue)
	}
	cleaned := filepath.Clean(pathValue)
	if cleaned == string(filepath.Separator) {
		return "", fmt.Errorf("filesystem root cannot be used as a policy path")
	}
	return cleaned, nil
}

func isCoreProtected(pathValue string) bool {
	for _, dir := range coreProtectedDirs {
		if pathValue == filepath.Clean(dir) {
			return true
		}
	}
	for _, dir := range additionalCoreProtectedDirs {
		if pathValue == filepath.Clean(dir) {
			return true
		}
	}
	return false
}
