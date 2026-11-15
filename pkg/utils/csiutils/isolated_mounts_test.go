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
	"errors"
	"fmt"
	"maps"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/resourceaccess"
	"github.com/openkruise/agents/pkg/utils"
)

func isolatedTestBinding(name string) (*corev1.PersistentVolume, *corev1.PersistentVolumeClaim, v1alpha1.CSIMountConfig) {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver: "custom.csi.test", VolumeAttributes: map[string]string{"path": "/base", "ordinary": "pv-value"},
			}},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
		},
	}
	// Retain the legacy test's fixture signature without creating a PVC.
	return pv, nil, v1alpha1.CSIMountConfig{PvName: name, MountPath: "/mnt/" + name}
}

func isolatedTestTemplate(name string) (*corev1.PersistentVolume, v1alpha1.CSIMountConfig) {
	pv, _, request := isolatedTestBinding(name)
	pv.Spec.CSI.Driver = isolatedOSSDriver
	pv.Spec.CSI.VolumeAttributes[volumeAttributeAuthType] = isolatedAgentIdentity
	pv.Spec.CSI.VolumeAttributes[isolatedProvider] = "provider"
	pv.Spec.CSI.VolumeAttributes["bucket"] = "bucket"
	return pv, request
}

// Every isolated test client permits PV reads only, including metadata refresh.
func isolatedTestClient(t *testing.T, pvs ...*corev1.PersistentVolume) client.WithWatch {
	t.Helper()
	objects := make([]client.Object, len(pvs))
	for i, pv := range pvs {
		objects[i] = pv
	}
	_, c, err := cachetest.NewTestCache(t, objects...)
	require.NoError(t, err)
	return interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		switch obj.(type) {
		case *corev1.PersistentVolumeClaim, *corev1.Secret:
			t.Fatalf("isolated path must never read %T", obj)
		}
		return c.Get(ctx, key, obj, opts...)
	}})
}

func TestGenerateIsolatedMountOptionsRejectsBatchBeforeProviders(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*corev1.PersistentVolume, *v1alpha1.CSIMountConfig)
	}{
		{"missing PV", func(pv *corev1.PersistentVolume, _ *v1alpha1.CSIMountConfig) {
			pv.Name = "other"
		}},
		{"empty PV request", func(_ *corev1.PersistentVolume, r *v1alpha1.CSIMountConfig) {
			r.PvName = ""
		}},
		{"non CSI PV", func(pv *corev1.PersistentVolume, _ *v1alpha1.CSIMountConfig) {
			pv.Spec.CSI = nil
		}},
		{"missing driver", func(pv *corev1.PersistentVolume, _ *v1alpha1.CSIMountConfig) {
			pv.Spec.CSI.Driver = ""
		}},
		{"nil PV attributes", func(pv *corev1.PersistentVolume, _ *v1alpha1.CSIMountConfig) {
			pv.Spec.CSI.VolumeAttributes = nil
		}},
		{"missing auth type", func(pv *corev1.PersistentVolume, _ *v1alpha1.CSIMountConfig) {
			delete(pv.Spec.CSI.VolumeAttributes, volumeAttributeAuthType)
		}},
		{"deleting PV", func(pv *corev1.PersistentVolume, _ *v1alpha1.CSIMountConfig) {
			now := metav1.Now()
			pv.DeletionTimestamp = &now
			pv.Finalizers = []string{"test"}
		}},
		{"reserved path even identical", func(_ *corev1.PersistentVolume, r *v1alpha1.CSIMountConfig) {
			r.Attributes = map[string]string{"path": "/base"}
		}},
		{"reserved authType even identical", func(_ *corev1.PersistentVolume, r *v1alpha1.CSIMountConfig) {
			r.Attributes = map[string]string{"authType": isolatedAgentIdentity}
		}},
		{"reserved CSI attribute", func(_ *corev1.PersistentVolume, r *v1alpha1.CSIMountConfig) {
			r.Attributes = map[string]string{"csi.storage.k8s.io/pod.uid": "spoofed"}
		}},
		{"changed CSI operational attribute", func(pv *corev1.PersistentVolume, r *v1alpha1.CSIMountConfig) {
			pv.Spec.CSI.VolumeAttributes["csi.custom"] = "value"
			r.Attributes = map[string]string{"csi.custom": "changed"}
		}},
		{"new CSI operational attribute", func(_ *corev1.PersistentVolume, r *v1alpha1.CSIMountConfig) {
			r.Attributes = map[string]string{"csi.custom": "value"}
		}},
		{"new raw attribute", func(_ *corev1.PersistentVolume, r *v1alpha1.CSIMountConfig) {
			r.Attributes = map[string]string{"new": "value"}
		}},
		{"new empty raw attribute", func(_ *corev1.PersistentVolume, r *v1alpha1.CSIMountConfig) {
			r.Attributes = map[string]string{"new": ""}
		}},
		{"changed raw attribute", func(_ *corev1.PersistentVolume, r *v1alpha1.CSIMountConfig) {
			r.Attributes = map[string]string{"ordinary": "changed"}
		}},
		{"path traversal", func(_ *corev1.PersistentVolume, r *v1alpha1.CSIMountConfig) {
			r.SubPath = "../other"
		}},
		{"identity scope missing provider", func(pv *corev1.PersistentVolume, _ *v1alpha1.CSIMountConfig) {
			delete(pv.Spec.CSI.VolumeAttributes, isolatedProvider)
		}},
		{"identity scope wildcard", func(pv *corev1.PersistentVolume, _ *v1alpha1.CSIMountConfig) {
			pv.Spec.CSI.VolumeAttributes["bucket"] = "*"
		}},
		{"identity default alias conflict", func(pv *corev1.PersistentVolume, _ *v1alpha1.CSIMountConfig) {
			pv.Spec.CSI.VolumeAttributes[isolatedProviderAlias] = "other"
		}},
		{"identity dot path before cleaning", func(pv *corev1.PersistentVolume, r *v1alpha1.CSIMountConfig) {
			pv.Spec.CSI.VolumeAttributes["path"] = "/base/../other"
			r.SubPath = "data"
		}},
		{"identity dot subpath before cleaning", func(_ *corev1.PersistentVolume, r *v1alpha1.CSIMountConfig) {
			r.SubPath = "data/../other"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			firstPV, first := isolatedTestTemplate("first")
			firstPV.Spec.CSI.NodePublishSecretRef = &corev1.SecretReference{Namespace: "system-ns", Name: "secret"}
			pv, request := isolatedTestTemplate("second")
			tt.change(pv, &request)
			reader := isolatedTestClient(t, firstPV, pv)
			registry := &mockStorageProviderRegistry{}
			registry.RegisterProvider(isolatedOSSDriver, isolatedTestProvider(func(context.Context, string, *corev1.PersistentVolume, bool, *corev1.Secret) (*csi.NodePublishVolumeRequest, error) {
				t.Fatal("invalid batch reached an execution provider")
				return nil, nil
			}))
			handler := NewCSIMountHandler(reader, nil, registry, "system-ns")
			for _, forExecution := range []bool{false, true} {
				mounts, err := handler.GenerateIsolatedMountOptions(context.Background(), "tenant-ns", "owner", []v1alpha1.CSIMountConfig{first, request}, forExecution)
				require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
				assert.Equal(t, resourceaccess.ErrAccessDenied.Error(), err.Error())
				assert.Nil(t, mounts)
				mounts, scope, err := handler.GenerateIsolatedMountOptionsWithScope(context.Background(), "tenant-ns", "owner", []v1alpha1.CSIMountConfig{first, request}, forExecution)
				require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
				assert.Nil(t, mounts)
				assert.Empty(t, scope)
			}
		})
	}
}

func TestGenerateIsolatedMountOptionsScopeAndReadOnly(t *testing.T) {
	for _, tt := range []struct {
		name     string
		change   func(*corev1.PersistentVolume, *v1alpha1.CSIMountConfig)
		readOnly bool
	}{
		{name: "read write"},
		{name: "request read only", change: func(_ *corev1.PersistentVolume, r *v1alpha1.CSIMountConfig) {
			r.ReadOnly = true
		}, readOnly: true},
		{name: "CSI read only", change: func(pv *corev1.PersistentVolume, _ *v1alpha1.CSIMountConfig) {
			pv.Spec.CSI.ReadOnly = true
		}, readOnly: true},
		{name: "PV read only modes", change: func(pv *corev1.PersistentVolume, _ *v1alpha1.CSIMountConfig) {
			pv.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany}
		}, readOnly: true},
		{name: "identical attributes", change: func(_ *corev1.PersistentVolume, r *v1alpha1.CSIMountConfig) {
			r.Attributes = map[string]string{"ordinary": "pv-value"}
		}},
		{name: "identical CSI operational attribute", change: func(pv *corev1.PersistentVolume, r *v1alpha1.CSIMountConfig) {
			pv.Spec.CSI.VolumeAttributes["csi.custom"] = "value"
			r.Attributes = map[string]string{"csi.custom": "value"}
		}},
		{name: "empty attributes", change: func(_ *corev1.PersistentVolume, r *v1alpha1.CSIMountConfig) {
			r.Attributes = map[string]string{}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pv, request := isolatedTestTemplate("volume")
			request.SubPath = "/tenant/data"
			if tt.change != nil {
				tt.change(pv, &request)
			}
			reader := isolatedTestClient(t, pv)
			registry := &mockStorageProviderRegistry{}
			registry.RegisterProvider(pv.Spec.CSI.Driver, &storages.MountProvider{})
			for _, forExecution := range []bool{false, true} {
				handler := NewCSIMountHandler(reader, nil, registry, "system-ns")
				mounts, err := handler.GenerateIsolatedMountOptions(context.Background(), "tenant-ns", "owner", []v1alpha1.CSIMountConfig{request}, forExecution)
				require.NoError(t, err)
				require.Len(t, mounts, 1)
				publish := mounts[0].PublishRequest
				assert.Equal(t, tt.readOnly, publish.Readonly)
				mode := csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
				if tt.readOnly {
					mode = csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY
				}
				assert.Equal(t, mode, publish.VolumeCapability.AccessMode.Mode)
				assert.Equal(t, "/base/tenant/data", publish.VolumeContext["path"])
				assert.Equal(t, "pv-value", publish.VolumeContext["ordinary"])
				for key, value := range request.Attributes {
					assert.Equal(t, value, publish.VolumeContext[key])
				}
				assert.Equal(t, request.MountPath, publish.TargetPath)
				publish.VolumeContext["ordinary"] = "mutated"
				cached := &corev1.PersistentVolume{}
				require.NoError(t, reader.Get(context.Background(), client.ObjectKey{Name: pv.Name}, cached))
				assert.Equal(t, pv.Spec.CSI.VolumeAttributes, cached.Spec.CSI.VolumeAttributes)
			}
		})
	}
}

func TestGenerateIsolatedMountOptionsRefreshWithoutExecutionDependencies(t *testing.T) {
	for _, namespace := range []string{"", "system-ns", "foreign"} {
		t.Run(namespace, func(t *testing.T) {
			pv, request := isolatedTestTemplate("volume")
			pv.Spec.CSI.NodePublishSecretRef = &corev1.SecretReference{Namespace: "unusable-for-refresh", Name: "nonexistent"}
			pv.Spec.CSI.VolumeAttributes = map[string]string{"authType": isolatedAgentIdentity, isolatedProviderAlias: "provider", "bucket": "bucket", "path": "/base", "kmsKeyId": "cmk"}
			request.SubPath = "tenant"
			// Neither registry nor APIReader exists. The nonexistent Secret and
			// invalid Secret namespace must not matter to credential refresh.
			handler := NewCSIMountHandler(isolatedTestClient(t, pv), nil, nil, namespace)
			saved := NodePublishVolumeEnricher
			NodePublishVolumeEnricher = func(context.Context, v1alpha1.CSIMountConfig, map[string]string) {
				t.Fatal("isolated path invoked legacy hook")
			}
			t.Cleanup(func() { NodePublishVolumeEnricher = saved })
			mounts, err := handler.GenerateIsolatedMountOptions(context.Background(), "tenant-ns", "owner", []v1alpha1.CSIMountConfig{request}, false)
			require.NoError(t, err)
			require.Len(t, mounts, 1)
			assert.Empty(t, mounts[0].PublishRequest.Secrets)
			key, value, err := BuildIsolatedStorageAuthAnnotation(context.Background(), mounts)
			require.NoError(t, err)
			assert.Equal(t, "provider", mounts[0].PublishRequest.VolumeContext[isolatedProvider])
			assert.Equal(t, "kms", mounts[0].PublishRequest.VolumeContext["encrypted"])
			assert.Equal(t, resourceaccess.StorageAuthAnnotation, key)
			assert.JSONEq(t, `[{"credentialProviderName":"provider","attributes":{"bucket-name":"bucket","sub-path":"base/tenant","kms-key-id":"cmk"}}]`, value)
			scopedMounts, scope, err := handler.GenerateIsolatedMountOptionsWithScope(context.Background(), "tenant-ns", "owner", []v1alpha1.CSIMountConfig{request}, false)
			require.NoError(t, err)
			require.Len(t, scopedMounts, 1)
			assert.Equal(t, mounts[0].PublishRequest.VolumeContext, scopedMounts[0].PublishRequest.VolumeContext)
			assert.Equal(t, value, scope)
		})
	}
}

func TestGenerateIsolatedMountOptionsSharedUnboundTemplate(t *testing.T) {
	pv, request := isolatedTestTemplate("shared")
	// A PV annotation is not customer ownership. Both admitted deliveries use
	// this same unbound template; downstream entitlement is checked separately.
	pv.Annotations = map[string]string{v1alpha1.AnnotationOwner: "platform"}
	pv.Spec.CSI.NodePublishSecretRef = &corev1.SecretReference{Namespace: "foreign", Name: "missing"}
	request.Attributes = map[string]string{"ordinary": "pv-value"}
	beforeRequest := request.DeepCopy()
	var fetched, before *corev1.PersistentVolume
	reader := interceptor.NewClient(isolatedTestClient(t, pv), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if err := c.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		fetched = obj.(*corev1.PersistentVolume)
		before = fetched.DeepCopy()
		return nil
	}})
	registry := &mockStorageProviderRegistry{}
	registry.RegisterProvider(pv.Spec.CSI.Driver, &storages.MountProvider{})
	handler := NewCSIMountHandler(reader, nil, registry, "")
	for _, caller := range []struct{ namespace, owner string }{{"tenant-a", "customer-a"}, {"tenant-b", "customer-b"}} {
		for _, forExecution := range []bool{false, true} {
			mounts, err := handler.GenerateIsolatedMountOptions(context.Background(), caller.namespace, caller.owner, []v1alpha1.CSIMountConfig{request}, forExecution)
			require.NoError(t, err)
			require.Len(t, mounts, 1)
			assert.Empty(t, mounts[0].PublishRequest.Secrets)
			assert.Equal(t, pv.Spec.CSI.VolumeAttributes, mounts[0].PublishRequest.VolumeContext)
			mounts[0].PublishRequest.VolumeContext["ordinary"] = "mutated"
			assert.Equal(t, before, fetched)
			assert.Equal(t, *beforeRequest, request)
		}
	}
}

func TestGenerateIsolatedMountOptionsDynamicSelectors(t *testing.T) {
	for _, tt := range []struct {
		name       string
		driver     string
		defaults   map[string]string
		attributes map[string]string
		want       map[string]string
		metadata   string
		denied     bool
	}{
		{name: "OSS template without provider or target", driver: isolatedOSSDriver,
			attributes: map[string]string{isolatedProviderAlias: "customer", "bucket": "customer-bucket", "region": "cn-shanghai", "kmsKeyId": "customer-key"},
			want:       map[string]string{isolatedProviderAlias: "customer", isolatedProvider: "customer", "bucket": "customer-bucket", "region": "cn-shanghai", "kmsKeyId": "customer-key", "encrypted": "kms"},
			metadata:   `[{"credentialProviderName":"customer","attributes":{"bucket-name":"customer-bucket","sub-path":"base/data","region":"cn-shanghai","kms-key-id":"customer-key"}}]`},
		{name: "OSS canonical provider without default", driver: isolatedOSSDriver,
			attributes: map[string]string{isolatedProvider: "customer", "bucket": "customer-bucket"},
			want:       map[string]string{isolatedProvider: "customer", "bucket": "customer-bucket"},
			metadata:   `[{"credentialProviderName":"customer","attributes":{"bucket-name":"customer-bucket","sub-path":"base/data"}}]`},
		{name: "explicit alias resolves conflicting defaults", driver: isolatedOSSDriver,
			defaults:   map[string]string{isolatedProvider: "old", isolatedProviderAlias: "different", "bucket": "old-bucket"},
			attributes: map[string]string{isolatedProviderAlias: "customer", "bucket": "new-bucket"},
			want:       map[string]string{isolatedProvider: "customer", isolatedProviderAlias: "customer", "bucket": "new-bucket"},
			metadata:   `[{"credentialProviderName":"customer","attributes":{"bucket-name":"new-bucket","sub-path":"base/data"}}]`},
		{name: "canonical selection updates existing alias", driver: isolatedOSSDriver,
			defaults:   map[string]string{isolatedProviderAlias: "old", "bucket": "bucket"},
			attributes: map[string]string{isolatedProvider: "customer"},
			want:       map[string]string{isolatedProvider: "customer", isolatedProviderAlias: "customer", "bucket": "bucket"},
			metadata:   `[{"credentialProviderName":"customer","attributes":{"bucket-name":"bucket","sub-path":"base/data"}}]`},
		{name: "matching request aliases", driver: isolatedOSSDriver,
			defaults:   map[string]string{isolatedProvider: "old", "bucket": "bucket"},
			attributes: map[string]string{isolatedProvider: "customer", isolatedProviderAlias: "customer"},
			want:       map[string]string{isolatedProvider: "customer", isolatedProviderAlias: "customer", "bucket": "bucket"},
			metadata:   `[{"credentialProviderName":"customer","attributes":{"bucket-name":"bucket","sub-path":"base/data"}}]`},
		{name: "default alias normalization", driver: isolatedOSSDriver,
			defaults: map[string]string{isolatedProviderAlias: "provider", "bucket": "bucket"},
			want:     map[string]string{isolatedProviderAlias: "provider", isolatedProvider: "provider", "bucket": "bucket"},
			metadata: `[{"credentialProviderName":"provider","attributes":{"bucket-name":"bucket","sub-path":"base/data"}}]`},
		{name: "BucketSpace explicitly clears prefix", driver: isolatedOSSDriver,
			defaults:   map[string]string{isolatedProvider: "provider", "bucketSpacePrefix": "old"},
			attributes: map[string]string{"bucketSpace": "space", "bucketSpacePrefix": ""},
			want:       map[string]string{isolatedProvider: "provider", "bucketSpace": "space", "bucketSpacePrefix": ""},
			metadata:   `[{"credentialProviderName":"provider","attributes":{"bucket-space-name":"space","sub-path":"base/data"}}]`},
		{name: "prefix explicitly clears BucketSpace", driver: isolatedOSSDriver,
			defaults:   map[string]string{isolatedProvider: "provider", "bucketSpace": "old", "region": "cn-hangzhou"},
			attributes: map[string]string{"bucketSpace": "", "bucketSpacePrefix": "tenant", "region": "cn-shanghai"},
			want:       map[string]string{isolatedProvider: "provider", "bucketSpace": "", "bucketSpacePrefix": "tenant", "region": "cn-shanghai"},
			metadata:   `[{"credentialProviderName":"provider","attributes":{"bucket-space-prefix":"tenant","sub-path":"base/data","region":"cn-shanghai"}}]`},
		{name: "request KMS key normalizes encryption", driver: isolatedOSSDriver,
			defaults:   map[string]string{isolatedProvider: "provider", "bucket": "bucket", "encrypted": "aes", "kmsKeyId": "old-key"},
			attributes: map[string]string{"kmsKeyId": "new-key"},
			want:       map[string]string{isolatedProvider: "provider", "bucket": "bucket", "encrypted": "kms", "kmsKeyId": "new-key"},
			metadata:   `[{"credentialProviderName":"provider","attributes":{"bucket-name":"bucket","sub-path":"base/data","kms-key-id":"new-key"}}]`},
		{name: "ordinary template attributes echoed exactly", driver: isolatedOSSDriver,
			defaults:   map[string]string{isolatedProvider: "provider", "bucket": "bucket", "ordinary": "fixed", "endpoint": "platform-endpoint"},
			attributes: map[string]string{"ordinary": "fixed", "endpoint": "platform-endpoint"},
			want:       map[string]string{isolatedProvider: "provider", "bucket": "bucket", "ordinary": "fixed", "endpoint": "platform-endpoint"},
			metadata:   `[{"credentialProviderName":"provider","attributes":{"bucket-name":"bucket","sub-path":"base/data"}}]`},
		{name: "NAS template without provider", driver: isolatedNASDriver,
			attributes: map[string]string{isolatedProviderAlias: "customer", "server": "ap-new.fs.cn-hangzhou.nas.aliyuncs.com"},
			want:       map[string]string{isolatedProviderAlias: "customer", isolatedProvider: "customer", "server": "ap-new.fs.cn-hangzhou.nas.aliyuncs.com"},
			metadata:   `[{"credentialProviderName":"customer","attributes":{"nas-ap-id":"ap-new"}}]`},
		{name: "NAS replaces provider and AP", driver: isolatedNASDriver,
			defaults:   map[string]string{isolatedProviderAlias: "old", isolatedProvider: "old", "server": "ap-old.fs.cn-hangzhou.nas.aliyuncs.com"},
			attributes: map[string]string{isolatedProvider: "customer", "server": "ap-new.fs.cn-shanghai.nas.aliyuncs.com"},
			want:       map[string]string{isolatedProviderAlias: "customer", isolatedProvider: "customer", "server": "ap-new.fs.cn-shanghai.nas.aliyuncs.com"},
			metadata:   `[{"credentialProviderName":"customer","attributes":{"nas-ap-id":"ap-new"}}]`},
		{name: "conflicting request aliases", driver: isolatedOSSDriver,
			defaults:   map[string]string{isolatedProvider: "provider", "bucket": "bucket"},
			attributes: map[string]string{isolatedProvider: "one", isolatedProviderAlias: "two"}, denied: true},
		{name: "empty request alias conflicts", driver: isolatedOSSDriver,
			defaults:   map[string]string{isolatedProvider: "provider", "bucket": "bucket"},
			attributes: map[string]string{isolatedProvider: "one", isolatedProviderAlias: ""}, denied: true},
		{name: "empty request provider cannot retain default", driver: isolatedOSSDriver,
			defaults:   map[string]string{isolatedProvider: "provider", "bucket": "bucket"},
			attributes: map[string]string{isolatedProviderAlias: ""}, denied: true},
		{name: "no selection preserves default conflict denial", driver: isolatedOSSDriver,
			defaults: map[string]string{isolatedProvider: "one", isolatedProviderAlias: "two", "bucket": "bucket"}, denied: true},
		{name: "request provider wildcard", driver: isolatedOSSDriver,
			defaults:   map[string]string{isolatedProvider: "provider", "bucket": "bucket"},
			attributes: map[string]string{isolatedProvider: "*"}, denied: true},
		{name: "BucketSpace must explicitly clear prefix", driver: isolatedOSSDriver,
			defaults:   map[string]string{isolatedProvider: "provider", "bucketSpacePrefix": "old"},
			attributes: map[string]string{"bucketSpace": "space"}, denied: true},
		{name: "prefix must explicitly clear BucketSpace", driver: isolatedOSSDriver,
			defaults:   map[string]string{isolatedProvider: "provider", "bucketSpace": "old"},
			attributes: map[string]string{"bucketSpacePrefix": "prefix"}, denied: true},
		{name: "empty KMS key cannot drop implicit restriction", driver: isolatedOSSDriver,
			defaults:   map[string]string{isolatedProvider: "provider", "bucket": "bucket", "kmsKeyId": "required"},
			attributes: map[string]string{"kmsKeyId": ""}, denied: true},
		{name: "empty KMS key cannot drop explicit restriction", driver: isolatedOSSDriver,
			defaults:   map[string]string{isolatedProvider: "provider", "bucket": "bucket", "kmsKeyId": "required", "encrypted": "kms"},
			attributes: map[string]string{"kmsKeyId": ""}, denied: true},
		{name: "unchanged invalid KMS mode is rejected", driver: isolatedOSSDriver,
			defaults: map[string]string{isolatedProvider: "provider", "bucket": "bucket", "kmsKeyId": "required", "encrypted": "aes"}, denied: true},
		{name: "request bucket wildcard", driver: isolatedOSSDriver,
			defaults:   map[string]string{isolatedProvider: "provider", "bucket": "bucket"},
			attributes: map[string]string{"bucket": "*"}, denied: true},
		{name: "request cannot clear every target", driver: isolatedOSSDriver,
			defaults:   map[string]string{isolatedProvider: "provider", "bucket": "bucket"},
			attributes: map[string]string{"bucket": ""}, denied: true},
		{name: "request KMS wildcard", driver: isolatedOSSDriver,
			defaults:   map[string]string{isolatedProvider: "provider", "bucket": "bucket"},
			attributes: map[string]string{"kmsKeyId": "*"}, denied: true},
		{name: "request region wildcard", driver: isolatedOSSDriver,
			defaults:   map[string]string{isolatedProvider: "provider", "bucket": "bucket"},
			attributes: map[string]string{"region": "*"}, denied: true},
		{name: "NAS AP on foreign server", driver: isolatedNASDriver,
			defaults:   map[string]string{isolatedProvider: "provider"},
			attributes: map[string]string{"server": "ap-abc.evil.test"}, denied: true},
		{name: "NAS truncated server", driver: isolatedNASDriver,
			defaults:   map[string]string{isolatedProvider: "provider"},
			attributes: map[string]string{"server": "ap-abc.cn-hangzhou.nas.aliyuncs.com"}, denied: true},
		{name: "NAS server with port", driver: isolatedNASDriver,
			defaults:   map[string]string{isolatedProvider: "provider"},
			attributes: map[string]string{"server": "ap-abc.fs.cn-hangzhou.nas.aliyuncs.com:2049"}, denied: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			firstPV, first := isolatedTestTemplate("first")
			pv, request := isolatedTestTemplate("dynamic")
			pv.Spec.CSI.Driver = tt.driver
			pv.Spec.CSI.VolumeAttributes = map[string]string{volumeAttributeAuthType: isolatedAgentIdentity, "path": "/base"}
			maps.Copy(pv.Spec.CSI.VolumeAttributes, tt.defaults)
			request.Attributes = maps.Clone(tt.attributes)
			request.SubPath = "data"
			beforeRequest, beforePV := request.DeepCopy(), pv.DeepCopy()
			reader := isolatedTestClient(t, firstPV, pv)
			registry := &mockStorageProviderRegistry{}
			provider := isolatedTestProvider(func(ctx context.Context, target string, pv *corev1.PersistentVolume, ro bool, secret *corev1.Secret) (*csi.NodePublishVolumeRequest, error) {
				if tt.denied {
					t.Fatal("invalid scope reached execution provider before full-batch validation")
				}
				assert.Nil(t, secret)
				return (&storages.MountProvider{}).GenerateCSINodePublishVolumeRequest(ctx, target, pv, ro, secret)
			})
			registry.RegisterProvider(isolatedOSSDriver, provider)
			registry.RegisterProvider(isolatedNASDriver, provider)
			handler := NewCSIMountHandler(reader, nil, registry, "")
			requests := []v1alpha1.CSIMountConfig{request}
			if tt.denied {
				requests = []v1alpha1.CSIMountConfig{first, request}
			}
			for _, forExecution := range []bool{false, true} {
				mounts, err := handler.GenerateIsolatedMountOptions(context.Background(), "tenant-ns", "owner", requests, forExecution)
				scopedMounts, scope, scopedErr := handler.GenerateIsolatedMountOptionsWithScope(context.Background(), "tenant-ns", "owner", requests, forExecution)
				if tt.denied {
					require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
					assert.Equal(t, resourceaccess.ErrAccessDenied.Error(), err.Error())
					assert.Nil(t, mounts)
					require.ErrorIs(t, scopedErr, resourceaccess.ErrAccessDenied)
					assert.Nil(t, scopedMounts)
					assert.Empty(t, scope)
				} else {
					require.NoError(t, err)
					require.Len(t, mounts, 1)
					want := map[string]string{volumeAttributeAuthType: isolatedAgentIdentity, "path": "/base/data"}
					maps.Copy(want, tt.want)
					assert.Equal(t, want, mounts[0].PublishRequest.VolumeContext)
					key, metadata, err := BuildIsolatedStorageAuthAnnotation(context.Background(), mounts)
					require.NoError(t, err)
					assert.Equal(t, resourceaccess.StorageAuthAnnotation, key)
					assert.JSONEq(t, tt.metadata, metadata)
					require.NoError(t, scopedErr)
					require.Len(t, scopedMounts, 1)
					assert.Equal(t, want, scopedMounts[0].PublishRequest.VolumeContext)
					assert.Equal(t, metadata, scope)
					mounts[0].PublishRequest.VolumeContext["bucket"] = "mutated"
				}
				assert.Equal(t, *beforeRequest, request)
				cached := &corev1.PersistentVolume{}
				require.NoError(t, reader.Get(context.Background(), client.ObjectKey{Name: pv.Name}, cached))
				assert.Equal(t, beforePV.Spec, cached.Spec)
			}
		})
	}
}

func TestGenerateIsolatedMountOptionsRejectsNonProjectionOverrides(t *testing.T) {
	for _, driver := range []string{isolatedOSSDriver, isolatedNASDriver} {
		keys := []string{"endpoint", "url", "akId", "akSecret", "accessKeyId", "accessKeySecret", "token", "roleArn", "secretName", "encrypted", "agenticBucket", "unknown"}
		if driver == isolatedOSSDriver {
			keys = append(keys, "server")
		} else {
			keys = append(keys, "bucket", "bucketSpace", "bucketSpacePrefix", "kmsKeyId", "region")
		}
		for _, key := range keys {
			for _, existing := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/existing=%t", driver, key, existing), func(t *testing.T) {
					pv, request := isolatedTestTemplate("volume")
					pv.Spec.CSI.Driver = driver
					pv.Spec.CSI.VolumeAttributes["server"] = "ap-abc.fs.cn-hangzhou.nas.aliyuncs.com"
					if existing {
						pv.Spec.CSI.VolumeAttributes[key] = "platform-value"
					} else {
						delete(pv.Spec.CSI.VolumeAttributes, key)
					}
					request.Attributes = map[string]string{key: "sensitive-token"}
					handler := NewCSIMountHandler(isolatedTestClient(t, pv), nil, nil, "")
					for _, forExecution := range []bool{false, true} {
						mounts, err := handler.GenerateIsolatedMountOptions(context.Background(), "tenant-ns", "owner", []v1alpha1.CSIMountConfig{request}, forExecution)
						require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
						assert.Equal(t, resourceaccess.ErrAccessDenied.Error(), err.Error())
						assert.Nil(t, mounts)
					}
				})
			}
		}
	}
}

func TestGenerateIsolatedMountOptionsRejectsStaticAuth(t *testing.T) {
	for _, authType := range []string{"", "static", "secret", "access-key", "rrsa", "Agent-Identity", "agent-identity "} {
		for _, ref := range []*corev1.SecretReference{nil, {Namespace: "system-ns", Name: "secret"}, {Namespace: "tenant-ns", Name: "secret"}, {}} {
			t.Run(fmt.Sprintf("%q/%v", authType, ref), func(t *testing.T) {
				firstPV, first := isolatedTestTemplate("first")
				pv, request := isolatedTestTemplate("static")
				pv.Spec.CSI.VolumeAttributes[volumeAttributeAuthType] = authType
				pv.Spec.CSI.NodePublishSecretRef = ref
				registry := &mockStorageProviderRegistry{}
				registry.RegisterProvider(isolatedOSSDriver, isolatedTestProvider(func(context.Context, string, *corev1.PersistentVolume, bool, *corev1.Secret) (*csi.NodePublishVolumeRequest, error) {
					t.Fatal("static auth reached an execution provider")
					return nil, nil
				}))
				handler := NewCSIMountHandler(isolatedTestClient(t, firstPV, pv), nil, registry, "system-ns")
				for _, forExecution := range []bool{false, true} {
					mounts, err := handler.GenerateIsolatedMountOptions(context.Background(), "tenant-ns", "owner", []v1alpha1.CSIMountConfig{first, request}, forExecution)
					require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
					assert.Equal(t, resourceaccess.ErrAccessDenied.Error(), err.Error())
					assert.Nil(t, mounts)
					mounts, scope, err := handler.GenerateIsolatedMountOptionsWithScope(context.Background(), "tenant-ns", "owner", []v1alpha1.CSIMountConfig{first, request}, forExecution)
					require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
					assert.Nil(t, mounts)
					assert.Empty(t, scope)
				}
			})
		}
	}
}

func TestGenerateIsolatedMountOptionsExecutionDependencies(t *testing.T) {
	for _, tt := range []struct {
		name      string
		registry  bool
		supported bool
		provider  bool
	}{
		{name: "registered driver", registry: true, supported: true, provider: true},
		{name: "missing registry"},
		{name: "unsupported driver", registry: true},
		{name: "missing provider", registry: true, supported: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pv, request := isolatedTestTemplate("volume")
			pv.Spec.CSI.NodePublishSecretRef = &corev1.SecretReference{Namespace: "foreign", Name: "nonexistent"}
			reader := isolatedTestClient(t, pv)
			var registry storages.VolumeMountProviderRegistry
			if tt.registry {
				r := &mockStorageProviderRegistry{supportedDrivers: map[string]bool{pv.Spec.CSI.Driver: tt.supported}}
				if tt.provider {
					r.RegisterProvider(pv.Spec.CSI.Driver, &storages.MountProvider{})
				}
				registry = r
			}
			refresh := NewCSIMountHandler(reader, nil, nil, "")
			mounts, err := refresh.GenerateIsolatedMountOptions(context.Background(), "tenant-ns", "owner", []v1alpha1.CSIMountConfig{request}, false)
			require.NoError(t, err)
			require.Len(t, mounts, 1)
			assert.Empty(t, mounts[0].PublishRequest.Secrets)
			handler := NewCSIMountHandler(reader, nil, registry, "system-ns")
			mounts, err = handler.GenerateIsolatedMountOptions(context.Background(), "tenant-ns", "owner", []v1alpha1.CSIMountConfig{request}, true)
			scopedMounts, scope, scopedErr := handler.GenerateIsolatedMountOptionsWithScope(context.Background(), "tenant-ns", "owner", []v1alpha1.CSIMountConfig{request}, true)
			if !tt.provider {
				require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
				assert.Nil(t, mounts)
				assert.NotContains(t, err.Error(), "sensitive-token")
				require.ErrorIs(t, scopedErr, resourceaccess.ErrAccessDenied)
				assert.Nil(t, scopedMounts)
				assert.Empty(t, scope)
			} else {
				require.NoError(t, err)
				require.Len(t, mounts, 1)
				assert.Empty(t, mounts[0].PublishRequest.Secrets)
				require.NoError(t, scopedErr)
				require.Len(t, scopedMounts, 1)
				assert.NotEmpty(t, scope)
			}
		})
	}
}

func TestGenerateIsolatedMountOptionsBackendErrors(t *testing.T) {
	backend := errors.New("backend unavailable with sensitive-token")
	backendStatus := apierrors.NewServiceUnavailable("backend unavailable with sensitive-token")
	for _, cause := range []error{backend, backendStatus, apierrors.NewNotFound(schema.GroupResource{Resource: "sensitive-resource"}, "sensitive-name"), apierrors.NewForbidden(schema.GroupResource{Resource: "sensitive-resource"}, "sensitive-name", errors.New("sensitive-token")), apierrors.NewUnauthorized("sensitive-token"), context.Canceled, context.DeadlineExceeded} {
		for _, forExecution := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/%t", cause, forExecution), func(t *testing.T) {
				pv, request := isolatedTestTemplate("volume")
				reader := interceptor.NewClient(isolatedTestClient(t, pv), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*corev1.PersistentVolume); ok {
						return cause
					}
					return c.Get(ctx, key, obj, opts...)
				}})
				registry := &mockStorageProviderRegistry{}
				registry.RegisterProvider(pv.Spec.CSI.Driver, &storages.MountProvider{})
				handler := NewCSIMountHandler(reader, nil, registry, "system-ns")
				mounts, err := handler.GenerateIsolatedMountOptions(context.Background(), "tenant-ns", "owner", []v1alpha1.CSIMountConfig{request}, forExecution)
				assert.Nil(t, mounts)
				require.Error(t, err)
				if cause == backend || cause == backendStatus || cause == context.Canceled || cause == context.DeadlineExceeded {
					require.ErrorIs(t, err, resourceaccess.ErrBackendRead)
					require.ErrorIs(t, err, cause)
					if cause == backendStatus {
						var status *apierrors.StatusError
						require.ErrorAs(t, err, &status)
						assert.Same(t, backendStatus, status)
					}
				} else {
					require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
					assert.Equal(t, resourceaccess.ErrAccessDenied.Error(), err.Error())
				}
				scopedMounts, scope, scopedErr := handler.GenerateIsolatedMountOptionsWithScope(context.Background(), "tenant-ns", "owner", []v1alpha1.CSIMountConfig{request}, forExecution)
				require.EqualError(t, scopedErr, err.Error())
				assert.Nil(t, scopedMounts)
				assert.Empty(t, scope)
			})
		}
	}
}

func TestGenerateIsolatedMountOptionsBatchReadOrdering(t *testing.T) {
	for _, tt := range []struct {
		name       string
		change     func(*corev1.PersistentVolume)
		wantDenied bool
	}{
		{name: "all PVs before providers"},
		{name: "ignored empty secret namespace", change: func(pv *corev1.PersistentVolume) { pv.Spec.CSI.NodePublishSecretRef.Namespace = "" }},
		{name: "ignored foreign secret namespace", change: func(pv *corev1.PersistentVolume) { pv.Spec.CSI.NodePublishSecretRef.Namespace = "foreign" }},
		{name: "ignored empty secret name", change: func(pv *corev1.PersistentVolume) { pv.Spec.CSI.NodePublishSecretRef.Name = "" }},
		{name: "late missing execution provider", change: func(pv *corev1.PersistentVolume) {
			pv.Spec.CSI.Driver = isolatedNASDriver
			pv.Spec.CSI.VolumeAttributes["server"] = "ap-abc.fs.cn-hangzhou.nas.aliyuncs.com"
		}, wantDenied: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			firstPV, first := isolatedTestTemplate("first")
			firstPV.Spec.CSI.NodePublishSecretRef = &corev1.SecretReference{Name: "secret"}
			pv, request := isolatedTestTemplate("second")
			pv.Spec.CSI.NodePublishSecretRef = &corev1.SecretReference{Namespace: "system-ns", Name: "secret"}
			if tt.change != nil {
				tt.change(pv)
			}
			c := isolatedTestClient(t, firstPV, pv)
			var reads []string
			reader := interceptor.NewClient(c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				reads = append(reads, key.Name)
				return c.Get(ctx, key, obj, opts...)
			}})
			apiReader := interceptor.NewClient(c, interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				t.Fatal("isolated reads must never fall back to APIReader")
				return nil
			}})
			registry := &mockStorageProviderRegistry{}
			calls := 0
			registry.RegisterProvider(isolatedOSSDriver, isolatedTestProvider(func(ctx context.Context, target string, pv *corev1.PersistentVolume, ro bool, secret *corev1.Secret) (*csi.NodePublishVolumeRequest, error) {
				calls++
				assert.Equal(t, []string{"first", "second"}, reads)
				assert.Nil(t, secret)
				return (&storages.MountProvider{}).GenerateCSINodePublishVolumeRequest(ctx, target, pv, ro, secret)
			}))
			handler := NewCSIMountHandler(reader, apiReader, registry, "system-ns")
			mounts, err := handler.GenerateIsolatedMountOptions(context.Background(), "tenant-ns", "owner", []v1alpha1.CSIMountConfig{first, request}, true)
			if tt.wantDenied {
				require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
				assert.Nil(t, mounts)
				assert.NotContains(t, err.Error(), "sensitive-token")
				assert.Zero(t, calls)
			} else {
				require.NoError(t, err)
				require.Len(t, mounts, 2)
				assert.Equal(t, 2, calls)
				assert.Empty(t, mounts[0].PublishRequest.Secrets)
				assert.Empty(t, mounts[1].PublishRequest.Secrets)
			}
			assert.Equal(t, []string{"first", "second"}, reads)
		})
	}
}

// The existing provider mock cannot return or mutate a fully formed request.
// This adapter is only for adversarial provider-result and batch-preflight tests.
type isolatedTestProvider func(context.Context, string, *corev1.PersistentVolume, bool, *corev1.Secret) (*csi.NodePublishVolumeRequest, error)

func (p isolatedTestProvider) GenerateCSINodePublishVolumeRequest(ctx context.Context, target string, pv *corev1.PersistentVolume, readOnly bool, secret *corev1.Secret) (*csi.NodePublishVolumeRequest, error) {
	return p(ctx, target, pv, readOnly, secret)
}

func TestGenerateIsolatedMountOptionsProviderCannotRedirect(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*csi.NodePublishVolumeRequest) error
	}{
		{"nil result", nil},
		{"volume target", func(r *csi.NodePublishVolumeRequest) error { r.VolumeContext["path"] = "/other"; return nil }},
		{"nil volume context", func(r *csi.NodePublishVolumeRequest) error { r.VolumeContext = nil; return nil }},
		{"empty volume context", func(r *csi.NodePublishVolumeRequest) error { r.VolumeContext = map[string]string{}; return nil }},
		{"auth downgrade", func(r *csi.NodePublishVolumeRequest) error { r.VolumeContext["authType"] = "static"; return nil }},
		{"provider selection redirect", func(r *csi.NodePublishVolumeRequest) error { r.VolumeContext[isolatedProvider] = "other"; return nil }},
		{"bucket redirect", func(r *csi.NodePublishVolumeRequest) error { r.VolumeContext["bucket"] = "other"; return nil }},
		{"mount target", func(r *csi.NodePublishVolumeRequest) error { r.TargetPath = "/other"; return nil }},
		{"empty mount target", func(r *csi.NodePublishVolumeRequest) error { r.TargetPath = ""; return nil }},
		{"read only downgrade", func(r *csi.NodePublishVolumeRequest) error { r.Readonly = false; return nil }},
		{"static credential fallback", func(r *csi.NodePublishVolumeRequest) error {
			r.Secrets = map[string]string{"token": "sensitive-token"}
			return nil
		}},
		{"provider error with partial secrets", func(r *csi.NodePublishVolumeRequest) error {
			r.Secrets = map[string]string{"token": "sensitive-token"}
			return errors.New("provider failed with sensitive-token")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			firstPV, first := isolatedTestTemplate("first")
			pv, request := isolatedTestTemplate("second")
			request.ReadOnly = true
			reader := isolatedTestClient(t, firstPV, pv)
			registry := &mockStorageProviderRegistry{}
			registry.RegisterProvider(pv.Spec.CSI.Driver, isolatedTestProvider(func(ctx context.Context, target string, pv *corev1.PersistentVolume, ro bool, secret *corev1.Secret) (*csi.NodePublishVolumeRequest, error) {
				assert.Nil(t, secret)
				if pv.Name == "first" {
					return (&storages.MountProvider{}).GenerateCSINodePublishVolumeRequest(ctx, target, pv, ro, secret)
				}
				if tt.change == nil {
					return nil, nil
				}
				publish, err := (&storages.MountProvider{}).GenerateCSINodePublishVolumeRequest(ctx, target, pv, ro, secret)
				if err != nil {
					return nil, err
				}
				return publish, tt.change(publish)
			}))
			handler := NewCSIMountHandler(reader, nil, registry, "system-ns")
			mounts, err := handler.GenerateIsolatedMountOptions(context.Background(), "tenant-ns", "owner", []v1alpha1.CSIMountConfig{first, request}, true)
			require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
			assert.NotContains(t, err.Error(), "sensitive-token")
			assert.Nil(t, mounts)
			mounts, scope, err := handler.GenerateIsolatedMountOptionsWithScope(context.Background(), "tenant-ns", "owner", []v1alpha1.CSIMountConfig{first, request}, true)
			require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
			assert.NotContains(t, err.Error(), "sensitive-token")
			assert.Nil(t, mounts)
			assert.Empty(t, scope)
		})
	}
}

func TestGenerateIsolatedMountOptionsWithScopeOwnsProviderResults(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reuse=%t", reuse), func(t *testing.T) {
			firstPV, first := isolatedTestTemplate("first")
			secondPV, second := isolatedTestTemplate("second")
			first.Attributes = map[string]string{isolatedProvider: "first-provider", "bucket": "first-bucket"}
			first.SubPath, first.ReadOnly = "first", true
			second.Attributes = map[string]string{isolatedProvider: "second-provider", "bucket": "second-bucket"}
			second.SubPath = "second"
			requests := []v1alpha1.CSIMountConfig{first, second}
			reader := isolatedTestClient(t, firstPV, secondPV)
			refresh := NewCSIMountHandler(reader, nil, nil, "")
			preflight, wantScope, err := refresh.GenerateIsolatedMountOptionsWithScope(context.Background(), "tenant-ns", "owner", requests, false)
			require.NoError(t, err)
			require.Len(t, preflight, 2)
			key, projectedScope, err := BuildIsolatedStorageAuthAnnotation(context.Background(), preflight)
			require.NoError(t, err)
			assert.Equal(t, resourceaccess.StorageAuthAnnotation, key)
			assert.Equal(t, projectedScope, wantScope)
			assert.JSONEq(t, `[{"credentialProviderName":"first-provider","attributes":{"bucket-name":"first-bucket","sub-path":"base/first"}},{"credentialProviderName":"second-provider","attributes":{"bucket-name":"second-bucket","sub-path":"base/second"}}]`, wantScope)

			var retained *csi.NodePublishVolumeRequest
			var snapshots []*csi.NodePublishVolumeRequest
			registry := &mockStorageProviderRegistry{}
			registry.RegisterProvider(isolatedOSSDriver, isolatedTestProvider(func(ctx context.Context, target string, pv *corev1.PersistentVolume, ro bool, secret *corev1.Secret) (*csi.NodePublishVolumeRequest, error) {
				assert.Nil(t, secret)
				publish, err := (&storages.MountProvider{}).GenerateCSINodePublishVolumeRequest(ctx, target, pv, ro, secret)
				require.NoError(t, err)
				if retained == nil {
					publish.PublishContext = map[string]string{"route": "first"}
					publish.Secrets = map[string]string{}
					publish.VolumeCapability.GetMount().MountFlags = []string{"first"}
					retained = publish
				} else {
					// Mutate the earlier result's scalars, maps and nested protobufs
					// even when returning a different request for the second mount.
					retained.VolumeId = publish.VolumeId
					retained.TargetPath = target
					retained.Readonly = ro
					clear(retained.VolumeContext)
					maps.Copy(retained.VolumeContext, publish.VolumeContext)
					retained.PublishContext["route"] = "second"
					retained.VolumeCapability.AccessMode.Mode = publish.VolumeCapability.AccessMode.Mode
					retained.VolumeCapability.GetMount().MountFlags[0] = "second"
					if reuse {
						publish = retained
					} else {
						retained.Secrets["token"] = "sensitive-token"
					}
				}
				snapshots = append(snapshots, proto.Clone(publish).(*csi.NodePublishVolumeRequest))
				return publish, nil
			}))
			handler := NewCSIMountHandler(reader, nil, registry, "")
			mounts, scope, err := handler.GenerateIsolatedMountOptionsWithScope(context.Background(), "tenant-ns", "owner", requests, true)
			require.NoError(t, err)
			require.Len(t, mounts, 2)
			require.Len(t, snapshots, 2)
			assert.Equal(t, wantScope, scope)
			for i, mount := range mounts {
				assert.NotSame(t, retained, mount.PublishRequest)
				assert.True(t, proto.Equal(snapshots[i], mount.PublishRequest), "mount %d changed after a later provider call", i)
				assert.Equal(t, preflight[i].PublishRequest.VolumeContext, mount.PublishRequest.VolumeContext)
			}
			retained.TargetPath = "/mutated"
			retained.VolumeContext["bucket"] = "mutated"
			retained.PublishContext["route"] = "mutated"
			retained.Secrets["token"] = "sensitive-token"
			retained.VolumeCapability.GetMount().MountFlags[0] = "mutated"
			for i, mount := range mounts {
				assert.True(t, proto.Equal(snapshots[i], mount.PublishRequest), "mount %d still aliases provider-owned data", i)
			}
			_, projectedScope, err = BuildIsolatedStorageAuthAnnotation(context.Background(), mounts)
			require.NoError(t, err)
			assert.Equal(t, scope, projectedScope)
			mounts[0].PublishRequest.VolumeContext["bucket"] = "caller-mutated"
			assert.Equal(t, wantScope, scope)
		})
	}
}

func TestGenerateIsolatedMountOptionsWithScopeProviderCancellation(t *testing.T) {
	for _, cancelAt := range []int{1, 2} {
		t.Run(fmt.Sprintf("provider=%d", cancelAt), func(t *testing.T) {
			firstPV, first := isolatedTestTemplate("first")
			secondPV, second := isolatedTestTemplate("second")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			registry := &mockStorageProviderRegistry{}
			registry.RegisterProvider(isolatedOSSDriver, isolatedTestProvider(func(ctx context.Context, target string, pv *corev1.PersistentVolume, ro bool, secret *corev1.Secret) (*csi.NodePublishVolumeRequest, error) {
				publish, err := (&storages.MountProvider{}).GenerateCSINodePublishVolumeRequest(ctx, target, pv, ro, secret)
				calls++
				if calls == cancelAt {
					cancel()
				}
				return publish, err
			}))
			handler := NewCSIMountHandler(isolatedTestClient(t, firstPV, secondPV), nil, registry, "")
			mounts, scope, err := handler.GenerateIsolatedMountOptionsWithScope(ctx, "tenant-ns", "owner", []v1alpha1.CSIMountConfig{first, second}, true)
			require.ErrorIs(t, err, context.Canceled)
			assert.Nil(t, mounts)
			assert.Empty(t, scope)
			assert.Equal(t, cancelAt, calls)
		})
	}
}

func TestGenerateIsolatedMountOptionsEmptyAndCaller(t *testing.T) {
	handler := NewCSIMountHandler(nil, nil, nil, "system-ns")
	for _, tt := range []struct{ namespace, owner string }{{"", "owner"}, {" ", "owner"}, {"tenant-ns", ""}, {"tenant-ns", " "}} {
		for _, forExecution := range []bool{false, true} {
			mounts, err := handler.GenerateIsolatedMountOptions(context.Background(), tt.namespace, tt.owner, nil, forExecution)
			require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
			assert.Nil(t, mounts)
			mounts, scope, err := handler.GenerateIsolatedMountOptionsWithScope(context.Background(), tt.namespace, tt.owner, nil, forExecution)
			require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
			assert.Nil(t, mounts)
			assert.Empty(t, scope)
		}
	}
	for _, forExecution := range []bool{false, true} {
		for _, requests := range [][]v1alpha1.CSIMountConfig{nil, {}} {
			mounts, err := handler.GenerateIsolatedMountOptions(context.Background(), "tenant-ns", "owner", requests, forExecution)
			require.NoError(t, err)
			assert.Empty(t, mounts)
			mounts, scope, err := handler.GenerateIsolatedMountOptionsWithScope(context.Background(), "tenant-ns", "owner", requests, forExecution)
			require.NoError(t, err)
			assert.Empty(t, mounts)
			assert.Empty(t, scope)
		}
		mounts, err := handler.GenerateIsolatedMountOptions(context.Background(), "tenant-ns", "owner", []v1alpha1.CSIMountConfig{{PvName: "volume"}}, forExecution)
		require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
		assert.Nil(t, mounts)
		mounts, scope, err := handler.GenerateIsolatedMountOptionsWithScope(context.Background(), "tenant-ns", "owner", []v1alpha1.CSIMountConfig{{PvName: "volume"}}, forExecution)
		require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
		assert.Nil(t, mounts)
		assert.Empty(t, scope)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mounts, err := handler.GenerateIsolatedMountOptions(ctx, "tenant-ns", "owner", nil, false)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, mounts)
	mounts, scope, err := handler.GenerateIsolatedMountOptionsWithScope(ctx, "tenant-ns", "owner", nil, false)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, mounts)
	assert.Empty(t, scope)
}

func TestLegacyCSIDefaultCompatibility(t *testing.T) {
	assert.Nil(t, NodePublishVolumeEnricher)
	assert.Nil(t, BuildStorageAuthAnnotation)
	pv, _, request := isolatedTestBinding("legacy")
	pv.Spec.ClaimRef = nil
	pv.Status.Phase = corev1.VolumeAvailable
	pv.Spec.CSI.NodePublishSecretRef = &corev1.SecretReference{Name: "secret"}
	request.Attributes = map[string]string{"ordinary": "request-override", "new": "value"}
	cache, _, err := cachetest.NewTestCache(t, pv, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "secret", Namespace: utils.DefaultSandboxDeployNamespace}, Data: map[string][]byte{"token": []byte("legacy-token")}})
	require.NoError(t, err)
	registry := &mockStorageProviderRegistry{}
	registry.RegisterProvider(pv.Spec.CSI.Driver, &storages.MountProvider{})
	handler := NewCSIMountHandler(cache.GetClient(), cache.GetAPIReader(), registry, "different-system-ns")
	_, publish, err := handler.GenerateNodePublishVolumeRequest(context.Background(), request)
	require.NoError(t, err)
	assert.Equal(t, "request-override", publish.VolumeContext["ordinary"])
	assert.Equal(t, "value", publish.VolumeContext["new"])
	assert.Equal(t, "legacy-token", publish.Secrets["token"])
}
