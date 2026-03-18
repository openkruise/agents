package e2b

import (
	"context"
	"encoding/base64"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/utils"
)

func (sc *Controller) generateNodePublishVolumeRequest(ctx context.Context, containerMountPoint, persistentVolumeName, accessPointSubPath string, readOnly bool) (string, *csi.NodePublishVolumeRequest, error) {
	log := klog.FromContext(ctx)
	if persistentVolumeName == "" {
		return "", nil, fmt.Errorf("no found persistent volume name")
	}
	// There are potential scenarios, such as incomplete cache synchronization,
	// where implementing a resilience or fault-tolerance mechanism can help mitigate spurious errors and improve system robustness.
	persistentVolumeObj, err := sc.cache.GetPersistentVolume(persistentVolumeName)
	if err != nil {
		log.Error(err, "failed to get persistent volume object by name using cache method", persistentVolumeName)
		persistentVolumeObj, err = sc.client.CoreV1().PersistentVolumes().Get(ctx, persistentVolumeName, metav1.GetOptions{})
		if err != nil {
			return "", nil, fmt.Errorf("failed to get persistent volume object by name: %s, err: %v", persistentVolumeName, err)
		}
	}
	if persistentVolumeObj == nil {
		return "", nil, fmt.Errorf("no found persistent volume object by name: %s", persistentVolumeName)
	}
	if persistentVolumeObj.Spec.CSI == nil {
		return "", nil, fmt.Errorf("no found csi object in persistent volume by name: %s", persistentVolumeName)
	}
	driverName := persistentVolumeObj.Spec.CSI.Driver
	if !sc.storageRegistry.IsSupported(driverName) {
		return "", nil, fmt.Errorf("driver %s is not supported in current environment", driverName)
	}

	// to fetch storage provider
	storageProvider, exists := sc.storageRegistry.GetProvider(driverName)
	if !exists {
		return "", nil, fmt.Errorf("no provider found for driver: %s", driverName)
	}

	// to fetch secret object
	var secretObj *corev1.Secret
	if persistentVolumeObj.Spec.CSI.NodePublishSecretRef != nil {
		nodePublishSecretRef := persistentVolumeObj.Spec.CSI.NodePublishSecretRef
		if nodePublishSecretRef.Namespace == "" {
			nodePublishSecretRef.Namespace = utils.DefaultSandboxDeployNamespace
		} else if nodePublishSecretRef.Namespace != sc.systemNamespace {
			return "", nil, fmt.Errorf("invalid node publish secret ref namespace: %s, expected: %s", nodePublishSecretRef.Namespace, utils.DefaultSandboxDeployNamespace)
		}
		secretObj, err = sc.cache.GetSecret(nodePublishSecretRef.Namespace, nodePublishSecretRef.Name)
		if err != nil {
			log.Error(err, "failed to get secret object by name using cache method", nodePublishSecretRef.Namespace, nodePublishSecretRef.Name)
			secretObj, err = sc.client.CoreV1().Secrets(nodePublishSecretRef.Namespace).Get(ctx, nodePublishSecretRef.Name, metav1.GetOptions{})
			if err != nil {
				return "", nil, fmt.Errorf("failed to get secret object by name:%s/%s, err: %v",
					nodePublishSecretRef.Namespace, nodePublishSecretRef.Name, err)
			}
		}
	}

	// to add access point sub path
	if accessPointSubPath != "" {
		persistentVolumeObj = persistentVolumeObj.DeepCopy()
		if persistentVolumeObj.Spec.CSI.VolumeAttributes == nil {
			persistentVolumeObj.Spec.CSI.VolumeAttributes = make(map[string]string)
		}
		basePath, exist := persistentVolumeObj.Spec.CSI.VolumeAttributes["path"]
		if !exist {
			basePath = "/"
		}
		mergedPath, err := mergeAndValidatePaths(basePath, accessPointSubPath)
		if err != nil {
			return "", nil, fmt.Errorf("failed to merge and validate paths: base path=%s, sub path=%s, err: %v", basePath, accessPointSubPath, err)
		}
		// Use a copy to avoid modifying the original PV object
		persistentVolumeObj.Spec.CSI.VolumeAttributes["path"] = mergedPath
	}

	// to generate csi node publish volume request
	csiRequest, err := storageProvider.GenerateCSINodePublishVolumeRequest(ctx, containerMountPoint, persistentVolumeObj, readOnly, secretObj)
	if err != nil {
		return "", csiRequest, err
	}
	return persistentVolumeObj.Spec.CSI.Driver, csiRequest, nil
}

func (sc *Controller) csiMountOptionsConfig(ctx context.Context, containerMountPoint, persistentVolumeName, accessPointSubPath string, readOnly bool) (string, string, error) {
	log := klog.FromContext(ctx)
	startTime := time.Now()
	driverName, csiRequest, err := sc.generateNodePublishVolumeRequest(ctx, containerMountPoint, persistentVolumeName, accessPointSubPath, readOnly)
	if err != nil {
		return "", "", fmt.Errorf("failed to convert to node publish volume request, err: %v", err)
	}
	jsonBytes, err := proto.Marshal(csiRequest)
	if err != nil {
		return "", "", fmt.Errorf("failed to protojson marshal, err: %v", err)
	}
	log.Info("generate csi mount options config for sandbox", "mountCost", time.Since(startTime))
	return driverName, base64.StdEncoding.EncodeToString(jsonBytes), nil
}

func mergeAndValidatePaths(basePath, subPath string) (string, error) {
	if basePath == "" {
		return "", fmt.Errorf("base path cannot be empty")
	}

	if !strings.HasPrefix(basePath, "/") {
		return "", fmt.Errorf("base path must be an absolute path starting with /, got: %s", basePath)
	}

	validatedSubPath, err := validateRelativePath(subPath)
	if err != nil {
		return "", fmt.Errorf("invalid sub path: %w", err)
	}

	mergedPath := path.Join(basePath, validatedSubPath)

	normalizedBasePath := strings.TrimRight(basePath, "/")
	if !strings.HasPrefix(mergedPath, normalizedBasePath+"/") && mergedPath != normalizedBasePath {
		return "", fmt.Errorf("merged path %s is not within base path %s", mergedPath, basePath)
	}

	return mergedPath, nil
}

func validateRelativePath(subPath string) (string, error) {
	if subPath == "" {
		return "", fmt.Errorf("sub path cannot be empty")
	}

	if strings.HasPrefix(subPath, "/") {
		subPath = strings.TrimLeft(subPath, "/")
	}

	if subPath == "" || subPath == "." || subPath == ".." {
		return "", fmt.Errorf("sub path cannot be . or ..")
	}

	cleanedPath := path.Clean(subPath)

	if strings.HasPrefix(cleanedPath, "..") {
		return "", fmt.Errorf("sub path must not traverse to parent directory, got: %s", subPath)
	}

	if strings.Contains(cleanedPath, "\x00") {
		return "", fmt.Errorf("sub path contains null byte")
	}

	return cleanedPath, nil
}
