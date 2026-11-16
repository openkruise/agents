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
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/openkruise/agents/pkg/resourceaccess"
	"github.com/openkruise/agents/pkg/utils/runtime/config"
)

const (
	isolatedOSSDriver     = "ossplugin.csi.alibabacloud.com"
	isolatedNASDriver     = "nasplugin.csi.alibabacloud.com"
	isolatedAgentIdentity = "agent-identity"
	isolatedProvider      = "sandboxCredProviderName"
	isolatedProviderAlias = "credentialProviderName"

	volumeAttributeKMSKeyID  = "kmsKeyId"
	volumeAttributeEncrypted = "encrypted"
	volumeAttributeKMSEnc    = "kms"
	volumeAttributeRegion    = "region"
)

// Require the complete AP, filesystem, region and NAS service hostname, not
// just an AP-looking first label on an arbitrary server.
var isolatedNASHost = regexp.MustCompile(`^(ap-[a-zA-Z0-9]+)\.(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?\.){2}nas\.aliyuncs\.com$`)

// BuildIsolatedStorageAuthAnnotation projects finalized mount contexts only.
// It performs no I/O, does not mutate mounts, and is independent of legacy hooks.
// It is NOT a customer grant check: the service and downstream IdP/storage MUST
// independently authorize the admitted delivery identity for the selected
// credential provider and backend target. Neither this projection nor token
// issuance proves local entitlement. Non-identity mounts produce no metadata,
// not permission to execute; empty results clear old metadata.
func BuildIsolatedStorageAuthAnnotation(ctx context.Context, mounts []config.MountConfig) (key, value string, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	// These JSON names are an external identity-provider contract. Keep this
	// neutral projection independent of pkg/identity and its lifecycle wiring.
	type storageAuthItem struct {
		CredentialProviderName string            `json:"credentialProviderName"`
		Attributes             map[string]string `json:"attributes,omitempty"`
	}
	var items []storageAuthItem
	for _, mount := range mounts {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		if mount.PublishRequest == nil {
			return "", "", resourceaccess.ErrAccessDenied
		}
		volumeContext := mount.PublishRequest.VolumeContext
		if volumeContext[volumeAttributeAuthType] != isolatedAgentIdentity {
			continue
		}
		provider := volumeContext[isolatedProvider]
		if !validIsolatedScopeValue(provider) {
			return "", "", resourceaccess.ErrAccessDenied
		}
		if alias, exists := volumeContext[isolatedProviderAlias]; exists && alias != provider {
			return "", "", resourceaccess.ErrAccessDenied
		}
		attributes := make(map[string]string)
		switch mount.Driver {
		case isolatedOSSDriver:
			bucket, space, prefix := volumeContext["bucket"], volumeContext["bucketSpace"], volumeContext["bucketSpacePrefix"]
			if (space != "" && prefix != "") || (bucket == "" && space == "" && prefix == "") {
				return "", "", resourceaccess.ErrAccessDenied
			}
			for _, scope := range []struct{ source, target string }{
				{"bucket", "bucket-name"}, {"bucketSpace", "bucket-space-name"}, {"bucketSpacePrefix", "bucket-space-prefix"},
				{volumeAttributeRegion, "region"}, {volumeAttributeKMSKeyID, "kms-key-id"},
			} {
				if value := volumeContext[scope.source]; value != "" {
					if !validIsolatedScopeValue(value) {
						return "", "", resourceaccess.ErrAccessDenied
					}
					attributes[scope.target] = value
				}
			}
			if volumeContext[volumeAttributeEncrypted] == volumeAttributeKMSEnc && volumeContext[volumeAttributeKMSKeyID] == "" ||
				volumeContext[volumeAttributeKMSKeyID] != "" && volumeContext[volumeAttributeEncrypted] != volumeAttributeKMSEnc {
				return "", "", resourceaccess.ErrAccessDenied
			}
			if finalPath := volumeContext[volumeAttributePath]; finalPath != "" {
				if !validIsolatedScopePath(finalPath) {
					return "", "", resourceaccess.ErrAccessDenied
				}
				// Remove exactly the mount-root slash. Do not clean OSS object
				// prefixes or trim a trailing slash into a broader policy scope.
				if subPath := strings.TrimPrefix(finalPath, "/"); subPath != "" {
					attributes["sub-path"] = subPath
				}
			}
		case isolatedNASDriver:
			match := isolatedNASHost.FindStringSubmatch(volumeContext["server"])
			if match == nil {
				return "", "", resourceaccess.ErrAccessDenied
			}
			attributes["nas-ap-id"] = match[1]
		default:
			return "", "", resourceaccess.ErrAccessDenied
		}
		items = append(items, storageAuthItem{provider, attributes})
	}
	if len(items) == 0 {
		return "", "", nil
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return "", "", fmt.Errorf("encode isolated storage metadata: %w", err)
	}
	return resourceaccess.StorageAuthAnnotation, string(raw), nil
}

func validIsolatedScopeValue(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "*?\x00/\\\r\n\t") {
		return false
	}
	return value != "." && value != ".."
}

func validIsolatedScopePath(value string) bool {
	if !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "*?\x00") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "." || component == ".." {
			return false
		}
	}
	return true
}
