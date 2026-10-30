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

package customfusevalidate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateSecrets(t *testing.T) {
	tests := []struct {
		name        string
		secrets     map[string]string
		expectError string
	}{
		{name: "nil secrets pass", secrets: nil, expectError: ""},
		{name: "empty secrets pass", secrets: map[string]string{}, expectError: ""},
		{
			name:        "credential keys pass",
			secrets:     map[string]string{"access_key": "AKID", "secret_key": "SK", "token": "tok"},
			expectError: "",
		},
		{
			name:        "key with dash rejected",
			secrets:     map[string]string{"access-key": "AKID"},
			expectError: "not a valid environment variable name",
		},
		{
			name:        "key with leading digit rejected",
			secrets:     map[string]string{"1secret": "v"},
			expectError: "not a valid environment variable name",
		},
		{
			name:        "BASH_ENV rejected",
			secrets:     map[string]string{"BASH_ENV": "/tmp/x.sh"},
			expectError: "not allowed",
		},
		{
			name:        "LD_PRELOAD rejected",
			secrets:     map[string]string{"LD_PRELOAD": "/tmp/x.so"},
			expectError: "not allowed",
		},
		{
			name:        "IFS rejected",
			secrets:     map[string]string{"IFS": ":"},
			expectError: "not allowed",
		},
		{
			name:        "reserved source key rejected",
			secrets:     map[string]string{"source": "redis://evil:6379/0"},
			expectError: "reserved",
		},
		{
			name:        "reserved readOnly key rejected",
			secrets:     map[string]string{"readOnly": "false"},
			expectError: "reserved",
		},
		{
			name:        "reserved url key rejected",
			secrets:     map[string]string{"url": "http://attacker:9000"},
			expectError: "reserved",
		},
		{
			name:        "reserved storageType key rejected",
			secrets:     map[string]string{"storageType": "oss"},
			expectError: "reserved",
		},
		{
			name:        "newline value rejected",
			secrets:     map[string]string{"access_key": "AK\nID"},
			expectError: "must not contain a newline",
		},
		{
			name:        "carriage return value rejected",
			secrets:     map[string]string{"access_key": "AK\rID"},
			expectError: "must not contain a newline",
		},
		{
			name:        "NUL byte value rejected",
			secrets:     map[string]string{"access_key": "AK\x00ID"},
			expectError: "must not contain a newline, tab or NUL byte",
		},
		{
			name:        "tab value rejected",
			secrets:     map[string]string{"access_key": "AK\tID"},
			expectError: "must not contain a newline, tab or NUL byte",
		},
		{
			name:        "lowercase dangerous key stays allowed",
			secrets:     map[string]string{"bash_env": "/tmp/x.sh"},
			expectError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSecrets(tt.secrets)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
		})
	}
}

func TestValidateSecretData(t *testing.T) {
	err := ValidateSecretData(map[string][]byte{
		"access_key": []byte("AKID"),
		"secret_key": []byte("SK\nline2"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "must not contain a newline")

	assert.NoError(t, ValidateSecretData(nil))
}

func TestValidateEnvKeyName(t *testing.T) {
	tests := []struct {
		name        string
		key         string
		expectError string
	}{
		{name: "plain name passes", key: "access_key", expectError: ""},
		{name: "name with dash rejected", key: "access-key", expectError: "not a valid environment variable name"},
		{name: "name with leading digit rejected", key: "1key", expectError: "not a valid environment variable name"},
		{name: "BASH_ENV rejected", key: "BASH_ENV", expectError: "not allowed"},
		{name: "LD_PRELOAD rejected", key: "LD_PRELOAD", expectError: "not allowed"},
		{name: "lowercase dangerous key passes", key: "bash_env", expectError: ""},
		{name: "overlong key rejected", key: strings.Repeat("a", 300), expectError: "too long"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateEnvKeyName(tt.key)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
		})
	}
}

func TestIsBlockedEnvKey(t *testing.T) {
	for _, key := range []string{"BASH_ENV", "LD_PRELOAD", "IFS", "PATH"} {
		assert.True(t, IsBlockedEnvKey(key), "expected %q to be blocked", key)
	}
	for _, key := range []string{"bash_env", "access_key", "custom-key", ""} {
		assert.False(t, IsBlockedEnvKey(key), "expected %q not to be blocked", key)
	}
}

func TestValidateMountOptions(t *testing.T) {
	tests := []struct {
		name        string
		opts        []string
		expectError string
	}{
		{name: "nil options pass", opts: nil, expectError: ""},
		{name: "safe options pass", opts: []string{"cache-size=1024", "no-update"}, expectError: ""},
		{name: "shell metachar rejected", opts: []string{"opt; rm -rf /"}, expectError: "invalid shell characters"},
		{name: "BASH_ENV rejected", opts: []string{"BASH_ENV=/tmp/x.sh"}, expectError: "not allowed"},
		{name: "LD_PRELOAD rejected", opts: []string{"LD_PRELOAD=/tmp/x.so"}, expectError: "not allowed"},
		{name: "PATH rejected", opts: []string{"PATH=/tmp"}, expectError: "not allowed"},
		{name: "space-padded dangerous key rejected", opts: []string{"  BASH_ENV=/tmp/x.sh"}, expectError: "not allowed"},
		{name: "tab-padded reserved key rejected", opts: []string{"\tsource=redis://evil:6379/0"}, expectError: "reserved"},
		{name: "bare dangerous key rejected", opts: []string{"IFS"}, expectError: "not allowed"},
		{name: "reserved url rejected", opts: []string{"url=http://attacker:9000"}, expectError: "reserved"},
		{name: "reserved source rejected", opts: []string{"source=redis://evil:6379/0"}, expectError: "reserved"},
		{name: "reserved readOnly rejected", opts: []string{"readOnly=false"}, expectError: "reserved"},
		{name: "subdir option rejected with path hint", opts: []string{"subdir=/my/sub/dir"}, expectError: "use the path volumeAttribute"},
		{name: "case variant of subdir stays allowed", opts: []string{"Subdir=/my/sub/dir"}, expectError: ""},
		{name: "space-separated entry hiding reserved key rejected", opts: []string{"cache-size=1024 source=evil"}, expectError: "reserved"},
		{name: "space-separated entry hiding dangerous key rejected", opts: []string{"cache-size=1024 BASH_ENV=/tmp/x.sh"}, expectError: "not allowed"},
		{name: "keyless entry rejected", opts: []string{"=value"}, expectError: "empty option key"},
		{name: "bare flag without equals stays allowed", opts: []string{"allow_other"}, expectError: ""},
		{name: "credential-like sub-key rejected", opts: []string{"cache-size=1024,token=xxx"}, expectError: "must not carry credential material"},
		{name: "password sub-key rejected", opts: []string{"password=secret"}, expectError: "must not carry credential material"},
		{
			name:        "blocked option error masks value",
			opts:        []string{"BASH_ENV=/tmp/x.sh"},
			expectError: "not allowed",
		},
		{
			name:        "lowercase dangerous key stays allowed",
			opts:        []string{"bash_env=/tmp/x.sh"},
			expectError: "",
		},
		{
			name:        "case variant of reserved key stays allowed",
			opts:        []string{"Url=http://attacker:9000"},
			expectError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMountOptions(tt.opts)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
		})
	}
}

func TestBlockedOptionErrorMasksValue(t *testing.T) {
	err := ValidateMountOptions([]string{"BASH_ENV=/tmp/x.sh"})
	assert.Error(t, err)
	assert.NotContains(t, err.Error(), "/tmp/x.sh")
}

func TestIsReservedKey(t *testing.T) {
	for _, key := range []string{"source", "readOnly", "mountpoint", "bucket", "url", "path", "otherOpts", "capacity", "fuseType", "authType", "storageType"} {
		assert.True(t, IsReservedKey(key), "expected %q to be reserved", key)
	}
	for _, key := range []string{"Url", "URL", "SOURCE", "access_key", "token", "cache-size", "no-update", ""} {
		assert.False(t, IsReservedKey(key), "expected %q not to be reserved", key)
	}
}

func TestHasShellMetachar(t *testing.T) {
	for _, c := range []string{";", "|", "&", "`", "$", "(", ")", "\\", "\r", "\n", "\x00"} {
		assert.True(t, HasShellMetachar("a"+c+"b"), "expected metachar %q", c)
	}
	for _, s := range []string{"", "cache-size=1024", "http://host:9000", "a b", "a<b>c", "a'b\"c"} {
		assert.False(t, HasShellMetachar(s), "expected no metachar in %q", s)
	}
}

func TestMaskOptionValues(t *testing.T) {
	assert.Equal(t, "passwd_file=***,allow_other", MaskOptionValues("passwd_file=/tmp/x,allow_other"))
	assert.Equal(t, "opt1; x", MaskOptionValues("opt1; x"))
	// Keys containing characters outside [A-Za-z0-9_-] must be masked too,
	// or their values would leak into error messages.
	assert.Equal(t, "x.y=***,allow_other", MaskOptionValues("x.y=Secret,allow_other"))
	assert.Equal(t, "x.y=*** BASH_ENV=***", MaskOptionValues("x.y=Secret BASH_ENV=/evil"))
	// Values are masked up to the next separator. A value containing a
	// comma is an invalid mount option; masking past the comma would
	// swallow the following option from the error message, so the mask
	// stops at the boundary.
	assert.Equal(t, "cache-size=***,no-update", MaskOptionValues("cache-size=1024,no-update"))
	assert.Equal(t, "cache-dir=***,b", MaskOptionValues("cache-dir=/path/a,b"))
	// Malformed keyless input ("=secret") is masked as well.
	assert.Equal(t, "=***", MaskOptionValues("=secret"))
}
