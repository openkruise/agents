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

	"github.com/container-storage-interface/spec/lib/go/csi"
	corev1 "k8s.io/api/core/v1"
)

type VolumeMountProvider interface {
	GenerateCSINodePublishVolumeRequest(ctx context.Context, containerMountTarget string, persistentVolumeObj *corev1.PersistentVolume, readOnly bool, secretObj *corev1.Secret) (*csi.NodePublishVolumeRequest, error)
}

type VolumeMountProviderRegistry interface {
	RegisterProvider(driverName string, provider VolumeMountProvider)
	GetProvider(driverName string) (VolumeMountProvider, bool)
	IsSupported(driverName string) bool
}
