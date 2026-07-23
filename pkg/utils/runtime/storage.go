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
	"fmt"
	"net/http"
	"time"

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
	// success it returns the runtime-resolved CreateMountResponse (mount path and
	// user-facing symlink path). It returns an error on transport failure, on a
	// non-2xx response (an *APIError), or when the runtime reports success=false.
	Mount(ctx context.Context, req CreateMountRequest) (CreateMountResponse, error)
}

// CreateMountRequest is the request body accepted by POST /v1/storage/mounts.
// It is the transport-neutral representation of a single CSI mount intent,
// identical to the arguments already carried by the sandbox-storage CLI:
//
//   - Driver: the CSI driver name (e.g. "ossplugin.csi.alibabacloud.com").
//   - Config: base64(proto.Marshal(csi.NodePublishVolumeRequest)), as produced by
//     csiutils.CSIMountHandler.CSIMountOptionsConfig.
type CreateMountRequest struct {
	Driver string `json:"driver"`
	Config string `json:"config"`
}

// CreateMountResponse mirrors the agent-runtime response body for
// POST /v1/storage/mounts. The runtime returns this shape on both success and
// failure; Success is authoritative, and MountPath/LinkPath are populated only
// on success.
type CreateMountResponse struct {
	// Success indicates whether the mount operation completed successfully.
	Success bool `json:"success"`
	// MountPath is the actual filesystem path where the volume was mounted.
	MountPath string `json:"mountPath,omitempty"`
	// LinkPath is the user-facing symlink path pointing to MountPath.
	LinkPath string `json:"linkPath,omitempty"`
	// Message provides additional context about the operation result.
	Message string `json:"message,omitempty"`
}

// storageAPI is the default StorageAPI implementation. It delegates transport to
// the owning runtimeClient and carries no domain logic of its own.
type storageAPI struct {
	r *runtimeClient
}

// Mount implements StorageAPI by posting the mount request to the runtime storage
// mount endpoint and decoding the structured response. A 2xx response whose
// Success flag is false is still treated as a failure.
//
// It records the total wall-clock cost of the operation (including any retries
// performed by the underlying transport) via the "cost" log field.
func (s *storageAPI) Mount(ctx context.Context, req CreateMountRequest) (CreateMountResponse, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.r.sbx), "driver", req.Driver)
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

	log.Info("csi mount completed", "mountPath", resp.MountPath, "linkPath", resp.LinkPath, "cost", time.Since(start))
	return resp, nil
}
