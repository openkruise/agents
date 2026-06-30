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

package models

// RegisterVolumeRequest is the POST /volumes request body.
type RegisterVolumeRequest struct {
	Name   string `json:"name"`
	PvName string `json:"pvName"`
	SizeGB int    `json:"sizeGB"`
}

// VolumeResponse is returned by POST, GET, and as a list element.
type VolumeResponse struct {
	VolumeID  string `json:"volumeID"`
	Name      string `json:"name"`
	PvName    string `json:"pvName"`
	SizeGB    int    `json:"sizeGB"`
	CreatedAt string `json:"createdAt"`
}

// DeleteVolumeResponse is returned by DELETE /volumes/{volumeID}.
type DeleteVolumeResponse struct {
	Warning    string   `json:"warning,omitempty"`
	AffectedBy []string `json:"affectedSandboxIDs,omitempty"`
}

// VolumeMountRequest is one entry in the POST /sandboxes volume_mounts array.
type VolumeMountRequest struct {
	VolumeID  string `json:"volumeID"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}
