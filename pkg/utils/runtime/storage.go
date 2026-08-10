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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/protoadapt"
	"k8s.io/klog/v2"
)

// storageMountsPath is the route of the agent-runtime storage mount endpoint,
// matching the server-side registration:
//
//	engine.Group("/v1").POST("/storage/mounts", ...)
const storageMountsPath = "/v1/storage/mounts"

// StorageAPI is the runtime storage capability group. It maps to the
// /v1/storage/* routes exposed by agent-runtime and will grow as more storage
// operations (e.g. unmount) are added, without disturbing existing callers.
type StorageAPI interface {
	// Mount performs a single CSI mount by calling POST /v1/storage/mounts. On
	// success it returns the runtime-resolved CreateMountResponse (mount path).
	// It returns an error when the request carries no CSI publish request, on
	// transport failure, on a non-2xx response (an *APIError), or when the
	// runtime reports success=false.
	Mount(ctx context.Context, req CreateMountRequest) (CreateMountResponse, error)
}

// CreateMountRequest is the request body accepted by POST /v1/storage/mounts.
// It is the transport-neutral representation of a single CSI mount intent:
//
//   - Driver: the CSI driver name (e.g. "ossplugin.csi.alibabacloud.com").
//   - PublishRequest: the CSI NodePublishVolume request describing the volume, as
//     produced by csiutils.CSIMountHandler.GenerateNodePublishVolumeRequest.
//
// Carrying the typed CSI message rather than an opaque base64 blob keeps the
// payload self-describing: callers no longer pre-encode the request and the
// runtime no longer has to agree on an out-of-band encoding. On the wire
// PublishRequest is the canonical protobuf JSON of the message (see
// MarshalJSON), so the runtime must be new enough to read the publishRequest
// field and must decode it with protojson: the VolumeCapability access-type
// oneof is a Go interface that encoding/json cannot unmarshal.
//
// PublishRequest carries the CSI Secrets and PublishContext of the volume in
// clear text, so the encoded payload must never be logged, and the runtime must
// not echo it back in an error response.
type CreateMountRequest struct {
	// Driver is the CSI driver name that must serve the mount.
	Driver string
	// PublishRequest is the CSI NodePublishVolume request to execute in the sandbox.
	PublishRequest *csi.NodePublishVolumeRequest
}

// createMountRequestWire is the JSON shape of CreateMountRequest. The CSI
// message is carried pre-encoded so protojson owns its encoding while the
// envelope stays plain JSON.
type createMountRequestWire struct {
	Driver         string          `json:"driver"`
	PublishRequest json.RawMessage `json:"publishRequest,omitempty"`
}

// The custom codec is the whole wire contract of the type: assert both halves
// stay wired up, since a value-receiver UnmarshalJSON would silently be ignored.
var (
	_ json.Marshaler   = CreateMountRequest{}
	_ json.Unmarshaler = (*CreateMountRequest)(nil)
)

// jsonNull is the encoding of an explicit JSON null, which protojson refuses as
// a top-level message and which therefore has to be short-circuited.
var jsonNull = []byte("null")

// MarshalJSON encodes the envelope with encoding/json and the embedded CSI
// message with protojson, the only encoder that honours the protobuf field
// names, enum names and oneof shape of NodePublishVolumeRequest.
func (r CreateMountRequest) MarshalJSON() ([]byte, error) {
	wire := createMountRequestWire{Driver: r.Driver}
	if r.PublishRequest != nil {
		raw, err := protojson.Marshal(protoadapt.MessageV2Of(r.PublishRequest))
		if err != nil {
			return nil, fmt.Errorf("failed to marshal CSI publish request for driver %q: %w", r.Driver, err)
		}
		wire.PublishRequest = raw
	}
	return json.Marshal(wire)
}

// UnmarshalJSON is the inverse of MarshalJSON. The client never decodes a mount
// request itself; the type stays symmetric so tests and the runtime side can
// round-trip the exact payload that is put on the wire. An absent or null
// publishRequest leaves PublishRequest nil rather than failing to decode.
//
// Only the CSI message is decoded strictly: protojson rejects an unknown field,
// so a misspelled attribute surfaces here instead of as a mount that silently
// lost its secrets or sub-path. The envelope itself stays lenient, so an unknown
// sibling of "driver"/"publishRequest" is discarded.
func (r *CreateMountRequest) UnmarshalJSON(data []byte) error {
	var wire createMountRequestWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("failed to unmarshal mount request envelope: %w", err)
	}
	r.Driver = wire.Driver
	r.PublishRequest = nil
	if len(wire.PublishRequest) == 0 || bytes.Equal(bytes.TrimSpace(wire.PublishRequest), jsonNull) {
		return nil
	}
	publishRequest := &csi.NodePublishVolumeRequest{}
	if err := protojson.Unmarshal(wire.PublishRequest, protoadapt.MessageV2Of(publishRequest)); err != nil {
		return fmt.Errorf("failed to unmarshal CSI publish request for driver %q: %w", wire.Driver, err)
	}
	r.PublishRequest = publishRequest
	return nil
}

// CreateMountResponse mirrors the agent-runtime response body for
// POST /v1/storage/mounts. The runtime returns this shape on both success and
// failure; Success is authoritative, and MountPath is populated only on
// success.
type CreateMountResponse struct {
	// Success indicates whether the mount operation completed successfully.
	Success bool `json:"success"`
	// MountPath is the actual filesystem path where the volume was mounted.
	MountPath string `json:"mountPath,omitempty"`
	// Message provides additional context about the operation result.
	Message string `json:"message,omitempty"`
}

// storageAPI is the default StorageAPI implementation. It delegates transport to
// the owning runtimeClient and carries no domain logic of its own.
type storageAPI struct {
	r *runtimeClient
}

// Mount implements StorageAPI by posting the mount request to the runtime storage
// mount endpoint and decoding the structured response. A request without a CSI
// publish request is rejected locally, and a 2xx response whose Success flag is
// false is still treated as a failure.
//
// It records the total wall-clock cost of the operation (including any retries
// performed by the underlying transport) via the "cost" log field.
func (s *storageAPI) Mount(ctx context.Context, req CreateMountRequest) (CreateMountResponse, error) {
	// The publish request is the whole payload of the operation: reject a missing
	// one locally instead of sending a request the runtime can only refuse.
	if req.PublishRequest == nil {
		err := fmt.Errorf("csi publish request is required for driver %q", req.Driver)
		klog.FromContext(ctx).Error(err, "csi mount rejected before dispatch",
			"sandbox", klog.KObj(s.r.sbx), "driver", req.Driver)
		return CreateMountResponse{}, err
	}

	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.r.sbx), "driver", req.Driver,
		"targetPath", req.PublishRequest.GetTargetPath())
	start := time.Now()

	var resp CreateMountResponse
	if err := s.r.call(ctx, http.MethodPost, storageMountsPath, req, &resp); err != nil {
		log.Error(err, "csi mount failed", "cost", time.Since(start))
		return CreateMountResponse{}, err
	}
	if !resp.Success {
		err := fmt.Errorf("runtime reported mount failure for driver %q: %s", req.Driver, resp.Message)
		log.Error(err, "csi mount rejected by runtime", "cost", time.Since(start))
		return resp, err
	}

	log.Info("csi mount completed", "mountPath", resp.MountPath, "cost", time.Since(start))
	return resp, nil
}
