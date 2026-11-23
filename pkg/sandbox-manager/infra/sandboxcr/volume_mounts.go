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

package sandboxcr

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/csiutils"
)

func validateExistingVolumeMounts(inputs []infra.ExistingVolumeMount) error {
	names, paths := map[string]bool{}, map[string]bool{}
	for _, input := range inputs {
		if len(validation.IsDNS1123Label(input.Name)) != 0 || len(validation.IsDNS1123Subdomain(input.VolumeName)) != 0 {
			return managererrors.NewError(managererrors.ErrorBadRequest, "invalid volume or claim name")
		}
		if !path.IsAbs(input.MountPath) || path.IsAbs(input.SubPath) {
			return managererrors.NewError(managererrors.ErrorBadRequest, "volume mountPath must be absolute and subPath must be relative")
		}
		for _, part := range strings.Split(input.SubPath, "/") {
			if part == ".." {
				return managererrors.NewError(managererrors.ErrorBadRequest, "volume subPath must not contain parent traversal")
			}
		}
		if names[input.Name] || paths[input.MountPath] {
			return managererrors.NewError(managererrors.ErrorBadRequest, "duplicate volume name or mount path")
		}
		names[input.Name], paths[input.MountPath] = true, true
	}
	return nil
}

func readExistingPVC(ctx context.Context, namespace, name string, reader client.Reader) (*corev1.PersistentVolumeClaim, error) {
	var pvc corev1.PersistentVolumeClaim
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, managererrors.NewError(managererrors.ErrorBadRequest, "PVC %s/%s does not exist; volume provisioning is not implemented", namespace, name)
		}
		return nil, managererrors.WrapError(managererrors.ErrorInternal, err, "read existing PVC %s/%s", namespace, name)
	}
	if !pvc.DeletionTimestamp.IsZero() {
		return nil, managererrors.NewError(managererrors.ErrorBadRequest, "PVC %s/%s is terminating", namespace, name)
	}
	return &pvc, nil
}

// resolveExistingCSIMounts translates existing claims through the native CSI
// provider. It changes only the unpersisted delivery, never the PVC or PV.
func (s *Sandbox) resolveExistingCSIMounts(ctx context.Context, inputs []infra.ExistingVolumeMount) (*config.CSIMountOptions, error) {
	if len(inputs) > csiMountCountLimit {
		return nil, managererrors.NewError(managererrors.ErrorBadRequest, "CSI mount count exceeds limit %d", csiMountCountLimit)
	}
	if err := validateExistingVolumeMounts(inputs); err != nil {
		return nil, err
	}
	c := s.Cache.GetClient()
	rawMounts := make([]v1alpha1.CSIMountConfig, 0, len(inputs))
	for _, input := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pvc, err := readExistingPVC(ctx, s.Namespace, input.VolumeName, c)
		if err != nil {
			return nil, err
		}
		if pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "" {
			return nil, managererrors.NewError(managererrors.ErrorBadRequest, "PVC %s/%s must already be bound for CSI mounting", s.Namespace, input.VolumeName)
		}
		var pv corev1.PersistentVolume
		if err := c.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, &pv); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, managererrors.WrapError(managererrors.ErrorBadRequest, err, "bound PV %s does not exist", pvc.Spec.VolumeName)
			}
			return nil, fmt.Errorf("read bound PV %s: %w", pvc.Spec.VolumeName, err)
		}
		ref := pv.Spec.ClaimRef
		if !pv.DeletionTimestamp.IsZero() || ref == nil || ref.Namespace != pvc.Namespace || ref.Name != pvc.Name || ref.UID != pvc.UID {
			return nil, managererrors.NewError(managererrors.ErrorBadRequest, "PV %s is not bound to the requested PVC", pv.Name)
		}
		mount := v1alpha1.CSIMountConfig{PvName: pv.Name, MountPath: input.MountPath, SubPath: input.SubPath, ReadOnly: input.ReadOnly}
		rawMounts = append(rawMounts, mount)
	}
	return s.resolveCSIMountConfigs(ctx, rawMounts)
}

// resolveRawCSIMounts materializes request-supplied native CSI configuration at
// the backend boundary. Already resolved native callers keep their existing path.
func (s *Sandbox) resolveRawCSIMounts(ctx context.Context, raw string) (*config.CSIMountOptions, error) {
	var mounts []v1alpha1.CSIMountConfig
	if err := json.Unmarshal([]byte(raw), &mounts); err != nil {
		return nil, managererrors.WrapError(managererrors.ErrorBadRequest, err, "invalid CSI mount configuration: %v", err)
	}
	return s.resolveCSIMountConfigs(ctx, mounts)
}

func (s *Sandbox) resolveCSIMountConfigs(ctx context.Context, mounts []v1alpha1.CSIMountConfig) (*config.CSIMountOptions, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	if len(mounts) > csiMountCountLimit {
		return nil, managererrors.NewError(managererrors.ErrorBadRequest, "CSI mount count exceeds limit %d", csiMountCountLimit)
	}
	c := s.Cache.GetClient()
	// Use the informer-backed client for both ordinary reads and Secret fallback.
	handler := csiutils.NewCSIMountHandler(c, c, s.storageRegistry, utils.DefaultSandboxDeployNamespace)
	resolved := &config.CSIMountOptions{Concurrency: config.DefaultCSIMountConcurrency}
	for _, mount := range mounts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		driver, request, err := handler.GenerateNodePublishVolumeRequest(ctx, mount)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("resolve CSI volume %s: %w", mount.PvName, ctx.Err())
			}
			return nil, managererrors.WrapError(managererrors.ErrorBadRequest, err, "resolve CSI volume %s: %v", mount.PvName, err)
		}
		resolved.MountOptionList = append(resolved.MountOptionList, config.MountConfig{Driver: driver, PublishRequest: request})
	}
	raw, err := json.Marshal(mounts)
	if err != nil {
		return nil, fmt.Errorf("encode CSI mount configuration: %w", err)
	}
	resolved.MountOptionListRaw = string(raw)
	var authKey, authValue string
	if hook := csiutils.BuildStorageAuthAnnotation; hook != nil {
		authKey, authValue, err = hook(ctx, c, mounts)
		if err != nil {
			return nil, fmt.Errorf("resolve CSI storage authentication: %w", err)
		}
	}
	annotations := maps.Clone(s.GetAnnotations())
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[v1alpha1.AnnotationCSIVolumeConfig] = resolved.MountOptionListRaw
	if authKey != "" && authValue != "" {
		annotations[authKey] = authValue
	}
	s.SetAnnotations(annotations)
	return resolved, nil
}
