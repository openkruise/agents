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

package storages

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	corev1 "k8s.io/api/core/v1"

	"github.com/openkruise/agents/pkg/agent-runtime/customfusevalidate"
)

// allowedFuseTypes and safeForwardPattern live in customfusevalidate so
// the provider and the sandbox-side node server share one allowlist and
// one charset; see customfusevalidate.AllowedFuseTypes and
// customfusevalidate.SafeForwardPattern.

// credentialKeys lives in customfusevalidate so the provider and the
// mount-option validation share one list; see
// customfusevalidate.IsCredentialKey.

// mountPathPattern matches absolute, shell-safe container mount targets.
var mountPathPattern = regexp.MustCompile(`^/[A-Za-z0-9_\-./]+$`)

// credInURLPattern matches userinfo (user:pass) embedded in a URL. It masks
// up to the last '@' before the path or whitespace so that URLs containing
// multiple '@' segments never leak credentials into error messages.
var credInURLPattern = regexp.MustCompile(`://[^/\s]*@`)

// credUserInfoPattern matches userinfo without a scheme (user:pass@host),
// which safeForwardPattern allows through but a scheme-bearing pattern would
// miss in error output: the value is rejected by the charset check only when
// it also carries an invalid character, and the error message must not echo
// the credential part.
var credUserInfoPattern = regexp.MustCompile(`([^/\s:@]+:)[^/@\s]*@`)

// queryValuePattern matches name=value pairs in URL query strings and
// fragments (e.g. ?token=xxx or ?X-Amz-Signature=yyy) so their values can be
// masked in error messages. Query URLs never pass the source allowlist, but
// they must not leak credentials into logs when rejected. A URL-encoded
// equals sign (%3D) counts as the separator too.
var queryValuePattern = regexp.MustCompile(`([?&#][^=&\s]*(?:=|%3[Dd]))[^&\s#]*`)

// CustomFuseMountProvider generates CSI NodePublishVolume requests for the
// generic FUSE driver (customfuseplugin.csi.openkruise.io). It validates
// inputs for shell safety and credential hygiene, then forwards the PV's
// volume attributes and the referenced Secret verbatim to the FUSE
// entrypoint inside the sandbox csi-sidecar container.
type CustomFuseMountProvider struct{}

// GenerateCSINodePublishVolumeRequest validates the PV, Secret and mount
// target, then builds a NodePublishVolumeRequest whose VolumeContext and
// Secrets pass through unchanged to the FUSE entrypoint. The returned
// request is never partially built alongside an error: callers can rely on
// (nil, err) when validation fails.
func (p *CustomFuseMountProvider) GenerateCSINodePublishVolumeRequest(
	ctx context.Context,
	containerMountTarget string,
	pv *corev1.PersistentVolume,
	readOnly bool,
	secret *corev1.Secret,
) (*csi.NodePublishVolumeRequest, error) {
	if err := p.validate(containerMountTarget, pv, secret); err != nil {
		return nil, err
	}

	// VolumeContext: copy all VolumeAttributes as-is
	// validate() guarantees VolumeAttributes is non-nil (source is required),
	// so the fuseType default below cannot assign to a nil map. The explicit
	// nil check keeps that guarantee from becoming a panic if the validate
	// call order ever changes.
	volCtx := maps.Clone(pv.Spec.CSI.VolumeAttributes)
	if volCtx == nil {
		return nil, fmt.Errorf("volumeAttributes must not be nil")
	}
	// Whitespace-only values count as unset everywhere; drop their keys
	// so the VolumeContext carries only meaningful entries. mount-proxy's
	// parseOptions extracts known fields only, so stray empty keys are
	// inert today — dropping them guards against a future revision that
	// exports every VolumeContext key as an environment variable.
	for k, v := range volCtx {
		if strings.TrimSpace(v) == "" {
			delete(volCtx, k)
		}
	}

	// Set fuseType default after copy so the original is not modified.
	// validate() accepts case-insensitive input; normalize to lowercase here
	// so the entrypoint always receives the canonical identifier. All case
	// variants of the key are collapsed into the canonical lowercase key so
	// parseOptions, which extracts keys case-insensitively, sees exactly one
	// value and the FsType/VolumeContext cross-check can never fire on
	// provider-generated requests. validateFuseType rejected conflicting
	// variants and mismatching CSI fsType, so the first non-empty value
	// below is unambiguous.
	fuseType := "customfuse"
	if fst := strings.ToLower(strings.TrimSpace(pv.Spec.CSI.FSType)); fst != "" {
		fuseType = fst
	}
	for k, v := range volCtx {
		if strings.EqualFold(k, "fuseType") {
			if strings.TrimSpace(v) != "" {
				fuseType = strings.ToLower(strings.TrimSpace(v))
			}
			delete(volCtx, k)
		}
	}
	volCtx["fuseType"] = fuseType

	// Secrets: copy all Secret entries as-is
	// Each key becomes an environment variable of the same name in the
	// entrypoint, so the entrypoint author controls the env-var schema by
	// naming the Secret keys accordingly.
	secrets := make(map[string]string)
	if secret != nil {
		for k, v := range secret.Data {
			secrets[k] = string(v)
		}
	}

	// Read-only must be derived from both the requested mount and the PV
	// access modes; do not weaken either source.
	isReadOnly := IsPureReadOnly(pv.Spec.AccessModes) || readOnly

	// The access mode follows the PV's access modes: the On-Demand Volume
	// Mounting doc shares one PV across sandboxes, so a ReadWriteMany
	// volume must advertise MULTI_NODE_MULTI_WRITER instead of a
	// single-node default. A read-only mount is advertised as READER_ONLY
	// so the CSI plugin and the entrypoint can rely on the AccessMode
	// semantics.
	accessMode := csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
	if isReadOnly {
		accessMode = csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY
		if hasAccessMode(pv, corev1.ReadOnlyMany) || hasAccessMode(pv, corev1.ReadWriteMany) {
			accessMode = csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY
		}
	} else if hasAccessMode(pv, corev1.ReadWriteMany) {
		accessMode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
	}

	// VolumeId must be stable per volume because the CSI node plugin keys
	// mount state by it. Prefer the storage-system handle and fall back to
	// the PV name for statically provisioned volumes.
	volumeID := pv.Name
	if pv.Spec.CSI.VolumeHandle != "" {
		volumeID = pv.Spec.CSI.VolumeHandle
	}

	// Build the CSI request
	return &csi.NodePublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: containerMountTarget,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{
					FsType:     volCtx["fuseType"],
					MountFlags: pv.Spec.MountOptions,
				},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: accessMode,
			},
		},
		VolumeContext: volCtx,
		Secrets:       secrets,
		Readonly:      isReadOnly,
	}, nil
}

// validate performs structural, security, and field-level checks on the
// provider inputs. It returns nil when the input is safe and complete enough
// to construct a CSI request.
func (p *CustomFuseMountProvider) validate(containerMountTarget string, pv *corev1.PersistentVolume, secret *corev1.Secret) error {
	if err := validateVolume(containerMountTarget, pv); err != nil {
		return err
	}
	volCtx := pv.Spec.CSI.VolumeAttributes
	if err := validateVolumeAttributeKeys(volCtx); err != nil {
		return err
	}
	if err := validateUnambiguousAttrs(volCtx); err != nil {
		return err
	}
	if err := validateForwardedFields(volCtx); err != nil {
		return err
	}
	if err := validateMountTarget(containerMountTarget); err != nil {
		return err
	}
	if err := validateFuseType(volCtx, pv.Spec.CSI.FSType); err != nil {
		return err
	}
	if err := validateShellSafeFields(volCtx, pv.Spec.MountOptions); err != nil {
		return err
	}
	if err := validateCredentialSeparation(volCtx); err != nil {
		return err
	}
	return validateSecret(secret)
}

// validateVolume checks the structural preconditions: a non-nil PV with a
// CSI spec and a non-empty mount target.
func validateVolume(containerMountTarget string, pv *corev1.PersistentVolume) error {
	if pv == nil {
		return fmt.Errorf("persistent volume object is nil")
	}
	if strings.TrimSpace(containerMountTarget) == "" {
		return fmt.Errorf("containerMountTarget is empty")
	}
	if pv.Spec.CSI == nil {
		return fmt.Errorf("no CSI spec in persistent volume")
	}
	return nil
}

// forEachAttr applies fn to the value of every volCtx key whose
// case-insensitive form matches attr. parseOptions extracts keys from the
// VolumeContext case-insensitively, so a key like "CAPACITY" would otherwise
// bypass every check here; every variant must be validated and any invalid
// value rejects the request.
func forEachAttr(volCtx map[string]string, attr string, fn func(value string) error) error {
	for k, v := range volCtx {
		if strings.EqualFold(k, attr) {
			if err := fn(v); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateVolumeAttributeKeys rejects volumeAttributes keys that are
// dangerous environment variable names. mount-proxy does not export
// VolumeContext keys as environment variables today (parseOptions extracts
// only known fields), but a future revision might — this is defense in
// depth. Invalid variable names stay allowed: bash cannot reference them,
// so they are inert rather than dangerous, and rejecting them would break
// the passthrough of arbitrary provider-specific keys.
func validateVolumeAttributeKeys(volCtx map[string]string) error {
	for key := range volCtx {
		if customfusevalidate.IsBlockedEnvKey(key) {
			return fmt.Errorf("volumeAttributes key %q is not allowed: it would be exported as a dangerous environment variable to the entrypoint", key)
		}
	}
	return nil
}

// validateUnambiguousAttrs rejects volumeAttributes where multiple case
// variants of the same known field carry different non-empty values.
// parseOptions extracts keys case-insensitively by iterating the map, so
// duplicate variants would make the effective value depend on map iteration
// order. fuseType is handled in validateFuseType.
func validateUnambiguousAttrs(volCtx map[string]string) error {
	for _, attr := range []string{"source", "bucket", "url", "path", "otherOpts", "mountOptions", "capacity", "storageType", "authType"} {
		first := ""
		if err := forEachAttr(volCtx, attr, func(value string) error {
			// Compare trimmed values so whitespace-only differences are
			// reported by the character-set check below (the more
			// accurate error) rather than as a spurious conflict.
			value = strings.TrimSpace(value)
			if value == "" {
				return nil
			}
			if first == "" {
				first = value
				return nil
			}
			if value != first {
				return fmt.Errorf("conflicting %s values %q and %q", attr, maskURLCreds(first), maskURLCreds(value))
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// validateForwardedFields checks every volumeAttributes field that is
// forwarded to the sidecar entrypoint as an environment variable.
//
// source is required and shell-safe: it may be interpolated into a mount
// command, so it must match a strict character set rather than a denylist.
// The charset check runs on the raw value because the raw value is what gets
// forwarded; the error message masks credentials embedded in the URL so they
// never reach logs.
//
// url, bucket, path and storageType are optional and must match the same
// safe character set. Whitespace-only values count as unset. url may embed
// credentials (http://user:pass@host), so its error output is masked.
func validateForwardedFields(volCtx map[string]string) error {
	// Every case variant of source is checked because parseOptions extracts
	// keys case-insensitively; whitespace-only variants count as unset and
	// an all-unset source set is missing.
	sourceSet := false
	if err := forEachAttr(volCtx, "source", func(value string) error {
		if strings.TrimSpace(value) == "" {
			return nil
		}
		sourceSet = true
		if !customfusevalidate.SafeForwardPattern.MatchString(value) {
			return fmt.Errorf("source %q contains invalid characters", maskURLCreds(value))
		}
		return nil
	}); err != nil {
		return err
	}
	if !sourceSet {
		return fmt.Errorf("source is required for customfuse driver (e.g. JuiceFS META-URL like redis://host:6379/0)")
	}

	// url, bucket, path and storageType are optional: whitespace-only values
	// count as unset. When present they must match the same safe character set
	// as source; url may embed credentials (http://user:pass@host), so its
	// error output is masked.
	for _, field := range []string{"url", "bucket", "path", "storageType"} {
		if err := forEachAttr(volCtx, field, func(value string) error {
			if strings.TrimSpace(value) == "" {
				return nil
			}
			if !customfusevalidate.SafeForwardPattern.MatchString(value) {
				return fmt.Errorf("%s %q contains invalid characters", field, maskURLCreds(value))
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// validateMountTarget checks that the container mount target is an absolute
// path made only of safe characters, and that it does not traverse to the
// parent directory. Traversal would let a malicious volume escape the
// mount-root subtree and shadow host paths.
func validateMountTarget(containerMountTarget string) error {
	if !strings.HasPrefix(containerMountTarget, "/") {
		return fmt.Errorf("containerMountTarget %q must be absolute", containerMountTarget)
	}
	if !mountPathPattern.MatchString(containerMountTarget) {
		return fmt.Errorf("containerMountTarget %q must contain only safe characters", containerMountTarget)
	}
	if strings.Contains(containerMountTarget, "//") {
		return fmt.Errorf("containerMountTarget %q must not contain empty path segments", containerMountTarget)
	}
	for _, seg := range strings.Split(containerMountTarget, "/") {
		if seg == ".." {
			return fmt.Errorf("containerMountTarget %q must not traverse to the parent directory", containerMountTarget)
		}
	}
	return nil
}

// validateFuseType restricts volumeAttributes.fuseType to the allowlist and
// checks it against the PV's CSI fsType. Empty means "customfuse" (the
// provider default), set by the caller after validation. The match is
// case-insensitive; the canonical lowercase value is what reaches the
// entrypoint. Conflicting case variants of fuseType are rejected: the
// effective value would otherwise depend on map iteration order.
func validateFuseType(volCtx map[string]string, fsType string) error {
	// fsType is trimmed for the same reason as the volumeAttributes
	// variants: trailing whitespace in YAML is invisible and must not
	// turn a valid value into an unknown one.
	fsType = strings.TrimSpace(fsType)
	if fsType != "" && !customfusevalidate.AllowedFuseTypes[strings.ToLower(fsType)] {
		return fmt.Errorf("unknown CSI fsType %q (allowed: %v)", customfusevalidate.MaskOptionValues(fsType), mapKeys(customfusevalidate.AllowedFuseTypes))
	}
	var explicit string
	// Case variants of the key are checked because parseOptions extracts
	// keys case-insensitively.
	if err := forEachAttr(volCtx, "fuseType", func(value string) error {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil
		}
		low := strings.ToLower(value)
		if !customfusevalidate.AllowedFuseTypes[low] {
			return fmt.Errorf("unknown fuseType %q (allowed: %v)", customfusevalidate.MaskOptionValues(value), mapKeys(customfusevalidate.AllowedFuseTypes))
		}
		if explicit != "" && explicit != low {
			return fmt.Errorf("conflicting fuseType values %q and %q", customfusevalidate.MaskOptionValues(explicit), customfusevalidate.MaskOptionValues(low))
		}
		explicit = low
		return nil
	}); err != nil {
		return err
	}
	if explicit != "" && fsType != "" && !strings.EqualFold(explicit, fsType) {
		return fmt.Errorf("fuseType %q conflicts with CSI fsType %q", customfusevalidate.MaskOptionValues(explicit), customfusevalidate.MaskOptionValues(fsType))
	}
	return nil
}

// validateShellSafeFields checks every option string that mount-proxy will
// export as an environment variable and that the entrypoint may interpolate
// into a mount command: the otherOpts/mountOptions/capacity volumeAttributes
// fields, and pv.Spec.MountOptions (each entry becomes one env var). Shell
// metacharacters are rejected, and mountOptions entries whose keys are
// environment variables with code-execution or command-redirection side
// effects are denied outright.
func validateShellSafeFields(volCtx map[string]string, mountOptions []string) error {
	// Case variants of the keys are checked because parseOptions extracts
	// keys case-insensitively.
	for _, field := range []string{"otherOpts", "mountOptions", "capacity"} {
		if err := forEachAttr(volCtx, field, func(value string) error {
			if customfusevalidate.HasShellMetachar(value) {
				return fmt.Errorf("%s contains invalid shell characters: %q", field, customfusevalidate.MaskOptionValues(value))
			}
			// Same format the node server and the JuiceFS entrypoint
			// accept, so a value passing the control plane never fails
			// later in the chain. Trimmed like every other field:
			// whitespace-only counts as unset.
			if field == "capacity" {
				trimmed := strings.TrimSpace(value)
				if trimmed != "" && !customfusevalidate.CapacityPattern.MatchString(trimmed) {
					return fmt.Errorf("invalid capacity %q: must be a plain integer or one of Ti/TiB, Gi/GiB, Mi/MiB, Ki/KiB units (e.g. 100, 100Gi)", customfusevalidate.MaskOptionValues(trimmed))
				}
			}
			// otherOpts/mountOptions are option lists ("key=value,...") that
			// the entrypoint appends to the -o string after the
			// provider-composed options, so a reserved key inside them would
			// override provider-injected mount semantics just like a
			// pv.Spec.MountOptions entry. capacity is a plain value and has no
			// option list. Options are split on both space and comma because
			// the entrypoint accepts either separator; splitting on comma
			// alone would let a space-separated entry ("cache-size=1024
			// source=evil") hide a reserved key inside one token.
			if field != "capacity" {
				for _, opt := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
					key, _, _ := strings.Cut(opt, "=")
					// Same rejection as ValidateMountOptions: a keyless
					// "=value" entry is malformed and would dodge the
					// key-based check below ("" is not reserved).
					if key == "" {
						return fmt.Errorf("%s option %q is not allowed: empty option key", field, customfusevalidate.MaskOptionValues(opt))
					}
					// Same rejection as ValidateMountOptions: subdir= follows
					// the official JuiceFS CSI mountOptions convention but is
					// not supported here (use the path volumeAttribute).
					// Exact match only, consistent with the case-sensitive
					// option checks: the client treats option keys
					// case-sensitively.
					if key == "subdir" {
						return fmt.Errorf("%s option %q is not supported: use the path volumeAttribute to mount a sub-directory", field, customfusevalidate.MaskOptionValues(opt))
					}
					if customfusevalidate.IsReservedKey(key) {
						return fmt.Errorf("%s option %q is reserved for provider-injected mount semantics and must not be overridden", field, customfusevalidate.MaskOptionValues(opt))
					}
					// Credential-like sub-keys inside option lists would
					// carry their values into mount options and logs.
					if customfusevalidate.IsCredentialKey(key) {
						return fmt.Errorf("%s option %q must not carry credential material, put it in Secret instead", field, customfusevalidate.MaskOptionValues(opt))
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return customfusevalidate.ValidateMountOptions(mountOptions)
}

// validateCredentialSeparation forces token, access keys, and passphrases to
// live in a Kubernetes Secret (PV.Spec.CSI.NodePublishSecretRef) instead of
// plain-text VolumeAttributes.
func validateCredentialSeparation(volCtx map[string]string) error {
	// Keys are matched case-insensitively against the shared credential
	// list (customfusevalidate.IsCredentialKey), mirroring parseOptions.
	for key := range volCtx {
		if customfusevalidate.IsCredentialKey(key) {
			return fmt.Errorf("credential %q must not be in volumeAttributes, put it in Secret instead", key)
		}
	}
	return nil
}

// validateSecret checks that every Secret key is a valid environment variable
// name, is not a dangerous environment key, is not a reserved provider key,
// and that every value is single-line. The customfuse contract maps each
// Secret key to an environment variable of the same name in the entrypoint:
// bash cannot reference names with dashes or leading digits, a key like
// BASH_ENV would be sourced by the entrypoint shell at startup (arbitrary
// code execution), a reserved key would override provider-injected mount
// semantics, and a value containing a newline would inject extra lines into
// the s3fs credential file.
func validateSecret(secret *corev1.Secret) error {
	if secret == nil {
		return nil
	}
	return customfusevalidate.ValidateSecretData(secret.Data)
}

// maskURLCreds replaces credentials embedded in URLs with *** so that error
// messages never leak them into logs. It masks userinfo and query/fragment
// values, where credentials canonically live. URL path segments are not
// masked: paths can legitimately carry bucket/object keys and masking them
// would corrupt the diagnostic value of the error message.
func maskURLCreds(s string) string {
	s = credInURLPattern.ReplaceAllString(s, "://***@")
	s = credUserInfoPattern.ReplaceAllString(s, "${1}***@")
	return queryValuePattern.ReplaceAllString(s, "${1}***")
}

// mapKeys returns the sorted keys of m. It is used only for error messages;
// sorting keeps the allowed list stable across calls and test runs.
func mapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
