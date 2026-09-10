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

// Package customfusevalidate holds the security checks shared between the
// control-plane CustomFuseMountProvider and the sandbox-side CSI node server.
// Both ends must reject the same inputs because mount-proxy exports every
// Secret entry and mount flag as an environment variable of the same name
// into the entrypoint shell: the provider validates the PV+Secret path, and
// the node server repeats the checks for requests that reach it directly
// over the per-driver unix socket without passing through the provider.
package customfusevalidate

import (
	"fmt"
	"regexp"
	"strings"
)

// secretKeyPattern matches Secret keys that can be exported as environment
// variables. The customfuse contract maps every Secret key to an env var of
// the same name consumed by the entrypoint, and bash cannot reference names
// with dashes ($access-key expands as $access plus the literal "-key").
var secretKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// blockedMountOptionKeys are mount options that would become environment
// variables with code-execution or command-redirection side effects once
// mount-proxy exports them to the entrypoint shell: bash sources BASH_ENV at
// startup, glibc loads LD_PRELOAD, PATH redirects command lookup, and
// IFS/PS4/SHELLOPTS alter shell behavior. These keys are rejected outright.
// The match is exact: bash and glibc read only the canonical uppercase names,
// so lowercase variants are harmless and stay allowed.
var blockedMountOptionKeys = map[string]bool{
	"BASH_ENV": true, "ENV": true, "BASHOPTS": true, "SHELLOPTS": true,
	"PROMPT_COMMAND": true, "PS4": true, "IFS": true, "BASH_XTRACEFD": true,
	"LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "LD_AUDIT": true,
	"LD_BIND_NOW": true, "LD_DEBUG": true, "GLIBC_TUNABLES": true,
	"PATH": true, "CDPATH": true, "HISTFILE": true,
}

// reservedSecretKeys are Secret keys that would overwrite the environment
// variables the provider itself injects for the entrypoint: mount-proxy
// exports every Secret entry verbatim and in a duplicate-key env the last
// occurrence wins, so a Secret key like "source" would replace the validated
// mount source, "readOnly" would weaken the read-only semantics derived from
// the CSI request, and "mountpoint" would redirect the mount target.
// Credentials never legitimately share names with these reserved keys.
var reservedSecretKeys = map[string]bool{
	"source": true, "readOnly": true, "mountpoint": true,
	"bucket": true, "url": true, "path": true, "otherOpts": true,
	"capacity": true, "fuseType": true, "authType": true,
	"storageType": true,
}

// optionValuePattern matches key=value pairs in option strings (e.g.
// "cache-size=1024") so that error messages can mask the value part, which
// may carry credential material. The key is any run of non-separator
// characters, possibly empty so a malformed "=secret" input is masked
// too: a narrow key charset would leave "x.y=Secret" unmasked and leak
// the value into error logs. Values are masked up to the next separator:
// a value containing a comma is an invalid mount option (comma is the
// option separator), and masking past the comma would swallow the
// following option from the error message entirely.
var optionValuePattern = regexp.MustCompile(`([^=,;\s]*)=([^,;\s]*)`)

// HasShellMetachar returns true when s contains characters that could be
// interpreted by a shell: ; | & ` $ ( ) \r \n \x00 and the escape
// character \ which can change how a later character is parsed.
func HasShellMetachar(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch c {
		case ';', '|', '&', '`', '$', '(', ')', '\\', '\r', '\n', '\x00':
			return true
		}
	}
	return false
}

// SafeForwardPattern matches values made only of characters that are safe
// to forward to the sidecar entrypoint: metadata URLs (redis://host:6379/0,
// s3://bucket, redis://[::1]:6379/0) and plain identifiers. It is shared by
// the control-plane provider and the sandbox-side node server so both ends
// reject the same inputs; the allowlist approach (rather than a denylist)
// keeps every future entrypoint safe even when it forgets to quote.
var SafeForwardPattern = regexp.MustCompile(`^[A-Za-z0-9_\-:/@.%\[\]]+$`)

// AllowedFuseTypes is the allowlist of FUSE client identifiers shared by
// the control-plane provider and the sandbox-side node server. Only clients
// with a shipped entrypoint belong here; "customfuse" is the default
// sentinel for "no client specified" (injected by the provider when neither
// fuseType nor CSI fsType is set), not a client type.
var AllowedFuseTypes = map[string]bool{
	"juicefs":    true,
	"s3fs":       true,
	"customfuse": true,
}

// CapacityPattern mirrors the JuiceFS entrypoint's accepted capacity forms:
// a plain integer or an integer with Ti/TiB, Gi/GiB, Mi/MiB, Ki/KiB units.
// It is shared by the control-plane provider and the sandbox-side node
// server so a value accepted at the control plane never fails later in the
// entrypoint. Both branches carry the 15-digit bound: unit-bearing values
// are multiplied in bash arithmetic, which must stay inside 64-bit range,
// and absurdly large plain integers are rejected early instead of failing
// quota set at mount time with only a warning.
var CapacityPattern = regexp.MustCompile(`^([0-9]{1,15}(TiB|Ti|GiB|Gi|MiB|Mi|KiB|Ki)|[0-9]{1,15})$`)

// MaskOptionValues masks the value of every key=value pair in an option
// string so error messages never echo potential credential material.
func MaskOptionValues(s string) string {
	return optionValuePattern.ReplaceAllString(s, "${1}=***")
}

// ValidateSecretData validates every entry of a Secret's Data map: the key
// must be a valid environment variable name, must not be a dangerous
// environment key, must not be a reserved provider key, and the value must
// be single-line. A value containing a newline would inject extra lines into
// the s3fs credential file.
func ValidateSecretData(data map[string][]byte) error {
	for key, value := range data {
		if err := validateSecretEntry(key, string(value)); err != nil {
			return err
		}
	}
	return nil
}

// ValidateSecrets validates every entry of a CSI request's Secrets map, which
// is how the sandbox-side node server receives Secret material.
func ValidateSecrets(secrets map[string]string) error {
	for key, value := range secrets {
		if err := validateSecretEntry(key, value); err != nil {
			return err
		}
	}
	return nil
}

func validateSecretEntry(key, value string) error {
	if err := ValidateEnvKeyName(key); err != nil {
		return fmt.Errorf("secret key %v", err)
	}
	if reservedSecretKeys[key] {
		return fmt.Errorf("secret key %q is reserved for provider-injected environment variables and must not be overridden", key)
	}
	// NUL is rejected along with newlines: environment values are C
	// strings, so an embedded NUL would truncate the variable at execve
	// time or make the exec fail outright. A tab is rejected too: the
	// s3fs credential file is parsed as access_key:secret_key pairs and
	// an embedded tab would corrupt that parsing.
	if strings.ContainsAny(value, "\n\r\x00\t") {
		return fmt.Errorf("secret %q must not contain a newline, tab or NUL byte", key)
	}
	return nil
}

// ValidateEnvKeyName checks a single key that may be exported as an
// environment variable to the entrypoint: it must be a valid variable name
// of a sane length and must not be one of the dangerous keys bash or glibc
// would act on.
func ValidateEnvKeyName(key string) error {
	if !secretKeyPattern.MatchString(key) {
		return fmt.Errorf("%q is not a valid environment variable name", key)
	}
	if len(key) > 256 {
		return fmt.Errorf("environment variable name is too long (%d bytes)", len(key))
	}
	if blockedMountOptionKeys[key] {
		return fmt.Errorf("%q is not allowed: it would be exported as a dangerous environment variable to the entrypoint", key)
	}
	return nil
}

// IsBlockedEnvKey reports whether key is one of the dangerous environment
// variables bash or glibc would act on. Used for VolumeContext keys on the
// control plane, where an invalid variable name is harmless (bash cannot
// reference it) but a dangerous exact-name key would take effect if
// mount-proxy ever starts exporting VolumeContext keys as environment
// variables — defense in depth.
func IsBlockedEnvKey(key string) bool {
	return blockedMountOptionKeys[key]
}

// IsReservedKey reports whether key is a provider-injected environment name
// that callers must not override through mount options or Secret keys.
func IsReservedKey(key string) bool {
	return reservedSecretKeys[key]
}

// credentialKeys lists key names that denote credential material and must
// not appear in volumeAttributes or in mount-option sub-keys. Credentials
// belong in a Kubernetes Secret referenced by the PV's
// NodePublishSecretRef, never in plain-text PV fields or option lists.
// The list is a best-effort guard against common mistakes; the security
// boundary is the Secret mechanism itself.
var credentialKeys = []string{
	"token", "accessKeyId", "accessKeySecret", "passphrase", "password", "passwd",
	"access-key", "secret-key", "access_key", "secret_key", "ak", "sk", "secret",
	"securityToken", "sessionToken", "authToken",
}

// IsCredentialKey reports whether key names a credential, matched
// case-insensitively. Mount-option sub-keys that look like credentials
// (e.g. "token=xxx" inside otherOpts) must be rejected rather than let
// the value travel into mount options and logs.
func IsCredentialKey(key string) bool {
	for _, credKey := range credentialKeys {
		if strings.EqualFold(key, credKey) {
			return true
		}
	}
	return false
}

// ValidateMountOptions checks every mount option entry (pv.Spec.MountOptions
// on the control plane, VolumeCapability.MountFlags on the node server). Each
// entry becomes one environment variable in the entrypoint, so shell
// metacharacters are rejected and entries whose keys are dangerous
// environment variables or provider-reserved keys are denied outright.
func ValidateMountOptions(opts []string) error {
	for _, opt := range opts {
		if HasShellMetachar(opt) {
			return fmt.Errorf("mount option %q contains invalid shell characters", MaskOptionValues(opt))
		}
		// Entries are split on space, tab and comma, mirroring the
		// entrypoint's option-list parsing: a comma-only split would let
		// "cache-size=1024 BASH_ENV=x" hide a dangerous key inside one
		// token. FieldsFunc also drops empty fields, so a whitespace
		// padded key (" BASH_ENV=x") cannot dodge the exact match either.
		for _, entry := range strings.FieldsFunc(opt, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			key, _, _ := strings.Cut(entry, "=")
			// A keyless "=value" entry is malformed and would dodge the
			// key-based checks below ("" is neither blocked nor reserved).
			if key == "" {
				return fmt.Errorf("mount option %q is not allowed: empty option key", MaskOptionValues(opt))
			}
			// subdir= follows the official JuiceFS CSI mountOptions
			// convention, but this driver mounts sub-directories via the
			// path volumeAttribute; a subdir option would reach the
			// client's -o string and be rejected or silently ignored.
			// Exact match only, consistent with the case-sensitive option
			// checks: the client treats option keys case-sensitively.
			if key == "subdir" {
				return fmt.Errorf("mount option %q is not supported: use the path volumeAttribute to mount a sub-directory", MaskOptionValues(opt))
			}
			if blockedMountOptionKeys[key] {
				return fmt.Errorf("mount option %q is not allowed: it would be exported as a dangerous environment variable to the entrypoint", MaskOptionValues(opt))
			}
			if reservedSecretKeys[key] {
				return fmt.Errorf("mount option %q is reserved for provider-injected mount semantics and must not be overridden", MaskOptionValues(opt))
			}
			if IsCredentialKey(key) {
				return fmt.Errorf("mount option %q must not carry credential material, put it in Secret instead", MaskOptionValues(opt))
			}
		}
	}
	return nil
}
