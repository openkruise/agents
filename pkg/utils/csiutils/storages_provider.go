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

package csiutils

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
	"k8s.io/klog/v2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/utils"
)

type CSIMountHandler struct {
	client          ctrlclient.Client
	apiReader       ctrlclient.Reader
	storageRegistry storages.VolumeMountProviderRegistry
	systemNamespace string
}

func NewCSIMountHandler(client ctrlclient.Client, apiReader ctrlclient.Reader, storageRegistry storages.VolumeMountProviderRegistry, systemNamespace string) *CSIMountHandler {
	return &CSIMountHandler{
		client:          client,
		apiReader:       apiReader,
		storageRegistry: storageRegistry,
		systemNamespace: systemNamespace,
	}
}

func (h *CSIMountHandler) GenerateNodePublishVolumeRequest(ctx context.Context, mountRequest v1alpha1.CSIMountConfig) (string, *csi.NodePublishVolumeRequest, error) {
	if mountRequest.PvName == "" {
		return "", nil, fmt.Errorf("no found persistent volume name")
	}
	// There are potential scenarios, such as incomplete cache synchronization,
	// where implementing a resilience or fault-tolerance mechanism can help mitigate spurious errors and improve system robustness.
	persistentVolumeObj := &corev1.PersistentVolume{}
	err := utils.GetFromInformerOrApiServer(ctx, persistentVolumeObj, ctrlclient.ObjectKey{Name: mountRequest.PvName}, h.client, h.apiReader)
	if err != nil {
		return "", nil, fmt.Errorf("failed to get persistent volume object by name: %s, err: %v", mountRequest.PvName, err)
	}
	if persistentVolumeObj.Spec.CSI == nil {
		return "", nil, fmt.Errorf("no found csi object in persistent volume by name: %s", mountRequest.PvName)
	}
	driverName := persistentVolumeObj.Spec.CSI.Driver
	if !h.storageRegistry.IsSupported(driverName) {
		return "", nil, fmt.Errorf("driver %s is not supported in current environment", driverName)
	}

	// to fetch storage provider
	storageProvider, exists := h.storageRegistry.GetProvider(driverName)
	if !exists {
		return "", nil, fmt.Errorf("no provider found for driver: %s", driverName)
	}

	// to fetch secret object
	var secretObj *corev1.Secret
	if persistentVolumeObj.Spec.CSI.NodePublishSecretRef != nil {
		nodePublishSecretRef := persistentVolumeObj.Spec.CSI.NodePublishSecretRef
		secretNamespace := nodePublishSecretRef.Namespace
		if secretNamespace == "" {
			secretNamespace = utils.DefaultSandboxDeployNamespace
		} else if secretNamespace != h.systemNamespace {
			return "", nil, fmt.Errorf("invalid node publish secret ref namespace: %s, expected: %s", secretNamespace, h.systemNamespace)
		}
		secretObj = &corev1.Secret{}
		err = utils.GetFromInformerOrApiServer(ctx, secretObj, ctrlclient.ObjectKey{Namespace: secretNamespace, Name: nodePublishSecretRef.Name}, h.client, h.apiReader)
		if err != nil {
			return "", nil, fmt.Errorf("failed to get secret object: %s/%s, err: %v", secretNamespace, nodePublishSecretRef.Name, err)
		}
	}

	// to add access point sub path
	if mountRequest.SubPath != "" {
		persistentVolumeObj = persistentVolumeObj.DeepCopy()
		if persistentVolumeObj.Spec.CSI.VolumeAttributes == nil {
			persistentVolumeObj.Spec.CSI.VolumeAttributes = make(map[string]string)
		}
		basePath, exist := persistentVolumeObj.Spec.CSI.VolumeAttributes["path"]
		if !exist {
			basePath = "/"
		}
		mergedPath, err := mergeAndValidatePaths(basePath, mountRequest.SubPath)
		if err != nil {
			return "", nil, fmt.Errorf("failed to merge and validate paths: base path=%s, sub path=%s, err: %v", basePath, mountRequest.SubPath, err)
		}
		// Use a copy to avoid modifying the original PV object
		persistentVolumeObj.Spec.CSI.VolumeAttributes["path"] = mergedPath
	}

	// to generate csi node publish volume request
	csiRequest, err := storageProvider.GenerateCSINodePublishVolumeRequest(ctx, mountRequest.MountPath, persistentVolumeObj, mountRequest.ReadOnly, secretObj)
	if err != nil {
		return "", csiRequest, err
	}
	return persistentVolumeObj.Spec.CSI.Driver, csiRequest, nil
}

func (h *CSIMountHandler) CSIMountOptionsConfig(ctx context.Context, mountRequest v1alpha1.CSIMountConfig) (string, string, error) {
	log := klog.FromContext(ctx)
	startTime := time.Now()
	driverName, csiRequest, err := h.GenerateNodePublishVolumeRequest(ctx, mountRequest)
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
