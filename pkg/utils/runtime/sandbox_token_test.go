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
	"encoding/json"
	"fmt"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/resourceaccess"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/csiutils"
	"github.com/openkruise/agents/pkg/utils/runtime/config"
)

func TestPrepareSandboxTokenPolicyAndSnapshot(t *testing.T) {
	for _, tt := range []struct {
		name, policy, owner, namespace string
		marked                         bool
		mounts                         []config.MountConfig
		wantErr                        bool
		unchanged                      bool
		wantScope                      bool
		canceled                       bool
	}{
		{name: "unmarked legacy performs no I/O", unchanged: true},
		{name: "trusted performs no I/O", marked: true, policy: "trusted-domain-v1", unchanged: true},
		{name: "trusted ignores cancellation", marked: true, policy: "trusted-domain-v1", unchanged: true, canceled: true},
		{name: "empty policy rejected", marked: true, wantErr: true},
		{name: "unknown policy rejected", marked: true, policy: "future-v1", wantErr: true},
		{name: "isolated missing owner", marked: true, policy: "isolated-v1", namespace: "team-a", mounts: []config.MountConfig{}, wantErr: true},
		{name: "isolated missing namespace", marked: true, policy: "isolated-v1", owner: "owner", mounts: []config.MountConfig{}, wantErr: true},
		{name: "isolated empty snapshot clears stale scope", marked: true, policy: "isolated-v1", owner: "owner", namespace: "team-a", mounts: []config.MountConfig{}},
		{name: "isolated empty snapshot respects cancellation", marked: true, policy: "isolated-v1", owner: "owner", namespace: "team-a", mounts: []config.MountConfig{}, canceled: true, wantErr: true},
		{name: "isolated nil publish rejected", marked: true, policy: "isolated-v1", owner: "owner", namespace: "team-a", mounts: []config.MountConfig{{}}, wantErr: true},
		{name: "isolated static snapshot rejected", marked: true, policy: "isolated-v1", owner: "owner", namespace: "team-a", wantErr: true,
			mounts: []config.MountConfig{{Driver: "ossplugin.csi.alibabacloud.com", PublishRequest: &csi.NodePublishVolumeRequest{VolumeContext: map[string]string{"bucket": "bucket"}}}}},
		{name: "isolated identity snapshot with static credentials rejected", marked: true, policy: "isolated-v1", owner: "owner", namespace: "team-a", wantErr: true,
			mounts: []config.MountConfig{{Driver: "ossplugin.csi.alibabacloud.com", PublishRequest: &csi.NodePublishVolumeRequest{
				VolumeContext: map[string]string{"authType": "agent-identity", "sandboxCredProviderName": "provider", "bucket": "bucket"},
				Secrets:       map[string]string{"key": "static-credential"},
			}}}},
		{name: "isolated exact snapshot bypasses persisted requests", marked: true, policy: "isolated-v1", owner: "owner", namespace: "team-a", wantScope: true,
			mounts: []config.MountConfig{{Driver: "ossplugin.csi.alibabacloud.com", PublishRequest: &csi.NodePublishVolumeRequest{VolumeContext: map[string]string{
				"authType": "agent-identity", "sandboxCredProviderName": "provider", "bucket": "authorized-bucket", "path": "/allowed/",
			}}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: tt.namespace, Annotations: map[string]string{
				agentsv1alpha1.AnnotationOwner:           tt.owner,
				agentsv1alpha1.AnnotationCSIVolumeConfig: "invalid JSON must not be parsed",
				resourceaccess.StorageAuthAnnotation:     "stale",
			}}}
			if tt.marked {
				sbx.Annotations[resourceaccess.AnnotationPolicy] = tt.policy
			}
			before := sbx.DeepCopy()
			c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				t.Error("unexpected Kubernetes read")
				return fmt.Errorf("unexpected read")
			}}).Build()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.canceled {
				cancel()
			}
			got, err := PrepareSandboxToken(ctx, c, sbx, tt.mounts)
			assert.Equal(t, before, sbx, "preparation must not mutate caller metadata")
			if tt.wantErr {
				require.Error(t, err)
				if tt.canceled {
					assert.ErrorIs(t, err, context.Canceled)
				}
				return
			}
			require.NoError(t, err)
			if tt.unchanged {
				assert.Same(t, sbx, got)
				return
			}
			assert.NotSame(t, sbx, got)
			if tt.wantScope {
				assert.JSONEq(t, `[{"credentialProviderName":"provider","attributes":{"bucket-name":"authorized-bucket","sub-path":"allowed/"}}]`, got.Annotations[resourceaccess.StorageAuthAnnotation])
			} else {
				assert.NotContains(t, got.Annotations, resourceaccess.StorageAuthAnnotation)
			}
		})
	}
}

func TestPrepareSandboxTokenUsesCurrentInputs(t *testing.T) {
	for _, tt := range []struct {
		name      string
		snapshot  string
		mutate    func(*config.MountConfig)
		wantError bool
	}{
		{name: "supplied snapshot recomputes changed scope", snapshot: "supplied"},
		{name: "supplied snapshot rejects changed auth type", snapshot: "supplied", wantError: true, mutate: func(m *config.MountConfig) { m.PublishRequest.VolumeContext["authType"] = "static" }},
		{name: "supplied snapshot rejects added credentials", snapshot: "supplied", wantError: true, mutate: func(m *config.MountConfig) { m.PublishRequest.Secrets = map[string]string{"key": "credential"} }},
		{name: "supplied snapshot rejects removed publish request", snapshot: "supplied", wantError: true, mutate: func(m *config.MountConfig) { m.PublishRequest = nil }},
		{name: "supplied snapshot rejects invalid changed scope", snapshot: "supplied", wantError: true, mutate: func(m *config.MountConfig) { m.PublishRequest.VolumeContext["bucket"] = "*" }},
		{name: "nil snapshot rereads current PV only"},
		{name: "explicit empty snapshot ignores persisted intent", snapshot: "empty"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scheme := k8sruntime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "volume"}, Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
					Driver: "ossplugin.csi.alibabacloud.com", VolumeHandle: "volume",
					VolumeAttributes: map[string]string{"authType": "agent-identity", "sandboxCredProviderName": "provider", "bucket": "before"},
				}},
			}}
			reads := 0
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pv).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					require.IsType(t, &corev1.PersistentVolume{}, obj)
					reads++
					return c.Get(ctx, key, obj, opts...)
				},
			}).Build()
			sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "team-a", UID: "before", Annotations: map[string]string{
				resourceaccess.AnnotationPolicy: "isolated-v1", agentsv1alpha1.AnnotationOwner: "owner",
				agentsv1alpha1.AnnotationCSIVolumeConfig: `[{"pvName":"volume","mountPath":"/data"}]`,
				resourceaccess.StorageAuthAnnotation:     "forged",
			}}}
			var mounts []config.MountConfig
			if tt.snapshot == "supplied" {
				h := csiutils.NewCSIMountHandler(c, nil, nil, "")
				var err error
				mounts, _, err = h.GenerateIsolatedMountOptionsWithScope(t.Context(), sbx.Namespace, "owner", []agentsv1alpha1.CSIMountConfig{{PvName: "volume", MountPath: "/data"}}, false)
				require.NoError(t, err)
			} else if tt.snapshot == "empty" {
				mounts = []config.MountConfig{}
			}
			first, err := PrepareSandboxToken(t.Context(), c, sbx, mounts)
			require.NoError(t, err)
			firstBefore := first.DeepCopy()
			if tt.snapshot == "supplied" {
				mounts[0].PublishRequest.VolumeContext["bucket"] = "current"
				if tt.mutate != nil {
					tt.mutate(&mounts[0])
				}
			} else {
				pv.Spec.CSI.VolumeAttributes["bucket"] = "current"
				require.NoError(t, c.Update(t.Context(), pv))
			}
			sbx.UID, sbx.ResourceVersion = "current-uid", "current-version"
			sbx.Labels = map[string]string{"current": "label"}
			sbx.Status.PodInfo.PodIP = "192.0.2.42"
			sbx.Annotations[agentsv1alpha1.AnnotationRuntimeURL] = "http://current-runtime"
			sbx.Annotations[agentsv1alpha1.AnnotationRuntimeAccessToken] = "current-runtime-token"
			sbx.Annotations[resourceaccess.StorageAuthAnnotation] = first.Annotations[resourceaccess.StorageAuthAnnotation]
			before := sbx.DeepCopy()
			readsBefore := reads
			got, err := PrepareSandboxToken(t.Context(), c, sbx, mounts)
			wantReads := 0
			if tt.snapshot == "" {
				wantReads = 1
			}
			assert.Equal(t, wantReads, reads-readsBefore)
			assert.Equal(t, before, sbx, "preparation must not mutate current metadata")
			assert.Equal(t, firstBefore, first, "earlier preparation must remain independent")
			if tt.wantError {
				require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			want := before.DeepCopy()
			delete(want.Annotations, resourceaccess.StorageAuthAnnotation)
			if tt.snapshot != "empty" {
				want.Annotations[resourceaccess.StorageAuthAnnotation] = `[{"credentialProviderName":"provider","attributes":{"bucket-name":"current"}}]`
			}
			assert.Equal(t, want, got, "copy current Sandbox metadata and overlay only fresh scope")
		})
	}
}

func TestPrepareSandboxTokenRevalidatesWithoutSecrets(t *testing.T) {
	for _, tt := range []struct {
		name, owner, namespace string
		mutate                 func(*corev1.PersistentVolume)
		attributes             map[string]string
		wantErr                bool
	}{
		{name: "shared PV scope rebuilt without driver registration"},
		{name: "another owner can prepare the same shared PV", owner: "another-owner"},
		{name: "another namespace can prepare the same shared PV", namespace: "team-b"},
		{name: "dynamic bucket selector becomes requested scope", attributes: map[string]string{"bucket": "requested-bucket"}},
		{name: "missing auth type rejected", mutate: func(pv *corev1.PersistentVolume) { delete(pv.Spec.CSI.VolumeAttributes, "authType") }, wantErr: true},
		{name: "static auth rejected", mutate: func(pv *corev1.PersistentVolume) { pv.Spec.CSI.VolumeAttributes["authType"] = "static" }, wantErr: true},
		{name: "access key auth rejected", mutate: func(pv *corev1.PersistentVolume) { pv.Spec.CSI.VolumeAttributes["authType"] = "access-key" }, wantErr: true},
		{name: "RRSA auth rejected", mutate: func(pv *corev1.PersistentVolume) { pv.Spec.CSI.VolumeAttributes["authType"] = "rrsa" }, wantErr: true},
		{name: "non CSI PV rejected", mutate: func(pv *corev1.PersistentVolume) { pv.Spec.CSI = nil }, wantErr: true},
		{name: "missing driver rejected", mutate: func(pv *corev1.PersistentVolume) { pv.Spec.CSI.Driver = "" }, wantErr: true},
		{name: "deleting PV rejected", mutate: func(pv *corev1.PersistentVolume) {
			now := metav1.Now()
			pv.DeletionTimestamp = &now
			pv.Finalizers = []string{"test/finalizer"}
		}, wantErr: true},
		{name: "invalid credential provider rejected", mutate: func(pv *corev1.PersistentVolume) { pv.Spec.CSI.VolumeAttributes["sandboxCredProviderName"] = "*" }, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scheme := k8sruntime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "volume"}, Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
					Driver: "ossplugin.csi.alibabacloud.com", VolumeHandle: "volume",
					VolumeAttributes:     map[string]string{"authType": "agent-identity", "sandboxCredProviderName": "provider", "bucket": "default-bucket", "path": "/base"},
					NodePublishSecretRef: &corev1.SecretReference{Namespace: utils.DefaultSandboxDeployNamespace, Name: "missing-secret"},
				}},
			}}
			if tt.mutate != nil {
				tt.mutate(pv)
			}
			var reads int
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pv).WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				require.IsType(t, &corev1.PersistentVolume{}, obj, "scope preparation must only read PVs, never PVCs or Secrets")
				reads++
				return c.Get(ctx, key, obj, opts...)
			}}).Build()
			raw, err := json.Marshal([]agentsv1alpha1.CSIMountConfig{{PvName: "volume", MountPath: "/mnt/data", SubPath: "child", Attributes: tt.attributes}})
			require.NoError(t, err)
			sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Annotations: map[string]string{
				resourceaccess.AnnotationPolicy: "isolated-v1", resourceaccess.StorageAuthAnnotation: "stale",
				agentsv1alpha1.AnnotationOwner: "owner", agentsv1alpha1.AnnotationCSIVolumeConfig: string(raw),
			}}}
			if tt.owner != "" {
				sbx.Annotations[agentsv1alpha1.AnnotationOwner] = tt.owner
			}
			if tt.namespace != "" {
				sbx.Namespace = tt.namespace
			}
			before := sbx.DeepCopy()
			got, err := PrepareSandboxToken(t.Context(), c, sbx, nil)
			assert.Equal(t, 1, reads)
			assert.Equal(t, before, sbx, "scope preparation must not mutate the original Sandbox")
			if tt.wantErr {
				require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			bucket := "default-bucket"
			if tt.attributes != nil {
				bucket = tt.attributes["bucket"]
			}
			assert.JSONEq(t, fmt.Sprintf(`[{"credentialProviderName":"provider","attributes":{"bucket-name":%q,"sub-path":"base/child"}}]`, bucket), got.Annotations[resourceaccess.StorageAuthAnnotation])
			assert.Equal(t, sbx.Namespace, got.Namespace)
			assert.Equal(t, sbx.Annotations[agentsv1alpha1.AnnotationOwner], got.Annotations[agentsv1alpha1.AnnotationOwner])
		})
	}
}

func TestResolveCSIMountFromAnnotationPolicy(t *testing.T) {
	for _, policy := range []string{"isolated-v1", "", "unknown"} {
		t.Run(policy, func(t *testing.T) {
			sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Annotations: map[string]string{resourceaccess.AnnotationPolicy: policy, agentsv1alpha1.AnnotationOwner: "owner"}}}
			got, err := ResolveCSIMountFromAnnotation(t.Context(), sbx, nil, nil, storages.NewStorageProvider())
			if policy != "isolated-v1" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.NotNil(t, got.MountOptionList)
		})
	}
}

func TestResolveCSIMountFromAnnotationRejectsStaticBeforeSecrets(t *testing.T) {
	for _, tt := range []struct {
		name, authType, secretNamespace string
	}{
		{name: "missing auth type with system Secret", secretNamespace: utils.DefaultSandboxDeployNamespace},
		{name: "static auth with default Secret namespace", authType: "static"},
		{name: "access key auth with tenant Secret", authType: "access-key", secretNamespace: "team-a"},
		{name: "RRSA auth with system Secret", authType: "rrsa", secretNamespace: utils.DefaultSandboxDeployNamespace},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scheme := k8sruntime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "volume"}, Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
					Driver: "ossplugin.csi.alibabacloud.com", VolumeHandle: "volume", NodePublishSecretRef: &corev1.SecretReference{Namespace: tt.secretNamespace, Name: "credentials"},
					VolumeAttributes: map[string]string{"authType": tt.authType, "sandboxCredProviderName": "provider", "bucket": "bucket"},
				}},
			}}
			reads := 0
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pv).WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				require.IsType(t, &corev1.PersistentVolume{}, obj, "isolated resolution must never read PVCs or Secrets")
				reads++
				return c.Get(ctx, key, obj, opts...)
			}}).Build()
			sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Annotations: map[string]string{
				resourceaccess.AnnotationPolicy: "isolated-v1", agentsv1alpha1.AnnotationOwner: "owner",
				agentsv1alpha1.AnnotationCSIVolumeConfig: `[{"pvName":"volume","mountPath":"/mnt/data"}]`,
			}}}
			registry := storages.NewStorageProvider()
			registry.RegisterProvider(pv.Spec.CSI.Driver, &storages.MountProvider{})
			mounts, err := ResolveCSIMountFromAnnotation(t.Context(), sbx, c, nil, registry)
			require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
			assert.Nil(t, mounts)
			assert.Equal(t, 1, reads)
		})
	}
}
