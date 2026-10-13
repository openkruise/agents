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

package opensandbox

import (
	"fmt"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/openkruise/agents/pkg/servers/web"
)

type createSource string

const (
	createSourceImage    createSource = "image"
	createSourceSnapshot createSource = "snapshotId"
	createSourcePool     createSource = "poolRef"
	createSourceTemplate createSource = "templateId"
)

// resolveCreateSource selects an operation, without reproducing Lifecycle Server
// field validation. Only missing or competing sources require adapter errors.
// Template mode preserves an optional capacity pool; ordinary Pool mode selects
// inventory and treats a supplied image as a per-allocation update.
func resolveCreateSource(request *CreateSandboxRequest) error {
	hasImage := request.Image != nil && strings.TrimSpace(request.Image.URI) != ""
	hasSnapshot := strings.TrimSpace(request.SnapshotID) != ""
	hasTemplate := request.TemplateID != nil && strings.TrimSpace(*request.TemplateID) != ""
	hasPool := strings.TrimSpace(request.Extensions["poolRef"]) != ""

	if (hasImage && hasSnapshot) || (hasTemplate && (hasImage || hasSnapshot)) {
		return fmt.Errorf("image.uri, snapshotId and templateId select competing workload sources")
	}
	if hasSnapshot && hasPool {
		return fmt.Errorf("snapshotId cannot be combined with ordinary poolRef: restore and pool allocation are different operations")
	}
	switch {
	case hasTemplate:
		request.source = createSourceTemplate
	case hasPool:
		request.source = createSourcePool
	case hasSnapshot:
		request.source = createSourceSnapshot
	case hasImage:
		request.source = createSourceImage
	default:
		return fmt.Errorf("one effective image.uri, snapshotId, templateId or poolRef is required to select a creation source")
	}
	return nil
}

// validateCreateExecution keeps unimplemented execution separate from source
// validity. These temporary errors are implementation gaps, not contract
// restrictions or evidence of compatibility. In particular, newly recognized
// inputs must never silently fall through to the legacy image claim path.
func validateCreateExecution(request CreateSandboxRequest) *web.ApiError {
	var message string
	switch {
	case request.source == createSourceTemplate && strings.TrimSpace(request.Extensions["poolRef"]) != "":
		message = "templateId with poolRef is deferred"
	case request.Image != nil && request.Image.Auth != nil:
		message = "image.auth execution is not implemented"
	case request.CredentialProxy != nil && request.CredentialProxy.Enabled:
		message = "credentialProxy.enabled=true execution is not implemented"
	case request.Lifecycle != nil && (request.Lifecycle.PreStart != nil || len(request.Lifecycle.Periodic) != 0):
		message = "lifecycle execution is not implemented"
	case request.SecureAccess != nil && *request.SecureAccess:
		message = "secureAccess execution is not implemented"
	}
	if message == "" {
		for name, value := range request.Extensions {
			// An ineffective pool selector does not request an operation.
			if name == "poolRef" && (request.source == createSourcePool || strings.TrimSpace(value) == "") {
				continue
			}
			if strings.HasPrefix(name, storedExtensionPrefix) {
				key := extensionAnnotationPrefix + strings.TrimPrefix(name, storedExtensionPrefix)
				if len(validation.IsQualifiedName(key)) != 0 {
					message = "invalid stored extension annotation key"
					break
				}
				continue
			}
			message = "non-empty extensions execution is not implemented"
			break
		}
	}
	if message == "" {
		for _, field := range []struct {
			name   string
			values *ResourceLimits
		}{{"resourceLimits", request.ResourceLimits}, {"resourceRequests", request.ResourceRequests}} {
			if field.values == nil {
				continue
			}
			for name := range *field.values {
				if name != "cpu" && name != "memory" {
					message = "extended " + field.name + " execution is not implemented"
					break
				}
			}
			if message != "" {
				break
			}
		}
	}
	if message == "" {
		message = unsupportedSourceStartup(request)
	}
	if message == "" {
		return nil
	}
	return &web.ApiError{Code: http.StatusBadRequest, Message: message}
}

func unsupportedSourceStartup(request CreateSandboxRequest) string {
	if request.source == createSourceTemplate {
		switch {
		case len(request.Entrypoint) != 0:
			return "templateId entrypoint overrides are not implemented"
		case request.Platform != nil && (request.Platform.OS != "" || request.Platform.Arch != ""):
			return "templateId platform overrides are not implemented"
		}
	}
	if request.source == createSourcePool {
		switch {
		case strings.TrimSpace(request.Extensions["poolRef"]) == "*":
			return "automatic pool selection is not implemented"
		case request.Platform != nil && (request.Platform.OS != "" || request.Platform.Arch != ""):
			return "pool platform overrides are not implemented"
		}
	}
	return ""
}
