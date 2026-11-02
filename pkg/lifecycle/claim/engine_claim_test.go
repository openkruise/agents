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

package claim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/autopause"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/utils/runtime"
	runtimeconfig "github.com/openkruise/agents/pkg/utils/runtime/config"
	utestutils "github.com/openkruise/agents/pkg/utils/testutils"
)

func GetSbsOwnerReference() []metav1.OwnerReference {
	sbs := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-sandboxset",
			UID:  "12345",
		},
	}
	return []metav1.OwnerReference{*metav1.NewControllerRef(sbs, v1alpha1.SandboxSetControllerKind)}
}

func CreateSandboxWithStatus(t *testing.T, c client.Client, sbx *v1alpha1.Sandbox) {
	t.Helper()
	require.NoError(t, c.Create(t.Context(), sbx))
	require.NoError(t, c.Status().Update(t.Context(), sbx))
}

// apiReaderOverrideCache wraps a cache Provider and overrides GetAPIReader and
// GetClient so a test can intercept specific reads (for example node capability
// lookups). It mirrors the sandboxcr helper of the same name.
type apiReaderOverrideCache struct {
	infracache.Provider
	apiReader client.Reader
	client    client.Client
}

func (c *apiReaderOverrideCache) GetAPIReader() client.Reader {
	if c.apiReader == nil {
		return c.Provider.GetAPIReader()
	}
	return c.apiReader
}

func (c *apiReaderOverrideCache) GetClient() client.Client {
	if c.client == nil {
		return c.Provider.GetClient()
	}
	return c.client
}

// claimTestInfra is the neutral stand-in for the sandboxcr Infra these engine
// tests used to build. It carries only what the engine needs: an informer-backed
// cache Provider and a pick cache.
type claimTestInfra struct {
	Cache     infracache.Provider
	pickCache sync.Map
	stop      func(context.Context)
}

func (i *claimTestInfra) Stop(ctx context.Context) {
	if i.stop != nil {
		i.stop(ctx)
	}
}

func newClaimTestInfra(t *testing.T) (*claimTestInfra, client.Client) {
	t.Helper()
	c, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context()))
	t.Cleanup(func() { c.Stop(context.Background()) })
	return &claimTestInfra{Cache: c, stop: c.Stop}, fc
}

func TestClearFailedSandboxIgnoresNilSandbox(t *testing.T) {
	// The old reserveFailedSandbox rejected a non-*Sandbox interface value with
	// "unsupported sandbox type". The neutral engine takes a concrete *Sandbox,
	// so that guard now lives in ClearFailedSandbox: a nil sandbox must return
	// early instead of being dereferenced by the reserve/delete paths.
	require.NotPanics(t, func() {
		ClearFailedSandbox(t.Context(), nil, errors.New("claim failed"), nil, nil, "")
	})
}

func TestClaimCreationOutcomes(t *testing.T) {
	tests := []struct {
		name                                                                string
		lockType                                                            LockType
		ready, failed, canceled, successor, refreshFailure, deliveryFailure bool
		wantReady                                                           *bool
		wantErr                                                             bool
		creationTime, readyTime                                             time.Time
		wantDuration                                                        *time.Duration
	}{
		{name: "cold start Ready", lockType: LockTypeCreate, ready: true, wantReady: ptr.To(true)},
		{name: "trusted time to first Ready is not the waiting latency", lockType: LockTypeCreate, ready: true, wantReady: ptr.To(true), creationTime: time.Unix(100, 0), readyTime: time.Unix(102, 0), wantDuration: ptr.To(2 * time.Second)},
		{name: "a genuine zero duration still keeps the sample", lockType: LockTypeCreate, ready: true, wantReady: ptr.To(true), creationTime: time.Unix(100, 0), readyTime: time.Unix(100, 0), wantDuration: ptr.To(time.Duration(0))},
		{name: "no duration when the creation time is missing", lockType: LockTypeCreate, ready: true, wantReady: ptr.To(true), readyTime: time.Unix(102, 0)},
		{name: "no duration when the Ready time is missing", lockType: LockTypeCreate, ready: true, wantReady: ptr.To(true), creationTime: time.Unix(100, 0)},
		{name: "no duration when the timestamps are out of order", lockType: LockTypeCreate, ready: true, wantReady: ptr.To(true), creationTime: time.Unix(100, 0), readyTime: time.Unix(99, 0)},
		{name: "delivery failure after Ready keeps the trusted creation duration", lockType: LockTypeCreate, ready: true, deliveryFailure: true, wantReady: ptr.To(true), wantErr: true, creationTime: time.Unix(100, 0), readyTime: time.Unix(102, 0), wantDuration: ptr.To(2 * time.Second)},
		{name: "cold start Failed", lockType: LockTypeCreate, failed: true, wantReady: ptr.To(false), wantErr: true},
		{name: "unknown timeout does not imply failure", lockType: LockTypeCreate, wantErr: true},
		{name: "unknown cancellation does not imply failure", lockType: LockTypeCreate, canceled: true, wantErr: true},
		{name: "refresh failure after Ready preserves success", lockType: LockTypeCreate, ready: true, refreshFailure: true, wantReady: ptr.To(true), wantErr: true},
		{name: "delivery failure after Ready preserves success", lockType: LockTypeCreate, ready: true, deliveryFailure: true, wantReady: ptr.To(true), wantErr: true},
		{name: "claiming a still-creating pool instance adds no creation outcome", lockType: LockTypeSpeculate, ready: true},
		{name: "pool hit adds no creation outcome", lockType: LockTypeUpdate, ready: true},
		{name: "same-name successor Ready is not attributed to the original instance", lockType: LockTypeCreate, ready: true, successor: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "creation-outcome", UID: types.UID(uuid.NewString()), Annotations: map[string]string{}},
				Status:     v1alpha1.SandboxStatus{Phase: v1alpha1.SandboxPending},
			}
			if tt.ready {
				sbx.Status = v1alpha1.SandboxStatus{Phase: v1alpha1.SandboxRunning, PodInfo: v1alpha1.PodInfo{PodIP: "10.0.0.1"}, Conditions: []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}}
			}
			if tt.failed {
				sbx.Status = v1alpha1.SandboxStatus{Phase: v1alpha1.SandboxFailed, Conditions: []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: v1alpha1.SandboxReadyReasonStartContainerFailed}}}
			}
			sbx.CreationTimestamp = metav1.NewTime(tt.creationTime)
			if tt.ready {
				sbx.Status.Conditions[0].LastTransitionTime = metav1.NewTime(tt.readyTime)
			}
			if tt.deliveryFailure {
				sbx.Annotations[v1alpha1.AnnotationRuntimeTLSPort] = "49984"
			}
			initial := sbx.DeepCopy()
			if tt.successor {
				sbx.UID = types.UID(uuid.NewString())
				initial.Status = v1alpha1.SandboxStatus{Phase: v1alpha1.SandboxPending}
			}
			cache, _, err := cachetest.NewTestCache(t)
			require.NoError(t, err)
			if !tt.refreshFailure {
				require.NoError(t, cache.GetClient().Create(t.Context(), sbx))
				require.NoError(t, cache.GetClient().Status().Update(t.Context(), sbx))
			}
			ctx := t.Context()
			if tt.canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			wrapped := &Sandbox{Sandbox: initial, Cache: cache}
			metrics := Metrics{}
			err = runClaimPostProcesses(ctx, wrapped, tt.lockType, Options{WaitReadyTimeout: 20 * time.Millisecond}, cache, &metrics)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tt.wantReady == nil {
				assert.Empty(t, metrics.CreationResults)
			} else {
				assert.Equal(t, []SandboxCreationResult{{UID: string(initial.UID), Namespace: initial.Namespace, Ready: *tt.wantReady, Duration: tt.wantDuration}}, metrics.CreationResults)
			}
		})
	}
}

func TestModifyPickedSandboxUserMetadata(t *testing.T) {
	for _, tt := range []struct {
		name                string
		explicit, malformed bool
	}{
		{name: "the explicit list is preserved and the caller is left unmodified", explicit: true},
		{name: "the legacy JSON list is preserved"},
		{name: "a malformed legacy JSON list reports an error", malformed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			keys := &v1alpha1.UpdatedMetadataInClaim{Annotations: []string{"user.example/key"}}
			raw, err := json.Marshal(keys)
			require.NoError(t, err)
			if tt.malformed {
				raw = []byte("{")
			}
			sbx := &Sandbox{Sandbox: &v1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{v1alpha1.AnnotationUpdatedMetadataInClaim: string(raw)}}}}
			opts := Options{}
			if tt.explicit {
				opts.UserMetadataKeys = keys
			}
			err = modifyPickedSandbox(sbx, LockTypeUpdate, opts)
			if tt.malformed {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			filtered := v1alpha1.UpdatedMetadataInClaim{}
			require.NoError(t, json.Unmarshal([]byte(sbx.Annotations[v1alpha1.AnnotationUpdatedMetadataInClaim]), &filtered))
			assert.Equal(t, []string{"user.example/key"}, filtered.Annotations)
			assert.Equal(t, []string{"user.example/key"}, keys.Annotations)
		})
	}
}

func TestSandboxReadyFailureMessage(t *testing.T) {
	tests := []struct {
		name string
		sbx  *v1alpha1.Sandbox
		want string
	}{
		{
			name: "controller has not observed latest generation",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "sbx-1",
					Namespace:  "default",
					Generation: 4,
				},
				Status: v1alpha1.SandboxStatus{
					Phase:              v1alpha1.SandboxRunning,
					ObservedGeneration: 3,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
							Reason: v1alpha1.SandboxReadyReasonPodReady,
						},
					},
					PodInfo: v1alpha1.PodInfo{PodIP: "1.2.3.4"},
				},
			},
			want: "sandbox default/sbx-1 is not ready before wait timeout: reason=controller has not observed latest generation, state=running, ready=PodReady, generation=3/4",
		},
		{
			name: "inplace update is still in progress",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "sbx-1",
					Namespace:  "default",
					Generation: 1,
				},
				Status: v1alpha1.SandboxStatus{
					Phase:              v1alpha1.SandboxRunning,
					ObservedGeneration: 1,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
							Reason: v1alpha1.SandboxReadyReasonPodReady,
						},
						{
							Type:   string(v1alpha1.SandboxConditionInplaceUpdate),
							Reason: v1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
						},
					},
					PodInfo: v1alpha1.PodInfo{PodIP: "1.2.3.4"},
				},
			},
			want: "sandbox default/sbx-1 is not ready before wait timeout: reason=inplace update is still in progress, state=running, ready=PodReady, inplaceUpdate=InplaceUpdating",
		},
		{
			name: "sandbox has no pod ip",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "sbx-1",
					Namespace:  "default",
					Generation: 1,
				},
				Status: v1alpha1.SandboxStatus{
					Phase:              v1alpha1.SandboxRunning,
					ObservedGeneration: 1,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
							Reason: v1alpha1.SandboxReadyReasonPodReady,
						},
					},
				},
			},
			want: "sandbox default/sbx-1 is not ready before wait timeout: reason=sandbox has no pod IP, state=running, ready=PodReady",
		},
		{
			name: "ready condition reports failure with message",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "sbx-1",
					Namespace:  "default",
					Generation: 1,
				},
				Status: v1alpha1.SandboxStatus{
					Phase:              v1alpha1.SandboxRunning,
					ObservedGeneration: 1,
					Conditions: []metav1.Condition{
						{
							Type:    string(v1alpha1.SandboxConditionReady),
							Reason:  v1alpha1.SandboxReadyReasonStartContainerFailed,
							Message: "process exited",
						},
					},
					PodInfo: v1alpha1.PodInfo{PodIP: "1.2.3.4"},
				},
			},
			want: "sandbox default/sbx-1 is not ready before wait timeout: reason=ready condition reports StartContainerFailed: process exited, state=dead, ready=StartContainerFailed",
		},
		{
			name: "ready condition reports unschedulable with message",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "sbx-1",
					Namespace:  "default",
					Generation: 1,
				},
				Status: v1alpha1.SandboxStatus{
					Phase:              v1alpha1.SandboxRunning,
					ObservedGeneration: 1,
					Conditions: []metav1.Condition{
						{
							Type:    string(v1alpha1.SandboxConditionReady),
							Reason:  v1alpha1.SandboxReadyReasonUnschedulable,
							Message: "insufficient CPU",
						},
					},
					PodInfo: v1alpha1.PodInfo{PodIP: "1.2.3.4"},
				},
			},
			want: "sandbox default/sbx-1 is not ready before wait timeout: reason=ready condition reports Unschedulable: insufficient CPU, state=dead, ready=Unschedulable",
		},
		{
			name: "unschedulable reason reported without pod ip",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "sbx-1",
					Namespace:  "default",
					Generation: 1,
				},
				Status: v1alpha1.SandboxStatus{
					Phase:              v1alpha1.SandboxRunning,
					ObservedGeneration: 1,
					Conditions: []metav1.Condition{
						{
							Type:    string(v1alpha1.SandboxConditionReady),
							Reason:  v1alpha1.SandboxReadyReasonUnschedulable,
							Message: "insufficient CPU",
						},
					},
				},
			},
			want: "sandbox default/sbx-1 is not ready before wait timeout: reason=ready condition reports Unschedulable: insufficient CPU, state=dead, ready=Unschedulable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sandboxReadyFailureMessage(tt.sbx))
		})
	}
}

func TestModifyPickedSandboxResourceResize(t *testing.T) {
	base := &Sandbox{
		Sandbox: &v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sbx-1",
				Namespace: "default",
				Labels: map[string]string{
					v1alpha1.LabelSandboxTemplate:  "test-template",
					v1alpha1.LabelSandboxIsClaimed: "false",
				},
				Annotations: map[string]string{},
			},
			Spec: v1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "main",
									Image: "img",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("250m"),
											corev1.ResourceMemory: resource.MustParse("256Mi"),
										},
										Limits: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("250m"),
											corev1.ResourceMemory: resource.MustParse("256Mi"),
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	err := modifyPickedSandbox(base, LockTypeUpdate, Options{
		User:     "u1",
		Template: "test-template",
		InplaceUpdate: &InplaceUpdateOptions{
			Resources: &InplaceUpdateResourcesOptions{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(500), base.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
	assert.Equal(t, int64(500), base.Spec.Template.Spec.Containers[0].Resources.Limits.Cpu().MilliValue())
	assert.Equal(t, resource.MustParse("512Mi"), *base.Spec.Template.Spec.Containers[0].Resources.Requests.Memory())
	assert.Equal(t, resource.MustParse("1Gi"), *base.Spec.Template.Spec.Containers[0].Resources.Limits.Memory())
}

func TestModifyPickedSandboxCPUResizeCases(t *testing.T) {
	tests := []struct {
		name string

		templateSpec   corev1.PodSpec
		inplaceReq     corev1.ResourceList
		inplaceLim     corev1.ResourceList
		wantReqCPU     int64
		wantLimCPU     int64
		wantSidecarCPU int64
	}{
		{
			name: "requests only - set target",
			templateSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "img",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("100m"),
							},
						},
					},
				},
			},
			inplaceReq: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
			wantReqCPU: 200,
			wantLimCPU: 0,
		},
		{
			name: "limits only - set target",
			templateSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "img",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("500m"),
							},
						},
					},
				},
			},
			inplaceLim: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			wantReqCPU: 0,
			wantLimCPU: 1000,
		},
		{
			name: "no cpu resources - no change",
			templateSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "img",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
						},
					},
				},
			},
			inplaceReq: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
			wantReqCPU: 0,
			wantLimCPU: 0,
		},
		{
			name: "empty resources - no change",
			templateSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:      "main",
						Image:     "img",
						Resources: corev1.ResourceRequirements{},
					},
				},
			},
			inplaceReq: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
			wantReqCPU: 0,
			wantLimCPU: 0,
		},
		{
			name: "set lower target",
			templateSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "img",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("500m"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("500m"),
							},
						},
					},
				},
			},
			inplaceReq: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
			inplaceLim: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
			wantReqCPU: 250,
			wantLimCPU: 250,
		},
		{
			name: "only first container gets target - sidecar unchanged",
			templateSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "img",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("100m"),
							},
						},
					},
					{
						Name:  "sidecar",
						Image: "img",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("200m"),
							},
						},
					},
				},
			},
			inplaceReq:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("300m")},
			inplaceLim:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("300m")},
			wantReqCPU:     300,
			wantLimCPU:     0,
			wantSidecarCPU: 200,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &Sandbox{
				Sandbox: &v1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sbx-1",
						Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxTemplate:  "test-template",
							v1alpha1.LabelSandboxIsClaimed: "false",
						},
						Annotations: map[string]string{},
					},
					Spec: v1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: tt.templateSpec,
							},
						},
					},
				},
			}

			err := modifyPickedSandbox(sbx, LockTypeUpdate, Options{
				User:     "u1",
				Template: "test-template",
				InplaceUpdate: &InplaceUpdateOptions{
					Resources: &InplaceUpdateResourcesOptions{
						Requests: tt.inplaceReq,
						Limits:   tt.inplaceLim,
					},
				},
			})
			require.NoError(t, err)

			if tt.wantReqCPU > 0 {
				assert.Equal(t, tt.wantReqCPU, sbx.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
			}
			if tt.wantLimCPU > 0 {
				assert.Equal(t, tt.wantLimCPU, sbx.Spec.Template.Spec.Containers[0].Resources.Limits.Cpu().MilliValue())
			}
			if tt.wantSidecarCPU > 0 {
				assert.Equal(t, tt.wantSidecarCPU, sbx.Spec.Template.Spec.Containers[1].Resources.Limits.Cpu().MilliValue())
			}
		})
	}
}

func TestModifyPickedSandboxMemoryResizeCases(t *testing.T) {
	tests := []struct {
		name          string
		templateSpec  corev1.PodSpec
		inplaceReq    corev1.ResourceList
		inplaceLim    corev1.ResourceList
		wantReqMemory string
		wantLimMemory string
	}{
		{
			name: "memory requests and limits are resized",
			templateSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "img",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
						},
					},
				},
			},
			inplaceReq:    corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("384Mi")},
			inplaceLim:    corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("768Mi")},
			wantReqMemory: "384Mi",
			wantLimMemory: "768Mi",
		},
		{
			name: "memory target is ignored when original memory is unset",
			templateSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "img",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("100m"),
							},
						},
					},
				},
			},
			inplaceReq: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("384Mi")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &Sandbox{
				Sandbox: &v1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "sbx-1",
						Namespace:   "default",
						Labels:      map[string]string{},
						Annotations: map[string]string{},
					},
					Spec: v1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: tt.templateSpec,
							},
						},
					},
				},
			}

			err := modifyPickedSandbox(sbx, LockTypeUpdate, Options{
				User:     "u1",
				Template: "test-template",
				InplaceUpdate: &InplaceUpdateOptions{
					Resources: &InplaceUpdateResourcesOptions{
						Requests: tt.inplaceReq,
						Limits:   tt.inplaceLim,
					},
				},
			})
			require.NoError(t, err)

			container := sbx.Spec.Template.Spec.Containers[0]
			if tt.wantReqMemory == "" {
				_, ok := container.Resources.Requests[corev1.ResourceMemory]
				assert.False(t, ok)
			} else {
				assert.Equal(t, resource.MustParse(tt.wantReqMemory), container.Resources.Requests[corev1.ResourceMemory])
			}
			if tt.wantLimMemory == "" {
				_, ok := container.Resources.Limits[corev1.ResourceMemory]
				assert.False(t, ok)
			} else {
				assert.Equal(t, resource.MustParse(tt.wantLimMemory), container.Resources.Limits[corev1.ResourceMemory])
			}
		})
	}
}

func TestModifyPickedSandboxResizeNilTemplateRejected(t *testing.T) {
	sbx := &Sandbox{
		Sandbox: &v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "sbx-1",
				Namespace:   "default",
				Labels:      map[string]string{},
				Annotations: map[string]string{},
			},
			Spec: v1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
					Template: nil,
				},
			},
		},
	}

	// Resizing a sandbox without a pod template must fail instead of claiming
	// it unchanged.
	err := modifyPickedSandbox(sbx, LockTypeUpdate, Options{
		User:     "u1",
		Template: "test-template",
		InplaceUpdate: &InplaceUpdateOptions{
			Resources: &InplaceUpdateResourcesOptions{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no pod template")
	assert.Nil(t, sbx.Spec.Template)
}

func TestBuildContainerCPUTargets(t *testing.T) {
	tests := []struct {
		name       string
		podSpec    corev1.PodSpec
		wantNames  []string
		wantReqCPU map[string]int64
		wantLimCPU map[string]int64
	}{
		{
			name: "only requests",
			podSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "c1", Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					}},
				},
			},
			wantNames:  []string{"c1"},
			wantReqCPU: map[string]int64{"c1": 100},
			wantLimCPU: map[string]int64{},
		},
		{
			name: "only limits",
			podSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "c1", Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
					}},
				},
			},
			wantNames:  []string{"c1"},
			wantReqCPU: map[string]int64{},
			wantLimCPU: map[string]int64{"c1": 200},
		},
		{
			name: "no cpu resources",
			podSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "c1", Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
					}},
				},
			},
			wantNames:  []string{},
			wantReqCPU: map[string]int64{},
			wantLimCPU: map[string]int64{},
		},
		{
			name: "init container with cpu",
			podSpec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					{Name: "init", Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
						Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					}},
				},
				Containers: []corev1.Container{
					{Name: "main", Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
					}},
				},
			},
			wantNames:  []string{"init", "main"},
			wantReqCPU: map[string]int64{"init": 50, "main": 200},
			wantLimCPU: map[string]int64{"init": 100},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{Spec: tt.podSpec}
			targets := buildContainerCPUTargets(pod)
			if len(tt.wantNames) == 0 {
				assert.Empty(t, targets)
				return
			}
			for _, name := range tt.wantNames {
				target, ok := targets[name]
				require.True(t, ok, "expected container %s in targets", name)
				if req, ok := tt.wantReqCPU[name]; ok {
					assert.Equal(t, req, target.request.MilliValue())
				}
				if lim, ok := tt.wantLimCPU[name]; ok {
					assert.Equal(t, lim, target.limit.MilliValue())
				}
			}
		})
	}
}

func TestIsPodCPUResizeApplied(t *testing.T) {
	tests := []struct {
		name                  string
		targets               map[string]containerCPUTarget
		containerStatuses     []corev1.ContainerStatus
		initContainerStatuses []corev1.ContainerStatus
		want                  bool
	}{
		{
			name:    "empty targets",
			targets: map[string]containerCPUTarget{},
			want:    false,
		},
		{
			name:    "container status missing",
			targets: map[string]containerCPUTarget{"main": {request: resource.MustParse("100m"), hasRequest: true}},
			containerStatuses: []corev1.ContainerStatus{
				{Name: "sidecar", Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				}},
			},
			want: false,
		},
		{
			name:    "nil resources in status",
			targets: map[string]containerCPUTarget{"main": {request: resource.MustParse("100m"), hasRequest: true}},
			containerStatuses: []corev1.ContainerStatus{
				{Name: "main", Resources: nil},
			},
			want: false,
		},
		{
			name:    "request not matching",
			targets: map[string]containerCPUTarget{"main": {request: resource.MustParse("200m"), hasRequest: true}},
			containerStatuses: []corev1.ContainerStatus{
				{Name: "main", Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				}},
			},
			want: false,
		},
		{
			name:    "limit not matching",
			targets: map[string]containerCPUTarget{"main": {limit: resource.MustParse("500m"), hasLimit: true}},
			containerStatuses: []corev1.ContainerStatus{
				{Name: "main", Resources: &corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("400m")},
				}},
			},
			want: false,
		},
		{
			name:    "applied",
			targets: map[string]containerCPUTarget{"main": {request: resource.MustParse("200m"), limit: resource.MustParse("400m"), hasRequest: true, hasLimit: true}},
			containerStatuses: []corev1.ContainerStatus{
				{Name: "main", Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("400m")},
				}},
			},
			want: true,
		},
		{
			name:    "init container applied",
			targets: map[string]containerCPUTarget{"init": {request: resource.MustParse("50m"), hasRequest: true}},
			containerStatuses: []corev1.ContainerStatus{
				{Name: "main", Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
				}},
			},
			initContainerStatuses: []corev1.ContainerStatus{
				{Name: "init", Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
				}},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses:     tt.containerStatuses,
					InitContainerStatuses: tt.initContainerStatuses,
				},
			}
			got := isPodCPUResizeApplied(pod, tt.targets)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBuildResourceResizedPod(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			QOSClass: corev1.PodQOSGuaranteed,
		},
	}

	targetCPU := resource.MustParse("500m")
	targetMemory := resource.MustParse("512Mi")
	requests := corev1.ResourceList{
		corev1.ResourceCPU:    targetCPU,
		corev1.ResourceMemory: targetMemory,
	}
	limits := corev1.ResourceList{
		corev1.ResourceCPU:    targetCPU,
		corev1.ResourceMemory: targetMemory,
	}
	got, changed, err := buildResourceResizedPod(pod, requests, limits)
	require.NoError(t, err)
	require.True(t, changed)
	assert.Equal(t, int64(500), got.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
	assert.Equal(t, int64(500), got.Spec.Containers[0].Resources.Limits.Cpu().MilliValue())
	assert.Equal(t, targetMemory, got.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory])
	assert.Equal(t, targetMemory, got.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory])
}

func TestBuildResourceResizedPod_MemoryDownscaleRejected(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "main",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
				},
			}},
		},
	}

	tests := []struct {
		name        string
		requests    corev1.ResourceList
		limits      corev1.ResourceList
		expectError string
	}{
		{
			name:        "lower memory request rejected",
			requests:    corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
			expectError: "in-place memory downscale is not supported",
		},
		{
			name:        "lower memory limit rejected",
			limits:      corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
			expectError: "in-place memory downscale is not supported",
		},
		{
			name:     "equal memory accepted",
			requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
			limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
		},
		{
			name:     "memory upscale accepted",
			requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
			limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
		},
		{
			name:     "cpu downscale still allowed",
			requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
			limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := buildResourceResizedPod(pod, tt.requests, tt.limits)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				// Downscale rejection is a client input error and must carry
				// ErrorBadRequest so the E2B API maps it to HTTP 400.
				assert.Equal(t, CodeBadRequest, GetErrCode(err))
				return
			}
			require.NoError(t, err)
		})
	}
}

// --- Pod resize state helpers (test-only, used by TestWaitForPodResizeState and related tests) ---

type containerCPUTarget struct {
	request    resource.Quantity
	hasRequest bool
	limit      resource.Quantity
	hasLimit   bool
}

func buildContainerCPUTargets(pod *corev1.Pod) map[string]containerCPUTarget {
	targets := make(map[string]containerCPUTarget, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
	for _, c := range pod.Spec.Containers {
		target := containerCPUTarget{}
		if c.Resources.Requests != nil {
			cpuReq, ok := c.Resources.Requests[corev1.ResourceCPU]
			if ok {
				target.request = cpuReq
				target.hasRequest = true
			}
		}
		if c.Resources.Limits != nil {
			cpuLim, ok := c.Resources.Limits[corev1.ResourceCPU]
			if ok {
				target.limit = cpuLim
				target.hasLimit = true
			}
		}
		if target.hasRequest || target.hasLimit {
			targets[c.Name] = target
		}
	}
	for _, c := range pod.Spec.InitContainers {
		target := containerCPUTarget{}
		if c.Resources.Requests != nil {
			cpuReq, ok := c.Resources.Requests[corev1.ResourceCPU]
			if ok {
				target.request = cpuReq
				target.hasRequest = true
			}
		}
		if c.Resources.Limits != nil {
			cpuLim, ok := c.Resources.Limits[corev1.ResourceCPU]
			if ok {
				target.limit = cpuLim
				target.hasLimit = true
			}
		}
		if target.hasRequest || target.hasLimit {
			targets[c.Name] = target
		}
	}
	return targets
}

func isPodCPUResizeApplied(pod *corev1.Pod, targets map[string]containerCPUTarget) bool {
	if len(targets) == 0 {
		return false
	}
	statuses := make(map[string]*corev1.ContainerStatus, len(pod.Status.ContainerStatuses)+len(pod.Status.InitContainerStatuses))
	for i := range pod.Status.ContainerStatuses {
		statuses[pod.Status.ContainerStatuses[i].Name] = &pod.Status.ContainerStatuses[i]
	}
	for i := range pod.Status.InitContainerStatuses {
		statuses[pod.Status.InitContainerStatuses[i].Name] = &pod.Status.InitContainerStatuses[i]
	}
	for name, target := range targets {
		status, ok := statuses[name]
		if !ok || status.Resources == nil {
			return false
		}
		if target.hasRequest {
			actualReq, ok := status.Resources.Requests[corev1.ResourceCPU]
			if !ok || actualReq.Cmp(target.request) != 0 {
				return false
			}
		}
		if target.hasLimit {
			actualLim, ok := status.Resources.Limits[corev1.ResourceCPU]
			if !ok || actualLim.Cmp(target.limit) != 0 {
				return false
			}
		}
	}
	return true
}

type podResizeStateSnapshot struct {
	pendingTrue       bool
	pendingReason     string
	pendingMessage    string
	inProgressTrue    bool
	inProgressReason  string
	inProgressMessage string
	resizeStatus      corev1.PodResizeStatus
	resizeApplied     bool
}

func getPodCondition(pod *corev1.Pod, condType corev1.PodConditionType) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		cond := &pod.Status.Conditions[i]
		if cond.Type == condType {
			return cond
		}
	}
	return nil
}

func inspectPodResizeState(pod *corev1.Pod, targets map[string]containerCPUTarget) podResizeStateSnapshot {
	pending := getPodCondition(pod, corev1.PodResizePending)
	inProgress := getPodCondition(pod, corev1.PodResizeInProgress)

	state := podResizeStateSnapshot{
		resizeStatus:  pod.Status.Resize,
		resizeApplied: isPodCPUResizeApplied(pod, targets),
	}
	if pending != nil && pending.Status == corev1.ConditionTrue {
		state.pendingTrue = true
		state.pendingReason = pending.Reason
		state.pendingMessage = pending.Message
	}
	if inProgress != nil && inProgress.Status == corev1.ConditionTrue {
		state.inProgressTrue = true
		state.inProgressReason = inProgress.Reason
		state.inProgressMessage = inProgress.Message
	}
	return state
}

func (s podResizeStateSnapshot) hasResizeSignal() bool {
	return s.pendingTrue || s.inProgressTrue || s.resizeStatus != ""
}

func (s podResizeStateSnapshot) terminalError() error {
	if s.pendingTrue && s.pendingReason == corev1.PodReasonInfeasible {
		return fmt.Errorf("pod resize is infeasible: %s", s.pendingMessage)
	}
	if s.inProgressTrue && s.inProgressReason == corev1.PodReasonError {
		return fmt.Errorf("pod resize has error: %s", s.inProgressMessage)
	}
	if s.resizeStatus == corev1.PodResizeStatusInfeasible {
		return fmt.Errorf("pod resize is infeasible")
	}
	return nil
}

func (s podResizeStateSnapshot) isSettledWithoutDeferral() bool {
	return !s.pendingTrue && !s.inProgressTrue && s.resizeStatus != corev1.PodResizeStatusDeferred
}

func shouldReturnOnCompleted(state podResizeStateSnapshot, sawResizeSignal bool) bool {
	if state.resizeApplied {
		return true
	}
	return sawResizeSignal && state.isSettledWithoutDeferral()
}

func waitForPodResizeState(ctx context.Context, c client.Client, namespace, name string,
	targetPod *corev1.Pod, timeout time.Duration) error {
	log := klog.FromContext(ctx).WithValues("pod", klog.KRef(namespace, name))
	if timeout <= 0 {
		return nil
	}
	targets := buildContainerCPUTargets(targetPod)

	sawResizeSignal := false
	lastPendingReason := ""
	lastPendingMessage := ""
	err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		var pod corev1.Pod
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &pod); err != nil {
			return false, err
		}
		state := inspectPodResizeState(&pod, targets)
		if state.hasResizeSignal() {
			sawResizeSignal = true
		}
		if state.pendingTrue {
			lastPendingReason = state.pendingReason
			lastPendingMessage = state.pendingMessage
		}
		if err := state.terminalError(); err != nil {
			return false, err
		}

		return shouldReturnOnCompleted(state, sawResizeSignal), nil
	})
	if err != nil {
		log.Error(err, "wait for pod resize state timeout")
		if lastPendingReason != "" {
			return fmt.Errorf("wait for pod resize state: %w (last pending reason=%s, message=%s)", err, lastPendingReason, lastPendingMessage)
		}
		return fmt.Errorf("wait for pod resize state: %w", err)
	}
	return nil
}

func TestWaitForPodResizeState(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	targetPod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("500m"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("1000m"),
						},
					},
				},
			},
		},
	}
	tests := []struct {
		name            string
		conditions      []corev1.PodCondition
		containerStatus []corev1.ContainerStatus
		resizeStatus    corev1.PodResizeStatus
		expectErr       string
	}{
		{
			name: "infeasible should fail",
			conditions: []corev1.PodCondition{
				{
					Type:    corev1.PodResizePending,
					Status:  corev1.ConditionTrue,
					Reason:  corev1.PodReasonInfeasible,
					Message: "insufficient cpu",
				},
			},
			expectErr: "infeasible",
		},
		{
			name:       "completed resize",
			conditions: nil,
			containerStatus: []corev1.ContainerStatus{
				{
					Name: "main",
					Resources: &corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("500m"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("1000m"),
						},
					},
				},
			},
			resizeStatus: "",
		},
		{
			name:         "should not succeed without resize signal or applied status",
			conditions:   nil,
			resizeStatus: "",
			expectErr:    "wait for pod resize state",
		},
		{
			name: "deferred should timeout with pending reason",
			conditions: []corev1.PodCondition{
				{
					Type:    corev1.PodResizePending,
					Status:  corev1.ConditionTrue,
					Reason:  corev1.PodReasonDeferred,
					Message: "Node didn't have enough resource: cpu",
				},
			},
			expectErr: "last pending reason=Deferred",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&corev1.Pod{}).
				Build()
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
			}
			err := fakeClient.Create(t.Context(), pod)
			require.NoError(t, err)
			pod.Status = corev1.PodStatus{
				Conditions:        tt.conditions,
				Resize:            tt.resizeStatus,
				ContainerStatuses: tt.containerStatus,
			}
			err = fakeClient.Status().Update(t.Context(), pod)
			require.NoError(t, err)

			err = waitForPodResizeState(t.Context(), fakeClient, "default", "test-pod", targetPod, 500*time.Millisecond)
			if tt.expectErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestModifyPickedSandbox_CSIMount(t *testing.T) {
	tests := []struct {
		name             string
		lockType         LockType
		opts             Options
		expectedAnnos    map[string]string
		notExpectedAnnos []string
	}{
		{
			name:     "with csi mount config",
			lockType: LockTypeUpdate,
			opts: Options{
				CSIMount: &runtimeconfig.CSIMountOptions{
					MountOptionListRaw: `{"mountOptionList":[{"pvName":"test-pv","mountPath":"/data"}]}`,
				},
			},
			expectedAnnos: map[string]string{
				v1alpha1.AnnotationInitRuntimeRequest: "", // should not be set
				v1alpha1.AnnotationCSIVolumeConfig:    `{"mountOptionList":[{"pvName":"test-pv","mountPath":"/data"}]}`,
			},
		},
		{
			name:     "with empty csi mount config",
			lockType: LockTypeUpdate,
			opts: Options{
				CSIMount: &runtimeconfig.CSIMountOptions{
					MountOptionListRaw: "",
				},
			},
			notExpectedAnnos: []string{
				v1alpha1.AnnotationCSIVolumeConfig,
			},
		},
		{
			name:     "with nil csi mount config",
			lockType: LockTypeUpdate,
			opts: Options{
				CSIMount: nil,
			},
			notExpectedAnnos: []string{
				v1alpha1.AnnotationCSIVolumeConfig,
			},
		},
		{
			name:     "with both init runtime and csi mount",
			lockType: LockTypeUpdate,
			opts: Options{
				InitRuntime: &runtimeconfig.InitRuntimeOptions{
					EnvVars: map[string]string{
						"KEY1": "value1",
						"KEY2": "value2",
					},
				},
				CSIMount: &runtimeconfig.CSIMountOptions{
					MountOptionListRaw: `{"mountOptionList":[{"pvName":"csi-pv","mountPath":"/mnt/data"}]}`,
				},
			},
			expectedAnnos: map[string]string{
				v1alpha1.AnnotationInitRuntimeRequest: `{"envVars":{"KEY1":"value1","KEY2":"value2"}}`,
				v1alpha1.AnnotationCSIVolumeConfig:    `{"mountOptionList":[{"pvName":"csi-pv","mountPath":"/mnt/data"}]}`,
			},
		},
		{
			name:     "csi mount with modifier",
			lockType: LockTypeUpdate,
			opts: Options{
				Modifier: func(sbx Handle) error {
					sbx.SetAnnotations(map[string]string{
						"custom-annotation": "custom-value",
					})
					return nil
				},
				CSIMount: &runtimeconfig.CSIMountOptions{
					MountOptionListRaw: `{"mountOptionList":[{"pvName":"test-pv","mountPath":"/custom"}]}`,
				},
			},
			expectedAnnos: map[string]string{
				"custom-annotation":                "custom-value",
				v1alpha1.AnnotationCSIVolumeConfig: `{"mountOptionList":[{"pvName":"test-pv","mountPath":"/custom"}]}`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a test sandbox
			sbx := &Sandbox{
				Sandbox: &v1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "test-sandbox",
						Namespace:   "default",
						Annotations: make(map[string]string),
					},
				},
			}

			// Call modifyPickedSandbox
			err := modifyPickedSandbox(sbx, tt.lockType, tt.opts)
			require.NoError(t, err)

			// Check expected annotations
			annotations := sbx.GetAnnotations()

			// Verify claim time annotation is always set
			assert.NotEmpty(t, annotations[v1alpha1.AnnotationClaimTime])

			// Verify claimed label is set
			labels := sbx.GetLabels()
			assert.Equal(t, v1alpha1.True, labels[v1alpha1.LabelSandboxIsClaimed])

			// Check expected annotations
			for key, expectedValue := range tt.expectedAnnos {
				if expectedValue != "" {
					assert.Equal(t, expectedValue, annotations[key], "annotation %s should match", key)
				} else {
					assert.Empty(t, annotations[key], "annotation %s should be empty", key)
				}
			}

			// Check not expected annotations
			for _, key := range tt.notExpectedAnnos {
				assert.Empty(t, annotations[key], "annotation %s should not be set", key)
			}
		})
	}
}

func TestShouldReleaseAdmissionAfterLockError(t *testing.T) {
	tests := []struct {
		name     string
		lockType LockType
		err      error
		want     bool
	}{
		{name: "nil error", lockType: LockTypeCreate, want: false},
		{name: "bad request rejected write", lockType: LockTypeCreate, err: apierrors.NewBadRequest("rejected"), want: true},
		{name: "conflict rejected write", lockType: LockTypeCreate, err: apierrors.NewConflict(v1alpha1.Resource("sandboxes"), "sbx", errors.New("exists")), want: true},
		{name: "pre-create context done", lockType: LockTypeCreate, err: errSandboxCreateNotAttempted, want: true},
		{name: "server timeout ambiguous create", lockType: LockTypeCreate, err: apierrors.NewServerTimeout(v1alpha1.Resource("sandboxes"), "create", 1), want: false},
		{name: "plain network error ambiguous create", lockType: LockTypeCreate, err: errors.New("connection reset"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ShouldReleaseAdmissionAfterLockError(tt.err))
		})
	}
}

func TestPreCheckCandidateResizeCompatibility(t *testing.T) {
	utestutils.InitLogOutput()

	memContainer := func(req, lim string) corev1.Container {
		c := corev1.Container{Name: "main", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{},
			Limits:   corev1.ResourceList{},
		}}
		if req != "" {
			c.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(req)
		}
		if lim != "" {
			c.Resources.Limits[corev1.ResourceMemory] = resource.MustParse(lim)
		}
		return c
	}
	qosContainer := func(reqCPU, reqMem, limCPU, limMem string) corev1.Container {
		return corev1.Container{
			Name: "main",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(reqCPU),
					corev1.ResourceMemory: resource.MustParse(reqMem),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(limCPU),
					corev1.ResourceMemory: resource.MustParse(limMem),
				},
			},
		}
	}
	sbxWithContainer := func(container *corev1.Container) *v1alpha1.Sandbox {
		sbx := &v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test-sbx",
				Namespace:         "default",
				Annotations:       map[string]string{},
				CreationTimestamp: metav1.Now(),
			},
		}
		if container != nil {
			sbx.Spec.Template = &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{*container}},
			}
		}
		return sbx
	}
	resourcesInplace := func(requests, limits map[corev1.ResourceName]string) *InplaceUpdateOptions {
		parse := func(m map[corev1.ResourceName]string) corev1.ResourceList {
			if len(m) == 0 {
				return nil
			}
			rl := corev1.ResourceList{}
			for name, q := range m {
				rl[name] = resource.MustParse(q)
			}
			return rl
		}
		return &InplaceUpdateOptions{
			Resources: &InplaceUpdateResourcesOptions{
				Requests: parse(requests),
				Limits:   parse(limits),
			},
		}
	}

	tests := []struct {
		name            string
		sbx             *v1alpha1.Sandbox
		inplace         *InplaceUpdateOptions
		expectErr       string
		expectResizeErr bool
	}{
		{
			name:    "no inplace update accepted",
			sbx:     sbxWithContainer(ptr.To(memContainer("256Mi", ""))),
			inplace: nil,
		},
		{
			name:    "inplace update without resources accepted",
			sbx:     sbxWithContainer(ptr.To(memContainer("256Mi", ""))),
			inplace: &InplaceUpdateOptions{},
		},
		{
			name:            "nil template with resize rejected",
			sbx:             sbxWithContainer(nil),
			inplace:         resourcesInplace(map[corev1.ResourceName]string{corev1.ResourceMemory: "128Mi"}, nil),
			expectErr:       "no pod template",
			expectResizeErr: true,
		},
		{
			name:    "memory upscale accepted",
			sbx:     sbxWithContainer(ptr.To(memContainer("256Mi", "512Mi"))),
			inplace: resourcesInplace(map[corev1.ResourceName]string{corev1.ResourceMemory: "512Mi"}, map[corev1.ResourceName]string{corev1.ResourceMemory: "1Gi"}),
		},
		{
			name:    "equal memory accepted",
			sbx:     sbxWithContainer(ptr.To(memContainer("256Mi", ""))),
			inplace: resourcesInplace(map[corev1.ResourceName]string{corev1.ResourceMemory: "256Mi"}, nil),
		},
		{
			name:            "memory request downscale rejected",
			sbx:             sbxWithContainer(ptr.To(memContainer("256Mi", ""))),
			inplace:         resourcesInplace(map[corev1.ResourceName]string{corev1.ResourceMemory: "128Mi"}, nil),
			expectErr:       "downscale",
			expectResizeErr: true,
		},
		{
			name:            "memory limit downscale rejected",
			sbx:             sbxWithContainer(ptr.To(memContainer("", "512Mi"))),
			inplace:         resourcesInplace(nil, map[corev1.ResourceName]string{corev1.ResourceMemory: "256Mi"}),
			expectErr:       "downscale",
			expectResizeErr: true,
		},
		{
			name:            "request raised above existing limit rejected",
			sbx:             sbxWithContainer(ptr.To(qosContainer("100m", "256Mi", "500m", "512Mi"))),
			inplace:         resourcesInplace(map[corev1.ResourceName]string{corev1.ResourceCPU: "1"}, nil),
			expectErr:       "above the container limit",
			expectResizeErr: true,
		},
		{
			name:            "qos class change rejected",
			sbx:             sbxWithContainer(ptr.To(qosContainer("100m", "256Mi", "200m", "512Mi"))),
			inplace:         resourcesInplace(map[corev1.ResourceName]string{corev1.ResourceCPU: "200m", corev1.ResourceMemory: "512Mi"}, nil),
			expectErr:       "QoS class",
			expectResizeErr: true,
		},
		{
			name:    "qos class preserved accepted",
			sbx:     sbxWithContainer(ptr.To(qosContainer("100m", "256Mi", "200m", "512Mi"))),
			inplace: resourcesInplace(map[corev1.ResourceName]string{corev1.ResourceCPU: "200m"}, nil),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := preCheckCandidate(tt.sbx, tt.inplace)
			if tt.expectErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectErr)
			if tt.expectResizeErr {
				var resizeErr *candidateResizeIncompatibleError
				require.ErrorAs(t, err, &resizeErr)
			}
		})
	}
}

func TestPickAnAvailableSandbox_CandidateCompatibilityUsesCandidateSpec(t *testing.T) {
	utestutils.InitLogOutput()

	template := "test-resize-compat-template"
	oldRevision := "rev-old"
	newRevision := "rev-new"

	newCandidate := func(name, templateHash, reqMem string) *v1alpha1.Sandbox {
		return &v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels: map[string]string{
					v1alpha1.LabelSandboxTemplate:  template,
					v1alpha1.LabelSandboxIsClaimed: "false",
					v1alpha1.LabelTemplateHash:     templateHash,
				},
				Annotations:       map[string]string{},
				CreationTimestamp: metav1.Now(),
				OwnerReferences:   GetSbsOwnerReference(),
			},
			Spec: v1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name: "main",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(reqMem)},
								},
							}},
						},
					},
				},
			},
			Status: v1alpha1.SandboxStatus{
				Phase:      v1alpha1.SandboxRunning,
				Conditions: []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}},
				PodInfo:    v1alpha1.PodInfo{PodIP: "1.2.3.4"},
				NodeName:   "test-node",
			},
		}
	}
	newProbes := func(prefix string, count int) []v1alpha1.Probe {
		probes := make([]v1alpha1.Probe, 0, count)
		for i := range count {
			probes = append(probes, v1alpha1.Probe{
				Name: fmt.Sprintf("%s-%d", prefix, i),
				Probe: corev1.Probe{ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{Command: []string{"true"}},
				}},
			})
		}
		return probes
	}

	// requiresActiveProbe is the minimal claim policy whose idle rule references
	// the "Active" probe, standing in for the former RequiredProbeNames option.
	requiresActiveProbe := &v1alpha1.AutoPausePolicy{
		Pause: &v1alpha1.PausePolicy{
			WhenProbedIdleState: &v1alpha1.ProbedIdleStateRule{Probe: "Active"},
		},
	}

	setup := func(t *testing.T) (*claimTestInfra, client.Client) {
		testInfra, c := newClaimTestInfra(t)
		require.NoError(t, c.Create(t.Context(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "test-node"}}))
		require.Eventually(t, func() bool {
			node := &corev1.Node{}
			return testInfra.Cache.GetClient().Get(t.Context(), client.ObjectKey{Name: "test-node"}, node) == nil
		}, time.Second, 10*time.Millisecond)
		// Rollout in progress: template memory raised to 1Gi, old sandboxes still lower.
		sbs := &v1alpha1.SandboxSet{
			ObjectMeta: metav1.ObjectMeta{Name: template, Namespace: "default"},
			Spec: v1alpha1.SandboxSetSpec{
				Replicas: 1,
				EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name: "main",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
								},
							}},
						},
					},
				},
			},
			Status: v1alpha1.SandboxSetStatus{UpdateRevision: newRevision},
		}
		require.NoError(t, c.Create(t.Context(), sbs))
		require.NoError(t, c.Status().Update(t.Context(), sbs))
		return testInfra, c
	}

	waitPool := func(t *testing.T, testInfra *claimTestInfra, count int) {
		require.Eventually(t, func() bool {
			objs, err := testInfra.Cache.ListSandboxesInPool(t.Context(), infracache.ListSandboxesInPoolOptions{Pool: template})
			return err == nil && len(objs) == count
		}, 200*time.Millisecond, 5*time.Millisecond)
	}

	t.Run("old revision candidate below new template memory stays claimable", func(t *testing.T) {
		testInfra, c := setup(t)
		CreateSandboxWithStatus(t, c, newCandidate("old-512mi", oldRevision, "512Mi"))
		waitPool(t, testInfra, 1)

		opts, err := ValidateAndInitClaimOptions(Options{
			Namespace: "default",
			User:      "test-user",
			Template:  template,
			InplaceUpdate: &InplaceUpdateOptions{
				Resources: &InplaceUpdateResourcesOptions{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("768Mi")},
				},
			},
		})
		require.NoError(t, err)

		sbx, lockType, err := pickAnAvailableSandbox(t.Context(), opts, &testInfra.pickCache, testInfra.Cache)
		require.NoError(t, err)
		require.Equal(t, LockTypeUpdate, lockType)
		assert.Equal(t, "old-512mi", sbx.GetName())
	})

	t.Run("all candidates resize incompatible returns retriable no-available error", func(t *testing.T) {
		testInfra, c := setup(t)
		CreateSandboxWithStatus(t, c, newCandidate("old-1gi", oldRevision, "1Gi"))
		waitPool(t, testInfra, 1)

		opts, err := ValidateAndInitClaimOptions(Options{
			Namespace: "default",
			User:      "test-user",
			Template:  template,
			InplaceUpdate: &InplaceUpdateOptions{
				Resources: &InplaceUpdateResourcesOptions{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
				},
			},
		})
		require.NoError(t, err)

		_, _, err = pickAnAvailableSandbox(t.Context(), opts, &testInfra.pickCache, testInfra.Cache)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "downscale")
		assert.Contains(t, err.Error(), "inplace resize")
		var retriable RetriableError
		assert.True(t, errors.As(err, &retriable), "resize-incompatible pool must stay retriable")
	})

	t.Run("old revision without a claim-required probe stays unclaimed", func(t *testing.T) {
		testInfra, c := setup(t)
		CreateSandboxWithStatus(t, c, newCandidate("old-without-probe", oldRevision, "512Mi"))
		waitPool(t, testInfra, 1)

		opts, err := ValidateAndInitClaimOptions(Options{
			Namespace:       "default",
			User:            "test-user",
			Template:        template,
			AutoPausePolicy: requiresActiveProbe,
		})
		require.NoError(t, err)

		claimed, _, err := TryClaimSandbox(t.Context(), opts, &testInfra.pickCache, testInfra.Cache, make(chan struct{}, 1), nil)
		require.Error(t, err)
		assert.Nil(t, claimed)
		assert.Contains(t, err.Error(), "required probes [Active]")
		var retriable RetriableError
		assert.True(t, errors.As(err, &retriable), "probe-incompatible pool must stay retriable")

		unchanged := &v1alpha1.Sandbox{}
		require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "old-without-probe"}, unchanged))
		assert.Equal(t, "false", unchanged.Labels[v1alpha1.LabelSandboxIsClaimed])
		assert.Nil(t, unchanged.Spec.AutoPausePolicy)
	})

	t.Run("missing probe diagnostic accumulates and deduplicates across candidates", func(t *testing.T) {
		testInfra, c := setup(t)
		for i, probes := range [][]v1alpha1.Probe{{{Name: "Active"}}, {{Name: "Cron"}}, {{Name: "Cron"}}} {
			candidate := newCandidate(fmt.Sprintf("missing-probes-%d", i), oldRevision, "512Mi")
			candidate.Spec.Probes = probes
			CreateSandboxWithStatus(t, c, candidate)
		}
		waitPool(t, testInfra, 3)
		opts, err := ValidateAndInitClaimOptions(Options{
			Namespace: "default", User: "test-user", Template: template,
			AutoPausePolicy: &v1alpha1.AutoPausePolicy{
				Pause:  &v1alpha1.PausePolicy{WhenProbedIdleState: &v1alpha1.ProbedIdleStateRule{Probe: "Active"}},
				Resume: &v1alpha1.ResumePolicy{WhenProbedScheduleTime: &v1alpha1.ProbedScheduleTimeRule{Probe: "Cron"}},
			},
		})
		require.NoError(t, err)
		_, _, err = pickAnAvailableSandbox(t.Context(), opts, &testInfra.pickCache, testInfra.Cache)
		require.ErrorContains(t, err, "required probes [Active Cron]")
		var retriable RetriableError
		require.ErrorAs(t, err, &retriable)
	})

	t.Run("new revision candidate without required probe falls back to old compatible revision", func(t *testing.T) {
		testInfra, c := setup(t)
		oldCandidate := newCandidate("old-with-probe", oldRevision, "512Mi")
		oldCandidate.Spec.Probes = []v1alpha1.Probe{{Name: "Active"}}
		updatedCandidate := newCandidate("updated-without-probe", newRevision, "1Gi")
		CreateSandboxWithStatus(t, c, oldCandidate)
		CreateSandboxWithStatus(t, c, updatedCandidate)
		waitPool(t, testInfra, 2)

		opts, err := ValidateAndInitClaimOptions(Options{
			Namespace:       "default",
			User:            "test-user",
			Template:        template,
			AutoPausePolicy: requiresActiveProbe,
		})
		require.NoError(t, err)

		sbx, lockType, err := pickAnAvailableSandbox(t.Context(), opts, &testInfra.pickCache, testInfra.Cache)
		require.NoError(t, err)
		assert.Equal(t, LockTypeUpdate, lockType)
		assert.Equal(t, "old-with-probe", sbx.GetName())
	})

	t.Run("claim-carried probe does not require pool pre-declaration", func(t *testing.T) {
		testInfra, c := setup(t)
		candidate := newCandidate("carried-probe-candidate", newRevision, "1Gi")
		CreateSandboxWithStatus(t, c, candidate)
		waitPool(t, testInfra, 1)

		opts, err := ValidateAndInitClaimOptions(Options{
			Namespace:       "default",
			User:            "test-user",
			Template:        template,
			AutoPausePolicy: requiresActiveProbe,
			// The claim carries the "Active" probe itself, so candidates
			// without it stay eligible: it is merged on at claim time.
			Probes: []v1alpha1.Probe{{Name: "Active"}}, ProbeUpdatesEnabled: true,
		})
		require.NoError(t, err)

		sbx, lockType, err := pickAnAvailableSandbox(t.Context(), opts, &testInfra.pickCache, testInfra.Cache)
		require.NoError(t, err)
		assert.Equal(t, LockTypeUpdate, lockType)
		assert.Equal(t, "carried-probe-candidate", sbx.GetName())
	})

	t.Run("old revision exceeding the merged probe limit falls back to creation", func(t *testing.T) {
		testInfra, c := setup(t)
		candidate := newCandidate("old-at-probe-limit", oldRevision, "512Mi")
		candidate.Spec.Probes = newProbes("old", autopause.MaxSandboxProbes)
		CreateSandboxWithStatus(t, c, candidate)
		waitPool(t, testInfra, 1)

		opts, err := ValidateAndInitClaimOptions(Options{
			Namespace:           "default",
			User:                "test-user",
			Template:            template,
			Probes:              newProbes("claim", 1),
			ProbeUpdatesEnabled: true,
			CreateOnNoStock:     true,
		})
		require.NoError(t, err)

		sbx, lockType, err := pickAnAvailableSandbox(t.Context(), opts, &testInfra.pickCache, testInfra.Cache)
		require.NoError(t, err)
		assert.Equal(t, LockTypeCreate, lockType)
		assert.NotEqual(t, candidate.Name, sbx.GetName())
	})

	t.Run("same-name probe replacement stays within the exact limit", func(t *testing.T) {
		testInfra, c := setup(t)
		candidate := newCandidate("old-at-exact-probe-limit", oldRevision, "512Mi")
		candidate.Spec.Probes = newProbes("pool", autopause.MaxSandboxProbes)
		CreateSandboxWithStatus(t, c, candidate)
		waitPool(t, testInfra, 1)

		replacement := candidate.Spec.Probes[0].DeepCopy()
		replacement.Exec.Command = []string{"replacement"}
		opts, err := ValidateAndInitClaimOptions(Options{
			Namespace:           "default",
			User:                "test-user",
			Template:            template,
			Probes:              []v1alpha1.Probe{*replacement},
			ProbeUpdatesEnabled: true,
		})
		require.NoError(t, err)

		sbx, lockType, err := pickAnAvailableSandbox(t.Context(), opts, &testInfra.pickCache, testInfra.Cache)
		require.NoError(t, err)
		assert.Equal(t, LockTypeUpdate, lockType)
		assert.Equal(t, candidate.Name, sbx.GetName())
		require.NoError(t, modifyPickedSandbox(sbx, lockType, opts))
		assert.Len(t, sbx.Spec.Probes, autopause.MaxSandboxProbes)
		assert.Equal(t, []string{"replacement"}, sbx.Spec.Probes[0].Exec.Command)
	})

	t.Run("locked candidates keep the generic no-available path", func(t *testing.T) {
		testInfra, c := setup(t)
		locked := newCandidate("locked-1gi", oldRevision, "1Gi")
		locked.Annotations[v1alpha1.AnnotationLock] = "someone-else"
		CreateSandboxWithStatus(t, c, locked)
		waitPool(t, testInfra, 1)

		opts, err := ValidateAndInitClaimOptions(Options{
			Namespace: "default",
			User:      "test-user",
			Template:  template,
			InplaceUpdate: &InplaceUpdateOptions{
				Resources: &InplaceUpdateResourcesOptions{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
				},
			},
		})
		require.NoError(t, err)

		_, _, err = pickAnAvailableSandbox(t.Context(), opts, &testInfra.pickCache, testInfra.Cache)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "inplace resize")
		var retriable RetriableError
		assert.True(t, errors.As(err, &retriable))
	})
}

func TestPickAnAvailableSandbox_ProbeUpdates(t *testing.T) {
	const template = "probe-update-pool"
	probe := v1alpha1.Probe{Name: "Active", Probe: corev1.Probe{ProbeHandler: corev1.ProbeHandler{
		Exec: &corev1.ExecAction{Command: []string{"true"}},
	}}}
	probePolicy := &v1alpha1.AutoPausePolicy{Pause: &v1alpha1.PausePolicy{
		WhenProbedIdleState: &v1alpha1.ProbedIdleStateRule{Probe: probe.Name},
	}}
	candidate := func(name, nodeName, revision string, phase v1alpha1.SandboxPhase) v1alpha1.Sandbox {
		return v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "default",
				Labels: map[string]string{v1alpha1.LabelTemplateHash: revision},
			},
			Status: v1alpha1.SandboxStatus{Phase: phase, NodeName: nodeName},
		}
	}
	running := v1alpha1.SandboxRunning
	pending := v1alpha1.SandboxPending
	tests := []struct {
		name                  string
		candidates            []v1alpha1.Sandbox
		probeUpdatesEnabled   bool
		claimHasProbes        bool
		claimHasPolicy        bool
		poolHasProbe          bool
		candidateCounts       int
		speculate             bool
		getErr                error
		nodeGetErrs           map[string]error
		cancelOnNodeGet       bool
		wantCandidate         string
		wantLock              LockType
		wantNoAvailable       bool
		wantCanceled          bool
		wantNodeGets          int
		wantProbeUpdateReason string
	}{
		{name: "available virtual kubelet candidate is skipped", candidates: []v1alpha1.Sandbox{candidate("vk-available", "vk-node", "current", running)}, probeUpdatesEnabled: true, claimHasProbes: true, wantNoAvailable: true, wantNodeGets: 1},
		{name: "creating virtual kubelet candidate is skipped", candidates: []v1alpha1.Sandbox{candidate("vk-creating", "vk-node", "current", pending)}, probeUpdatesEnabled: true, claimHasProbes: true, speculate: true, wantNoAvailable: true, wantNodeGets: 1},
		{name: "available real node candidate is accepted", candidates: []v1alpha1.Sandbox{candidate("real-available", "real-node", "current", running)}, probeUpdatesEnabled: true, claimHasProbes: true, wantCandidate: "real-available", wantLock: LockTypeUpdate, wantNodeGets: 1},
		{name: "creating real node candidate is accepted", candidates: []v1alpha1.Sandbox{candidate("real-creating", "real-node", "current", pending)}, probeUpdatesEnabled: true, claimHasProbes: true, speculate: true, wantCandidate: "real-creating", wantLock: LockTypeSpeculate, wantNodeGets: 1},
		{name: "missing node is skipped", candidates: []v1alpha1.Sandbox{candidate("unknown-node", "missing-node", "current", running)}, probeUpdatesEnabled: true, claimHasProbes: true, wantNoAvailable: true, wantNodeGets: 1},
		{name: "node get error is skipped", candidates: []v1alpha1.Sandbox{candidate("node-get-error", "real-node", "current", running)}, probeUpdatesEnabled: true, claimHasProbes: true, getErr: errors.New("node lookup failed"), wantNoAvailable: true, wantNodeGets: 1},
		{name: "node get cancellation aborts selection", candidates: []v1alpha1.Sandbox{candidate("node-get-canceled", "real-node", "current", running)}, probeUpdatesEnabled: true, claimHasProbes: true, cancelOnNodeGet: true, wantCanceled: true, wantNodeGets: 1},
		{name: "empty node name is skipped", candidates: []v1alpha1.Sandbox{candidate("no-node-name", "", "current", running)}, probeUpdatesEnabled: true, claimHasProbes: true, wantNoAvailable: true},
		{name: "disabled capability skips probe-bearing candidates", candidates: []v1alpha1.Sandbox{candidate("disabled-vk", "vk-node", "current", running)}, claimHasProbes: true, wantNoAvailable: true, wantProbeUpdateReason: "probe updates disabled: 1"},
		{name: "virtual kubelet is not hidden by a later node read failure", candidates: []v1alpha1.Sandbox{candidate("a-vk", "vk-node", "current", running), candidate("b-unread", "flaky-node", "current", running)}, probeUpdatesEnabled: true, claimHasProbes: true, nodeGetErrs: map[string]error{"flaky-node": errors.New("node lookup failed")}, wantNoAvailable: true, wantNodeGets: 2, wantProbeUpdateReason: "virtual-kubelet: 1, node read failed: 1"},
		{name: "virtual kubelet is not hidden by an earlier node read failure", candidates: []v1alpha1.Sandbox{candidate("a-unread", "flaky-node", "current", running), candidate("b-vk", "vk-node", "current", running)}, probeUpdatesEnabled: true, claimHasProbes: true, nodeGetErrs: map[string]error{"flaky-node": errors.New("node lookup failed")}, wantNoAvailable: true, wantNodeGets: 2, wantProbeUpdateReason: "virtual-kubelet: 1, node read failed: 1"},
		{name: "unscheduled candidate is not hidden by a node read failure", candidates: []v1alpha1.Sandbox{candidate("a-none", "", "current", running), candidate("b-unread", "flaky-node", "current", running)}, probeUpdatesEnabled: true, claimHasProbes: true, nodeGetErrs: map[string]error{"flaky-node": errors.New("node lookup failed")}, wantNoAvailable: true, wantNodeGets: 1, wantProbeUpdateReason: "unscheduled: 1, node read failed: 1"},
		{name: "no claim probes leave virtual kubelet candidate eligible", candidates: []v1alpha1.Sandbox{candidate("no-probes-vk", "vk-node", "current", running)}, probeUpdatesEnabled: true, wantCandidate: "no-probes-vk", wantLock: LockTypeUpdate},
		{name: "policy-only claim does not inspect node capability", candidates: []v1alpha1.Sandbox{candidate("policy-only-real", "real-node", "current", running)}, probeUpdatesEnabled: true, claimHasPolicy: true, poolHasProbe: true, wantCandidate: "policy-only-real", wantLock: LockTypeUpdate},
		{name: "virtual kubelet before real node does not consume candidate count", candidates: []v1alpha1.Sandbox{candidate("a-vk", "vk-node", "current", running), candidate("b-real", "real-node", "current", running)}, probeUpdatesEnabled: true, claimHasProbes: true, candidateCounts: 1, wantCandidate: "b-real", wantLock: LockTypeUpdate, wantNodeGets: 2},
		{name: "available candidate precedes creating candidate", candidates: []v1alpha1.Sandbox{candidate("a-creating", "real-node", "current", pending), candidate("b-available", "real-node", "old", running)}, probeUpdatesEnabled: true, claimHasProbes: true, candidateCounts: 1, speculate: true, wantCandidate: "b-available", wantLock: LockTypeUpdate, wantNodeGets: 2},
		{name: "current revision candidate keeps priority", candidates: []v1alpha1.Sandbox{candidate("a-old", "real-node", "old", running), candidate("b-current", "real-node", "current", running)}, probeUpdatesEnabled: true, claimHasProbes: true, candidateCounts: 2, wantCandidate: "b-current", wantLock: LockTypeUpdate, wantNodeGets: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testInfra, fc := newClaimTestInfra(t)
			cacheClient, ok := testInfra.Cache.GetClient().(client.WithWatch)
			require.True(t, ok)
			apiReader, ok := fc.(client.WithWatch)
			require.True(t, ok)
			var nodeGetCount, apiReaderNodeGetCount atomic.Int32
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			testInfra.Cache = &apiReaderOverrideCache{
				Provider: testInfra.Cache,
				apiReader: interceptor.NewClient(apiReader, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*corev1.Node); ok {
							apiReaderNodeGetCount.Add(1)
							return errors.New("selection must use cached node reads")
						}
						return c.Get(ctx, key, obj, opts...)
					},
				}),
				client: interceptor.NewClient(cacheClient, interceptor.Funcs{
					Get: func(getCtx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*corev1.Node); ok {
							nodeGetCount.Add(1)
							if tt.cancelOnNodeGet {
								cancel()
								return context.Canceled
							}
							if err, ok := tt.nodeGetErrs[key.Name]; ok {
								return err
							}
							if tt.getErr != nil {
								return tt.getErr
							}
						}
						return c.Get(getCtx, key, obj, opts...)
					},
				}),
			}

			sbs := &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: template, Namespace: "default"},
				Spec: v1alpha1.SandboxSetSpec{EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "test"}}}},
				}},
				Status: v1alpha1.SandboxSetStatus{UpdateRevision: "current"},
			}
			require.NoError(t, fc.Create(t.Context(), sbs))
			require.NoError(t, fc.Status().Update(t.Context(), sbs))
			require.NoError(t, fc.Create(t.Context(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "real-node"}}))
			require.NoError(t, fc.Create(t.Context(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "vk-node", Labels: map[string]string{"type": "virtual-kubelet"}}}))
			for i := range tt.candidates {
				candidate := tt.candidates[i].DeepCopy()
				candidate.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
				candidate.Labels[v1alpha1.LabelSandboxTemplate] = template
				candidate.Labels[v1alpha1.LabelSandboxIsClaimed] = "false"
				candidate.OwnerReferences = GetSbsOwnerReference()
				candidate.Spec.EmbeddedSandboxTemplate = *sbs.Spec.EmbeddedSandboxTemplate.DeepCopy()
				if candidate.Status.Phase == v1alpha1.SandboxRunning {
					candidate.Status.Conditions = []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}
					candidate.Status.PodInfo = v1alpha1.PodInfo{PodIP: "1.2.3.4"}
				}
				if tt.poolHasProbe {
					candidate.Spec.Probes = []v1alpha1.Probe{probe}
				}
				CreateSandboxWithStatus(t, fc, candidate)
			}
			require.Eventually(t, func() bool {
				_, setErr := testInfra.Cache.PickSandboxSet(t.Context(), infracache.PickSandboxSetOptions{Namespace: "default", Name: template})
				pool, poolErr := testInfra.Cache.ListSandboxesInPool(t.Context(), infracache.ListSandboxesInPoolOptions{Namespace: "default", Pool: template})
				return setErr == nil && poolErr == nil && len(pool) == len(tt.candidates)
			}, time.Second, 10*time.Millisecond)
			nodeGetCount.Store(0)
			if tt.name == "virtual kubelet before real node does not consume candidate count" {
				pool, err := testInfra.Cache.ListSandboxesInPool(t.Context(), infracache.ListSandboxesInPoolOptions{Namespace: "default", Pool: template})
				require.NoError(t, err)
				require.Len(t, pool, 2)
				assert.Equal(t, "a-vk", pool[0].Name, "the virtual-kubelet candidate must be encountered first")
			}

			claimProbes := []v1alpha1.Probe(nil)
			if tt.claimHasProbes {
				claimProbes = []v1alpha1.Probe{probe}
			}
			claimPolicy := (*v1alpha1.AutoPausePolicy)(nil)
			if tt.claimHasPolicy {
				claimPolicy = probePolicy
			}
			speculateDuration := time.Duration(0)
			if tt.speculate {
				speculateDuration = time.Second
			}
			opts, err := ValidateAndInitClaimOptions(Options{
				Namespace: "default", User: "test-user", Template: template,
				Probes: claimProbes, AutoPausePolicy: claimPolicy,
				ProbeUpdatesEnabled: tt.probeUpdatesEnabled, CandidateCounts: tt.candidateCounts,
				SpeculateCreatingDuration: speculateDuration,
			})
			require.NoError(t, err)
			pickCtx := t.Context()
			if tt.cancelOnNodeGet {
				pickCtx = ctx
			}
			sbx, lockType, pickErr := pickAnAvailableSandbox(pickCtx, opts, &testInfra.pickCache, testInfra.Cache)
			assert.Equal(t, int32(tt.wantNodeGets), nodeGetCount.Load())
			assert.Zero(t, apiReaderNodeGetCount.Load(), "node capability must be read through the cache client")
			if tt.wantCanceled {
				require.ErrorIs(t, pickErr, context.Canceled)
				assert.Nil(t, sbx)
				return
			}
			if tt.wantNoAvailable {
				require.Error(t, pickErr)
				var retriable RetriableError
				require.ErrorAs(t, pickErr, &retriable)
				if tt.wantProbeUpdateReason != "" {
					assert.Contains(t, pickErr.Error(), "no candidate supports probe updates: "+tt.wantProbeUpdateReason)
				}
				assert.Nil(t, sbx)
				return
			}
			require.NoError(t, pickErr)
			require.NotNil(t, sbx)
			assert.Equal(t, tt.wantCandidate, sbx.GetName())
			assert.Equal(t, tt.wantLock, lockType)
		})
	}
}

func TestModifyPickedSandbox_InitRuntime(t *testing.T) {
	tests := []struct {
		name             string
		opts             Options
		expectedAnnos    map[string]string
		notExpectedAnnos []string
	}{
		{
			name: "with init runtime options",
			opts: Options{
				InitRuntime: &runtimeconfig.InitRuntimeOptions{
					EnvVars: map[string]string{
						"ENV1": "value1",
						"ENV2": "value2",
					},
					AccessToken: "test-token",
				},
			},
			expectedAnnos: map[string]string{
				v1alpha1.AnnotationInitRuntimeRequest: `{"envVars":{"ENV1":"value1","ENV2":"value2"},"accessToken":"test-token"}`,
				v1alpha1.AnnotationRuntimeAccessToken: "test-token",
			},
		},
		{
			name: "with init runtime without access token",
			opts: Options{
				InitRuntime: &runtimeconfig.InitRuntimeOptions{
					EnvVars: map[string]string{
						"TEST_ENV": "test_value",
					},
				},
			},
			expectedAnnos: map[string]string{
				v1alpha1.AnnotationInitRuntimeRequest: `{"envVars":{"TEST_ENV":"test_value"}}`,
			},
			notExpectedAnnos: []string{
				v1alpha1.AnnotationRuntimeAccessToken,
			},
		},
		{
			name: "without init runtime",
			opts: Options{
				InitRuntime: nil,
			},
			notExpectedAnnos: []string{
				v1alpha1.AnnotationInitRuntimeRequest,
				v1alpha1.AnnotationRuntimeAccessToken,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &Sandbox{
				Sandbox: &v1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "test-sandbox",
						Namespace:   "default",
						Annotations: make(map[string]string),
					},
				},
			}

			err := modifyPickedSandbox(sbx, LockTypeUpdate, tt.opts)
			require.NoError(t, err)

			annotations := sbx.GetAnnotations()

			// Verify claim time annotation is always set
			assert.NotEmpty(t, annotations[v1alpha1.AnnotationClaimTime])

			// Check expected annotations
			for key, expectedValue := range tt.expectedAnnos {
				if expectedValue != "" {
					assert.Equal(t, expectedValue, annotations[key], "annotation %s should match", key)
				} else {
					assert.Empty(t, annotations[key], "annotation %s should be empty", key)
				}
			}

			// Check not expected annotations
			for _, key := range tt.notExpectedAnnos {
				assert.Empty(t, annotations[key], "annotation %s should not be set", key)
			}
		})
	}
}

func TestModifyPickedSandboxClaimMethod(t *testing.T) {
	tests := []struct {
		name     string
		lockType LockType
		initial  string
		want     string
		wantErr  string
		source   string
	}{
		{name: "manager-create", lockType: LockTypeCreate, source: v1alpha1.SandboxSourceE2B, want: v1alpha1.ClaimMethodColdStart},
		{name: "manager-update", lockType: LockTypeUpdate, source: v1alpha1.SandboxSourceE2B, want: v1alpha1.ClaimMethodHotStart},
		{name: "manager-speculate", lockType: LockTypeSpeculate, source: v1alpha1.SandboxSourceE2B, want: v1alpha1.ClaimMethodHotStart},
		{name: "controller-create", lockType: LockTypeCreate, source: v1alpha1.SandboxSourceK8s, want: v1alpha1.ClaimMethodColdStart},
		{name: "controller-update", lockType: LockTypeUpdate, source: v1alpha1.SandboxSourceK8s, want: v1alpha1.ClaimMethodHotStart},
		{name: "unknown-source", lockType: LockTypeUpdate, source: "other", want: v1alpha1.ClaimMethodHotStart},
		{name: "create", lockType: LockTypeCreate, want: v1alpha1.ClaimMethodColdStart},
		{name: "update", lockType: LockTypeUpdate, initial: v1alpha1.ClaimMethodColdStart, want: v1alpha1.ClaimMethodHotStart},
		{name: "speculate", lockType: LockTypeSpeculate, initial: v1alpha1.ClaimMethodColdStart, want: v1alpha1.ClaimMethodHotStart},
		{name: "unknown", lockType: LockType("other"), wantErr: `unsupported lock type "other"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testInfra, fc := newClaimTestInfra(t)
			sbx := &Sandbox{Sandbox: &v1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
				Name:        "sandbox-" + tt.name,
				Namespace:   "default",
				Labels:      map[string]string{},
				Annotations: map[string]string{},
			}, Spec: v1alpha1.SandboxSpec{EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{},
			}}}}
			if tt.initial != "" {
				sbx.Labels[v1alpha1.LabelSandboxClaimMethod] = tt.initial
			}
			if tt.lockType != LockTypeCreate {
				require.NoError(t, fc.Create(t.Context(), sbx.Sandbox))
			}

			err := modifyPickedSandbox(sbx, tt.lockType, Options{
				User:               "test-user",
				Template:           "test-template",
				LockString:         "lock-" + tt.name,
				ControlPlaneSource: tt.source,
				Modifier: func(s Handle) error {
					s.GetLabels()[v1alpha1.LabelSandboxSource] = "forged"
					return nil
				},
			})
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.NoError(t, performLockSandbox(t.Context(), sbx, tt.lockType, Options{
				User:       "test-user",
				LockString: "lock-" + tt.name,
			}, testInfra.Cache))

			persisted := &v1alpha1.Sandbox{}
			require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(sbx.Sandbox), persisted))
			assert.Equal(t, tt.want, persisted.Labels[v1alpha1.LabelSandboxClaimMethod])
			wantSource := v1alpha1.SandboxSourceK8s
			if tt.source == v1alpha1.SandboxSourceE2B {
				wantSource = v1alpha1.SandboxSourceE2B
			}
			assert.Equal(t, wantSource, persisted.Labels[v1alpha1.LabelSandboxSource])
		})
	}
}

// TestModifyPickedSandbox_AutoPausePolicy covers the claim-time auto-pause
// policy overlay: a nil overlay keeps the policy the pool sandbox already
// carries, while a claim policy replaces it with a deep copy so the sandbox
// spec never aliases the SandboxClaim spec.
func TestModifyPickedSandbox_AutoPausePolicy(t *testing.T) {
	poolPolicy := &v1alpha1.AutoPausePolicy{
		Pause: &v1alpha1.PausePolicy{
			WhenProbedIdleState: &v1alpha1.ProbedIdleStateRule{Probe: "Active"},
		},
	}
	claimPolicy := &v1alpha1.AutoPausePolicy{
		Resume: &v1alpha1.ResumePolicy{
			OnIngressTraffic: &v1alpha1.IngressTrafficRule{},
		},
	}

	newSandbox := func() *Sandbox {
		return &Sandbox{Sandbox: &v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-sandbox",
				Namespace:   "default",
				Annotations: map[string]string{},
			},
			Spec: v1alpha1.SandboxSpec{AutoPausePolicy: poolPolicy.DeepCopy()},
		}}
	}

	t.Run("nil overlay keeps the pool policy", func(t *testing.T) {
		sbx := newSandbox()
		require.NoError(t, modifyPickedSandbox(sbx, LockTypeUpdate, Options{}))
		assert.Equal(t, poolPolicy, sbx.Spec.AutoPausePolicy)
	})

	t.Run("claim policy replaces the pool policy and is deep-copied", func(t *testing.T) {
		sbx := newSandbox()
		require.NoError(t, modifyPickedSandbox(sbx, LockTypeUpdate, Options{
			AutoPausePolicy: claimPolicy,
		}))
		assert.Equal(t, claimPolicy, sbx.Spec.AutoPausePolicy)
		sbx.Spec.AutoPausePolicy.Resume.OnIngressTraffic.PauseTimeout = &metav1.Duration{Duration: time.Minute}
		assert.Nil(t, claimPolicy.Resume.OnIngressTraffic.PauseTimeout)
	})
}

// TestModifyPickedSandbox_Probes covers the claim-time probe merge: claim
// probes replace same-name pool probes and append new ones, the result is a
// deep copy so the sandbox spec never aliases the SandboxClaim spec, and a
// policy rule may reference a probe the claim itself carries.
func TestModifyPickedSandbox_Probes(t *testing.T) {
	probe := func(name, command string) v1alpha1.Probe {
		return v1alpha1.Probe{
			Name: name,
			Probe: corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{Command: []string{command}},
				},
			},
		}
	}
	requiresCronProbe := &v1alpha1.AutoPausePolicy{
		Pause: &v1alpha1.PausePolicy{
			WhenProbedIdleState: &v1alpha1.ProbedIdleStateRule{Probe: "Cron"},
		},
	}

	newSandbox := func(probes ...v1alpha1.Probe) *Sandbox {
		return &Sandbox{Sandbox: &v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-sandbox",
				Namespace:   "default",
				Annotations: map[string]string{},
			},
			Spec: v1alpha1.SandboxSpec{Probes: probes},
		}}
	}

	t.Run("empty claim probes keep the pool probes", func(t *testing.T) {
		sbx := newSandbox(probe("Active", "pool-cmd"))
		require.NoError(t, modifyPickedSandbox(sbx, LockTypeUpdate, Options{}))
		assert.Equal(t, []v1alpha1.Probe{probe("Active", "pool-cmd")}, sbx.Spec.Probes)
	})

	t.Run("claim probes replace by name and append new ones", func(t *testing.T) {
		sbx := newSandbox(probe("Active", "pool-cmd"), probe("Audit", "audit-cmd"))
		claimProbes := []v1alpha1.Probe{probe("Active", "claim-cmd"), probe("Cron", "cron-cmd")}
		require.NoError(t, modifyPickedSandbox(sbx, LockTypeUpdate, Options{
			AutoPausePolicy: requiresCronProbe,
			Probes:          claimProbes,
		}))
		// Pool order first with the claim version of "Active", then new names.
		assert.Equal(t, []v1alpha1.Probe{probe("Active", "claim-cmd"), probe("Audit", "audit-cmd"), probe("Cron", "cron-cmd")}, sbx.Spec.Probes)
		assert.Equal(t, requiresCronProbe, sbx.Spec.AutoPausePolicy)
		// Deep copy: changing the merged nested command never changes the claim.
		sbx.Spec.Probes[0].Probe.Exec.Command[0] = "modified"
		assert.Equal(t, "claim-cmd", claimProbes[0].Probe.Exec.Command[0])
	})
}

// TestNewSandboxFromSandboxSet_TemplateRef covers the SandboxTemplate
// resolution branch in newSandboxFromSandboxSet: when the SandboxSet uses
// spec.templateRef, the referenced SandboxTemplate must be fetched from the
// cache and its pod template labels/annotations propagated to the new
// Sandbox; if the SandboxTemplate cannot be resolved the function must
// return a NoAvailable error rather than panicking.
func TestNewSandboxFromSandboxSet_TemplateRef(t *testing.T) {
	utestutils.InitLogOutput()

	const templateName = "ref-sbs"
	const refName = "my-sbt"
	requiresActiveProbe := &v1alpha1.AutoPausePolicy{
		Pause: &v1alpha1.PausePolicy{
			WhenProbedIdleState: &v1alpha1.ProbedIdleStateRule{Probe: "Active"},
		},
	}
	maxPoolProbes := make([]v1alpha1.Probe, 0, autopause.MaxSandboxProbes)
	for i := range autopause.MaxSandboxProbes {
		maxPoolProbes = append(maxPoolProbes, v1alpha1.Probe{
			Name: fmt.Sprintf("pool-%d", i),
			Probe: corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{"true"}},
			}},
		})
	}

	tests := []struct {
		name       string
		createSBT  bool
		policy     *v1alpha1.AutoPausePolicy
		poolProbes []v1alpha1.Probe
		probes     []v1alpha1.Probe
		wantErr    string
		wantLabels map[string]string
		wantAnnos  map[string]string
	}{
		{
			name:      "templateRef resolved and labels inherited",
			createSBT: true,
			wantLabels: map[string]string{
				"app":                          "from-sbt",
				v1alpha1.LabelSandboxTemplate:  refName,
				v1alpha1.LabelSandboxPool:      templateName,
				v1alpha1.LabelSandboxIsClaimed: "false",
			},
			wantAnnos: map[string]string{
				"source":                           "sbt",
				v1alpha1.SandboxAnnotationPriority: "100",
			},
		},
		{
			name:      "templateRef not found returns NoAvailable error",
			createSBT: false,
			wantErr:   "cannot resolve sandbox template",
		},
		{
			name:      "templateRef sandbox missing required probe is rejected",
			createSBT: true,
			policy:    requiresActiveProbe,
			wantErr:   "new sandbox does not declare required probes [Active]",
		},
		{
			name:      "claim-carried probe satisfies templateRef policy",
			createSBT: true,
			policy:    requiresActiveProbe,
			probes:    []v1alpha1.Probe{{Name: "Active"}},
		},
		{
			name:       "current sandboxset merged probes over the limit is rejected",
			createSBT:  true,
			poolProbes: maxPoolProbes,
			probes: []v1alpha1.Probe{{
				Name: "claim-extra",
				Probe: corev1.Probe{ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{Command: []string{"true"}},
				}},
			}},
			wantErr: "new sandbox merged probes exceed the Sandbox limit of 16 (17 probes)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testInfra, fc := newClaimTestInfra(t)
			defer testInfra.Stop(t.Context())

			// SandboxSet that references the external SandboxTemplate.
			sbs := &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      templateName,
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Probes: tt.poolProbes,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: refName},
					},
				},
			}
			require.NoError(t, fc.Create(t.Context(), sbs))

			if tt.createSBT {
				sbt := &v1alpha1.SandboxTemplate{
					ObjectMeta: metav1.ObjectMeta{Name: refName, Namespace: "default"},
					Spec: v1alpha1.SandboxTemplateSpec{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels:      map[string]string{"app": "from-sbt"},
								Annotations: map[string]string{"source": "sbt"},
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "img:v1"}},
							},
						},
					},
				}
				require.NoError(t, fc.Create(t.Context(), sbt))
			}

			// Wait for the SandboxSet to be visible through the cache so
			// PickSandboxSet can find it.
			require.Eventually(t, func() bool {
				_, err := testInfra.Cache.PickSandboxSet(t.Context(), infracache.PickSandboxSetOptions{Name: templateName})
				return err == nil
			}, time.Second, 10*time.Millisecond)

			if tt.createSBT {
				// Also wait for the SandboxTemplate to be visible.
				require.Eventually(t, func() bool {
					got := &v1alpha1.SandboxTemplate{}
					return testInfra.Cache.GetClient().Get(t.Context(),
						client.ObjectKey{Namespace: "default", Name: refName}, got) == nil
				}, time.Second, 10*time.Millisecond)
			}

			opts := Options{
				Template:        templateName,
				User:            "test-user",
				AutoPausePolicy: tt.policy,
				Probes:          tt.probes,
			}
			sbx, _, err := newSandboxFromSandboxSet(t.Context(), opts, testInfra.Cache)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				var retriable RetriableError
				require.ErrorAs(t, err, &retriable)
				assert.Nil(t, sbx)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, sbx)
			for k, v := range tt.wantLabels {
				assert.Equal(t, v, sbx.GetLabels()[k], "label %s mismatch", k)
			}
			for k, v := range tt.wantAnnos {
				assert.Equal(t, v, sbx.GetAnnotations()[k], "annotation %s mismatch", k)
			}
			// The referenced template is materialized into the Sandbox spec.
			require.NotNil(t, sbx.Spec.Template)
			assert.Nil(t, sbx.Spec.TemplateRef)
			assert.Equal(t, "img:v1", sbx.Spec.Template.Spec.Containers[0].Image)
			assert.Equal(t, templateName, sbx.Spec.Template.Labels[v1alpha1.LabelSandboxPool])
			assert.Equal(t, refName, sbx.Spec.Template.Labels[v1alpha1.LabelSandboxTemplate])
		})
	}
}

// TestRunClaimPostProcesses_RuntimeTransport pins the claim-level behaviour of
// the runtime transport gate. runtime.TestTransportOptionsFor already covers the
// resolution matrix itself; what this test adds is how claim post-processing
// reacts to it:
//
//   - a sandbox advertising the runtime TLS capability while the caller holds no
//     bundle fails the claim instead of falling back to plaintext;
//   - the gate runs ahead of the InitRuntime and CSIMount branches, so it fails
//     even when the claim would make no runtime call at all — every case below
//     leaves both options nil, so moving the resolution inside either branch
//     would regress silently;
//   - that failure stays non-retriable, since retrying cannot conjure
//     certificate material;
//   - a legacy sandbox carrying no capability annotation keeps working
//     unchanged once the manager is configured with a bundle.
func TestRunClaimPostProcesses_RuntimeTransport(t *testing.T) {
	bundle := &runtime.TLSBundle{CABundle: []byte("pem")}

	tests := []struct {
		name        string
		annotations map[string]string
		bundle      *runtime.TLSBundle
		wantErr     bool
		errContains string
	}{
		{
			name:        "tls-capable sandbox without bundle fails the claim",
			annotations: map[string]string{v1alpha1.AnnotationRuntimeTLSPort: "49984"},
			wantErr:     true,
			errContains: "no client TLS bundle is configured",
		},
		{
			name:        "tls-capable sandbox with bundle passes the transport gate",
			annotations: map[string]string{v1alpha1.AnnotationRuntimeTLSPort: "49984"},
			bundle:      bundle,
		},
		{
			// The direct evidence that enabling runtime TLS leaves already
			// running sandboxes alone: they carry no capability annotation, so
			// they stay on the plaintext paths even though a bundle is present.
			name:   "legacy sandbox is unaffected once runtime TLS is enabled",
			bundle: bundle,
		},
		{
			name: "legacy sandbox stays on the plaintext path without a bundle",
		},
		{
			// A present but unparsable annotation means a broken injection
			// template, which must not be papered over either.
			name:        "unparsable capability annotation fails the claim",
			annotations: map[string]string{v1alpha1.AnnotationRuntimeTLSPort: "not-a-port"},
			bundle:      bundle,
			wantErr:     true,
			errContains: "invalid runtime TLS port annotation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &Sandbox{Sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-sandbox",
					Namespace:   "default",
					Annotations: tt.annotations,
				},
			}}
			// LockTypeUpdate without InplaceUpdate skips the wait-ready gate, and
			// nil InitRuntime/CSIMount skip every runtime call, leaving the
			// transport gate as the only step exercised here. The sandbox also
			// carries no identity annotations, so both token steps are skipped
			// and the nil cache is never dereferenced.
			opts := Options{RuntimeTLSBundle: tt.bundle}

			err := runClaimPostProcesses(t.Context(), sbx, LockTypeUpdate, opts, nil, &Metrics{})

			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errContains)
			// Missing or broken certificate material is a configuration error:
			// classifying it as retriable would make ClaimSandbox burn its retry
			// budget on an outcome that cannot change.
			var retryErr RetriableError
			assert.False(t, errors.As(err, &retryErr), "transport resolution failure must not be retriable")
		})
	}
}

// newPendingSandboxForTransport builds a freshly created sandbox: no runtime
// TLS stamp and not ready yet. It is the state the manager observes right
// after its own create, before the controller reconciles the pod.
func newPendingSandboxForTransport(name string) *v1alpha1.Sandbox {
	return &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			Annotations: map[string]string{},
		},
		Spec: v1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "main", Image: "test-image"}},
					},
				},
			},
		},
	}
}

// stampAndMarkReady replays what the sandbox controller does between the
// manager's create and the manager's wait-ready gate: it stamps the runtime
// TLS capability (a meta patch issued just before the pod is created, see
// PodControl.stampRuntimeTLSAnnotation) and then reports the sandbox Running
// and Ready. Both writes bump the resource version, so a manager holding the
// pre-stamp snapshot only observes them through a refresh.
func stampAndMarkReady(t *testing.T, c client.Client, sbx *v1alpha1.Sandbox, tlsPort string) {
	t.Helper()
	live := &v1alpha1.Sandbox{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(sbx), live))
	if tlsPort != "" {
		if live.Annotations == nil {
			live.Annotations = map[string]string{}
		}
		live.Annotations[v1alpha1.AnnotationRuntimeTLSPort] = tlsPort
		require.NoError(t, c.Update(t.Context(), live))
	}
	live.Status = v1alpha1.SandboxStatus{
		Phase:              v1alpha1.SandboxRunning,
		ObservedGeneration: live.Generation,
		Conditions: []metav1.Condition{
			{
				Type:               string(v1alpha1.SandboxConditionReady),
				Status:             metav1.ConditionTrue,
				Reason:             "PodReady",
				LastTransitionTime: metav1.Now(),
			},
		},
		PodInfo: v1alpha1.PodInfo{PodIP: "1.2.3.4"},
	}
	require.NoError(t, c.Status().Update(t.Context(), live))
}

// TestRunClaimPostProcesses_ResolvesTransportAfterWaitReady locks the ordering
// invariant between the wait-ready gate and the runtime transport resolution.
//
// The controller stamps AnnotationRuntimeTLSPort onto the sandbox immediately
// before it creates the pod (see PodControl.stampRuntimeTLSAnnotation), so the
// capability is only visible to the manager once WaitForSandboxReady has
// refreshed the in-memory object. Resolving the transport any earlier reads a
// pre-stamp snapshot and silently selects plaintext for a TLS-only runtime.
//
// The ordering is asserted through the observable behaviour of
// TransportOptionsFor: a stamped sandbox combined with a missing client TLS
// bundle is a configuration error, and that error can only surface when the
// resolution runs after the refresh. Both cases leave InitRuntime and CSIMount
// nil, so no runtime HTTP call is made.
func TestRunClaimPostProcesses_ResolvesTransportAfterWaitReady(t *testing.T) {
	tests := []struct {
		name string
		// stampedTLSPort is what the controller stamps onto the sandbox after
		// the manager already holds its pre-stamp snapshot.
		stampedTLSPort string
		wantErr        bool
		wantErrPart    string
	}{
		{
			name:           "stamp visible only after refresh surfaces the missing-bundle configuration error",
			stampedTLSPort: "49984",
			wantErr:        true,
			wantErrPart:    "no client TLS bundle is configured",
		},
		{
			name:           "unstamped sandbox resolves to plaintext and proceeds",
			stampedTLSPort: "",
			wantErr:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testInfra, fc := newClaimTestInfra(t)
			require.NotNil(t, testInfra)

			created := newPendingSandboxForTransport("claim-transport-order")
			require.NoError(t, fc.Create(t.Context(), created))
			// The snapshot the manager carries into post-processing: taken before
			// the controller stamps anything, hence an older resource version.
			snapshot := created.DeepCopy()

			stampAndMarkReady(t, fc, created, tt.stampedTLSPort)
			require.Eventually(t, func() bool {
				var got v1alpha1.Sandbox
				if err := testInfra.Cache.GetClient().Get(t.Context(), types.NamespacedName{
					Namespace: created.Namespace, Name: created.Name,
				}, &got); err != nil {
					return false
				}
				return got.Status.Phase == v1alpha1.SandboxRunning &&
					got.Annotations[v1alpha1.AnnotationRuntimeTLSPort] == tt.stampedTLSPort
			}, 2*time.Second, 10*time.Millisecond)

			sbx := &Sandbox{Sandbox: snapshot, Cache: testInfra.Cache}
			require.Empty(t, sbx.Sandbox.Annotations[v1alpha1.AnnotationRuntimeTLSPort],
				"the pre-stamp snapshot must not carry the capability")

			opts := Options{
				Namespace:        created.Namespace,
				WaitReadyTimeout: 10 * time.Second,
				// No bundle configured: a stamped sandbox must be reported as a
				// configuration error rather than silently downgraded.
				RuntimeTLSBundle: nil,
			}
			metrics := &Metrics{}

			err := runClaimPostProcesses(t.Context(), sbx, LockTypeCreate, opts, testInfra.Cache, metrics)

			if tt.wantErr {
				require.Error(t, err, "resolution must run after the refresh and reject the unusable stamp")
				assert.Contains(t, err.Error(), tt.wantErrPart)
				assert.NotErrorIs(t, err, RetriableError{},
					"a missing TLS bundle is a configuration error, not a retriable one")
			} else {
				require.NoError(t, err)
			}

			// The refresh performed by the wait-ready gate is what makes the
			// capability observable; without it the resolution above would have
			// silently picked plaintext.
			assert.Equal(t, tt.stampedTLSPort, sbx.Sandbox.Annotations[v1alpha1.AnnotationRuntimeTLSPort],
				"WaitForSandboxReady must refresh the sandbox before the transport is resolved")
		})
	}
}
