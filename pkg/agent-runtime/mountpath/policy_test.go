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
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePolicyEmpty(t *testing.T) {
	for _, raw := range []string{"", " ", "{}", `{"add":[],"allow":[]}`} {
		policy, err := ParsePolicy(raw)
		require.NoError(t, err, "raw %q", raw)
		assert.Nil(t, policy, "raw %q", raw)
	}
}

func TestParsePolicyNormalizes(t *testing.T) {
	policy, err := ParsePolicy(`{"add":["/srv/","/srv","/data"],"allow":["/home/"]}`)
	require.NoError(t, err)
	assert.Equal(t, []string{"/srv", "/data"}, policy.Add)
	assert.Equal(t, []string{"/home"}, policy.Allow)
}

func TestParsePolicyRejectsMalformedInput(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "invalid json", raw: `{`},
		{name: "relative add", raw: `{"add":["srv"]}`},
		{name: "dotdot allow", raw: `{"allow":["/home/../etc"]}`},
		{name: "filesystem root", raw: `{"allow":["/"]}`},
		{name: "nul byte", raw: `{"add":["/srv\u0000"]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy, err := ParsePolicy(tt.raw)
			assert.Error(t, err)
			assert.Nil(t, policy)
		})
	}
}

func TestParsePolicyRejectsCorePaths(t *testing.T) {
	for _, pathValue := range []string{
		"/etc",
		"/proc",
		"/bin",
		"/usr/bin",
		"/var/run",
		"/mnt/envd",
	} {
		t.Run(pathValue, func(t *testing.T) {
			policy, err := ParsePolicy(fmt.Sprintf(`{"allow":[%q]}`, pathValue))
			assert.Error(t, err)
			assert.Nil(t, policy)
		})
	}
}

func TestPolicyAllowsCoreAlwaysFalse(t *testing.T) {
	// A programmatically built policy bypasses ParsePolicy, so Allows enforces
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

	policy, err := ParsePolicy(fmt.Sprintf(`{"allow":[%q]}`, linkDir))
	require.NoError(t, err)
	assert.True(t, policy.Allows(linkDir))
	assert.True(t, policy.Allows(realDir))
}

func TestValidateWithPolicyAddAndAllow(t *testing.T) {
	protectedDir := filepath.Join(t.TempDir(), "protected")
	mountPath := filepath.Join(protectedDir, "child")
	require.NoError(t, os.MkdirAll(mountPath, 0o755))

	added, err := ParsePolicy(fmt.Sprintf(`{"add":[%q]}`, protectedDir))
	require.NoError(t, err)
	require.ErrorIs(t, ValidateWithPolicy(mountPath, "", added), ErrUnsafeMountPath)

	allowed, err := ParsePolicy(fmt.Sprintf(`{"add":[%q],"allow":[%q]}`, protectedDir, protectedDir))
	require.NoError(t, err)
	assert.NoError(t, ValidateWithPolicy(mountPath, "", allowed))
}

func TestValidateWithPolicyAllowsBaselinePath(t *testing.T) {
	policy, err := ParsePolicy(`{"allow":["/home"]}`)
	require.NoError(t, err)
	assert.NoError(t, ValidateWithPolicy("/home/user/data", "", policy))
}

func TestDefaultPolicyFromEnv(t *testing.T) {
	resetDefaultPolicy(t)
	t.Setenv(MountPathPolicyEnv, `{"allow":["/home"]}`)
	t.Cleanup(func() { resetDefaultPolicy(t) })

	policy := DefaultPolicy()
	require.NotNil(t, policy)
	assert.True(t, policy.Allows("/home"))
}

func TestValidateUsesDefaultPolicy(t *testing.T) {
	resetDefaultPolicy(t)
	t.Setenv(MountPathPolicyEnv, `{"allow":["/home"]}`)
	t.Cleanup(func() { resetDefaultPolicy(t) })

	assert.NoError(t, Validate("/home/user/data", ""))
}

func TestDefaultPolicyInvalidFallsBackToBaseline(t *testing.T) {
	resetDefaultPolicy(t)
	t.Setenv(MountPathPolicyEnv, `{`)
	t.Cleanup(func() { resetDefaultPolicy(t) })

	assert.Nil(t, DefaultPolicy())
}

func resetDefaultPolicy(t *testing.T) {
	t.Helper()
	defaultPolicyOnce = sync.Once{}
	defaultPolicy = nil
}
