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
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/resourceaccess"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/csiutils"
	"github.com/openkruise/agents/pkg/utils/runtime"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
	testutils "github.com/openkruise/agents/test/utils"
)

func TestCloneResourceAccessAdmission(t *testing.T) {
	for _, tt := range []struct {
		name          string
		mode          resourceaccess.Mode
		policy        string
		owner         string
		user          string
		denied        bool
		invalid       bool
		withAdmission bool
		callbackErr   bool
	}{
		{name: "default cross-owner legacy", owner: "key-a", user: "key-b"},
		{name: "trusted cross-owner", mode: resourceaccess.TrustedDomain, policy: "trusted-domain-v1", owner: "key-a", user: "key-b"},
		{name: "isolated requires admission even for same owner", mode: resourceaccess.Isolated, owner: "key-a", user: "key-a", denied: true},
		{name: "trusted requires admission for isolated source", policy: "isolated-v1", owner: "key-a", user: "key-a", denied: true},
		{name: "isolated delegates use policy to admission", mode: resourceaccess.Isolated, owner: "key-a", user: "key-b", withAdmission: true},
		{name: "trusted delegates isolated source to admission", policy: "isolated-v1", owner: "key-a", user: "key-b", withAdmission: true},
		{name: "unknown source policy", policy: "future-v9", owner: "key-a", user: "key-a", withAdmission: true, invalid: true},
		{name: "empty source policy", policy: "empty", owner: "key-a", user: "key-a", withAdmission: true, invalid: true},
		{name: "callback rejects before template", owner: "key-a", user: "key-a", withAdmission: true, callbackErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cp := &v1alpha1.Checkpoint{ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "customer", Annotations: map[string]string{v1alpha1.AnnotationOwner: tt.owner}}, Status: v1alpha1.CheckpointStatus{CheckpointId: "snapshot"}}
			if tt.policy != "" {
				cp.Annotations[resourceaccess.AnnotationPolicy] = tt.policy
				if tt.policy == "empty" {
					cp.Annotations[resourceaccess.AnnotationPolicy] = ""
				}
			}
			tmpl := &v1alpha1.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "customer"}, Spec: v1alpha1.SandboxTemplateSpec{Template: &corev1.PodTemplateSpec{}}}
			cache, fc, err := cachetest.NewTestCache(t, cp, tmpl)
			require.NoError(t, err)
			var reads, creates, admitted, callbacks int
			c := interceptor.NewClient(fc.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*v1alpha1.SandboxTemplate); ok {
						reads++
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					creates++
					return assert.AnError
				},
			})
			opts := infra.CloneSandboxOptions{
				ResourceAccessMode: tt.mode, User: tt.user, CheckPointID: "snapshot", SkipWaitCheckpoint: true,
				Admission: &infra.SandboxAdmission{Acquire: func(context.Context, string, infra.SandboxResource) error { admitted++; return assert.AnError }},
			}
			if tt.withAdmission {
				opts.CheckpointAdmission = func(owner string, sourceMode resourceaccess.Mode) error {
					callbacks++
					assert.Zero(t, reads)
					assert.Equal(t, tt.owner, owner)
					wantMode, err := resourceaccess.FromAnnotations(cp.Annotations)
					require.NoError(t, err)
					assert.Equal(t, wantMode, sourceMode)
					if tt.callbackErr {
						return assert.AnError
					}
					return nil
				}
			}
			_, _, err = CloneSandbox(t.Context(), opts, &apiReaderOverrideCache{Provider: cache, client: c, apiReader: c})
			require.Error(t, err)
			assert.Zero(t, creates)
			if tt.denied || tt.invalid {
				assert.Zero(t, reads)
				assert.Zero(t, admitted)
				assert.Zero(t, callbacks)
				if tt.denied {
					assert.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
				}
			} else if tt.callbackErr {
				assert.ErrorIs(t, err, assert.AnError)
				assert.Equal(t, 1, callbacks)
				assert.Zero(t, reads)
				assert.Zero(t, admitted)
			} else {
				assert.ErrorIs(t, err, assert.AnError)
				if tt.withAdmission {
					assert.Equal(t, 1, callbacks)
				} else {
					assert.Zero(t, callbacks)
				}
				assert.Equal(t, 1, reads)
				assert.Equal(t, 1, admitted)
			}
			assert.Equal(t, tt.owner, cp.Annotations[v1alpha1.AnnotationOwner])
		})
	}
}

func TestIsolatedClaimUsesResolvedTokenSnapshot(t *testing.T) {
	for _, tt := range []struct {
		name    string
		empty   bool
		invalid bool
	}{
		{name: "authorized mount"},
		{name: "empty snapshot clears inherited metadata", empty: true},
		{name: "Prepare denial preserves sentinel", invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cache, fc, err := cachetest.NewTestCache(t)
			require.NoError(t, err)
			reads, issues := 0, 0
			c := interceptor.NewClient(fc.(client.WithWatch), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				reads++
				return assert.AnError
			}})
			sbx := AsSandbox(&v1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "customer", Annotations: map[string]string{
				resourceaccess.AnnotationPolicy: "isolated-v1", v1alpha1.AnnotationOwner: "key", identity.AnnotationAgentName: "agent",
				resourceaccess.StorageAuthAnnotation: "forged", v1alpha1.AnnotationCSIVolumeConfig: `[{"pvName":"different-target","mountPath":"/data"}]`,
			}}}, &apiReaderOverrideCache{Provider: cache, client: c})
			mounts := []config.MountConfig{}
			if !tt.empty {
				mounts = append(mounts, config.MountConfig{Driver: "ossplugin.csi.alibabacloud.com", PublishRequest: &csi.NodePublishVolumeRequest{TargetPath: "/data", VolumeContext: map[string]string{"authType": "agent-identity", "sandboxCredProviderName": "provider", "bucket": "authorized-bucket", "path": "/narrow"}}})
			}
			if tt.invalid {
				mounts[0].PublishRequest = nil
			}
			identity.RegisterProvider(&mockIdentityProvider{issueTokenFunc: func(_ context.Context, prepared *v1alpha1.Sandbox) (*identity.TokenResponse, error) {
				issues++
				if tt.empty {
					assert.NotContains(t, prepared.Annotations, resourceaccess.StorageAuthAnnotation)
				} else {
					assert.JSONEq(t, `[{"credentialProviderName":"provider","attributes":{"bucket-name":"authorized-bucket","sub-path":"narrow"}}]`, prepared.Annotations[resourceaccess.StorageAuthAnnotation])
				}
				return nil, assert.AnError
			}})
			t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })
			err = runClaimPostProcesses(t.Context(), sbx, infra.LockTypeUpdate, infra.ClaimSandboxOptions{ResourceAccessMode: resourceaccess.Isolated, CSIMount: &config.CSIMountOptions{MountOptionList: mounts}}, sbx.Cache, &infra.ClaimMetrics{})
			if tt.invalid {
				require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
				var retryErr retriableError
				assert.False(t, errors.As(err, &retryErr))
				assert.Zero(t, issues)
				assert.Zero(t, reads)
				return
			}
			require.ErrorContains(t, err, assert.AnError.Error())
			assert.Equal(t, 1, issues)
			assert.Zero(t, reads, "token issuance must not resolve a different PV snapshot")
		})
	}
}

func TestPrepareIsolatedMounts(t *testing.T) {
	const driver = "ossplugin.csi.alibabacloud.com"
	identity.RegisterProvider(&mockIdentityProvider{})
	t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })
	for _, tt := range []struct {
		name             string
		owner            string
		namespace        string
		raw              string
		inherited        string
		resolved         bool
		static           bool
		authType         string
		secretNamespace  *string
		wantDenied       bool
		wantLimit        bool
		wantRuntimeError bool
		wantMounts       int
	}{
		{name: "actual selected namespace and owner", owner: "key-a", namespace: "team-a", raw: `[{"pvName":"pv","mountPath":"/data","subPath":"child"}]`, wantMounts: 1},
		{name: "inherited intent is prepared", owner: "key-a", namespace: "team-a", inherited: `[{"pvName":"pv","mountPath":"/data"}]`, wantMounts: 1},
		{name: "explicit empty replaces inherited", owner: "key-a", namespace: "team-a", raw: `[]`, inherited: `[{"pvName":"foreign"}]`},
		{name: "empty clears stale auth", owner: "key-a", namespace: "team-a"},
		{name: "shared PV with different key", owner: "key-b", namespace: "team-a", raw: `[{"pvName":"pv","mountPath":"/data"}]`, wantMounts: 1},
		{name: "shared PV with different namespace", owner: "key-b", namespace: "team-b", raw: `[{"pvName":"pv","mountPath":"/data"}]`, wantMounts: 1},
		{name: "empty owner denied", namespace: "team-a", raw: `[{"pvName":"pv","mountPath":"/data"}]`, wantDenied: true},
		{name: "admin empty namespace denied", owner: "key-a", raw: `[{"pvName":"pv","mountPath":"/data"}]`, wantDenied: true},
		{name: "shared PV in selected system namespace", owner: "key-a", namespace: "sandbox-system", raw: `[{"pvName":"pv","mountPath":"/data"}]`, wantMounts: 1},
		{name: "pre-resolved input cannot bypass", owner: "key-a", namespace: "team-a", resolved: true, wantDenied: true},
		{name: "explicit raw wins pre-resolved", owner: "key-a", namespace: "team-a", raw: `[]`, resolved: true},
		{name: "limit before all reads", owner: "key-a", namespace: "team-a", raw: `[{"pvName":"pv","mountPath":"/a"},{"pvName":"pv","mountPath":"/b"}]`, static: true, wantLimit: true},
		{name: "claim batch count precedes missing init validation", owner: "key-a", namespace: "team-a", raw: `[{"pvName":"pv","mountPath":"/a"},{"pvName":"pv","mountPath":"/b"}]`, static: true, wantLimit: true, wantRuntimeError: true},
		{name: "missing batch member denied", owner: "key-a", namespace: "team-a", raw: `[{"pvName":"pv","mountPath":"/a"},{"pvName":"missing","mountPath":"/b"}]`, wantDenied: true},
		{name: "static system Secret denied", owner: "key-a", namespace: "team-a", raw: `[{"pvName":"pv","mountPath":"/data"}]`, static: true, wantDenied: true},
		{name: "static empty Secret namespace denied", owner: "key-a", namespace: "team-a", raw: `[{"pvName":"pv","mountPath":"/data"}]`, static: true, secretNamespace: ptr.To(""), wantDenied: true},
		{name: "tenant Secret ref is denied before Secret read", owner: "key-a", namespace: "team-a", raw: `[{"pvName":"pv","mountPath":"/data"}]`, static: true, secretNamespace: ptr.To("team-a"), wantDenied: true},
		{name: "static AK mode denied", owner: "key-a", namespace: "team-a", raw: `[{"pvName":"pv","mountPath":"/data"}]`, static: true, authType: "ak", wantDenied: true},
		{name: "claim inherited mounts still require init runtime", owner: "key-a", namespace: "team-a", inherited: `[{"pvName":"pv","mountPath":"/data"}]`, static: true, wantRuntimeError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantLimit {
				withCSIMountCountLimit(t, 1)
			}
			pv := &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "pv"},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: driver, VolumeHandle: "volume", VolumeAttributes: map[string]string{
						"authType": "agent-identity", "credentialProviderName": "provider", "bucket": "shared-bucket", "path": "/root",
					}}},
				},
			}
			if tt.static {
				pv.Spec.CSI.VolumeAttributes["authType"] = tt.authType
				pv.Spec.CSI.NodePublishSecretRef = &corev1.SecretReference{Name: "storage", Namespace: ptr.Deref(tt.secretNamespace, utils.DefaultSandboxDeployNamespace)}
			}
			cache, fc, err := cachetest.NewTestCache(t, pv)
			require.NoError(t, err)
			var reads, forbiddenReads int
			c := interceptor.NewClient(fc.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				reads++
				switch obj.(type) {
				case *corev1.Secret, *corev1.PersistentVolumeClaim:
					forbiddenReads++
					return assert.AnError
				}
				return c.Get(ctx, key, obj, opts...)
			}})
			source := &v1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: tt.namespace, Annotations: map[string]string{resourceaccess.StorageAuthAnnotation: "forged", v1alpha1.AnnotationCSIVolumeConfig: tt.inherited}}}
			if !tt.static {
				source.Annotations[identity.AnnotationAgentName] = "agent"
			}
			sbx := AsSandbox(source, &apiReaderOverrideCache{Provider: cache, client: c})
			sbx.storageRegistry.RegisterProvider(driver, &storages.MountProvider{})
			input := &config.CSIMountOptions{MountOptionListRaw: tt.raw}
			if tt.resolved {
				input.MountOptionList = make([]config.MountConfig, 3)
			}
			got, err := prepareIsolatedMounts(t.Context(), sbx, tt.owner, input, !tt.wantRuntimeError)
			assert.Zero(t, forbiddenReads, "isolated preparation must only read PVs")
			assert.Equal(t, "forged", source.Annotations[resourceaccess.StorageAuthAnnotation], "source copy must remain untouched")
			if tt.wantDenied || tt.wantLimit || tt.wantRuntimeError {
				require.Error(t, err)
				assert.Nil(t, got)
				if tt.wantLimit || tt.wantRuntimeError {
					assert.Equal(t, managererrors.ErrorBadRequest, managererrors.GetErrCode(err))
					assert.Zero(t, reads)
					if tt.wantLimit {
						assert.ErrorContains(t, err, "at most 1 CSI mounts")
					} else {
						assert.ErrorContains(t, err, "init runtime is required")
					}
				} else {
					assert.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
				}
				return
			}
			require.NoError(t, err)
			require.NotNil(t, got.MountOptionList, "empty snapshot must not trigger a later PV re-read")
			assert.Len(t, got.MountOptionList, tt.wantMounts)
			if tt.wantMounts == 0 {
				assert.NotContains(t, sbx.Annotations, resourceaccess.StorageAuthAnnotation)
			} else {
				assert.Empty(t, got.MountOptionList[0].PublishRequest.Secrets)
				wantAuth := `[{"credentialProviderName":"provider","attributes":{"bucket-name":"shared-bucket","sub-path":"root"}}]`
				if tt.name == "actual selected namespace and owner" {
					wantAuth = `[{"credentialProviderName":"provider","attributes":{"bucket-name":"shared-bucket","sub-path":"root/child"}}]`
				}
				assert.JSONEq(t, wantAuth, sbx.Annotations[resourceaccess.StorageAuthAnnotation])
			}
			if !tt.resolved {
				assert.Nil(t, input.MountOptionList, "attempt-local options must not mutate captured request options")
			}
		})
	}
}

type preflightIdentityProvider struct {
	*mockIdentityProvider
	ready bool
}

func (p *preflightIdentityProvider) IdentityProviderConfigured() bool { return p.ready }

type preflightMountProvider struct {
	storages.MountProvider
	invalidPublishContext bool
	mounts                []config.MountConfig
}

func (p *preflightMountProvider) GenerateCSINodePublishVolumeRequest(ctx context.Context, target string, pv *corev1.PersistentVolume, readOnly bool, secret *corev1.Secret) (*csi.NodePublishVolumeRequest, error) {
	publish, err := p.MountProvider.GenerateCSINodePublishVolumeRequest(ctx, target, pv, readOnly, secret)
	if err != nil {
		return nil, err
	}
	if p.invalidPublishContext && pv.Name == "pv-second" {
		publish.PublishContext = map[string]string{"credential": "private-storage-material\xff\xfe"}
	}
	p.mounts = append(p.mounts, config.MountConfig{Driver: pv.Spec.CSI.Driver, PublishRequest: publish})
	return publish, nil
}

func TestIsolatedDeliveryIdentityPreflight(t *testing.T) {
	for _, operation := range []string{"claim", "clone"} {
		for _, tt := range []struct {
			name               string
			agentAuth          bool
			requestID          bool
			provider           string
			noMounts           bool
			badPublishContext  bool
			cloneWithoutInit   bool
			issuerDenied       bool
			storageUnavailable bool
			otherCustomer      bool
			driver             string
			raw                string
			wantAuth           string
			wantError          string
			wantCode           managererrors.ErrorCode
		}{
			{name: "storage auth needs agent identity", agentAuth: true, provider: "ready", wantError: "isolated storage authentication requires an agent identity", wantCode: managererrors.ErrorBadRequest},
			{name: "placeholder cannot issue ID", agentAuth: true, requestID: true, provider: "placeholder", wantError: "identity provider is not configured for isolated sandbox", wantCode: managererrors.ErrorInternal},
			{name: "unready provider cannot issue ID", agentAuth: true, requestID: true, provider: "unready", wantError: "identity provider is not configured for isolated sandbox", wantCode: managererrors.ErrorInternal},
			{name: "ID without mounts still needs provider", requestID: true, noMounts: true, wantError: "identity provider is not configured for isolated sandbox", wantCode: managererrors.ErrorInternal},
			{name: "static credentials denied before IO", requestID: true, provider: "ready", wantError: resourceaccess.ErrAccessDenied.Error()},
			{name: "binary publish context on second mount rejects whole batch", agentAuth: true, requestID: true, provider: "ready", badPublishContext: true, wantError: "invalid CSI mount request at index 1", wantCode: managererrors.ErrorBadRequest},
			{name: "storage provider unavailable before IO", agentAuth: true, requestID: true, provider: "ready", storageUnavailable: true, wantError: resourceaccess.ErrAccessDenied.Error()},
			{name: "malformed dynamic scope before IO", agentAuth: true, requestID: true, provider: "ready", raw: `[{"pvName":"pv","mountPath":"/data","attributes":{"bucket":"wildcard*"}}]`, wantError: resourceaccess.ErrAccessDenied.Error()},
			{name: "configured identity succeeds", agentAuth: true, requestID: true, provider: "ready"},
			{name: "shared PV succeeds for another key and namespace", agentAuth: true, requestID: true, provider: "ready", otherCustomer: true},
			{name: "dynamic OSS provider bucket region and KMS", agentAuth: true, requestID: true, provider: "ready",
				raw:      `[{"pvName":"pv","mountPath":"/data","subPath":"child","attributes":{"credentialProviderName":"customer-provider","bucket":"customer-bucket","region":"cn-hangzhou","kmsKeyId":"customer-kms"}}]`,
				wantAuth: `[{"credentialProviderName":"customer-provider","attributes":{"bucket-name":"customer-bucket","region":"cn-hangzhou","kms-key-id":"customer-kms","sub-path":"root/child"}}]`},
			{name: "dynamic bucket space and canonical provider", agentAuth: true, requestID: true, provider: "ready",
				raw:      `[{"pvName":"pv","mountPath":"/data","attributes":{"sandboxCredProviderName":"space-provider","bucket":"","bucketSpace":"customer-space"}}]`,
				wantAuth: `[{"credentialProviderName":"space-provider","attributes":{"bucket-space-name":"customer-space","sub-path":"root"}}]`},
			{name: "dynamic bucket space prefix", agentAuth: true, requestID: true, provider: "ready",
				raw:      `[{"pvName":"pv","mountPath":"/data","attributes":{"credentialProviderName":"prefix-provider","bucket":"","bucketSpacePrefix":"customer-prefix"}}]`,
				wantAuth: `[{"credentialProviderName":"prefix-provider","attributes":{"bucket-space-prefix":"customer-prefix","sub-path":"root"}}]`},
			{name: "dynamic NAS server", agentAuth: true, requestID: true, provider: "ready", driver: "nasplugin.csi.alibabacloud.com",
				raw:      `[{"pvName":"pv","mountPath":"/data","attributes":{"credentialProviderName":"nas-provider","server":"ap-customer.filesystem.cn-hangzhou.nas.aliyuncs.com"}}]`,
				wantAuth: `[{"credentialProviderName":"nas-provider","attributes":{"nas-ap-id":"ap-customer"}}]`},
			{name: "IdP denies final storage scopes", agentAuth: true, requestID: true, provider: "ready", issuerDenied: true},
			{name: "agent identity CSI succeeds without saved init", agentAuth: true, requestID: true, provider: "ready", cloneWithoutInit: true},
		} {
			t.Run(operation+"/"+tt.name, func(t *testing.T) {
				driver := tt.driver
				if driver == "" {
					driver = "ossplugin.csi.alibabacloud.com"
				}
				wantAuth := tt.wantAuth
				if wantAuth == "" {
					wantAuth = `[{"credentialProviderName":"provider","attributes":{"bucket-name":"shared-default","sub-path":"root/child"}}]`
				}
				var spy struct {
					quota, releases, creates, updates, patches, secrets, pvcs, pvs, issues, propagations int
					runtimeCalls, initCalls, mounts                                                      atomic.Int32
				}
				namespace, owner := "team-a", "key-a"
				if tt.otherCustomer {
					namespace, owner = "team-b", "key-b"
				}
				mountProvider := &preflightMountProvider{invalidPublishContext: tt.badPublishContext}
				var persisted *v1alpha1.Sandbox
				provider := &preflightIdentityProvider{ready: tt.provider == "ready", mockIdentityProvider: &mockIdentityProvider{
					issueTokenFunc: func(ctx context.Context, sbx *v1alpha1.Sandbox) (*identity.TokenResponse, error) {
						spy.issues++
						require.NotNil(t, persisted, "issuance follows Sandbox persistence, not preflight")
						assert.NotEmpty(t, sbx.UID)
						assert.Equal(t, persisted.UID, sbx.UID)
						assert.Equal(t, namespace, sbx.Namespace)
						assert.Equal(t, owner, sbx.Annotations[v1alpha1.AnnotationOwner])
						assert.JSONEq(t, wantAuth, sbx.Annotations[resourceaccess.StorageAuthAnnotation])
						assert.Equal(t, persisted.Annotations[resourceaccess.StorageAuthAnnotation], sbx.Annotations[resourceaccess.StorageAuthAnnotation], "persisted and issued scope must match exactly")
						require.Len(t, mountProvider.mounts, 1)
						assert.Equal(t, "/data", mountProvider.mounts[0].PublishRequest.TargetPath)
						assert.True(t, mountProvider.mounts[0].PublishRequest.Readonly, "PV read-only mode survives preparation")
						assert.Empty(t, mountProvider.mounts[0].PublishRequest.Secrets)
						_, finalAuth, err := csiutils.BuildIsolatedStorageAuthAnnotation(ctx, mountProvider.mounts)
						require.NoError(t, err)
						assert.JSONEq(t, finalAuth, sbx.Annotations[resourceaccess.StorageAuthAnnotation], "issued scopes match the exact executable mount snapshot")
						assert.Zero(t, spy.mounts.Load(), "no mount before the IdP grant")
						if tt.issuerDenied {
							return nil, resourceaccess.ErrAccessDenied
						}
						return &identity.TokenResponse{AccessToken: "test-id-token"}, nil
					},
					propagateFunc: func(context.Context, *v1alpha1.Sandbox, *identity.TokenResponse) error {
						spy.propagations++
						return nil
					},
				}}
				switch tt.provider {
				case "ready", "unready":
					identity.RegisterProvider(provider)
				case "placeholder":
					identity.RegisterProvider(identity.NewDefaultIdentityProvider())
				default:
					identity.RegisterProvider(nil)
				}
				t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })
				runtimeServer := testutils.NewTestRuntimeServer(testutils.TestRuntimeServerOptions{
					RunCommandImmediately: true, RunCommandResult: runtime.RunCommandResult{PID: 1, Exited: true},
				})
				t.Cleanup(runtimeServer.Close)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					spy.runtimeCalls.Add(1)
					if r.URL.Path == "/init" {
						spy.initCalls.Add(1)
					}
					if r.URL.Path == processconnect.ProcessStartProcedure {
						spy.mounts.Add(1)
					}
					runtimeServer.Config.Handler.ServeHTTP(w, r)
				}))
				t.Cleanup(server.Close)
				pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv"}, Spec: corev1.PersistentVolumeSpec{
					AccessModes:            []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany},
					PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: driver, VolumeHandle: "volume", VolumeAttributes: map[string]string{"bucket": "shared-default", "path": "/root"}}},
				}}
				if tt.agentAuth {
					pv.Spec.CSI.VolumeAttributes["authType"] = "agent-identity"
					pv.Spec.CSI.VolumeAttributes["credentialProviderName"] = "provider"
				} else {
					pv.Spec.CSI.NodePublishSecretRef = &corev1.SecretReference{Name: "storage", Namespace: utils.DefaultSandboxDeployNamespace}
				}
				raw := tt.raw
				if raw == "" {
					raw = `[{"pvName":"pv","mountPath":"/data","subPath":"child"}]`
				}
				if tt.noMounts {
					raw = "[]"
				}
				cp := &v1alpha1.Checkpoint{ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: namespace, Annotations: map[string]string{v1alpha1.AnnotationOwner: owner, v1alpha1.AnnotationCSIVolumeConfig: raw}}, Status: v1alpha1.CheckpointStatus{CheckpointId: "snapshot"}}
				if !tt.cloneWithoutInit {
					initRaw, err := json.Marshal(config.InitRuntimeOptions{AccessToken: runtime.AccessToken})
					require.NoError(t, err)
					cp.Annotations[v1alpha1.AnnotationInitRuntimeRequest] = string(initRaw)
				}
				tmpl := &v1alpha1.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: namespace}, Spec: v1alpha1.SandboxTemplateSpec{Template: &corev1.PodTemplateSpec{}}}
				cache, fc, err := cachetest.NewTestCache(t, pv, cp, tmpl)
				require.NoError(t, err)
				if tt.badPublishContext {
					secondPV := pv.DeepCopy()
					secondPV.Name = "pv-second"
					secondPV.ResourceVersion = ""
					require.NoError(t, fc.Create(t.Context(), secondPV))
					raw = `[{"pvName":"pv","mountPath":"/data"},{"pvName":"pv-second","mountPath":"/second"}]`
				}
				if operation == "claim" {
					createAvailableSandboxForFailureRecord(t, fc, "pool", func(s *v1alpha1.Sandbox) { s.Namespace = namespace; s.UID = "pooled-uid" })
				}
				var sourceBefore client.Object
				if tt.badPublishContext {
					if operation == "claim" {
						sourceBefore = &v1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "test-sbx", Namespace: namespace}}
					} else {
						sourceBefore = cp.DeepCopy()
					}
					require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(sourceBefore), sourceBefore))
				}
				c := interceptor.NewClient(fc.(client.WithWatch), interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						switch obj.(type) {
						case *corev1.PersistentVolume:
							spy.pvs++
						case *corev1.Secret:
							spy.secrets++
							return assert.AnError
						case *corev1.PersistentVolumeClaim:
							spy.pvcs++
							return assert.AnError
						}
						return c.Get(ctx, key, obj, opts...)
					},
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						spy.creates++
						if sbx, ok := obj.(*v1alpha1.Sandbox); ok {
							assert.JSONEq(t, wantAuth, sbx.Annotations[resourceaccess.StorageAuthAnnotation], "scope must be present before Create")
							assert.JSONEq(t, raw, sbx.Annotations[v1alpha1.AnnotationCSIVolumeConfig], "raw intent must be present before Create")
							assert.Zero(t, spy.issues)
							sbx.UID = "created-uid"
						}
						err := c.Create(ctx, obj, opts...)
						if sbx, ok := obj.(*v1alpha1.Sandbox); ok && err == nil {
							persisted = sbx.DeepCopy()
						}
						return err
					},
					Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						spy.updates++
						if sbx, ok := obj.(*v1alpha1.Sandbox); ok {
							assert.JSONEq(t, wantAuth, sbx.Annotations[resourceaccess.StorageAuthAnnotation], "scope must be present before Update")
							assert.JSONEq(t, raw, sbx.Annotations[v1alpha1.AnnotationCSIVolumeConfig], "raw intent must be present before Update")
						}
						err := c.Update(ctx, obj, opts...)
						if sbx, ok := obj.(*v1alpha1.Sandbox); ok && err == nil {
							persisted = sbx.DeepCopy()
						}
						return err
					},
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						spy.patches++
						return c.Patch(ctx, obj, patch, opts...)
					},
				})
				wrappedCache := &apiReaderOverrideCache{Provider: cache, client: c}
				oldCreate := DefaultCreateSandbox
				DefaultCreateSandbox = func(ctx context.Context, sbx *v1alpha1.Sandbox, c client.Client) (*v1alpha1.Sandbox, error) {
					created, err := oldCreate(ctx, sbx, c)
					if err == nil {
						markSandboxReadyForTest(t, ctx, c, created, "1.2.3.4")
					}
					return created, err
				}
				t.Cleanup(func() { DefaultCreateSandbox = oldCreate })
				modifier := func(s infra.Sandbox) error {
					if tt.storageUnavailable {
						s.(*Sandbox).storageRegistry.RegisterProvider(driver, nil)
					} else {
						s.(*Sandbox).storageRegistry.RegisterProvider(driver, mountProvider)
					}
					annotations := s.GetAnnotations()
					if annotations == nil {
						annotations = make(map[string]string)
						s.SetAnnotations(annotations)
					}
					annotations[v1alpha1.AnnotationRuntimeURL] = server.URL
					annotations[v1alpha1.AnnotationRuntimeAccessToken] = runtime.AccessToken
					annotations[resourceaccess.StorageAuthAnnotation] = "forged"
					if tt.requestID {
						annotations[identity.AnnotationAgentName] = "agent"
					}
					return nil
				}
				admission := &infra.SandboxAdmission{
					Acquire: func(context.Context, string, infra.SandboxResource) error { spy.quota++; return nil },
					Release: func(context.Context, string) error { spy.releases++; return nil },
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				var delivered infra.Sandbox
				mountOptions := &config.CSIMountOptions{MountOptionListRaw: raw}
				mountOptionsBefore := *mountOptions
				if operation == "claim" {
					opts, optsErr := ValidateAndInitClaimOptions(infra.ClaimSandboxOptions{
						ResourceAccessMode: resourceaccess.Isolated, User: owner, Namespace: namespace, Template: "pool", Modifier: modifier, Admission: admission,
						InitRuntime: &config.InitRuntimeOptions{AccessToken: runtime.AccessToken}, CSIMount: mountOptions,
					})
					require.NoError(t, optsErr)
					i := &Infra{Cache: wrappedCache, claimLockChannel: make(chan struct{}, 1)}
					var metrics infra.ClaimMetrics
					delivered, metrics, err = i.ClaimSandbox(ctx, opts)
					assert.Zero(t, metrics.Retries)
				} else {
					var explicitMounts *config.CSIMountOptions
					if tt.badPublishContext {
						explicitMounts = mountOptions
					}
					i := &Infra{Cache: wrappedCache}
					var metrics infra.CloneMetrics
					delivered, metrics, err = i.CloneSandbox(ctx, infra.CloneSandboxOptions{
						ResourceAccessMode: resourceaccess.Isolated, User: owner, Namespace: namespace, CheckPointID: "snapshot", SkipWaitCheckpoint: true,
						Name: "cloned", WaitReadyTimeout: time.Second, Modifier: modifier, Admission: admission, CSIMount: explicitMounts,
						CheckpointAdmission: func(sourceOwner string, _ resourceaccess.Mode) error {
							if sourceOwner != owner {
								return resourceaccess.ErrAccessDenied
							}
							return nil
						},
					})
					assert.Zero(t, metrics.Retries)
				}
				assert.Equal(t, mountOptionsBefore, *mountOptions, "delivery must not mutate caller mount options")
				if tt.issuerDenied {
					require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
					assert.Nil(t, delivered, "an IdP denial must not yield a successful delivery")
					assert.Equal(t, 1, spy.issues)
					assert.Zero(t, spy.propagations)
					assert.Zero(t, spy.mounts.Load())
					assert.Equal(t, 1, spy.quota, "cloud authorization follows persistence, not preflight")
					assert.Equal(t, 1, spy.releases)
				} else if tt.wantError != "" {
					require.Error(t, err)
					if tt.wantError == resourceaccess.ErrAccessDenied.Error() {
						assert.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
					} else {
						assert.Equal(t, tt.wantCode, managererrors.GetErrCode(err))
						assert.Equal(t, managererrors.NewError(tt.wantCode, "%s", tt.wantError).Error(), err.Error())
					}
					assert.Nil(t, delivered)
					assert.Zero(t, spy.quota)
					assert.Zero(t, spy.releases)
					assert.Zero(t, spy.creates)
					assert.Zero(t, spy.updates)
					assert.Zero(t, spy.patches)
					assert.Zero(t, spy.runtimeCalls.Load())
					assert.Zero(t, spy.initCalls.Load())
					assert.Zero(t, spy.mounts.Load())
					assert.Zero(t, spy.issues)
					assert.Zero(t, spy.propagations)
					if tt.badPublishContext {
						assert.Len(t, mountProvider.mounts, 2, "the full executable batch is generated before encoding")
						assert.NotContains(t, err.Error(), "private-storage-material")
						sourceAfter := sourceBefore.DeepCopyObject().(client.Object)
						readErr := fc.Get(t.Context(), client.ObjectKeyFromObject(sourceBefore), sourceAfter)
						assert.NoError(t, readErr, "rejection must not consume the source")
						if readErr == nil {
							assert.Equal(t, sourceBefore, sourceAfter, "rejection must leave the source unchanged")
						}
					}
				} else {
					require.NoError(t, err)
					require.NotNil(t, delivered)
					assert.JSONEq(t, wantAuth, delivered.GetAnnotations()[resourceaccess.StorageAuthAnnotation])
					assert.Equal(t, 1, spy.quota)
					assert.Equal(t, int32(1), spy.mounts.Load())
					if operation == "clone" && tt.cloneWithoutInit {
						assert.Zero(t, spy.initCalls.Load(), "restored runtime remounts without a saved init request")
						assert.NotContains(t, delivered.GetAnnotations(), v1alpha1.AnnotationInitRuntimeRequest)
					} else {
						assert.Equal(t, int32(1), spy.initCalls.Load())
					}
					if tt.requestID {
						assert.Equal(t, 1, spy.issues)
						assert.Equal(t, 1, spy.propagations)
					}
				}
				assert.Zero(t, spy.secrets)
				assert.Zero(t, spy.pvcs)
				if tt.wantError == "" {
					assert.Equal(t, 1, spy.pvs, "issuance must reuse the resolved mounts without rereading PVs")
				}
			})
		}
	}
}

func TestIsolatedClaimDenialLeavesCandidateAvailable(t *testing.T) {
	i, fc := NewTestInfra(t)
	createAvailableSandboxForFailureRecord(t, fc, "pool", func(sbx *v1alpha1.Sandbox) { sbx.Name = "pooled"; sbx.Namespace = "customer" })
	admitted := 0
	opts, err := ValidateAndInitClaimOptions(infra.ClaimSandboxOptions{
		ResourceAccessMode: resourceaccess.Isolated, User: "key-a", Template: "pool", InitRuntime: &config.InitRuntimeOptions{},
		CSIMount:  &config.CSIMountOptions{MountOptionListRaw: `[{"pvName":"foreign","mountPath":"/data"}]`},
		Admission: &infra.SandboxAdmission{Acquire: func(context.Context, string, infra.SandboxResource) error { admitted++; return nil }},
	})
	require.NoError(t, err)
	got, _, err := TryClaimSandbox(t.Context(), opts, &i.pickCache, i.Cache, i.claimLockChannel, i.createLimiter)
	require.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
	assert.Nil(t, got)
	assert.Zero(t, admitted)
	_, picked := i.pickCache.Load("customer/pooled")
	assert.False(t, picked)
	source := &v1alpha1.Sandbox{}
	require.NoError(t, fc.Get(t.Context(), client.ObjectKey{Namespace: "customer", Name: "pooled"}, source))
	assert.Empty(t, source.Annotations[v1alpha1.AnnotationLock])
	assert.NotContains(t, source.Annotations, resourceaccess.AnnotationPolicy)

	opts.CSIMount = &config.CSIMountOptions{MountOptionListRaw: "[]"}
	opts.InitRuntime = nil
	got, _, err = TryClaimSandbox(t.Context(), opts, &i.pickCache, i.Cache, i.claimLockChannel, i.createLimiter)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "customer", got.GetNamespace())
	assert.Equal(t, "isolated-v1", got.GetAnnotations()[resourceaccess.AnnotationPolicy])
	assert.Equal(t, "key-a", got.GetAnnotations()[v1alpha1.AnnotationOwner])
	assert.Equal(t, 1, admitted)
}

func TestCloneResourceAccessRestoration(t *testing.T) {
	for _, tt := range []struct {
		name       string
		mode       resourceaccess.Mode
		raw        string
		wantCreate bool
		wantLimit  bool
	}{
		{name: "isolated inherited denied", mode: resourceaccess.Isolated},
		{name: "isolated explicit empty clears inherited", mode: resourceaccess.Isolated, raw: "[]", wantCreate: true},
		{name: "isolated raw limit before create", mode: resourceaccess.Isolated, raw: `[{"pvName":"a"},{"pvName":"b"}]`, wantLimit: true},
		{name: "trusted clone gets new policy", raw: "[]", wantCreate: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			withCSIMountCountLimit(t, 1)
			cp := &v1alpha1.Checkpoint{ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "customer", Annotations: map[string]string{
				v1alpha1.AnnotationOwner: "key-a", resourceaccess.AnnotationPolicy: "isolated-v1", resourceaccess.StorageAuthAnnotation: "inherited",
				v1alpha1.AnnotationCSIVolumeConfig: `[{"pvName":"foreign","mountPath":"/data"}]`, v1alpha1.AnnotationInitRuntimeRequest: `{}`,
			}}, Status: v1alpha1.CheckpointStatus{CheckpointId: "snapshot"}}
			tmpl := &v1alpha1.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "customer"}, Spec: v1alpha1.SandboxTemplateSpec{Template: &corev1.PodTemplateSpec{}}}
			cache, _, err := cachetest.NewTestCache(t, cp, tmpl)
			require.NoError(t, err)
			var created *v1alpha1.Sandbox
			oldCreate := DefaultCreateSandbox
			DefaultCreateSandbox = func(_ context.Context, sbx *v1alpha1.Sandbox, _ client.Client) (*v1alpha1.Sandbox, error) {
				created = sbx.DeepCopy()
				return nil, apierrors.NewBadRequest("stop after admission")
			}
			t.Cleanup(func() { DefaultCreateSandbox = oldCreate })
			opts := infra.CloneSandboxOptions{ResourceAccessMode: tt.mode, User: "key-a", CheckPointID: "snapshot", SkipWaitCheckpoint: true,
				CheckpointAdmission: func(owner string, sourceMode resourceaccess.Mode) error {
					assert.Equal(t, resourceaccess.Isolated, sourceMode)
					if owner != "key-a" {
						return resourceaccess.ErrAccessDenied
					}
					return nil
				},
				Modifier: func(s infra.Sandbox) error {
					s.SetAnnotations(map[string]string{resourceaccess.AnnotationPolicy: "forged", resourceaccess.StorageAuthAnnotation: "explicit"})
					return nil
				},
			}
			if tt.raw != "" {
				opts.CSIMount = &config.CSIMountOptions{MountOptionListRaw: tt.raw}
			}
			_, _, err = CloneSandbox(t.Context(), opts, cache)
			require.Error(t, err)
			if !tt.wantCreate {
				assert.Nil(t, created)
				if tt.wantLimit {
					assert.Equal(t, managererrors.ErrorBadRequest, managererrors.GetErrCode(err))
				} else {
					assert.ErrorIs(t, err, resourceaccess.ErrAccessDenied)
				}
				return
			}
			require.NotNil(t, created)
			mode, err := resourceaccess.FromAnnotations(created.Annotations)
			require.NoError(t, err)
			want, err := resourceaccess.Normalize(tt.mode)
			require.NoError(t, err)
			assert.Equal(t, want, mode)
			assert.Equal(t, "[]", created.Annotations[v1alpha1.AnnotationCSIVolumeConfig])
			if tt.mode == resourceaccess.Isolated {
				assert.NotContains(t, created.Annotations, resourceaccess.StorageAuthAnnotation)
			} else {
				assert.Equal(t, "inherited", created.Annotations[resourceaccess.StorageAuthAnnotation])
			}
			assert.Equal(t, "isolated-v1", cp.Annotations[resourceaccess.AnnotationPolicy])
		})
	}
}

func TestResourceAccessPolicyPropagationAndValidation(t *testing.T) {
	for _, mode := range []resourceaccess.Mode{"", resourceaccess.TrustedDomain, resourceaccess.Isolated, "invalid"} {
		t.Run(string(mode), func(t *testing.T) {
			claim, claimErr := ValidateAndInitClaimOptions(infra.ClaimSandboxOptions{ResourceAccessMode: mode, User: "key", Template: "pool"})
			clone, cloneErr := ValidateAndInitCloneOptions(infra.CloneSandboxOptions{ResourceAccessMode: mode, User: "key", CheckPointID: "cp"})
			if mode == "invalid" {
				require.Error(t, claimErr)
				require.Error(t, cloneErr)
				return
			}
			require.NoError(t, claimErr)
			require.NoError(t, cloneErr)
			want, err := resourceaccess.Normalize(mode)
			require.NoError(t, err)
			assert.Equal(t, want, claim.ResourceAccessMode)
			assert.Equal(t, want, clone.ResourceAccessMode)
			sbx := AsSandbox(&v1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{resourceaccess.AnnotationPolicy: "forged"}}}, nil)
			require.NoError(t, modifyPickedSandbox(sbx, infra.LockTypeCreate, claim))
			cp := &v1alpha1.Checkpoint{}
			PropagateAnnotationsToCheckpoint(sbx.Sandbox, cp)
			assert.Equal(t, string(want)+"-v1", cp.Annotations[resourceaccess.AnnotationPolicy])
		})
	}
	for _, policy := range []string{"", "unknown-v9"} {
		sbx := &v1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{resourceaccess.AnnotationPolicy: policy}}}
		cp := &v1alpha1.Checkpoint{}
		PropagateAnnotationsToCheckpoint(sbx, cp)
		_, err := resourceaccess.FromAnnotations(cp.Annotations)
		require.Error(t, err)
	}
	for _, wrap := range []func(error) error{buildCloneError, func(err error) error { return buildClaimError(err, err, nil) }} {
		assert.True(t, errors.Is(wrap(resourceaccess.ErrAccessDenied), resourceaccess.ErrAccessDenied))
	}
}
