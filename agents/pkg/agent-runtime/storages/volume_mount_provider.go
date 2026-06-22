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
	"fmt"

	"github.com/container-storage-interface/spec/lib/go/csi"
	corev1 "k8s.io/api/core/v1"
)

type MountProvider struct{}

func (p *MountProvider) GenerateCSINodePublishVolumeRequest(
	ctx context.Context,
	containerMountTarget string,
	persistentVolumeObj *corev1.PersistentVolume, readOnly bool,
	secretObj *corev1.Secret,
) (*csi.NodePublishVolumeRequest, error) {
	if persistentVolumeObj == nil {
		return nil, fmt.Errorf("persistent volume object is nil")
	}
	if persistentVolumeObj.Spec.CSI == nil {
		return nil, fmt.Errorf("no found csi object in persistent volume")
	}
	volumeCapability := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{
				FsType:     persistentVolumeObj.Spec.CSI.FSType,   // nfs ossfs ...
				MountFlags: persistentVolumeObj.Spec.MountOptions, // oss mount options, e.g.,"-o close_to_open=false"
			},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
	// if the mode is read only, modify the access mode
	isReadOnly := IsPureReadOnly(persistentVolumeObj.Spec.AccessModes) || readOnly
	if isReadOnly {
		volumeCapability.AccessMode.Mode = csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY
	}
	csiReq := &csi.NodePublishVolumeRequest{
		VolumeId:         fmt.Sprintf("%v-%s", persistentVolumeObj.Name, generateRandomString(6)),
		TargetPath:       containerMountTarget, // mount target path in container
		VolumeCapability: volumeCapability,
		Readonly:         isReadOnly,
	}
	// extensions config is required
	if csiReq.PublishContext == nil {
		csiReq.PublishContext = make(map[string]string)
		csiReq.PublishContext = driversConfig
	}
	// volume context for csi volume attributes
	if csiReq.VolumeContext == nil {
		csiReq.VolumeContext = make(map[string]string)
	}
	csiReq.VolumeContext = persistentVolumeObj.Spec.CSI.VolumeAttributes
	// when the secret is not nil, add the data to csiReq config
	if secretObj != nil {
		// secret config is required
		if csiReq.Secrets == nil {
			csiReq.Secrets = make(map[string]string)
		}
		for key, valueRaw := range secretObj.Data {
			csiReq.Secrets[key] = string(valueRaw)
		}
	}
	return csiReq, nil
}
