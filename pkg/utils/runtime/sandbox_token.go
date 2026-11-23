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

package runtime

import (
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/resourceaccess"
	"github.com/openkruise/agents/pkg/utils/csiutils"
	"github.com/openkruise/agents/pkg/utils/runtime/config"
)

// PrepareSandboxToken rebuilds requested isolated storage scope from validated mounts.
// A nil mounts slice revalidates persisted requests using only shared PV reads;
// a non-nil slice is the exact snapshot already validated by the caller.
// The owner comes from trusted Sandbox metadata, not PV ownership. This scope is
// a request, not a local grant: the external IdP/storage authorizes access.
// Trusted-domain deliveries, including unmarked legacy objects, are untouched.
func PrepareSandboxToken(ctx context.Context, c client.Client, sbx *agentsv1alpha1.Sandbox, mounts []config.MountConfig) (*agentsv1alpha1.Sandbox, error) {
	if sbx == nil {
		return nil, fmt.Errorf("sandbox is required")
	}
	mode, err := resourceaccess.FromAnnotations(sbx.Annotations)
	if err != nil {
		return nil, err
	}
	if mode == resourceaccess.TrustedDomain {
		return sbx, nil
	}
	owner := sbx.Annotations[agentsv1alpha1.AnnotationOwner]
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(sbx.Namespace) == "" {
		return nil, fmt.Errorf("%w: isolated sandbox requires owner and namespace", resourceaccess.ErrAccessDenied)
	}
	var scope string
	if mounts == nil {
		requests, err := GetCsiMountExtensionRequest(sbx)
		if err != nil {
			return nil, err
		}
		h := csiutils.NewCSIMountHandler(c, nil, nil, "")
		_, scope, err = h.GenerateIsolatedMountOptionsWithScope(ctx, sbx.Namespace, owner, requests, false)
		if err != nil {
			return nil, fmt.Errorf("revalidate isolated storage scope: %w", err)
		}
	} else {
		for _, mount := range mounts {
			if mount.PublishRequest == nil || mount.PublishRequest.VolumeContext["authType"] != "agent-identity" || len(mount.PublishRequest.Secrets) != 0 {
				return nil, fmt.Errorf("%w: isolated storage requires agent-identity without static credentials", resourceaccess.ErrAccessDenied)
			}
		}
		_, scope, err = csiutils.BuildIsolatedStorageAuthAnnotation(ctx, mounts)
		if err != nil {
			return nil, fmt.Errorf("build isolated storage scope: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("build isolated storage scope: %w", err)
	}
	prepared := sbx.DeepCopy()
	delete(prepared.Annotations, resourceaccess.StorageAuthAnnotation)
	if scope != "" {
		if prepared.Annotations == nil {
			prepared.Annotations = make(map[string]string)
		}
		prepared.Annotations[resourceaccess.StorageAuthAnnotation] = scope
	}
	return prepared, nil
}
