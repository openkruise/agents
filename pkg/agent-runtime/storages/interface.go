package storages

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	corev1 "k8s.io/api/core/v1"
)

type VolumeMountProvider interface {
	GenerateCSINodePublishVolumeRequest(ctx context.Context, containerMountTarget string, persistentVolumeObj *corev1.PersistentVolume, secretObj *corev1.Secret) (*csi.NodePublishVolumeRequest, error)
}

type VolumeMountProviderRegistry interface {
	RegisterProvider(driverName string, provider VolumeMountProvider)
	GetProvider(driverName string) (VolumeMountProvider, bool)
	IsSupported(driverName string) bool
}
