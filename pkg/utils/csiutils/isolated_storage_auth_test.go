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
	"maps"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/openkruise/agents/pkg/resourceaccess"
	"github.com/openkruise/agents/pkg/utils/runtime/config"
)

func TestBuildIsolatedStorageAuthAnnotation(t *testing.T) {
	for _, tt := range []struct {
		name       string
		driver     string
		attributes map[string]string
		want       string
		denied     bool
	}{
		{name: "static custom driver nil map", driver: "custom.csi.test"},
		{name: "static custom driver empty map", driver: "custom.csi.test", attributes: map[string]string{}},
		{name: "static ignores identity-like attributes", attributes: map[string]string{"credentialProviderName": "provider", "bucket": "*"}},
		{name: "OSS full finalized scope", driver: isolatedOSSDriver,
			attributes: map[string]string{"bucket": "bucket", "path": "/base/tenant/", "kmsKeyId": "cmk-123", "encrypted": "kms", "region": "cn-hangzhou"},
			want:       `[{"credentialProviderName":"provider","attributes":{"bucket-name":"bucket","sub-path":"base/tenant/","kms-key-id":"cmk-123","region":"cn-hangzhou"}}]`},
		{name: "OSS root", driver: isolatedOSSDriver, attributes: map[string]string{"bucket": "bucket", "path": "/"},
			want: `[{"credentialProviderName":"provider","attributes":{"bucket-name":"bucket"}}]`},
		{name: "OSS absent path is root", driver: isolatedOSSDriver, attributes: map[string]string{"bucket": "bucket"},
			want: `[{"credentialProviderName":"provider","attributes":{"bucket-name":"bucket"}}]`},
		{name: "OSS opaque double slash", driver: isolatedOSSDriver, attributes: map[string]string{"bucket": "bucket", "path": "//base//tenant/"},
			want: `[{"credentialProviderName":"provider","attributes":{"bucket-name":"bucket","sub-path":"/base//tenant/"}}]`},
		{name: "BucketSpace without bucket", driver: isolatedOSSDriver, attributes: map[string]string{"bucketSpace": "space", "region": "cn-hangzhou"},
			want: `[{"credentialProviderName":"provider","attributes":{"bucket-space-name":"space","region":"cn-hangzhou"}}]`},
		{name: "BucketSpace prefix with bucket", driver: isolatedOSSDriver, attributes: map[string]string{"bucket": "bucket", "bucketSpacePrefix": "tenant", "region": "cn-hangzhou", "path": "/data/"},
			want: `[{"credentialProviderName":"provider","attributes":{"bucket-name":"bucket","bucket-space-prefix":"tenant","region":"cn-hangzhou","sub-path":"data/"}}]`},
		{name: "NAS full hostname", driver: isolatedNASDriver, attributes: map[string]string{"server": "ap-abc.fs-123.cn-hangzhou.nas.aliyuncs.com", "bucket": "ignored", "path": "/ignored"},
			want: `[{"credentialProviderName":"provider","attributes":{"nas-ap-id":"ap-abc"}}]`},
		{name: "missing provider", driver: isolatedOSSDriver, attributes: map[string]string{isolatedProvider: "", "bucket": "bucket"}, denied: true},
		{name: "blank provider", driver: isolatedOSSDriver, attributes: map[string]string{isolatedProvider: " ", "bucket": "bucket"}, denied: true},
		{name: "wildcard provider", driver: isolatedOSSDriver, attributes: map[string]string{isolatedProvider: "*", "bucket": "bucket"}, denied: true},
		{name: "alias conflict", driver: isolatedOSSDriver, attributes: map[string]string{isolatedProviderAlias: "other", "bucket": "bucket"}, denied: true},
		{name: "empty alias conflict", driver: isolatedOSSDriver, attributes: map[string]string{isolatedProviderAlias: "", "bucket": "bucket"}, denied: true},
		{name: "matching alias", driver: isolatedOSSDriver, attributes: map[string]string{isolatedProviderAlias: "provider", "bucket": "bucket"},
			want: `[{"credentialProviderName":"provider","attributes":{"bucket-name":"bucket"}}]`},
		{name: "unknown agent identity driver", driver: "unknown", denied: true},
		{name: "missing target", driver: isolatedOSSDriver, denied: true},
		{name: "agentic bucket alone is not a scoped target", driver: isolatedOSSDriver, attributes: map[string]string{"agenticBucket": "agentic"}, denied: true},
		{name: "conflicting BucketSpaces", driver: isolatedOSSDriver, attributes: map[string]string{"bucketSpace": "space", "bucketSpacePrefix": "prefix"}, denied: true},
		{name: "bucket wildcard", driver: isolatedOSSDriver, attributes: map[string]string{"bucket": "bucket*"}, denied: true},
		{name: "BucketSpace wildcard", driver: isolatedOSSDriver, attributes: map[string]string{"bucketSpace": "space?"}, denied: true},
		{name: "prefix wildcard", driver: isolatedOSSDriver, attributes: map[string]string{"bucketSpacePrefix": "*"}, denied: true},
		{name: "bucket whitespace", driver: isolatedOSSDriver, attributes: map[string]string{"bucket": " bucket"}, denied: true},
		{name: "bucket dot component", driver: isolatedOSSDriver, attributes: map[string]string{"bucket": "a/../b"}, denied: true},
		{name: "relative final path", driver: isolatedOSSDriver, attributes: map[string]string{"bucket": "bucket", "path": "base/tenant"}, denied: true},
		{name: "dot final path", driver: isolatedOSSDriver, attributes: map[string]string{"bucket": "bucket", "path": "/base/./tenant"}, denied: true},
		{name: "parent final path", driver: isolatedOSSDriver, attributes: map[string]string{"bucket": "bucket", "path": "/base/../tenant"}, denied: true},
		{name: "wildcard path", driver: isolatedOSSDriver, attributes: map[string]string{"bucket": "bucket", "path": "/base/*"}, denied: true},
		{name: "null path", driver: isolatedOSSDriver, attributes: map[string]string{"bucket": "bucket", "path": "/base/\x00"}, denied: true},
		{name: "wildcard region", driver: isolatedOSSDriver, attributes: map[string]string{"bucket": "bucket", "region": "*"}, denied: true},
		{name: "wildcard KMS", driver: isolatedOSSDriver, attributes: map[string]string{"bucket": "bucket", "kmsKeyId": "*", "encrypted": "kms"}, denied: true},
		{name: "missing KMS key", driver: isolatedOSSDriver, attributes: map[string]string{"bucket": "bucket", "encrypted": "kms"}, denied: true},
		{name: "KMS mode mismatch", driver: isolatedOSSDriver, attributes: map[string]string{"bucket": "bucket", "kmsKeyId": "cmk", "encrypted": "aes"}, denied: true},
		{name: "projection does not normalize KMS", driver: isolatedOSSDriver, attributes: map[string]string{"bucket": "bucket", "kmsKeyId": "cmk"}, denied: true},
		{name: "NAS missing server", driver: isolatedNASDriver, denied: true},
		{name: "NAS AP prefix on foreign server", driver: isolatedNASDriver, attributes: map[string]string{"server": "ap-abc.example.com"}, denied: true},
		{name: "NAS truncated hostname", driver: isolatedNASDriver, attributes: map[string]string{"server": "ap-abc.cn-hangzhou.nas.aliyuncs.com"}, denied: true},
		{name: "NAS suffix injection", driver: isolatedNASDriver, attributes: map[string]string{"server": "ap-abc.fs.cn-hangzhou.nas.aliyuncs.com.evil.test"}, denied: true},
		{name: "NAS port", driver: isolatedNASDriver, attributes: map[string]string{"server": "ap-abc.fs.cn-hangzhou.nas.aliyuncs.com:2049"}, denied: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			attributes := maps.Clone(tt.attributes)
			if tt.want != "" || tt.denied {
				if attributes == nil {
					attributes = make(map[string]string)
				}
				attributes[volumeAttributeAuthType] = isolatedAgentIdentity
				if _, exists := attributes[isolatedProvider]; !exists {
					attributes[isolatedProvider] = "provider"
				}
			}
			publish := &csi.NodePublishVolumeRequest{VolumeContext: attributes, Secrets: map[string]string{"secret": "never-in-metadata"}}
			before := proto.Clone(publish)
			beforeContext := maps.Clone(publish.VolumeContext)
			key, value, err := BuildIsolatedStorageAuthAnnotation(context.Background(), []config.MountConfig{{Driver: tt.driver, PublishRequest: publish}})
			assert.True(t, proto.Equal(before, publish), "builder mutated the publish request")
			assert.Equal(t, beforeContext, publish.VolumeContext, "builder changed nil/empty map semantics")
			assert.NotContains(t, value, "never-in-metadata")
			if tt.denied {
				require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
				assert.Equal(t, resourceaccess.ErrAccessDenied.Error(), err.Error())
				assert.Empty(t, key)
				assert.Empty(t, value)
			} else {
				require.NoError(t, err)
				if tt.want == "" {
					assert.Empty(t, key)
					assert.Empty(t, value)
				} else {
					assert.Equal(t, resourceaccess.StorageAuthAnnotation, key)
					assert.JSONEq(t, tt.want, value)
				}
			}
		})
	}
}

func TestBuildIsolatedStorageAuthAnnotationBatch(t *testing.T) {
	valid := config.MountConfig{Driver: isolatedOSSDriver, PublishRequest: &csi.NodePublishVolumeRequest{VolumeContext: map[string]string{
		volumeAttributeAuthType: isolatedAgentIdentity, isolatedProvider: "provider", "bucket": "bucket",
	}}}
	other := config.MountConfig{Driver: isolatedNASDriver, PublishRequest: &csi.NodePublishVolumeRequest{VolumeContext: map[string]string{
		volumeAttributeAuthType: isolatedAgentIdentity, isolatedProvider: "other-provider", "server": "ap-other.fs.cn-hangzhou.nas.aliyuncs.com",
	}}}
	for _, tt := range []struct {
		name   string
		mounts []config.MountConfig
		want   string
		denied bool
	}{
		{name: "nil"},
		{name: "empty", mounts: []config.MountConfig{}},
		{name: "nil publish request", mounts: []config.MountConfig{{}}, denied: true},
		{name: "no partial metadata", mounts: []config.MountConfig{valid, {}}, denied: true},
		{name: "projection preserves scopes and order without granting access", mounts: []config.MountConfig{valid, other},
			want: `[{"credentialProviderName":"provider","attributes":{"bucket-name":"bucket"}},{"credentialProviderName":"other-provider","attributes":{"nas-ap-id":"ap-other"}}]`},
		{name: "repeated scopes remain distinct mount items", mounts: []config.MountConfig{valid, valid},
			want: `[{"credentialProviderName":"provider","attributes":{"bucket-name":"bucket"}},{"credentialProviderName":"provider","attributes":{"bucket-name":"bucket"}}]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			key, value, err := BuildIsolatedStorageAuthAnnotation(context.Background(), tt.mounts)
			if tt.denied {
				require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
			} else {
				require.NoError(t, err)
			}
			if tt.want == "" {
				assert.Empty(t, key)
				assert.Empty(t, value)
			} else {
				assert.Equal(t, resourceaccess.StorageAuthAnnotation, key)
				assert.JSONEq(t, tt.want, value)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	key, value, err := BuildIsolatedStorageAuthAnnotation(ctx, nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, key)
	assert.Empty(t, value)
}
