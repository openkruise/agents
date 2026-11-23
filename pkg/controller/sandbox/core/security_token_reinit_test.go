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

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/resourceaccess"
	"github.com/openkruise/agents/pkg/utils"
	utilruntime "github.com/openkruise/agents/pkg/utils/runtime"
)

// fakeReinitProvider is a test IdentityProvider that records issue/propagate
// calls and returns canned responses / errors so the security-token
// reinitialization logic can be exercised without a real provider backend.
type fakeReinitProvider struct {
	issueCalls     int
	propagateCalls int
	gotPropSbx     *agentsv1alpha1.Sandbox
	resp           *identity.TokenResponse
	issueErr       error
	propagateErr   error
	onIssue        func(*agentsv1alpha1.Sandbox)
	onPropagate    func()
}

func (f *fakeReinitProvider) IssueToken(_ context.Context, sbx *agentsv1alpha1.Sandbox, _ identity.TokenOptions) (*identity.TokenResponse, error) {
	f.issueCalls++
	if f.onIssue != nil {
		f.onIssue(sbx)
	}
	if f.issueErr != nil {
		return nil, f.issueErr
	}
	return f.resp, nil
}

func (f *fakeReinitProvider) PropagateSecurityToken(_ context.Context, sbx *agentsv1alpha1.Sandbox, _ *identity.TokenResponse,
	_ ...utilruntime.Option) error {
	f.propagateCalls++
	if f.onPropagate != nil {
		f.onPropagate()
	}
	f.gotPropSbx = sbx
	return f.propagateErr
}

func newReinitScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(sch))
	require.NoError(t, agentsv1alpha1.AddToScheme(sch))
	return sch
}

func TestInitializeIsolatedPreflightOrdering(t *testing.T) {
	for _, tt := range []struct {
		name, policy                                                     string
		wantErr, malformed, deleting, invalidProvider                    bool
		missingIdentity, missingProvider, noMounts, nilAPIReader, static bool
		badSecond, invalidWire                                           bool
		secretNamespace                                                  string
	}{
		{name: "shared PV requested snapshot used through token and mount", policy: "isolated-v1"},
		{name: "malformed PV rejected before init", policy: "isolated-v1", malformed: true, wantErr: true},
		{name: "deleting PV rejected before init", policy: "isolated-v1", deleting: true, wantErr: true},
		{name: "invalid credential provider rejected before init", policy: "isolated-v1", invalidProvider: true, wantErr: true},
		{name: "malformed second PV rejects whole batch before init", policy: "isolated-v1", badSecond: true, wantErr: true},
		{name: "invalid second wire request rejects whole batch before init", policy: "isolated-v1", invalidWire: true, wantErr: true},
		{name: "unknown policy rejected before init", policy: "future-v1", wantErr: true},
		{name: "empty policy rejected before init", wantErr: true},
		{name: "identity scope without agent name rejected before init", policy: "isolated-v1", missingIdentity: true, wantErr: true},
		{name: "placeholder provider rejected before init", policy: "isolated-v1", missingProvider: true, wantErr: true},
		{name: "identity without mounts still requires real provider", policy: "isolated-v1", missingProvider: true, noMounts: true, wantErr: true},
		{name: "isolated initialization does not need APIReader", policy: "isolated-v1", nilAPIReader: true},
		{name: "static system Secret rejected before init", policy: "isolated-v1", static: true, secretNamespace: utils.DefaultSandboxDeployNamespace, wantErr: true},
		{name: "static empty Secret namespace rejected before init", policy: "isolated-v1", static: true, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stage atomic.Int32
			var reads atomic.Int32
			advance := func(from, to int32) {
				assert.True(t, stage.CompareAndSwap(from, to), "unexpected initialization order at %d", to)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/init":
					advance(0, 1)
					w.WriteHeader(http.StatusNoContent)
				case "/v1/storage/mounts":
					advance(4, 5)
					var req utilruntime.CreateMountRequest
					if assert.NoError(t, json.NewDecoder(r.Body).Decode(&req)) && assert.NotNil(t, req.PublishRequest) {
						assert.Empty(t, req.PublishRequest.Secrets)
						assert.Equal(t, "requested-bucket", req.PublishRequest.VolumeContext["bucket"])
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"success":true}`))
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			fp := &fakeReinitProvider{resp: &identity.TokenResponse{AccessToken: "token", AccessTokenExpiration: "2099-01-01T00:00:00Z"},
				onIssue: func(sbx *agentsv1alpha1.Sandbox) {
					advance(1, 2)
					assert.JSONEq(t, `[{"credentialProviderName":"provider","attributes":{"bucket-name":"requested-bucket"}}]`, sbx.Annotations[resourceaccess.StorageAuthAnnotation])
					assert.Equal(t, "owner", sbx.Annotations[agentsv1alpha1.AnnotationOwner])
					assert.Equal(t, "team-a", sbx.Namespace)
					assert.EqualValues(t, "current-uid", sbx.UID)
					assert.Equal(t, "192.0.2.42", sbx.Status.PodInfo.PodIP, "issuance uses the new runtime status")
					assert.Equal(t, "current-runtime-token", sbx.Annotations[agentsv1alpha1.AnnotationRuntimeAccessToken], "issuance must not use a pre-init metadata copy")
				}, onPropagate: func() { advance(2, 3) },
			}
			identity.RegisterProvider(fp)
			if tt.missingProvider {
				identity.RegisterProvider(identity.NewDefaultIdentityProvider())
			}
			t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })
			box := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "team-a", UID: "current-uid", Annotations: map[string]string{
				resourceaccess.AnnotationPolicy: tt.policy, agentsv1alpha1.AnnotationOwner: "owner", identity.AnnotationAgentName: "agent",
				agentsv1alpha1.AnnotationInitRuntimeRequest: `{}`, agentsv1alpha1.AnnotationRuntimeURL: server.URL,
				agentsv1alpha1.AnnotationCSIVolumeConfig: `[{"pvName":"volume","mountPath":"/mnt/data","attributes":{"bucket":"requested-bucket"}}]`,
			}}}
			box.Status.PodInfo.PodIP = "192.0.2.1"
			if tt.missingIdentity {
				delete(box.Annotations, identity.AnnotationAgentName)
			}
			if tt.noMounts {
				delete(box.Annotations, agentsv1alpha1.AnnotationCSIVolumeConfig)
			}
			pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "volume"}, Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
					Driver: "ossplugin.csi.alibabacloud.com", VolumeHandle: "volume",
					NodePublishSecretRef: &corev1.SecretReference{Namespace: tt.secretNamespace, Name: "missing"},
					VolumeAttributes:     map[string]string{"authType": "agent-identity", "sandboxCredProviderName": "provider", "bucket": "default-bucket"},
				}},
			}}
			if tt.malformed {
				pv.Spec.CSI.Driver = ""
			}
			if tt.deleting {
				now := metav1.Now()
				pv.DeletionTimestamp = &now
				pv.Finalizers = []string{"test/finalizer"}
			}
			if tt.invalidProvider {
				pv.Spec.CSI.VolumeAttributes["sandboxCredProviderName"] = "*"
			}
			if tt.static {
				delete(pv.Spec.CSI.VolumeAttributes, "authType")
			}
			objects := []client.Object{box, pv}
			if tt.badSecond || tt.invalidWire {
				second := pv.DeepCopy()
				second.Name = "second"
				if tt.badSecond {
					second.Spec.CSI = nil
				}
				objects = append(objects, second)
				box.Annotations[agentsv1alpha1.AnnotationCSIVolumeConfig] = `[{"pvName":"volume","mountPath":"/mnt/first"},{"pvName":"second","mountPath":"/mnt/second"}]`
			}
			c := fakeclient.NewClientBuilder().WithScheme(newReinitScheme(t)).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					assert.Zero(t, stage.Load(), "PV reads must precede runtime init")
					require.IsType(t, &corev1.PersistentVolume{}, obj, "isolated initialization must never read PVCs or Secrets")
					reads.Add(1)
					err := c.Get(ctx, key, obj, opts...)
					if err == nil && tt.invalidWire && key.Name == "second" {
						// Inject after the fake client's JSON conversion normalizes strings.
						obj.(*corev1.PersistentVolume).Spec.CSI.FSType = "\xff"
					}
					return err
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					advance(3, 4)
					return c.Patch(ctx, obj, patch, opts...)
				},
			}).Build()
			registry := storages.NewStorageProvider()
			registry.RegisterProvider(pv.Spec.CSI.Driver, &storages.MountProvider{})
			var apiReader client.Reader = c
			if tt.nilAPIReader {
				apiReader = nil
			}
			before := box.DeepCopy()
			newStatus := box.Status.DeepCopy()
			newStatus.PodInfo.PodIP = "192.0.2.42"
			err := Initialize(t.Context(), box, newStatus, c, apiReader, registry, utilruntime.WithRetry(wait.Backoff{Steps: 1}),
				utilruntime.WithRefresh(func(context.Context) (*agentsv1alpha1.Sandbox, error) {
					box.Annotations[agentsv1alpha1.AnnotationRuntimeAccessToken] = "current-runtime-token"
					return nil, nil
				}))
			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, before, box)
				assert.Zero(t, stage.Load())
				assert.Zero(t, fp.issueCalls)
				assert.Zero(t, fp.propagateCalls)
				if tt.badSecond || tt.invalidWire {
					assert.Equal(t, int32(2), reads.Load(), "validate the full batch before effects")
				}
				return
			}
			require.NoError(t, err)
			before.Annotations[agentsv1alpha1.AnnotationRuntimeAccessToken] = "current-runtime-token"
			before.Annotations[identity.AgentKeyTokenRefreshStatus] = `{"accessTokenExpiration":"2099-01-01T00:00:00Z"}`
			assert.Equal(t, before, box, "only init's metadata change and the successfully recorded expiration may change")
			assert.Equal(t, int32(5), stage.Load())
			assert.Equal(t, int32(1), reads.Load(), "token issuance must not re-resolve the snapshot")
		})
	}
}

func TestIsolatedInitializerMissingClientCannotSucceed(t *testing.T) {
	for _, withAPIReader := range []bool{false, true} {
		t.Run(fmt.Sprintf("withAPIReader=%t", withAPIReader), func(t *testing.T) {
			box := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "team-a", Annotations: map[string]string{
				resourceaccess.AnnotationPolicy: "isolated-v1", agentsv1alpha1.AnnotationOwner: "owner",
			}}}
			initializer := &defaultSandboxInitializer{recorder: record.NewFakeRecorder(2), storageRegistry: storages.NewStorageProvider()}
			if withAPIReader {
				initializer.apiReader = fakeclient.NewClientBuilder().Build()
			}
			status := box.Status.DeepCopy()
			require.ErrorContains(t, initializer.Initialize(t.Context(), box, status), "requires a Kubernetes client")
			condition := utils.GetSandboxCondition(status, string(agentsv1alpha1.RuntimeInitialized))
			require.NotNil(t, condition)
			assert.Equal(t, metav1.ConditionFalse, condition.Status)
		})
	}
}

func TestReinitSecurityToken(t *testing.T) {
	const expiration = "2030-01-01T00:00:00Z"

	tests := []struct {
		name             string
		agentName        string // sets the agent-name opt-in annotation when non-empty
		issueErr         error
		propagateErr     error
		expectError      string
		expectIssueCalls int
		expectPropCalls  int
		expectAnnotation bool // whether the token-status annotation should be persisted
	}{
		{
			name:      "identity provider not requested - no-op",
			agentName: "",
		},
		{
			name:             "issue and propagate success - annotation recorded",
			agentName:        "my-agent",
			expectIssueCalls: 1,
			expectPropCalls:  1,
			expectAnnotation: true,
		},
		{
			name:             "issue failure - no propagation and no annotation",
			agentName:        "my-agent",
			issueErr:         fmt.Errorf("issuer down"),
			expectError:      "failed to issue security token",
			expectIssueCalls: 1,
		},
		{
			name:             "propagate failure - annotation not recorded",
			agentName:        "my-agent",
			propagateErr:     fmt.Errorf("runtime unreachable"),
			expectError:      "failed to propagate security token",
			expectIssueCalls: 1,
			expectPropCalls:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fp := &fakeReinitProvider{
				resp: &identity.TokenResponse{
					AccessToken:           "tok",
					AccessTokenExpiration: expiration,
				},
				issueErr:     tt.issueErr,
				propagateErr: tt.propagateErr,
			}
			identity.RegisterProvider(fp)
			t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })

			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx",
					Namespace: "default",
				},
			}
			if tt.agentName != "" {
				box.Annotations = map[string]string{identity.AnnotationAgentName: tt.agentName}
			}
			sbxForInit := &agentsv1alpha1.Sandbox{ObjectMeta: box.ObjectMeta}

			sch := newReinitScheme(t)
			c := fakeclient.NewClientBuilder().WithScheme(sch).WithObjects(box.DeepCopy()).Build()

			err := reinitSecurityToken(context.Background(), c, sbxForInit)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.expectIssueCalls, fp.issueCalls, "unexpected IssueToken call count")
			assert.Equal(t, tt.expectPropCalls, fp.propagateCalls, "unexpected PropagateSecurityToken call count")

			got := &agentsv1alpha1.Sandbox{}
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(box), got))
			raw, present := got.Annotations[identity.AgentKeyTokenRefreshStatus]
			assert.Equal(t, tt.expectAnnotation, present, "unexpected token-status annotation presence")
			if tt.expectAnnotation {
				assert.Contains(t, raw, expiration, "token-status annotation should carry the new expiration")
			}
		})
	}
}

func TestInitializeInvokesSecurityTokenReinit(t *testing.T) {
	const expiration = "2030-01-01T00:00:00Z"

	tests := []struct {
		name        string
		issueErr    error
		expectError string
		expectIssue int
	}{
		{
			name:        "reinit success is propagated as nil",
			expectIssue: 1,
		},
		{
			name:        "reinit error aborts initialization",
			issueErr:    fmt.Errorf("issuer down"),
			expectError: "failed to issue security token",
			expectIssue: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fp := &fakeReinitProvider{
				resp: &identity.TokenResponse{
					AccessToken:           "tok",
					AccessTokenExpiration: expiration,
				},
				issueErr: tt.issueErr,
			}
			identity.RegisterProvider(fp)
			t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })

			sch := newReinitScheme(t)
			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "sbx",
					Namespace:   "default",
					Annotations: map[string]string{identity.AnnotationAgentName: "my-agent"},
				},
			}
			c := fakeclient.NewClientBuilder().WithScheme(sch).WithObjects(box.DeepCopy()).Build()

			err := Initialize(context.Background(), box, &agentsv1alpha1.SandboxStatus{}, c, c, storages.NewStorageProvider())

			assert.Equal(t, tt.expectIssue, fp.issueCalls, "expected Initialize to trigger security-token reissue")
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
