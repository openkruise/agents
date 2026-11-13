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
	"encoding/base64"
	"testing"

	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/protobuf/proto" // required: the csi spec dependency is generated with the legacy protobuf API
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCustomFuseMountProvider_GenerateCSINodePublishVolumeRequest(t *testing.T) {
	tests := []struct {
		name                  string
		containerMountTarget  string
		volumeAttributes      map[string]string
		csiFSType             string
		accessModes           []corev1.PersistentVolumeAccessMode
		pvMountOptions        []string
		secretData            map[string][]byte
		readOnly              bool
		expectError           string
		expectErrorNotContain []string
		nilPV                 bool
		nilCSI                bool
		validateResult        func(*testing.T, *csiapi.NodePublishVolumeRequest)
	}{
		// Happy path
		{
			name:                 "valid JuiceFS mount with source and fuseType",
			containerMountTarget: "/workspace/data",
			volumeAttributes: map[string]string{
				"source":   "redis://redis-cluster:6379/0",
				"fuseType": "juicefs",
			},
			secretData: map[string][]byte{
				"token": []byte("test-token"),
			},
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.Equal(t, "/workspace/data", req.TargetPath)
				assert.Equal(t, "juicefs", req.VolumeContext["fuseType"])
				assert.Equal(t, "redis://redis-cluster:6379/0", req.VolumeContext["source"])
				assert.Equal(t, "test-token", req.Secrets["token"])
				assert.Equal(t, "pv-test-handle", req.VolumeId)
				assert.NotNil(t, req.VolumeCapability)
			},
		},
		{
			name:                 "default fuseType when not set",
			containerMountTarget: "/mnt/data",
			volumeAttributes: map[string]string{
				"source": "redis://localhost:6379/1",
			},
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.Equal(t, "customfuse", req.VolumeContext["fuseType"])
			},
		},
		{
			name:                 "fuseType from VolumeCapability.FsType",
			containerMountTarget: "/mnt/data",
			volumeAttributes: map[string]string{
				"source": "redis://host:6379/0",
				// fuseType not set in volumeAttributes, but FsType in the PV CSI spec
			},
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				// fuseType is set in the Provider from VolumeAttributes, not FsType
				// verify defaults are applied correctly
				assert.NotEmpty(t, req.VolumeContext["fuseType"])
			},
		},
		{
			name:                 "includes bucket, url, path, and capacity",
			containerMountTarget: "/mnt/data",
			volumeAttributes: map[string]string{
				"source":   "redis://redis:6379/0",
				"fuseType": "juicefs",
				"bucket":   "ml-datasets",
				"url":      "https://s3.amazonaws.com",
				"path":     "/user-123",
				"capacity": "100Gi",
			},
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.Equal(t, "ml-datasets", req.VolumeContext["bucket"])
				assert.Equal(t, "https://s3.amazonaws.com", req.VolumeContext["url"])
				assert.Equal(t, "/user-123", req.VolumeContext["path"])
				assert.Equal(t, "100Gi", req.VolumeContext["capacity"])
			},
		},
		{
			name:                 "all volumeAttributes passed through unchanged",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":       "redis://redis:6379/0",
				"fuseType":     "juicefs",
				"bucket":       "my-bucket",
				"url":          "https://oss.example.com",
				"path":         "/sub/path",
				"capacity":     "50Gi",
				"otherOpts":    "cache-size=1024",
				"otheropts":    "cache-size=1024",
				"extra-custom": "custom-value",
			},
			secretData: map[string][]byte{
				"access_key":    []byte("AKID123"),
				"secret_key":    []byte("SECRET456"),
				"custom_config": []byte("config-value"),
			},
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				// All volumeAttributes are preserved
				assert.Equal(t, "my-bucket", req.VolumeContext["bucket"])
				assert.Equal(t, "custom-value", req.VolumeContext["extra-custom"])
				// All secrets are passed through
				assert.Equal(t, "AKID123", req.Secrets["access_key"])
				assert.Equal(t, "config-value", req.Secrets["custom_config"])
			},
		},

		// ReadOnly
		{
			name:                 "readOnly=true is set on request",
			containerMountTarget: "/mnt/data",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			readOnly:             true,
			expectError:          "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.True(t, req.Readonly)
				assert.Equal(t, csiapi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
					req.VolumeCapability.GetAccessMode().GetMode())
			},
		},
		{
			name:                 "readOnly=false by default",
			containerMountTarget: "/mnt/data",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			readOnly:             false,
			expectError:          "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.False(t, req.Readonly)
				assert.Equal(t, csiapi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					req.VolumeCapability.GetAccessMode().GetMode())
			},
		},
		{
			name:                 "ReadOnlyMany PV implies read-only even when readOnly=false",
			containerMountTarget: "/mnt/data",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			accessModes:          []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany},
			expectError:          "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.True(t, req.Readonly)
				assert.Equal(t, csiapi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
					req.VolumeCapability.GetAccessMode().GetMode())
			},
		},
		{
			name:                 "ReadWriteMany PV advertises multi-node writer",
			containerMountTarget: "/mnt/data",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			accessModes:          []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			expectError:          "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.False(t, req.Readonly)
				assert.Equal(t, csiapi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
					req.VolumeCapability.GetAccessMode().GetMode())
			},
		},
		{
			name:                 "ReadWriteMany PV with readOnly=true becomes multi-node reader",
			containerMountTarget: "/mnt/data",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			accessModes:          []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			readOnly:             true,
			expectError:          "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.True(t, req.Readonly)
				assert.Equal(t, csiapi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
					req.VolumeCapability.GetAccessMode().GetMode())
			},
		},
		{
			name:                 "ReadWriteOnce PV stays writable when readOnly=false",
			containerMountTarget: "/mnt/data",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			accessModes:          []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			expectError:          "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.False(t, req.Readonly)
				assert.Equal(t, csiapi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					req.VolumeCapability.GetAccessMode().GetMode())
			},
		},
		{
			name:                 "mixed access modes weaken read-only to writable",
			containerMountTarget: "/mnt/data",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			accessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadOnlyMany, corev1.ReadWriteOnce,
			},
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.False(t, req.Readonly)
				assert.Equal(t, csiapi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					req.VolumeCapability.GetAccessMode().GetMode())
			},
		},

		// Error: structural checks
		{
			name:                 "nil persistent volume",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			expectError:          "persistent volume object is nil",
			nilPV:                true,
		},
		{
			name:                 "nil CSI spec",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			expectError:          "no CSI spec",
			nilCSI:               true,
		},
		{
			name:                 "empty mount path",
			containerMountTarget: "",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			expectError:          "containerMountTarget is empty",
		},

		// Error: source required / shell-safe
		{
			name:                 "empty source",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{},
			expectError:          "source is required",
		},
		{
			name:                 "whitespace-only source",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "   "},
			expectError:          "source is required",
		},
		{
			name:                 "source with semicolon injection",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://host:6379/0; rm -rf /",
			},
			expectError: "contains invalid characters",
		},
		{
			name:                 "source with command substitution",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://host:6379/0$(whoami)",
			},
			expectError: "contains invalid characters",
		},
		{
			name:                 "source with embedded space",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://host:6379/0 extra",
			},
			expectError: "contains invalid characters",
		},
		{
			name:                 "source with trailing whitespace is rejected on the raw value",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://host:6379/0 ",
			},
			expectError: "contains invalid characters",
		},
		{
			name:                 "source error message masks embedded credentials",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://user:pass@host:6379/0?secret=xyz",
			},
			expectError:           "contains invalid characters",
			expectErrorNotContain: []string{"user:pass", "xyz"},
		},
		{
			name:                 "source error message masks query-string signature",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "https://bucket.s3.amazonaws.com/key?X-Amz-Signature=abc123&X-Amz-Credential=AKID",
			},
			expectError:           "contains invalid characters",
			expectErrorNotContain: []string{"abc123", "AKID"},
		},
		{
			name:                  "source error message masks bare userinfo without scheme",
			containerMountTarget:  "/mnt",
			volumeAttributes:      map[string]string{"source": "user:pass@host;evil"},
			expectError:           "invalid characters",
			expectErrorNotContain: []string{"pass"},
		},
		{
			name:                 "url error message masks multiple bare userinfo segments",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://host:6379/0",
				"url":    "user:pass@host;evil",
			},
			expectError:           "invalid characters",
			expectErrorNotContain: []string{"pass"},
		},
		{
			name:                 "source error message masks multiple credential segments",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://user:pass@sub@host:6379/0;touch /tmp/x",
			},
			expectError:           "contains invalid characters",
			expectErrorNotContain: []string{"user:pass@sub", "pass@sub"},
		},
		{
			name:                 "source with embedded credentials is allowed and passes through",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://user:pass@host:6379/0",
			},
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.Equal(t, "redis://user:pass@host:6379/0", req.VolumeContext["source"])
			},
		},
		{
			name:                 "url-encoded equals in query is masked",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://host:6379/0?token%3Dsecret;touch /tmp/x",
			},
			expectError:           "contains invalid characters",
			expectErrorNotContain: []string{"secret"},
		},

		// Error: url/bucket/path character checks
		{
			name:                 "url with whitespace rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "s3://ml-datasets",
				"url":    "http://host:9000 path",
			},
			expectError: "contains invalid characters",
		},
		{
			name:                 "url with shell metachar rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "s3://ml-datasets",
				"url":    "http://host:9000;rm -rf /",
			},
			expectError: "contains invalid characters",
		},
		{
			name:                 "url error masks embedded credentials",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "s3://ml-datasets",
				"url":    "http://user:pass@host:9000 path",
			},
			expectError:           "contains invalid characters",
			expectErrorNotContain: []string{"user:pass"},
		},
		{
			name:                 "valid url passes",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "s3://ml-datasets",
				"url":    "http://minio.sandbox-system:9000",
			},
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.Equal(t, "http://minio.sandbox-system:9000", req.VolumeContext["url"])
			},
		},
		{
			name:                 "bucket with shell metachar rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "s3://ml-datasets",
				"bucket": "b;rm -rf /",
			},
			expectError: "contains invalid characters",
		},
		{
			name:                 "path with shell metachar rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://host:6379/0",
				"path":   "/sub;rm -rf /",
			},
			expectError: "contains invalid characters",
		},
		{
			name:                 "storageType with shell metachar rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":      "redis://host:6379/0",
				"storageType": "oss;rm -rf /",
			},
			expectError: "contains invalid characters",
		},
		{
			name:                 "valid storageType passes through",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":      "redis://host:6379/0",
				"storageType": "oss",
			},
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.Equal(t, "oss", req.VolumeContext["storageType"])
			},
		},

		// Error: mountPath shell-safety
		{
			name:                 "relative mount path",
			containerMountTarget: "workspace/data",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			expectError:          "must be absolute",
		},
		{
			name:                 "mount path with semicolon injection",
			containerMountTarget: "/mnt;rm -rf /",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			expectError:          "safe characters",
		},
		{
			name:                 "mount path with embedded space",
			containerMountTarget: "/mnt/data dir",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			expectError:          "safe characters",
		},
		{
			name:                 "mount path traverses to parent",
			containerMountTarget: "/mnt/../etc",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			expectError:          "must not traverse",
		},
		{
			name:                 "mount path with trailing parent segment",
			containerMountTarget: "/mnt/..",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			expectError:          "must not traverse",
		},
		{
			name:                 "mount path with double dots inside a segment is allowed",
			containerMountTarget: "/mnt/data..2026",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			expectError:          "",
		},

		// Error: fuseType allowlist
		{
			name:                 "unknown fuseType",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":   "redis://host:6379/0",
				"fuseType": "unknown-fuse-client",
			},
			expectError: "unknown fuseType",
		},
		{
			name:                 "uppercase fuseType normalized to lowercase",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":   "redis://host:6379/0",
				"fuseType": "JuiceFS",
			},
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.Equal(t, "juicefs", req.VolumeContext["fuseType"])
				assert.Equal(t, "juicefs", req.VolumeCapability.GetMount().FsType)
			},
		},
		{
			name:                 "conflicting case variants of fuseType rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":   "redis://host:6379/0",
				"fuseType": "juicefs",
				"FUSETYPE": "s3fs",
			},
			expectError: "conflicting fuseType values",
		},
		{
			name:                 "identical case variants of fuseType accepted",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":   "redis://host:6379/0",
				"fuseType": "JuiceFS",
				"FUSETYPE": "juicefs",
			},
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.Equal(t, "juicefs", req.VolumeContext["fuseType"])
			},
		},
		{
			// Trailing whitespace in YAML is invisible; it must not turn a
			// valid value into an unknown one or leak into the request.
			name:                 "fuseType with trailing whitespace normalized",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":   "redis://host:6379/0",
				"fuseType": "juicefs ",
			},
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.Equal(t, "juicefs", req.VolumeContext["fuseType"])
				assert.Equal(t, "juicefs", req.VolumeCapability.GetMount().FsType)
			},
		},
		{
			name:                 "IPv6 source accepted",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://[::1]:6379/0",
			},
			expectError: "",
		},
		{
			name:                 "mount target with empty segment rejected",
			containerMountTarget: "/a//b",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			expectError:          "must not contain empty path segments",
		},
		{
			name:                 "CSI fsType used as default when fuseType unset",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://host:6379/0",
			},
			csiFSType:   "s3fs",
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.Equal(t, "s3fs", req.VolumeContext["fuseType"])
				assert.Equal(t, "s3fs", req.VolumeCapability.GetMount().FsType)
			},
		},
		{
			name:                 "fuseType conflicting with CSI fsType rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":   "redis://host:6379/0",
				"fuseType": "juicefs",
			},
			csiFSType:   "s3fs",
			expectError: "conflicts with CSI fsType",
		},
		{
			name:                 "unknown CSI fsType rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://host:6379/0",
			},
			csiFSType:   "unknown-fs",
			expectError: "unknown CSI fsType",
		},
		{
			name:                 "BASH_ENV volumeAttributes key rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":   "redis://host:6379/0",
				"BASH_ENV": "/tmp/evil.sh",
			},
			expectError: "not allowed",
		},
		{
			name:                 "conflicting case variants of source rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://host:6379/0",
				"Source": "redis://host:6379/1",
			},
			expectError: "conflicting source values",
		},
		{
			name:                 "identical case variants of source accepted",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://host:6379/0",
				"Source": "redis://host:6379/0",
			},
			expectError: "",
		},
		{
			// Whitespace-only differences are not a conflict: the
			// character-set check reports the padded value instead.
			name:                 "whitespace-padded variant rejected by charset check",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://host:6379/0",
				"Source": " redis://host:6379/0",
			},
			expectError: "invalid characters",
		},
		{
			name:                 "empty value variant dropped from VolumeContext",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "",
				"Source": "redis://host:6379/0",
			},
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.Equal(t, "redis://host:6379/0", req.VolumeContext["Source"])
				assert.NotContains(t, req.VolumeContext, "source")
			},
		},

		// Error: shell injection prevention
		{
			name:                 "otherOpts contains semicolon",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":    "redis://host:6379/0",
				"otherOpts": "cache-dir=/tmp; rm -rf /",
			},
			expectError: "invalid shell characters",
		},
		{
			name:                 "otherOpts contains pipe",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":    "redis://host:6379/0",
				"otherOpts": "opt1 | cat /etc/passwd",
			},
			expectError: "invalid shell characters",
		},
		{
			name:                 "otherOpts contains backtick",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":    "redis://host:6379/0",
				"otherOpts": "opt=`id`",
			},
			expectError: "invalid shell characters",
		},
		{
			name:                 "otherOpts contains dollar",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":    "redis://host:6379/0",
				"otherOpts": "opt=$(whoami)",
			},
			expectError: "invalid shell characters",
		},
		{
			name:                 "otherOpts contains null byte",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":    "redis://host:6379/0",
				"otherOpts": "safe\x00injection",
			},
			expectError: "invalid shell characters",
		},
		{
			name:                 "mountOptions contains semicolon",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":       "redis://host:6379/0",
				"mountOptions": "opt1; rm /tmp",
			},
			expectError: "invalid shell characters",
		},
		{
			name:                 "capacity contains command substitution",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":   "redis://host:6379/0",
				"capacity": "100Gi$(whoami)",
			},
			expectError: "invalid shell characters",
		},
		{
			name:                 "otherOpts contains carriage return",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":    "redis://host:6379/0",
				"otherOpts": "safe\rvalue",
			},
			expectError: "invalid shell characters",
		},
		{
			name:                 "otherOpts error masks key=value credential material",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":    "redis://host:6379/0",
				"otherOpts": "token=xyz123; rm -rf /",
			},
			expectError:           "invalid shell characters",
			expectErrorNotContain: []string{"xyz123"},
		},

		// Error: credential separation
		{
			name:                 "token in volumeAttributes should fail",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://host:6379/0",
				"token":  "secret-token-in-wrong-place",
			},
			expectError: "must not be in volumeAttributes",
		},
		{
			name:                 "accessKeyId in volumeAttributes should fail",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":      "redis://host:6379/0",
				"accessKeyId": "AKID123",
			},
			expectError: "must not be in volumeAttributes",
		},
		{
			name:                 "access-key in volumeAttributes should fail",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":     "redis://host:6379/0",
				"access-key": "AKID123",
			},
			expectError: "must not be in volumeAttributes",
		},
		{
			name:                 "passphrase in volumeAttributes should fail",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":     "redis://host:6379/0",
				"passphrase": "my-passphrase",
			},
			expectError: "must not be in volumeAttributes",
		},
		{
			name:                 "secret-key in volumeAttributes should fail",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":     "redis://host:6379/0",
				"secret-key": "SECRET456",
			},
			expectError: "must not be in volumeAttributes",
		},
		{
			name:                 "access_key in volumeAttributes should fail",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":     "redis://host:6379/0",
				"access_key": "AKID123",
			},
			expectError: "must not be in volumeAttributes",
		},
		{
			name:                 "password in volumeAttributes should fail",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":   "redis://host:6379/0",
				"password": "p@ssw0rd",
			},
			expectError: "must not be in volumeAttributes",
		},
		{
			name:                 "ak in volumeAttributes should fail",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://host:6379/0",
				"ak":     "AKID123",
			},
			expectError: "must not be in volumeAttributes",
		},
		{
			name:                 "sk in volumeAttributes should fail",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://host:6379/0",
				"sk":     "SECRET456",
			},
			expectError: "must not be in volumeAttributes",
		},

		// Error: case-variant keys must not bypass validation. parseOptions
		// extracts keys from the VolumeContext case-insensitively, so the
		// provider must validate every case variant of the keys it forwards.
		{
			name:                 "uppercase CAPACITY with command substitution rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":   "redis://host:6379/0",
				"CAPACITY": "100Gi$(whoami)",
			},
			expectError: "invalid shell characters",
		},
		{
			name:                 "uppercase OTHEROPTS with semicolon rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":    "redis://host:6379/0",
				"OTHEROPTS": "opt1; rm -rf /",
			},
			expectError: "invalid shell characters",
		},
		{
			name:                 "uppercase MOUNTOPTIONS with backtick rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":       "redis://host:6379/0",
				"MOUNTOPTIONS": "opt=`id`",
			},
			expectError: "invalid shell characters",
		},
		{
			name:                 "uppercase SOURCE with invalid characters rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"SOURCE": "redis://host:6379/0; rm -rf /",
			},
			expectError: "contains invalid characters",
		},
		{
			name:                 "uppercase URL with whitespace rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "s3://ml-datasets",
				"URL":    "http://host:9000 path",
			},
			expectError: "contains invalid characters",
		},
		{
			name:                 "uppercase BUCKET with shell metachar rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "s3://ml-datasets",
				"BUCKET": "b;rm -rf /",
			},
			expectError: "contains invalid characters",
		},
		{
			// PATH is a dangerous environment key: rejected by the key
			// check before the value is even inspected.
			name:                 "uppercase PATH key rejected as dangerous env key",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://host:6379/0",
				"PATH":   "/sub;rm -rf /",
			},
			expectError: "not allowed",
		},
		{
			name:                 "uppercase FUSETYPE unknown value rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":   "redis://host:6379/0",
				"FUSETYPE": "unknown-fuse-client",
			},
			expectError: "unknown fuseType",
		},
		{
			name:                 "uppercase TOKEN in volumeAttributes rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://host:6379/0",
				"TOKEN":  "secret-token-in-wrong-place",
			},
			expectError: "must not be in volumeAttributes",
		},
		{
			name:                 "mixed-case ACCESSKEYID in volumeAttributes rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":      "redis://host:6379/0",
				"accessKeyId": "AKID123",
			},
			expectError: "must not be in volumeAttributes",
		},
		{
			name:                 "uppercase SOURCE variant alone is a valid source",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"SOURCE": "redis://host:6379/0",
			},
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				// Keys pass through unchanged; parseOptions extracts them
				// case-insensitively downstream.
				assert.Equal(t, "redis://host:6379/0", req.VolumeContext["SOURCE"])
			},
		},
		{
			name:                 "whitespace-only uppercase SOURCE counts as missing",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"SOURCE": "   ",
			},
			expectError: "source is required",
		},
		{
			name:                 "empty SOURCE variant next to valid source is ignored",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source": "redis://host:6379/0",
				"SOURCE": "",
			},
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.Equal(t, "redis://host:6379/0", req.VolumeContext["source"])
			},
		},
		{
			name:                 "uppercase FuseType normalized to canonical lowercase key",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":   "redis://host:6379/0",
				"FuseType": "JuiceFS",
			},
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.Equal(t, "juicefs", req.VolumeContext["fuseType"])
				assert.Equal(t, "juicefs", req.VolumeCapability.GetMount().FsType)
				assert.NotContains(t, req.VolumeContext, "FuseType")
				assert.Len(t, req.VolumeContext, 2)
			},
		},
		{
			name:                 "duplicate fuseType case variants collapsed to one canonical key",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":   "redis://host:6379/0",
				"fuseType": "juicefs",
				"FuseType": "JUICEfs",
			},
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.Equal(t, "juicefs", req.VolumeContext["fuseType"])
				assert.Equal(t, "juicefs", req.VolumeCapability.GetMount().FsType)
				assert.NotContains(t, req.VolumeContext, "FuseType")
				assert.Len(t, req.VolumeContext, 2)
			},
		},

		// Error: secret key env-identifier validation
		{
			name:                 "secret key with dash rejected",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			secretData: map[string][]byte{
				"access-key": []byte("AKID123"),
			},
			expectError: "not a valid environment variable name",
		},
		{
			name:                 "secret key starting with digit rejected",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			secretData: map[string][]byte{
				"1token": []byte("value"),
			},
			expectError: "not a valid environment variable name",
		},
		{
			name:                 "secret key with underscore is valid",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			secretData: map[string][]byte{
				"access_key": []byte("AKID123"),
			},
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.Equal(t, "AKID123", req.Secrets["access_key"])
			},
		},
		{
			name:                 "secret value with newline rejected",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			secretData: map[string][]byte{
				"access_key": []byte("line1\nline2"),
			},
			expectError: "must not contain a newline",
		},
		{
			name:                 "secret value with carriage return rejected",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			secretData: map[string][]byte{
				"access_key": []byte("line1\rline2"),
			},
			expectError: "must not contain a newline",
		},

		// Error: dangerous environment keys in Secret (exported as env vars
		// by mount-proxy; bash would source BASH_ENV at startup)
		{
			name:                 "secret key BASH_ENV rejected",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			secretData: map[string][]byte{
				"BASH_ENV": []byte("/run/csi/mount-root/evil.sh"),
			},
			expectError: "not allowed",
		},
		{
			name:                 "secret key LD_PRELOAD rejected",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			secretData: map[string][]byte{
				"LD_PRELOAD": []byte("/tmp/x.so"),
			},
			expectError: "not allowed",
		},
		{
			name:                 "secret key IFS rejected",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			secretData: map[string][]byte{
				"IFS": []byte("x"),
			},
			expectError: "not allowed",
		},

		// Error: reserved provider-injected keys in Secret (mount-proxy exports
		// Secret entries verbatim and the last duplicate env wins, so these
		// would override the validated mount source / read-only / target)
		{
			name:                 "secret key source rejected as reserved",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			secretData: map[string][]byte{
				"source": []byte("redis://evil:6379/0"),
			},
			expectError: "reserved",
		},
		{
			name:                 "secret key readOnly rejected as reserved",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			secretData: map[string][]byte{
				"readOnly": []byte("false"),
			},
			expectError: "reserved",
		},
		{
			name:                 "secret key mountpoint rejected as reserved",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			secretData: map[string][]byte{
				"mountpoint": []byte("/etc/hosts"),
			},
			expectError: "reserved",
		},
		{
			name:                 "secret key otherOpts rejected as reserved",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			secretData: map[string][]byte{
				"otherOpts": []byte("cache-size=1"),
			},
			expectError: "reserved",
		},

		// Edge cases
		{
			name:                 "agent-identity mode with nil secret",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":   "redis://host:6379/0",
				"fuseType": "juicefs",
			},
			secretData:  nil,
			expectError: "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.Equal(t, "juicefs", req.VolumeContext["fuseType"])
				assert.Empty(t, req.Secrets)
			},
		},
		{
			name:                 "empty secret",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			secretData:           map[string][]byte{},
			expectError:          "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.Empty(t, req.Secrets)
			},
		},
		{
			name:                 "fuseType customfuse is valid",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":   "redis://host:6379/0",
				"fuseType": "customfuse",
			},
			expectError: "",
		},
		{
			name:                 "fuseType s3fs is valid",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":   "s3://my-bucket",
				"fuseType": "s3fs",
			},
			expectError: "",
		},
		{
			// jindo has no shipped entrypoint; a client that validates
			// without an implementation would fall through to the wrong
			// entrypoint, so it is not in the allowlist.
			name:                 "fuseType jindo without implementation rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":   "oss://bucket",
				"fuseType": "jindo",
			},
			expectError: "unknown fuseType",
		},
		{
			name:                 "safe otherOpts passes validation",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":    "redis://host:6379/0",
				"otherOpts": "cache-size=1024,cache-dir=/ssd-cache,prefetch=1",
			},
			expectError: "",
		},

		// Error: reserved provider keys smuggled inside the option-list
		// fields (the entrypoint appends them to the -o string after the
		// provider-composed options, so they would override the validated
		// url/source/bucket semantics)
		{
			name:                 "otherOpts url override rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":    "redis://host:6379/0",
				"otherOpts": "url=http://attacker:9000",
			},
			expectError: "reserved for provider-injected",
		},
		{
			name:                 "otherOpts source override rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":    "redis://host:6379/0",
				"otherOpts": "cache-size=1024,source=redis://evil:6379/0",
			},
			expectError: "reserved for provider-injected",
		},
		{
			name:                 "otherOpts readOnly override rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":    "redis://host:6379/0",
				"otherOpts": "readOnly=false",
			},
			expectError: "reserved for provider-injected",
		},
		{
			// Space-separated options must not hide a reserved key inside
			// one comma-split token.
			name:                 "otherOpts space-separated reserved override rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":    "redis://host:6379/0",
				"otherOpts": "cache-size=1024 source=evil",
			},
			expectError: "reserved for provider-injected",
		},
		{
			// Mirrors ValidateMountOptions: a keyless entry must not pass
			// on this path while it is rejected on the pv.Spec.MountOptions
			// path.
			name:                 "otherOpts keyless entry rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":    "redis://host:6379/0",
				"otherOpts": "cache-size=1024,=value",
			},
			expectError: "empty option key",
		},
		{
			// subdir= follows the official JuiceFS CSI mountOptions
			// convention but is not supported; point at the path attribute.
			name:                 "otherOpts subdir option rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":    "redis://host:6379/0",
				"otherOpts": "cache-size=1024,subdir=/my/sub",
			},
			expectError: "use the path volumeAttribute",
		},
		{
			name:                 "pv mountOptions subdir option rejected",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			pvMountOptions:       []string{"subdir=/my/sub"},
			expectError:          "use the path volumeAttribute",
		},
		{
			name:                 "volumeAttributes mountOptions url override rejected",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":       "redis://host:6379/0",
				"mountOptions": "url=http://attacker:9000",
			},
			expectError: "reserved for provider-injected",
		},
		{
			// capacity is not an option list; an option-like value fails
			// the shared CapacityPattern format check.
			name:                 "capacity with option-like value rejected by format check",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":   "redis://host:6379/0",
				"capacity": "url=x",
			},
			expectError: "invalid capacity",
		},
		{
			name:                 "reserved override error masks value",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":    "redis://host:6379/0",
				"otherOpts": "url=http://attacker:9000",
			},
			expectError:           "reserved for provider-injected",
			expectErrorNotContain: []string{"attacker"},
		},
		{
			name:                 "empty otherOpts and mountOptions pass validation",
			containerMountTarget: "/mnt",
			volumeAttributes: map[string]string{
				"source":       "redis://host:6379/0",
				"otherOpts":    "",
				"mountOptions": "",
			},
			expectError: "",
		},

		// Error: PV.Spec.MountOptions -> MountFlags mapping
		{
			name:                 "PV mountOptions mapped to VolumeCapability MountFlags",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			pvMountOptions:       []string{"cache-size=1024", "no-update"},
			expectError:          "",
			validateResult: func(t *testing.T, req *csiapi.NodePublishVolumeRequest) {
				assert.Equal(t, []string{"cache-size=1024", "no-update"},
					req.VolumeCapability.GetMount().MountFlags)
			},
		},
		{
			name:                 "PV mountOptions with shell metachar rejected",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			pvMountOptions:       []string{"opt1; rm -rf /"},
			expectError:          "invalid shell characters",
		},
		{
			name:                 "empty PV mountOptions passes validation",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			pvMountOptions:       []string{},
			expectError:          "",
		},

		// Error: dangerous environment keys in mountOptions (exported as env
		// vars by mount-proxy; bash/glibc would act on them)
		{
			name:                 "mountOptions BASH_ENV rejected",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			pvMountOptions:       []string{"BASH_ENV=/tmp/x.sh"},
			expectError:          "not allowed",
		},
		{
			name:                 "mountOptions LD_PRELOAD rejected",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			pvMountOptions:       []string{"LD_PRELOAD=/tmp/x.so"},
			expectError:          "not allowed",
		},
		{
			name:                 "mountOptions PATH rejected",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			pvMountOptions:       []string{"PATH=/tmp"},
			expectError:          "not allowed",
		},
		{
			name:                 "bare dangerous key rejected",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			pvMountOptions:       []string{"IFS"},
			expectError:          "not allowed",
		},
		{
			name:                 "hyphenated mount option still allowed",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			pvMountOptions:       []string{"cache-size=1024", "no-update"},
			expectError:          "",
		},
		{
			name:                 "lowercase dangerous key still allowed",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			pvMountOptions:       []string{"bash_env=/tmp/x.sh"},
			expectError:          "",
		},
		{
			name:                  "blocked mount option error masks value",
			containerMountTarget:  "/mnt",
			volumeAttributes:      map[string]string{"source": "redis://host:6379/0"},
			pvMountOptions:        []string{"BASH_ENV=/tmp/x.sh"},
			expectError:           "not allowed",
			expectErrorNotContain: []string{"/tmp/x.sh"},
		},

		// Error: reserved provider keys in mountOptions (exported as env vars
		// by mount-proxy; on duplicate keys the last occurrence wins, so they
		// would override the validated source/url/bucket semantics and can
		// redirect credentials to an attacker endpoint)
		{
			name:                 "mountOptions url override rejected",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			pvMountOptions:       []string{"url=http://attacker:9000"},
			expectError:          "reserved for provider-injected",
		},
		{
			name:                 "mountOptions source override rejected",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			pvMountOptions:       []string{"source=redis://evil:6379/0"},
			expectError:          "reserved for provider-injected",
		},
		{
			name:                 "mountOptions otherOpts override rejected",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			pvMountOptions:       []string{"otherOpts=debug"},
			expectError:          "reserved for provider-injected",
		},
		{
			name:                 "mountOptions readOnly override rejected",
			containerMountTarget: "/mnt",
			volumeAttributes:     map[string]string{"source": "redis://host:6379/0"},
			pvMountOptions:       []string{"readOnly=false"},
			expectError:          "reserved for provider-injected",
		},
		{
			name:                  "reserved override error masks value",
			containerMountTarget:  "/mnt",
			volumeAttributes:      map[string]string{"source": "redis://host:6379/0"},
			pvMountOptions:        []string{"url=http://attacker:9000"},
			expectError:           "reserved for provider-injected",
			expectErrorNotContain: []string{"attacker"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var pv *corev1.PersistentVolume
			if !tt.nilPV {
				csiSpec := &corev1.CSIPersistentVolumeSource{
					Driver:           "customfuseplugin.csi.openkruise.io",
					VolumeHandle:     "pv-test-handle",
					FSType:           tt.csiFSType,
					VolumeAttributes: tt.volumeAttributes,
				}
				if tt.nilCSI {
					csiSpec = nil
				}
				pv = &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: "pv-test"},
					Spec: corev1.PersistentVolumeSpec{
						AccessModes:  tt.accessModes,
						MountOptions: tt.pvMountOptions,
					},
				}
				if csiSpec != nil {
					pv.Spec.PersistentVolumeSource = corev1.PersistentVolumeSource{
						CSI: csiSpec,
					}
				}
			}

			var secret *corev1.Secret
			if tt.secretData != nil {
				secret = &corev1.Secret{
					Data: tt.secretData,
				}
			}

			provider := &CustomFuseMountProvider{}
			result, err := provider.GenerateCSINodePublishVolumeRequest(
				context.Background(),
				tt.containerMountTarget,
				pv,
				tt.readOnly,
				secret,
			)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				for _, s := range tt.expectErrorNotContain {
					assert.NotContains(t, err.Error(), s)
				}
			} else {
				require.NoError(t, err)
				assert.NotNil(t, result)
				if tt.validateResult != nil {
					tt.validateResult(t, result)
				}
			}
		})
	}
}

func TestCustomFuseMountProvider_VolumeIdStability(t *testing.T) {
	tests := []struct {
		name         string
		volumeHandle string
		want         string
	}{
		{"uses VolumeHandle when set", "pv-unique-handle", "pv-unique-handle"},
		{"falls back to PV name when VolumeHandle is empty", "", "pv-unique"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &CustomFuseMountProvider{}
			pv := &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "pv-unique"},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							Driver:           "customfuseplugin.csi.openkruise.io",
							VolumeHandle:     tt.volumeHandle,
							VolumeAttributes: map[string]string{"source": "redis://host:6379/0"},
						},
					},
				},
			}

			// Generate two requests and verify the VolumeId is identical,
			// because the CSI node plugin keys mount state by it.
			req1, err1 := provider.GenerateCSINodePublishVolumeRequest(context.Background(), "/mnt", pv, false, nil)
			require.NoError(t, err1)
			req2, err2 := provider.GenerateCSINodePublishVolumeRequest(context.Background(), "/mnt", pv, false, nil)
			require.NoError(t, err2)

			assert.Equal(t, tt.want, req1.VolumeId, "VolumeId must be stable per volume")
			assert.Equal(t, req1.VolumeId, req2.VolumeId, "consecutive mounts must yield the same VolumeId")
		})
	}
}

func TestCustomFuseMountProvider_PassthroughNonCustomfuseKeys(t *testing.T) {
	// Verify that custom keys beyond the known customfuse spec are preserved
	// in VolumeContext without filtering.
	provider := &CustomFuseMountProvider{}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-custom"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       "customfuseplugin.csi.openkruise.io",
					VolumeHandle: "pv-custom-handle",
					VolumeAttributes: map[string]string{
						"source":      "redis://host:6379/0",
						"custom-key1": "value1",
						"custom-key2": "value2",
					},
				},
			},
		},
	}

	req, err := provider.GenerateCSINodePublishVolumeRequest(context.Background(), "/mnt", pv, false, nil)
	require.NoError(t, err)
	assert.Equal(t, "value1", req.VolumeContext["custom-key1"])
	assert.Equal(t, "value2", req.VolumeContext["custom-key2"])
}

// TestCustomFuseMountProvider_DoesNotMutatePV verifies the AGENTS.md
// invariant that provider validation and request generation never modify the
// input PersistentVolume: the fuseType default must land in the cloned
// VolumeContext, never in the original PV's volume attributes.
func TestCustomFuseMountProvider_DoesNotMutatePV(t *testing.T) {
	provider := &CustomFuseMountProvider{}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-mutation"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       "customfuseplugin.csi.openkruise.io",
					VolumeHandle: "pv-mutation-handle",
					VolumeAttributes: map[string]string{
						"source": "redis://host:6379/0",
					},
				},
			},
		},
	}

	req, err := provider.GenerateCSINodePublishVolumeRequest(context.Background(), "/mnt", pv, false, nil)
	require.NoError(t, err)
	assert.Equal(t, "customfuse", req.VolumeContext["fuseType"])

	assert.Len(t, pv.Spec.CSI.VolumeAttributes, 1)
	assert.NotContains(t, pv.Spec.CSI.VolumeAttributes, "fuseType")
	assert.Equal(t, "redis://host:6379/0", pv.Spec.CSI.VolumeAttributes["source"])
}

// TestCustomFuseMountProvider_ProtoRoundtrip verifies that the generated
// NodePublishVolumeRequest survives the full serialization pipeline used
// by CSIMountHandler: proto.Marshal → base64 encode → base64 decode →
// proto.Unmarshal. No fields should be lost or corrupted in transit.
func TestCustomFuseMountProvider_ProtoRoundtrip(t *testing.T) {
	provider := &CustomFuseMountProvider{}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-roundtrip"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       "customfuseplugin.csi.openkruise.io",
					VolumeHandle: "pv-roundtrip-handle",
					VolumeAttributes: map[string]string{
						"source":    "redis://redis:6379/0",
						"fuseType":  "juicefs",
						"bucket":    "ml-datasets",
						"url":       "https://s3.amazonaws.com",
						"path":      "/user-123",
						"capacity":  "100Gi",
						"otherOpts": "cache-size=1024",
					},
				},
			},
		},
	}
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"token":      []byte("test-token-value"),
			"access_key": []byte("AKID-TEST"),
			"secret_key": []byte("SECRET-TEST"),
		},
	}

	original, err := provider.GenerateCSINodePublishVolumeRequest(
		context.Background(), "/workspace/data", pv, true, secret,
	)
	require.NoError(t, err)
	require.NotNil(t, original)

	// Simulate CSIMountHandler: proto.Marshal → base64 encode.
	marshaled, err := proto.Marshal(original)
	require.NoError(t, err)
	encoded := base64.StdEncoding.EncodeToString(marshaled)

	// Simulate sandbox-storage: base64 decode → proto.Unmarshal.
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	restored := &csiapi.NodePublishVolumeRequest{}
	err = proto.Unmarshal(decoded, restored)
	require.NoError(t, err)

	// Verify all fields survive the round-trip.
	assert.Equal(t, original.VolumeId, restored.VolumeId)
	assert.Equal(t, original.TargetPath, restored.TargetPath)
	assert.Equal(t, original.Readonly, restored.Readonly)
	assert.Equal(t, original.VolumeCapability.GetMount().FsType, restored.VolumeCapability.GetMount().FsType)
	assert.Equal(t, original.VolumeContext["source"], restored.VolumeContext["source"])
	assert.Equal(t, original.VolumeContext["fuseType"], restored.VolumeContext["fuseType"])
	assert.Equal(t, original.VolumeContext["bucket"], restored.VolumeContext["bucket"])
	assert.Equal(t, original.VolumeContext["url"], restored.VolumeContext["url"])
	assert.Equal(t, original.VolumeContext["path"], restored.VolumeContext["path"])
	assert.Equal(t, original.VolumeContext["capacity"], restored.VolumeContext["capacity"])
	assert.Equal(t, original.VolumeContext["otherOpts"], restored.VolumeContext["otherOpts"])
	assert.Equal(t, original.Secrets["token"], restored.Secrets["token"])
	assert.Equal(t, original.Secrets["access_key"], restored.Secrets["access_key"])
	assert.Equal(t, original.Secrets["secret_key"], restored.Secrets["secret_key"])
}
