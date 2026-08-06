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

package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"
)

// testMountRequest builds a mount request for driver whose CSI publish request
// exercises the non-trivial encoding paths: the access-type oneof, an enum and
// the string maps.
func testMountRequest(driver string) CreateMountRequest {
	return CreateMountRequest{
		Driver: driver,
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
			VolumeContext: map[string]string{"csi.storage.k8s.io/pod.uid": "test-pod-uid"},
			Secrets:       map[string]string{"akId": "id", "akSecret": "secret"},
		},
	}
}

// protoEqual compares two legacy CSI messages through the protobuf v2 runtime,
// so field presence and the access-type oneof are compared semantically rather
// than by Go struct identity.
func protoEqual(a, b *csi.NodePublishVolumeRequest) bool {
	return proto.Equal(protoadapt.MessageV2Of(a), protoadapt.MessageV2Of(b))
}

// TestCreateMountRequestJSON covers the wire contract of the mount request: the
// envelope is plain JSON while the CSI message is protobuf JSON, and decoding is
// lossless for the shapes the storage providers actually produce.
func TestCreateMountRequestJSON(t *testing.T) {
	tests := []struct {
		name string
		req  CreateMountRequest
		// wantFields are the protobuf JSON names that must key the encoded
		// publishRequest object.
		wantFields []string
		// wantMountCapability asserts the access-type oneof is flattened to
		// "mount" and the access mode enum is encoded by name.
		wantMountCapability bool
		// wantOmitted asserts the publishRequest key is absent entirely.
		wantOmitted bool
	}{
		{
			name:                "full request with oneof, enum and maps",
			req:                 testMountRequest("ossplugin.csi.alibabacloud.com"),
			wantFields:          []string{"volumeId", "targetPath", "volumeCapability", "volumeContext", "secrets"},
			wantMountCapability: true,
		},
		{
			name: "request as the providers build it, with publish context and readonly",
			req: CreateMountRequest{
				Driver: "nasplugin.csi.alibabacloud.com",
				PublishRequest: &csi.NodePublishVolumeRequest{
					VolumeId:          "pv-nas-abc123",
					TargetPath:        "/data/shared",
					StagingTargetPath: "/var/lib/kubelet/staging",
					Readonly:          true,
					PublishContext:    map[string]string{"driverType": "nas"},
					VolumeContext:     map[string]string{"path": "/share/sub"},
				},
			},
			wantFields: []string{"volumeId", "targetPath", "stagingTargetPath", "readonly", "publishContext", "volumeContext"},
		},
		{
			name: "minimal request",
			req: CreateMountRequest{
				Driver:         "nasplugin.csi.alibabacloud.com",
				PublishRequest: &csi.NodePublishVolumeRequest{VolumeId: "pv-nas", TargetPath: "/data"},
			},
			wantFields: []string{"volumeId", "targetPath"},
		},
		{
			name:        "absent publish request is omitted",
			req:         CreateMountRequest{Driver: "oss"},
			wantOmitted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.req)
			require.NoError(t, err)

			// The envelope must stay a plain JSON object carrying "driver" and
			// the protobuf-JSON encoded "publishRequest".
			var envelope map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(data, &envelope))
			var driver string
			require.NoError(t, json.Unmarshal(envelope["driver"], &driver))
			assert.Equal(t, tt.req.Driver, driver)

			publishRaw, ok := envelope["publishRequest"]
			if tt.wantOmitted {
				assert.False(t, ok, "publishRequest must be omitted when there is no CSI request")
			} else {
				require.True(t, ok, "publishRequest must be present")
				var publishFields map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(publishRaw, &publishFields))
				for _, field := range tt.wantFields {
					assert.Contains(t, publishFields, field,
						"protobuf JSON must key the field by its canonical name")
				}
				if tt.wantMountCapability {
					var capability map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(publishFields["volumeCapability"], &capability))
					assert.Contains(t, capability, "mount", "the access-type oneof must be flattened to its case name")
					// The access mode must travel as an enum name so a peer that
					// renumbers nothing still reads the same capability.
					assert.JSONEq(t, `{"mode":"SINGLE_NODE_WRITER"}`, string(capability["accessMode"]))
				}
			}

			// Decoding the payload must reproduce the original CSI message,
			// including the access-type oneof that encoding/json cannot handle.
			var decoded CreateMountRequest
			require.NoError(t, json.Unmarshal(data, &decoded))
			assert.Equal(t, tt.req.Driver, decoded.Driver)
			if tt.req.PublishRequest == nil {
				assert.Nil(t, decoded.PublishRequest)
				return
			}
			require.NotNil(t, decoded.PublishRequest)
			assert.True(t, protoEqual(tt.req.PublishRequest, decoded.PublishRequest),
				"round-tripped CSI request must equal the original")
		})
	}
}

// TestCreateMountRequestUnmarshalErrors covers the decoding failure modes a
// malformed or mismatched peer can produce.
func TestCreateMountRequestUnmarshalErrors(t *testing.T) {
	tests := []struct {
		name        string
		payload     string
		expectError string
		wantNil     bool
	}{
		{
			name:    "explicit null publish request decodes to nil",
			payload: `{"driver":"oss","publishRequest":null}`,
			wantNil: true,
		},
		{
			// A syntax error is rejected by encoding/json before the custom
			// decoder runs, so exercise the envelope with a well-formed document
			// whose driver has the wrong type.
			name:        "envelope with a mistyped driver",
			payload:     `{"driver":123}`,
			expectError: "failed to unmarshal mount request envelope",
		},
		{
			name:        "unknown field is rejected rather than discarded",
			payload:     `{"driver":"oss","publishRequest":{"bogusField":1}}`,
			expectError: "failed to unmarshal CSI publish request",
		},
		{
			name:        "a peer still sending the base64 config shape is rejected",
			payload:     `{"driver":"oss","publishRequest":"Cgdwdi1vc3M="}`,
			expectError: "failed to unmarshal CSI publish request",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req CreateMountRequest
			err := json.Unmarshal([]byte(tt.payload), &req)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, req.PublishRequest)
			}
		})
	}
}

// TestStorageMount_SendsProtoJSONBody verifies end to end that the body observed
// by the runtime is the protobuf JSON payload and can be decoded back into the
// exact CSI request the caller passed in.
func TestStorageMount_SendsProtoJSONBody(t *testing.T) {
	type observed struct {
		contentType string
		body        []byte
	}
	// The handler runs on the server goroutine: hand the observation over a
	// channel instead of sharing a variable with the test goroutine.
	seen := make(chan observed, 1)
	server, sbx := newMountTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		seen <- observed{contentType: r.Header.Get("Content-Type"), body: raw}
		writeMountResponse(t, w, http.StatusOK, CreateMountResponse{Success: true, MountPath: "/m"})
	})
	_ = server

	req := testMountRequest("ossplugin.csi.alibabacloud.com")
	rt := NewRuntime(sbx, WithRetry(fastBackoff))
	_, err := rt.Storage().Mount(context.Background(), req)
	require.NoError(t, err)

	got := <-seen
	assert.Equal(t, "application/json", got.contentType)
	var received CreateMountRequest
	require.NoError(t, json.Unmarshal(got.body, &received))
	assert.Equal(t, req.Driver, received.Driver)
	assert.True(t, protoEqual(req.PublishRequest, received.PublishRequest))
}
