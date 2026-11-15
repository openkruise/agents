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

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/resourceaccess"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/csiutils"
	"github.com/openkruise/agents/pkg/utils/runtime"
)

// prepareIsolatedMounts resolves a single attempt's effective shared-PV intent
// after modifiers/restoration, before quota or persistence. It validates metadata,
// encoding and provider availability, not PVC ownership or a cloud grant. The
// downstream service authorizes the requested scope; issuance can use the
// persisted Sandbox UID to establish its trusted customer identity.
func prepareIsolatedMounts(ctx context.Context, sbx *Sandbox, owner string, input *config.CSIMountOptions, runtimeAvailable bool) (*config.CSIMountOptions, error) {
	if owner == "" || sbx.Namespace == "" {
		return nil, resourceaccess.ErrAccessDenied
	}
	opts := config.CSIMountOptions{}
	if input != nil {
		opts = *input
	}
	raw := opts.MountOptionListRaw
	if raw == "" {
		raw = sbx.Annotations[v1alpha1.AnnotationCSIVolumeConfig]
	}
	if raw == "" && len(opts.MountOptionList) != 0 {
		return nil, fmt.Errorf("%w: CSI mount intent is required", resourceaccess.ErrAccessDenied)
	}
	requests := []v1alpha1.CSIMountConfig{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &requests); err != nil {
			return nil, managererrors.WrapError(managererrors.ErrorBadRequest, err, "invalid CSI mount config")
		}
	}
	if err := enforceCSIMountCount(len(requests)); err != nil {
		return nil, err
	}
	if len(requests) > 0 && !runtimeAvailable {
		return nil, managererrors.NewError(managererrors.ErrorBadRequest, "init runtime is required when csi mount is specified")
	}
	// Pass the admitted delivery's namespace and owner, not constructor defaults.
	h := csiutils.NewCSIMountHandler(sbx.Cache.GetClient(), nil, sbx.storageRegistry, utils.DefaultSandboxDeployNamespace)
	mounts, scope, err := h.GenerateIsolatedMountOptionsWithScope(ctx, sbx.Namespace, owner, requests, true)
	if err != nil {
		return nil, err
	}
	// Validate the entire wire batch before any concurrent mount can start.
	for i, mount := range mounts {
		if _, err := runtime.EncodePublishRequest(mount.PublishRequest); err != nil {
			return nil, managererrors.NewError(managererrors.ErrorBadRequest, "invalid CSI mount request at index %d", i)
		}
	}
	if mounts == nil {
		mounts = []config.MountConfig{}
	}
	opts.MountOptionList = mounts
	opts.MountOptionListRaw = raw
	if err := enforceCSIMountLimit(&opts); err != nil {
		return nil, err
	}
	idRequested := identity.IsIDTokenRequested(sbx.Sandbox)
	if scope != "" && !idRequested {
		return nil, managererrors.NewError(managererrors.ErrorBadRequest, "isolated storage authentication requires an agent identity")
	}
	if idRequested && !identity.HasIdentityProvider() {
		return nil, managererrors.NewError(managererrors.ErrorInternal, "identity provider is not configured for isolated sandbox")
	}
	// Requested storage scopes come only from this exact resolved mount set.
	// Copy even when the caller supplied a freshly built object.
	sbx.Sandbox = sbx.Sandbox.DeepCopy()
	if sbx.Annotations == nil {
		sbx.Annotations = make(map[string]string)
	}
	delete(sbx.Annotations, resourceaccess.StorageAuthAnnotation)
	if scope != "" {
		sbx.Annotations[resourceaccess.StorageAuthAnnotation] = scope
	}
	if raw != "" {
		sbx.Annotations[v1alpha1.AnnotationCSIVolumeConfig] = raw
	}
	return &opts, nil
}
