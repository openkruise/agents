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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"k8s.io/klog/v2"
)

// MountPathPolicyEnv configures a deployment-level adjustment to the protected
// mount paths. Its value is a JSON object with two optional lists:
//
//	{"add":["/srv","/data"],"allow":["/home","/opt"]}
//
// "add" only extends protection. "allow" removes protection and therefore
// lowers the security boundary; it is refused for coreProtectedDirs, which the
// mount attaches depend on. An empty value keeps the built-in baseline, and an
// invalid value fails closed to that same baseline instead of applying a
// partial policy.
//
// The value is read once per process, so pin it to the runtime image version it
// was verified against: an allowance is only as safe as the filesystem layout
// of the version that receives it.
const MountPathPolicyEnv = "AGENT_RUNTIME_MOUNT_PATH_POLICY"

// Policy is a deployment-level adjustment layered over the built-in protected
// path lists.
type Policy struct {
	// Add lists extra protected paths. It can only raise the security boundary
	// and is applied to every caller, including the in-container storage CLI.
	Add []string `json:"add,omitempty"`

	// Allow lists protected paths this deployment tolerates mounting over. An
	// entry names the protected path itself, not a descendant: protection is
	// checked with overlap, so mounting /home/user remains refused while /home
	// is still protected. Symlink aliases are matched by their resolved path.
	Allow []string `json:"allow,omitempty"`

	// allowSet holds the lexical and resolved forms of Allow. It is built by
	// ParsePolicy and never marshaled; a nil set falls back to a lexical scan.
	allowSet map[string]struct{}
}

// coreProtectedDirs can never be removed by a policy. They are either runtime
// plumbing the mount attaches depend on, kernel interfaces where a mount would
// break the sandbox, or executable/library locations where a mount would allow
// binary or library replacement.
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

// additionalCoreProtectedDirs lets the internal build protect more paths than
// the community baseline while keeping them non-removable. It is empty in the
// community build; an internal same-package inner_*.go appends its entries from
// init, so the community tree stays self-contained.
var additionalCoreProtectedDirs []string

var (
	defaultPolicyOnce sync.Once
	defaultPolicy     *Policy
)

// DefaultPolicy returns the policy from MountPathPolicyEnv. The result is
// cached because this is deployment configuration, fixed for the life of the
// process; callers that need a different policy use ValidateWithPolicy.
func DefaultPolicy() *Policy {
	defaultPolicyOnce.Do(func() {
		policy, err := ParsePolicy(os.Getenv(MountPathPolicyEnv))
		if err != nil {
			klog.ErrorS(err, "ignoring invalid mount path policy; keeping the built-in baseline", "env", MountPathPolicyEnv)
			return
		}
		defaultPolicy = policy
		if policy != nil {
			klog.InfoS("loaded mount path policy", "env", MountPathPolicyEnv,
				"add", policy.Add, "allow", policy.Allow)
		}
	})
	return defaultPolicy
}

// ParsePolicy decodes and normalizes a policy. A blank value returns a nil
// policy, which is the built-in baseline. Any malformed path rejects the whole
// policy rather than applying part of a relaxation.
func ParsePolicy(raw string) (*Policy, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var policy Policy
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return nil, fmt.Errorf("decode mount path policy: %w", err)
	}
	add, err := normalizePolicyPaths(policy.Add)
	if err != nil {
		return nil, fmt.Errorf("invalid add list: %w", err)
	}
	allow, err := normalizePolicyPaths(policy.Allow)
	if err != nil {
		return nil, fmt.Errorf("invalid allow list: %w", err)
	}
	allowSet := make(map[string]struct{}, len(allow)*2)
	for _, pathValue := range allow {
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
	policy.Add, policy.Allow, policy.allowSet = add, allow, allowSet
	if len(policy.Add) == 0 && len(policy.Allow) == 0 {
		return nil, nil
	}
	return &policy, nil
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
		for _, allowed := range p.Allow {
			if filepath.Clean(allowed) == candidate {
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
// minus allowed paths, with the policy from MountPathPolicyEnv. The result is a
// fresh slice, so an appending caller cannot reach the package-level slices and
// change the next caller's policy.
//
// Callers outside this package need the list because their preflight runs where
// Validate is not reachable — in front of CSI NodePublishVolume, or before the
// path crosses the helper boundary — and a second transcription of the paths is
// how the two sides drift apart. Applying MountPathPolicyEnv here as well keeps
// the baseline side of the preflight and Validate on the same answer.
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

// baselineProtectedDirs returns a fresh copy of the community baseline. The
// deployment additions in common are layered on top by that package.
func baselineProtectedDirs() []string {
	entries := make([]string, 0, len(standardExecutableDirs)+len(protectedSystemDirs))
	entries = append(entries, standardExecutableDirs...)
	entries = append(entries, protectedSystemDirs...)
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
