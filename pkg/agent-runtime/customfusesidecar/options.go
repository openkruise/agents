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

package customfusesidecar

import (
	"fmt"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/openkruise/agents/pkg/agent-runtime/customfusevalidate"
)

// fuseOptions are the parsed volume attributes of a customfuse volume.
// Field semantics follow the alibaba-cloud-csi-driver customfuse driver so
// entrypoints written for it behave identically here.
type fuseOptions struct {
	// Source is the mount source passed to the FUSE entrypoint as $source.
	// Its business format is opaque to the CSI side — a JuiceFS META-URL
	// (e.g. "redis://host:6379/1"), an OSS-style bucket:path, ... — but it
	// is still constrained to the shared SafeForwardPattern character set.
	Source string
	// Bucket is the object storage bucket name, passed as $bucket.
	Bucket string
	// URL is the object storage endpoint, passed as $url.
	URL string
	// OtherOpts originates from volumeAttributes.otherOpts, passed as a
	// single $otherOpts string that the entrypoint splits and appends to
	// -o. mountOptions entries (from volumeAttributes.mountOptions or
	// pv.Spec.MountOptions) become one env var each instead — a different
	// transport, not a drop-in equivalent.
	OtherOpts string
	// Path is the sub-path within the volume, passed as $path.
	Path string
	// ReadOnly is derived from the CSI readOnly flag or the volume access
	// mode, passed as $readOnly to the entrypoint.
	ReadOnly bool
	// FuseType identifies the FUSE client (e.g. "juicefs", "s3fs").
	// Defaults to "customfuse" when unset. Validated against the shared
	// AllowedFuseTypes allowlist.
	FuseType string
	// MountOptions combines volumeAttributes.mountOptions entries (split
	// by parseOptions) and pv.Spec.MountOptions (via
	// VolumeCapability.Mount.MountFlags). Each entry is a "key=value" or
	// bare "key" string, passed as env var $key in the entrypoint.
	MountOptions []string
	// AuthType selects the authentication method. Only the default (empty)
	// is supported, which passes Secret entries directly as env vars.
	AuthType string
	// Capacity is the volume quota passed as $capacity to the entrypoint.
	// Validated against customfusevalidate.CapacityPattern: a plain integer
	// or an integer with Ti/TiB, Gi/GiB, Mi/MiB, Ki/KiB units, both
	// branches bounded to 15 digits — the same set the JuiceFS entrypoint
	// accepts.
	Capacity string
	// StorageType is the object storage backend passed as $storageType to the
	// entrypoint (e.g. "s3", "oss", "minio"). Only the JuiceFS entrypoint
	// consumes it, during format.
	StorageType string
}

// customFuseFsType is the fsType reported to mount-proxy; it selects the
// customfuse driver there.
const customFuseFsType = "customfuse"

// parseOptions converts a NodePublishVolumeRequest into fuseOptions. Keys in
// VolumeContext are matched case-insensitively after trimming.
func parseOptions(req *csi.NodePublishVolumeRequest) (*fuseOptions, error) {
	opts := &fuseOptions{
		FuseType: customFuseFsType,
	}

	if err := applyVolumeContext(opts, req.GetVolumeContext()); err != nil {
		return nil, err
	}
	if err := applyVolumeCapability(opts, req.GetVolumeCapability()); err != nil {
		return nil, err
	}
	// The allowlist check runs after the FsType override so a value that
	// arrives via VolumeCapability.Mount.FsType is validated too.
	if opts.FuseType != "" && !customfusevalidate.AllowedFuseTypes[strings.ToLower(opts.FuseType)] {
		return nil, fmt.Errorf("unknown fuseType %q", customfusevalidate.MaskOptionValues(opts.FuseType))
	}
	if req.GetReadonly() {
		opts.ReadOnly = true
	}
	synthesizeSource(opts)
	if err := validateForwardedFields(opts); err != nil {
		return nil, err
	}
	return opts, nil
}

// applyVolumeContext merges the request's VolumeContext keys into opts.
// Multiple case variants of the same key are rejected when their values
// differ: the provider does the same, and a direct socket caller must
// not get a mount whose source depends on map iteration order.
func applyVolumeContext(opts *fuseOptions, volCtx map[string]string) error {
	seen := map[string]string{}
	for k, v := range volCtx {
		key := strings.TrimSpace(strings.ToLower(k))
		if key == "" {
			continue
		}
		value := strings.TrimSpace(v)
		if value == "" {
			continue
		}
		if existing, ok := seen[key]; ok {
			if existing != value {
				return fmt.Errorf("conflicting values for %q: %q and %q", key, customfusevalidate.MaskOptionValues(existing), customfusevalidate.MaskOptionValues(value))
			}
			continue
		}
		seen[key] = value
		switch key {
		case "source":
			opts.Source = value
		case "bucket":
			opts.Bucket = value
		case "path":
			opts.Path = value
		case "url":
			opts.URL = value
		case "otheropts":
			opts.OtherOpts = value
		case "mountoptions":
			// Same option-list semantics as pv.Spec.MountOptions; split on
			// the separator set the provider's ValidateMountOptions uses so
			// each entry becomes one env var in the entrypoint.
			opts.MountOptions = append(opts.MountOptions, strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '	' })...)
		case "fusetype":
			opts.FuseType = value
		case "authtype":
			opts.AuthType = value
		case "capacity":
			if !customfusevalidate.CapacityPattern.MatchString(value) {
				return fmt.Errorf("invalid capacity %q: must be a plain integer or one of Ti/TiB, Gi/GiB, Mi/MiB, Ki/KiB units (e.g. 100, 100Gi)", customfusevalidate.MaskOptionValues(value))
			}
			opts.Capacity = value
		case "storagetype":
			opts.StorageType = value
		}
	}
	return nil
}

// applyVolumeCapability merges the CSI volume capability into opts.
func applyVolumeCapability(opts *fuseOptions, volCap *csi.VolumeCapability) error {
	if volCap == nil {
		return nil
	}
	if mount := volCap.GetMount(); mount != nil {
		if mount.FsType != "" {
			// Case-insensitive like the provider's cross-check: a
			// casing-only difference is not a conflict.
			if opts.FuseType != customFuseFsType && !strings.EqualFold(opts.FuseType, mount.FsType) {
				return fmt.Errorf("fuseType %q from volumeAttributes conflicts with fsType %q from PV spec", opts.FuseType, mount.FsType)
			}
			opts.FuseType = mount.FsType
		}
		// Empty flags are dropped for parity with the volumeAttributes
		// mountOptions path, where FieldsFunc filters them out.
		for _, f := range mount.MountFlags {
			if strings.TrimSpace(f) != "" {
				opts.MountOptions = append(opts.MountOptions, f)
			}
		}
	}
	switch volCap.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY:
		opts.ReadOnly = true
	}
	return nil
}

// synthesizeSource derives the mount source from bucket (with optional path)
// when no explicit source was given.
func synthesizeSource(opts *fuseOptions) {
	if opts.Source == "" && opts.Bucket != "" {
		if opts.Path != "" {
			opts.Source = opts.Bucket + ":" + opts.Path
		} else {
			opts.Source = opts.Bucket
		}
	}
}

// validateForwardedFields enforces the shared character set and
// shell-metachar checks on the fields forwarded to the entrypoint, mirroring
// the control-plane provider's defense in depth.
func validateForwardedFields(opts *fuseOptions) error {
	for _, field := range []struct{ name, value string }{
		{"source", opts.Source}, {"bucket", opts.Bucket}, {"path", opts.Path},
		{"url", opts.URL}, {"storageType", opts.StorageType},
	} {
		if field.value != "" && !customfusevalidate.SafeForwardPattern.MatchString(field.value) {
			return fmt.Errorf("%s contains invalid characters: %q", field.name, customfusevalidate.MaskOptionValues(field.value))
		}
	}
	for _, field := range []struct{ name, value string }{
		{"otherOpts", opts.OtherOpts}, {"capacity", opts.Capacity},
	} {
		if customfusevalidate.HasShellMetachar(field.value) {
			return fmt.Errorf("%s contains invalid shell characters: %q", field.name, customfusevalidate.MaskOptionValues(field.value))
		}
	}
	return nil
}

// precheckAuthConfig validates the auth configuration before mounting.
// Only the default auth type (Secret passthrough) is supported.
func precheckAuthConfig(opts *fuseOptions) error {
	if opts.AuthType != "" {
		return fmt.Errorf("unsupported authType %q; only default (secret passthrough) is currently supported", opts.AuthType)
	}
	return nil
}

// makeMountOptions serializes volume attributes as key=value pairs carried
// through the mount-proxy protocol. mount-proxy maps them to env vars for
// the entrypoint. Source is passed separately via proxyMountRequest.Source.
func (o *fuseOptions) makeMountOptions() []string {
	var opts []string
	if o.Bucket != "" {
		opts = append(opts, "bucket="+o.Bucket)
	}
	if o.URL != "" {
		opts = append(opts, "url="+o.URL)
	}
	if o.Path != "" {
		opts = append(opts, "path="+o.Path)
	}
	if o.OtherOpts != "" {
		opts = append(opts, "otherOpts="+o.OtherOpts)
	}
	if o.Capacity != "" {
		opts = append(opts, "capacity="+o.Capacity)
	}
	if o.StorageType != "" {
		opts = append(opts, "storageType="+o.StorageType)
	}
	opts = append(opts, o.MountOptions...)
	// readOnly is appended last so a conflicting entry in pv.Spec.MountOptions
	// (e.g. "readOnly=false") cannot weaken the read-only semantics derived
	// from the CSI request: mount-proxy builds the entrypoint env from this
	// slice in order, and in a duplicate-key env the last occurrence wins.
	if o.ReadOnly {
		opts = append(opts, "readOnly=true")
	}
	return opts
}

// The capacity validation lives in customfusevalidate.CapacityPattern so
// the provider and the node server share one regexp.
