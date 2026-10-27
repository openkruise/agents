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

package opensandbox

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/web"
)

// resolveExistingVolumes selects the existing-PVC path. Backend reads and
// mount construction remain in Infra. Provisioning is not implied by this path.
func resolveExistingVolumes(raw []json.RawMessage) ([]infra.ExistingVolumeMount, *web.ApiError) {
	var mounts []infra.ExistingVolumeMount
	for index, value := range raw {
		var volume struct {
			Name      string `json:"name"`
			MountPath string `json:"mountPath"`
			SubPath   string `json:"subPath"`
			ReadOnly  bool   `json:"readOnly"`
			PVC       *struct {
				ClaimName string `json:"claimName"`
			} `json:"pvc"`
			Host  *json.RawMessage `json:"host"`
			OSSFS *json.RawMessage `json:"ossfs"`
		}
		if err := json.Unmarshal(value, &volume); err != nil {
			return nil, &web.ApiError{Code: http.StatusBadRequest, Message: fmt.Sprintf("invalid volume %d: %v", index, err)}
		}
		if volume.PVC == nil || volume.Host != nil || volume.OSSFS != nil {
			return nil, &web.ApiError{Code: http.StatusBadRequest, Message: "volumes currently require an existing PVC source"}
		}
		mounts = append(mounts, infra.ExistingVolumeMount{Name: volume.Name, VolumeName: volume.PVC.ClaimName,
			MountPath: volume.MountPath, SubPath: volume.SubPath, ReadOnly: volume.ReadOnly})
	}
	return mounts, nil
}
