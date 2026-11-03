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
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewPolicyEmpty(t *testing.T) {
	tests := []struct {
		name  string
		add   []string
		allow []string
	}{
		{name: "nil lists"},
		{name: "empty lists", add: []string{}, allow: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy, err := NewPolicy(tt.add, tt.allow)
			require.NoError(t, err)
			assert.Nil(t, policy)
		})
	}
}

func TestNewPolicyNormalizes(t *testing.T) {
	policy, err := NewPolicy([]string{"/srv/", "/srv", "/data"}, []string{"/opt/app/"})
	require.NoError(t, err)
	assert.Equal(t, []string{"/srv", "/data"}, policy.Add)
	assert.Equal(t, []string{"/opt/app"}, policy.Allow)
}

func TestNewPolicyRejectsMalformedInput(t *testing.T) {
	tests := []struct {
		name  string
		add   []string
		allow []string
	}{
		{name: "empty entry", add: []string{""}},
		{name: "relative add", add: []string{"srv"}},
		{name: "dotdot allow", allow: []string{"/home/../etc"}},
		{name: "filesystem root", allow: []string{"/"}},
		{name: "nul byte", add: []string{"/srv\x00"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy, err := NewPolicy(tt.add, tt.allow)
			assert.Error(t, err)
			assert.Nil(t, policy)
		})
	}
}

func TestNewPolicyRejectsCorePaths(t *testing.T) {
	for _, pathValue := range []string{
		"/etc",
		"/proc",
		"/bin",
		"/usr/bin",
		"/var/run",
		"/mnt/envd",
		"/root",
		"/boot",
	} {
		t.Run(pathValue, func(t *testing.T) {
			policy, err := NewPolicy(nil, []string{pathValue})
			assert.Error(t, err)
			assert.Nil(t, policy)
		})
	}
}

func TestPolicyAllowsCoreAlwaysFalse(t *testing.T) {
	// A programmatically built policy bypasses NewPolicy, so Allows enforces
	// the core guard itself.
	policy := &Policy{Allow: []string{"/etc"}}
	assert.False(t, policy.Allows("/etc"))
}

func TestPolicyAllowsResolvedAlias(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	require.NoError(t, os.MkdirAll(realDir, 0o755))
	linkDir := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(realDir, linkDir))

	policy, err := NewPolicy(nil, []string{linkDir})
	require.NoError(t, err)
	assert.True(t, policy.Allows(linkDir))
	assert.True(t, policy.Allows(realDir))
}

func TestNewPolicyRejectsAliasToCorePath(t *testing.T) {
	alias := filepath.Join(t.TempDir(), "usr-bin-link")
	require.NoError(t, os.Symlink("/usr/bin", alias))

	// The alias is not itself core, but it resolves to one: allowing it would
	// remove a path the mount attaches depend on.
	policy, err := NewPolicy(nil, []string{alias})
	assert.Error(t, err)
	assert.Nil(t, policy)
}

func TestValidateWithPolicyAddAndAllow(t *testing.T) {
	protectedDir := filepath.Join(t.TempDir(), "protected")
	mountPath := filepath.Join(protectedDir, "child")
	require.NoError(t, os.MkdirAll(mountPath, 0o755))

	added, err := NewPolicy([]string{protectedDir}, nil)
	require.NoError(t, err)
	require.ErrorIs(t, ValidateWithPolicy(mountPath, "", added), ErrUnsafeMountPath)

	allowed, err := NewPolicy([]string{protectedDir}, []string{protectedDir})
	require.NoError(t, err)
	assert.NoError(t, ValidateWithPolicy(mountPath, "", allowed))
}

func TestValidateWithPolicyBaselinePathCannotBeAllowed(t *testing.T) {
	// /root and /boot are core as well as baseline, so a policy cannot relax
	// them. /home is the one baseline exception, covered below.
	for _, pathValue := range []string{"/root", "/boot", "/etc"} {
		policy, err := NewPolicy(nil, []string{pathValue})
		assert.Errorf(t, err, "path %q must not be allowed away", pathValue)
		assert.Nil(t, policy)
	}

	assert.ErrorIs(t, ValidateWithPolicy("/root/data", "", nil), ErrUnsafeMountPath)
}

func TestValidateWithPolicyHomeIsRemovable(t *testing.T) {
	// /home stays protected by default but removable: an image that keeps its
	// working data there needs a way to mount over it.
	assert.ErrorIs(t, ValidateWithPolicy("/home/user/data", "", nil), ErrUnsafeMountPath)

	policy, err := NewPolicy(nil, []string{"/home"})
	require.NoError(t, err)
	assert.NoError(t, ValidateWithPolicy("/home/user/data", "", policy))
}

func TestDefaultPolicyFromEnv(t *testing.T) {
	resetDefaultPolicy(t)
	t.Setenv(MountPathPolicyAddEnv, "/srv;/data")
	t.Cleanup(func() { resetDefaultPolicy(t) })

	policy := DefaultPolicy()
	require.NotNil(t, policy)
	assert.Equal(t, []string{"/srv", "/data"}, policy.Add)
}

func TestValidateUsesDefaultPolicy(t *testing.T) {
	added := filepath.Join(t.TempDir(), "added")
	require.NoError(t, os.MkdirAll(added, 0o755))

	resetDefaultPolicy(t)
	t.Setenv(MountPathPolicyAddEnv, added)
	t.Cleanup(func() { resetDefaultPolicy(t) })

	assert.ErrorIs(t, Validate(filepath.Join(added, "child"), ""), ErrUnsafeMountPath)
}

func TestDefaultPolicyInvalidFallsBackToBaseline(t *testing.T) {
	resetDefaultPolicy(t)
	t.Setenv(MountPathPolicyAddEnv, "relative/path")
	t.Cleanup(func() { resetDefaultPolicy(t) })

	assert.Nil(t, DefaultPolicy())
}

func TestDefaultPolicyRejectsCoreAllowAndFallsBackToBaseline(t *testing.T) {
	resetDefaultPolicy(t)
	t.Setenv(MountPathPolicyAllowEnv, "/home;/root")
	t.Cleanup(func() { resetDefaultPolicy(t) })

	// One refused entry rejects the whole policy; /home must not be applied alone.
	assert.Nil(t, DefaultPolicy())
}

func TestDefaultPolicyFromBothEnvs(t *testing.T) {
	resetDefaultPolicy(t)
	t.Setenv(MountPathPolicyAddEnv, "/srv;/data")
	t.Setenv(MountPathPolicyAllowEnv, "/home")
	t.Cleanup(func() { resetDefaultPolicy(t) })

	policy := DefaultPolicy()
	require.NotNil(t, policy)
	assert.Equal(t, []string{"/srv", "/data"}, policy.Add)
	assert.Equal(t, []string{"/home"}, policy.Allow)
	assert.True(t, policy.Allows("/home"))
}

func TestSplitPolicyPaths(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "empty", raw: ""},
		{name: "blank", raw: "   "},
		{name: "single", raw: "/src", want: []string{"/src"}},
		{name: "two", raw: "/src;/data", want: []string{"/src", "/data"}},
		{name: "surrounding spaces", raw: "/src ; /data", want: []string{"/src", "/data"}},
		{name: "trailing separator", raw: "/src;", want: []string{"/src"}},
		{name: "empty entries", raw: "/src;;/data", want: []string{"/src", "/data"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, splitPolicyPaths(tt.raw))
		})
	}
}

func TestProtectedDirsWithPolicy(t *testing.T) {
	t.Run("nil policy returns the baseline", func(t *testing.T) {
		assert.ElementsMatch(t, baselineProtectedDirs(), ProtectedDirsWithPolicy(nil))
	})

	t.Run("adds deployment paths", func(t *testing.T) {
		policy, err := NewPolicy([]string{"/srv", "/data"}, nil)
		require.NoError(t, err)

		dirs := ProtectedDirsWithPolicy(policy)
		assert.Contains(t, dirs, "/srv")
		assert.Contains(t, dirs, "/data")
		assert.Contains(t, dirs, "/etc")
		assert.Len(t, dirs, len(baselineProtectedDirs())+2)
	})

	t.Run("does not duplicate a path already in the baseline", func(t *testing.T) {
		policy, err := NewPolicy([]string{"/etc"}, nil)
		require.NoError(t, err)

		assert.Len(t, ProtectedDirsWithPolicy(policy), len(baselineProtectedDirs()))
	})

	t.Run("removes an allowed baseline path and keeps core paths", func(t *testing.T) {
		policy, err := NewPolicy(nil, []string{"/home"})
		require.NoError(t, err)

		dirs := ProtectedDirsWithPolicy(policy)
		assert.NotContains(t, dirs, "/home")
		assert.Len(t, dirs, len(baselineProtectedDirs())-1)
		// Core paths survive: Allows refuses to match them.
		assert.Contains(t, dirs, "/etc")
		assert.Contains(t, dirs, "/root")
	})

	t.Run("removes an allowed addition", func(t *testing.T) {
		policy, err := NewPolicy([]string{"/srv"}, []string{"/srv"})
		require.NoError(t, err)

		assert.NotContains(t, ProtectedDirsWithPolicy(policy), "/srv")
	})
}

func TestProtectedDirsReturnsFreshSlice(t *testing.T) {
	first := ProtectedDirsWithPolicy(nil)
	require.NotEmpty(t, first)
	first[0] = "/mutated"

	// The package-level slices must not be reachable through the result.
	second := ProtectedDirsWithPolicy(nil)
	assert.Equal(t, standardExecutableDirs[0], second[0])
}

func TestProtectedDirsFromEnv(t *testing.T) {
	resetDefaultPolicy(t)
	t.Setenv(MountPathPolicyAddEnv, "/srv")
	t.Setenv(MountPathPolicyAllowEnv, "/home")
	t.Cleanup(func() { resetDefaultPolicy(t) })

	dirs := ProtectedDirs()
	assert.Contains(t, dirs, "/srv")
	assert.NotContains(t, dirs, "/home")
}

func TestProtectedDirsWithInvalidEnvUsesBaseline(t *testing.T) {
	resetDefaultPolicy(t)
	t.Setenv(MountPathPolicyAddEnv, "relative/path")
	t.Cleanup(func() { resetDefaultPolicy(t) })

	// DefaultPolicy returns nil for a refused value, so the list is the baseline.
	assert.ElementsMatch(t, baselineProtectedDirs(), ProtectedDirs())
}

func TestPolicyAllowsWithoutAllowSet(t *testing.T) {
	// A programmatically built Policy carries no allowSet, so Allows falls back to
	// comparing the cleaned path against the entries as given.
	policy := &Policy{Allow: []string{"/home"}}
	assert.True(t, policy.Allows("/home/"))
	assert.False(t, policy.Allows("/opt"))
	assert.False(t, policy.Allows("/etc"), "core paths are never allowed")
}

func TestIsCoreProtectedIncludesAdditionalDirs(t *testing.T) {
	original := additionalCoreProtectedDirs
	additionalCoreProtectedDirs = []string{"/opt/vendor"}
	t.Cleanup(func() { additionalCoreProtectedDirs = original })

	assert.True(t, isCoreProtected("/opt/vendor"))
	assert.False(t, isCoreProtected("/opt/unprotected"))
}

func resetDefaultPolicy(t *testing.T) {
	t.Helper()
	defaultPolicyOnce = sync.Once{}
	defaultPolicy = nil
}
