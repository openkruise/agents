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

package config

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewDefaultAccessToken(t *testing.T) {
	token := NewDefaultAccessToken()
	assert.NotEmpty(t, token, "token should not be empty")
	_, err := uuid.Parse(token)
	require.NoError(t, err, "token should be a valid UUID")

	// Consecutive calls should produce unique tokens
	token2 := NewDefaultAccessToken()
	assert.NotEqual(t, token, token2, "consecutive calls should produce unique tokens")
}

func TestDefaultCSIMountConcurrency(t *testing.T) {
	assert.Equal(t, 3, DefaultCSIMountConcurrency, "default CSI mount concurrency should be 3")
}

// testMountConfig builds a mount config whose publish request exercises the
// shapes the storage providers actually produce: the access-type oneof, an enum,
// the string maps and the credential-bearing Secrets.
func testMountConfig() MountConfig {
	return MountConfig{
		Driver: "ossplugin.csi.alibabacloud.com",
		PublishRequest: &csi.NodePublishVolumeRequest{
			VolumeId:   "pv-test-abc123",
			TargetPath: "/data/workspace",
			VolumeCapability: &csi.VolumeCapability{
				AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{
						FsType:     "ossfs",
						MountFlags: []string{"-o close_to_open=false"},
					},
				},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
			VolumeContext: map[string]string{"path": "/share/sub"},
			Secrets:       map[string]string{"akId": "test-ak-id", "akSecret": testAccessKeySecret},
		},
	}
}

// testAccessKeySecret is the credential that must never reach a log line.
const testAccessKeySecret = "test-ak-secret-must-not-leak"

// TestMountConfigRedactsPublishRequest pins the log-safety contract of
// MountConfig: the publish request carries the volume Secrets and the generated
// CSI message renders them in full, so no rendering path may expose it, while the
// driver and target path stay visible for diagnostics.
//
// The table deliberately covers both sinks a log line can go through: fmt (which
// consults the nested Stringer) and encoding/json — the fallback klog uses for
// any value that is neither a Stringer nor a logr.Marshaler, which is exactly how
// sandbox-manager's option structs reach the log.
func TestMountConfigRedactsPublishRequest(t *testing.T) {
	cfg := testMountConfig()
	opts := CSIMountOptions{MountOptionList: []MountConfig{cfg}}

	marshal := func(v any) string {
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		return string(raw)
	}

	tests := []struct {
		name     string
		rendered string
	}{
		{name: "fmt value", rendered: fmt.Sprintf("%v", cfg)},
		{name: "fmt pointer", rendered: fmt.Sprintf("%v", &cfg)},
		{name: "fmt enclosing options", rendered: fmt.Sprintf("%+v", opts)},
		{name: "json value", rendered: marshal(cfg)},
		{name: "json enclosing options", rendered: marshal(opts)},
		// The shape klog produces for a struct that merely contains a
		// MountConfig: the nested Stringer is not consulted there, so only a
		// redacting MarshalJSON keeps the secrets out.
		{name: "json enclosing options pointer", rendered: marshal(struct {
			CSIMount *CSIMountOptions `json:"CSIMount"`
		}{CSIMount: &opts})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NotContains(t, tt.rendered, testAccessKeySecret, "publish request secrets must never be rendered")
			assert.NotContains(t, tt.rendered, "akSecret", "not even the secret keys may be rendered")
			assert.Contains(t, tt.rendered, "ossplugin.csi.alibabacloud.com", "driver should stay visible")
			assert.Contains(t, tt.rendered, "/data/workspace", "target path should stay visible")
			assert.Contains(t, tt.rendered, redactedPublishRequest)
		})
	}
}

// TestMountConfigJSONShape pins the redacted JSON shape itself: the request is
// replaced by a fixed placeholder, and it is absent altogether when there is no
// request. The lossless wire encoding of the CSI message lives on
// runtime.CreateMountRequest and is covered by its own tests.
func TestMountConfigJSONShape(t *testing.T) {
	tests := []struct {
		name string
		cfg  MountConfig
		want mountConfigView
	}{
		{
			name: "full request is replaced by the placeholder",
			cfg:  testMountConfig(),
			want: mountConfigView{
				Driver:         "ossplugin.csi.alibabacloud.com",
				TargetPath:     "/data/workspace",
				PublishRequest: redactedPublishRequest,
			},
		},
		{
			name: "no publish request renders neither placeholder nor path",
			cfg:  MountConfig{Driver: "nasplugin.csi.alibabacloud.com"},
			want: mountConfigView{Driver: "nasplugin.csi.alibabacloud.com"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.cfg)
			require.NoError(t, err)

			var got mountConfigView
			require.NoError(t, json.Unmarshal(data, &got))
			assert.Equal(t, tt.want, got)

			// Nothing but the three view fields may appear: an accidentally
			// embedded request would show up as an extra key.
			var keys map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(data, &keys))
			for key := range keys {
				assert.Contains(t, []string{"driver", "targetPath", "publishRequest"}, key)
			}
		})
	}
}
