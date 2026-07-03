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

import (
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Volume represents an E2B-compatible volume.
type Volume struct {
	Name     string `json:"name"`     // PVC Name (e.g., harness-data)
	VolumeID string `json:"volumeID"` // Underlying PV Name (e.g., d-bp1j7dd96qrivw02u7gy)
}

// NewVolumeRequest represents the request to create a volume.
type NewVolumeRequest struct {
	Name       string           `json:"name"`
	Extensions VolumeExtensions `json:"-"`
}

// VolumeExtensions holds parsed extension parameters from headers.
type VolumeExtensions struct {
	StorageSize      resource.Quantity
	StorageClass     string
	AccessMode       string
	WaitBoundSeconds time.Duration
}
