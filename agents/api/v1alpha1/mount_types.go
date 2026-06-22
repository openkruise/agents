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

package v1alpha1

type CSIMountConfig struct {
	MountID   string `json:"mountID,omitempty"`  // mount id
	PvName    string `json:"pvName"`             // persistent volume name for mounting
	MountPath string `json:"mountPath"`          // mount target in container to mount the persistent volume
	SubPath   string `json:"subPath,omitempty"`  // sub path address in persistent volume
	ReadOnly  bool   `json:"readOnly,omitempty"` // whether to mount the persistent volume as read-only
}
