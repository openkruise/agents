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

package e2b

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/resourceaccess"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/utils/csiutils"
)

func resourceAccessController(t *testing.T, mode resourceaccess.Mode) *Controller {
	t.Helper()
	cache, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	opts := config.InitOptions(config.SandboxManagerOptions{ResourceAccessMode: mode, SystemNamespace: "sandbox-system"})
	m, err := sandboxmanager.NewSandboxManagerBuilder(opts).WithCustomInfra(func() (infra.Builder, error) {
		return sandboxcr.NewInfraBuilder(opts).WithCache(cache).WithAPIReader(fc).WithRouteReader(proxy.NewServer(opts)), nil
	}).Build()
	require.NoError(t, err)
	return &Controller{manager: m, cache: cache, mgrOpts: opts, maxTimeout: models.DefaultMaxTimeout, keyCfg: &keys.Config{}, storageRegistry: storages.NewStorageProvider()}
}

func TestIsolatedResourceAccessAPI(t *testing.T) {
	sc := resourceAccessController(t, resourceaccess.Isolated)
	for _, field := range []string{"volumeMounts", "metadata"} {
		t.Run("explicit empty "+field, func(t *testing.T) {
			body := map[string]any{"templateID": "snapshot"}
			if field == "volumeMounts" {
				body[field] = []any{}
			} else {
				body[field] = map[string]string{models.ExtensionKeyClaimWithCSIMount_MountConfig: "[]"}
			}
			request, apiErr := sc.parseCreateSandboxRequest(NewRequest(t, nil, body, nil, adminTestUser()))
			require.Nil(t, apiErr)
			opts, err := sc.buildCSIMountOptions(t.Context(), request)
			require.NoError(t, err)
			require.NotNil(t, opts)
			assert.Equal(t, "[]", opts.MountOptionListRaw)
		})
	}
	for _, requests := range [][]v1alpha1.CSIMountConfig{
		nil, {},
		{{PvName: "does-not-exist", MountPath: "/data", Attributes: map[string]string{"bucket": "request"}}},
		{{PvName: "shared-oss", MountPath: "/data", SubPath: "customer", Attributes: map[string]string{"credentialProviderName": "customer-provider", "bucket": "customer-bucket", "bucketSpace": "customer-space", "kmsKeyId": "customer-key", "region": "cn-hangzhou"}}},
		{{PvName: "shared-nas", MountPath: "/data", Attributes: map[string]string{"sandboxCredProviderName": "customer-provider", "server": "ap-customer.filesystem.cn-hangzhou.nas.aliyuncs.com"}}},
	} {
		t.Run(stringMustJSON(t, requests), func(t *testing.T) {
			opts, err := sc.buildCSIMountOptions(t.Context(), models.NewSandboxRequest{Extensions: models.NewSandboxRequestExtension{CSIMount: models.CSIMountExtension{MountConfigs: requests}}})
			require.NoError(t, err, "API must serialize intent without any eager PV lookup")
			if requests == nil {
				assert.Nil(t, opts)
				return
			}
			require.NotNil(t, opts)
			assert.Nil(t, opts.MountOptionList)
			assert.JSONEq(t, stringMustJSON(t, requests), opts.MountOptionListRaw)
		})
	}
	for _, mode := range []resourceaccess.Mode{resourceaccess.TrustedDomain, resourceaccess.Isolated} {
		controller := resourceAccessController(t, mode)
		for _, key := range []string{resourceaccess.AnnotationPolicy, resourceaccess.StorageAuthAnnotation} {
			t.Run(string(mode)+key, func(t *testing.T) {
				r := NewRequest(t, nil, models.NewSandboxRequest{TemplateID: "pool", Metadata: map[string]string{key: "forged"}}, nil, adminTestUser())
				_, apiErr := controller.parseCreateSandboxRequest(r)
				if key == resourceaccess.StorageAuthAnnotation && mode == resourceaccess.TrustedDomain {
					assert.Nil(t, apiErr, "legacy public metadata behavior is unchanged")
				} else {
					require.NotNil(t, apiErr)
					assert.Equal(t, http.StatusBadRequest, apiErr.Code)
				}
			})
		}
	}
}

func stringMustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return string(raw)
}

func TestIsolatedCloneAPIAdmission(t *testing.T) {
	for _, tt := range []struct {
		name            string
		mode            resourceaccess.Mode
		sourcePolicy    string
		missingTemplate bool
		admin           bool
		sameOwner       bool
		inherited       bool
		staticMount     bool
		wantCreate      bool
	}{
		{name: "same team other key is denied", mode: resourceaccess.Isolated},
		{name: "own checkpoint inherited missing PV is denied", mode: resourceaccess.Isolated, sameOwner: true, inherited: true},
		{name: "shared static PV denied before create", mode: resourceaccess.Isolated, sameOwner: true, staticMount: true},
		{name: "inherited shared static PV denied before create", mode: resourceaccess.Isolated, sameOwner: true, inherited: true, staticMount: true},
		{name: "admin uses selected customer namespace", mode: resourceaccess.Isolated, admin: true, sameOwner: true, wantCreate: true},
		{name: "denial precedes missing paired template", mode: resourceaccess.Isolated, missingTemplate: true, inherited: true},
		{name: "trusted admission cannot unprotect isolated source", mode: resourceaccess.TrustedDomain, sourcePolicy: "isolated-v1", missingTemplate: true, inherited: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sc := resourceAccessController(t, tt.mode)
			provider := &e2bTrafficTokenProvider{}
			identity.RegisterProvider(provider)
			t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })
			user := &models.CreatedTeamAPIKey{ID: uuid.New(), Team: &models.Team{Name: "customer"}}
			if tt.admin {
				user = adminTestUser()
				assert.Empty(t, sc.getNamespaceOfUser(user))
			}
			owner := uuid.NewString()
			if tt.sameOwner {
				owner = user.ID.String()
			}
			cp := &v1alpha1.Checkpoint{ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "customer", Annotations: map[string]string{v1alpha1.AnnotationOwner: owner, v1alpha1.AnnotationInitRuntimeRequest: "{}"}}, Status: v1alpha1.CheckpointStatus{CheckpointId: "snapshot"}}
			if tt.sourcePolicy != "" {
				cp.Annotations[resourceaccess.AnnotationPolicy] = tt.sourcePolicy
			}
			if tt.inherited {
				cp.Annotations[v1alpha1.AnnotationCSIVolumeConfig] = `[{"pvName":"foreign","mountPath":"/data"}]`
			}
			if tt.staticMount {
				cp.Annotations[identity.AnnotationAgentName] = "agent"
				require.NoError(t, sc.cache.GetClient().Create(t.Context(), &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: "foreign"},
					Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
						Driver: "ossplugin.csi.alibabacloud.com", VolumeHandle: "shared-volume",
						VolumeAttributes:     map[string]string{"bucket": "shared-bucket"},
						NodePublishSecretRef: &corev1.SecretReference{Name: "storage", Namespace: "sandbox-system"},
					}}},
				}))
			}
			require.NoError(t, sc.cache.GetClient().Create(t.Context(), cp))
			cp.Status.CheckpointId = "snapshot"
			require.NoError(t, sc.cache.GetClient().Status().Update(t.Context(), cp))
			if !tt.missingTemplate {
				require.NoError(t, sc.cache.GetClient().Create(t.Context(), &v1alpha1.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "customer"}, Spec: v1alpha1.SandboxTemplateSpec{Template: &corev1.PodTemplateSpec{}}}))
			}
			calls := 0
			oldHook := csiutils.BuildStorageAuthAnnotation
			csiutils.BuildStorageAuthAnnotation = func(context.Context, client.Client, []v1alpha1.CSIMountConfig) (string, string, error) {
				calls++
				return "", "", assert.AnError
			}
			t.Cleanup(func() { csiutils.BuildStorageAuthAnnotation = oldHook })
			var created *v1alpha1.Sandbox
			oldCreate := sandboxcr.DefaultCreateSandbox
			sandboxcr.DefaultCreateSandbox = func(_ context.Context, sbx *v1alpha1.Sandbox, _ client.Client) (*v1alpha1.Sandbox, error) {
				created = sbx.DeepCopy()
				return nil, apierrors.NewBadRequest("stop before write")
			}
			t.Cleanup(func() { sandboxcr.DefaultCreateSandbox = oldCreate })
			request := models.NewSandboxRequest{TemplateID: "snapshot", Timeout: 600}
			if !tt.inherited {
				request.Extensions.CSIMount.MountConfigs = []v1alpha1.CSIMountConfig{{PvName: "foreign", MountPath: "/data"}}
				if tt.wantCreate {
					request.Extensions.CSIMount.MountConfigs = []v1alpha1.CSIMountConfig{}
				}
			}
			_, apiErr := sc.createSandboxWithClone(t.Context(), request, user, "")
			require.NotNil(t, apiErr)
			assert.Zero(t, calls, "isolated API skips legacy hooks")
			assert.Zero(t, provider.calls.Load(), "preflight rejection precedes token issuance")
			if !tt.wantCreate {
				assert.Equal(t, http.StatusNotFound, apiErr.Code)
				assert.Equal(t, "Resource not found", apiErr.Message)
				assert.Nil(t, created)
				return
			}
			require.NotNil(t, created)
			assert.Equal(t, "customer", created.Namespace)
			assert.Equal(t, user.ID.String(), created.Annotations[v1alpha1.AnnotationOwner])
			assert.Equal(t, "isolated-v1", created.Annotations[resourceaccess.AnnotationPolicy])
			assert.Equal(t, "[]", created.Annotations[v1alpha1.AnnotationCSIVolumeConfig])
		})
	}
}

func TestResourceAccessStartupValidation(t *testing.T) {
	for _, tt := range []struct {
		mode resourceaccess.Mode
		want string
	}{
		{mode: resourceaccess.Isolated, want: "requires E2B authentication"},
		{mode: "invalid", want: "unsupported resource access mode"},
	} {
		t.Run(string(tt.mode), func(t *testing.T) {
			sc := NewController(ControllerOptions{Manager: config.SandboxManagerOptions{ResourceAccessMode: tt.mode}})
			require.ErrorContains(t, sc.Init(t.Context()), tt.want)
		})
	}
}
