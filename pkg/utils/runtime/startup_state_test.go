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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/runtime/config"
)

func TestRecordStartupDelivered(t *testing.T) {
	for _, tt := range []struct {
		name                                                   string
		change                                                 func(*agentsv1alpha1.Sandbox, *corev1.Pod)
		alreadyDelivered, conflict, onceConflict, lostResponse bool
		shallowCache                                           bool
		apiOp                                                  string
		apiErr                                                 error
		once, cancel                                           bool
		wantErr                                                string
		wantGets                                               int
	}{
		{name: "records the delivered generation"},
		{name: "same generation is immutable", alreadyDelivered: true},
		{name: "retries a lost completion response", lostResponse: true, wantGets: 4},
		{name: "retries one optimistic conflict", onceConflict: true, wantGets: 4},
		{name: "preserves exhausted optimistic conflict", conflict: true, wantErr: "record startup completion", wantGets: 12},
		{name: "rejected patch cannot contaminate shallow informer reads", conflict: true, shallowCache: true, wantErr: "record startup completion", wantGets: 12},
		{name: "retries transient sandbox read", apiOp: "sandbox", apiErr: errors.New("connection reset"), once: true, wantGets: 3},
		{name: "retries transient pod read", apiOp: "pod", apiErr: errors.New("connection reset"), once: true, wantGets: 4},
		{name: "sandbox forbidden is terminal", apiOp: "sandbox", apiErr: apierrors.NewForbidden(schema.GroupResource{Resource: "sandboxes"}, "claimed", errors.New("denied")), wantErr: "forbidden", wantGets: 1},
		{name: "sandbox missing is terminal", apiOp: "sandbox", apiErr: apierrors.NewNotFound(schema.GroupResource{Resource: "sandboxes"}, "claimed"), wantErr: "not found", wantGets: 1},
		{name: "pod forbidden is terminal", apiOp: "pod", apiErr: apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "claimed", errors.New("denied")), wantErr: "forbidden"},
		{name: "pod missing is terminal", apiOp: "pod", apiErr: apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "claimed"), wantErr: "not found"},
		{name: "patch forbidden is terminal", apiOp: "patch", apiErr: apierrors.NewForbidden(schema.GroupResource{Resource: "sandboxes"}, "claimed", errors.New("denied")), wantErr: "forbidden"},
		{name: "patch missing is terminal", apiOp: "patch", apiErr: apierrors.NewNotFound(schema.GroupResource{Resource: "sandboxes"}, "claimed"), wantErr: "not found"},
		{name: "cancellation stops retries", conflict: true, cancel: true, wantErr: "context canceled"},
		{name: "sandbox recreated", change: func(box *agentsv1alpha1.Sandbox, _ *corev1.Pod) { box.UID = "other-sandbox" }, wantErr: "generation changed"},
		{name: "pod recreated", change: func(_ *agentsv1alpha1.Sandbox, pod *corev1.Pod) { pod.UID = "other-pod" }, wantErr: "generation changed"},
		{name: "pod owner changed", change: func(_ *agentsv1alpha1.Sandbox, pod *corev1.Pod) { pod.OwnerReferences[0].UID = "other-sandbox" }, wantErr: "generation changed"},
		{name: "pod unowned", change: func(_ *agentsv1alpha1.Sandbox, pod *corev1.Pod) { pod.OwnerReferences = nil }, wantErr: "generation changed"},
		{name: "startup record missing", change: func(box *agentsv1alpha1.Sandbox, _ *corev1.Pod) {
			delete(box.Annotations, agentsv1alpha1.AnnotationInitRuntimeRequest)
		}, wantErr: "no recorded startup data"},
		{name: "startup record changed", change: func(box *agentsv1alpha1.Sandbox, _ *corev1.Pod) {
			box.Annotations[agentsv1alpha1.AnnotationInitRuntimeRequest] = `{"skipInit":true,"command":["false"]}`
		}, wantErr: "request changed"},
		{name: "completed startup payload changed", alreadyDelivered: true, change: func(box *agentsv1alpha1.Sandbox, _ *corev1.Pod) {
			box.Annotations[agentsv1alpha1.AnnotationInitRuntimeRequest] = `{"skipInit":true,"command":["false"],"startupDelivered":true,"startupPodUID":"pod-uid"}`
		}, wantErr: "request changed"},
		{name: "startup record invalid", change: func(box *agentsv1alpha1.Sandbox, _ *corev1.Pod) {
			box.Annotations[agentsv1alpha1.AnnotationInitRuntimeRequest] = `{`
		}, wantErr: "unmarshal"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
			request := config.InitRuntimeOptions{SkipInit: true, Command: []string{"true"}}
			if tt.alreadyDelivered {
				request.StartupDelivered = true
				request.StartupPodUID = "pod-uid"
			}
			raw, err := json.Marshal(request)
			require.NoError(t, err)
			box := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
				Name: "claimed", Namespace: "default", UID: "sandbox-uid",
				Annotations: map[string]string{agentsv1alpha1.AnnotationInitRuntimeRequest: string(raw)},
			}, Status: agentsv1alpha1.SandboxStatus{PodInfo: agentsv1alpha1.PodInfo{PodUID: "pod-uid"}}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: box.Name, Namespace: box.Namespace, UID: box.Status.PodInfo.PodUID,
				OwnerReferences: []metav1.OwnerReference{{UID: box.UID, Name: box.Name, Kind: "Sandbox", APIVersion: agentsv1alpha1.GroupVersion.String(), Controller: ptr.To(true)}},
			}}
			delivery := box.DeepCopy()
			if tt.change != nil {
				tt.change(box, pod)
			}
			box.Annotations["unrelated"] = "preserved"
			box.Status.Phase = agentsv1alpha1.SandboxRunning
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			patches, gets, failures := 0, 0, 0
			var sharedCache *agentsv1alpha1.Sandbox
			cli := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(box).WithObjects(box, pod).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					gets++
					_, isPod := obj.(*corev1.Pod)
					if tt.apiErr != nil && (tt.apiOp == "pod" && isPod || tt.apiOp == "sandbox" && !isPod) && (!tt.once || failures == 0) {
						failures++
						return tt.apiErr
					}
					if tt.shallowCache && !isPod {
						if sharedCache == nil {
							sharedCache = &agentsv1alpha1.Sandbox{}
							if err := c.Get(ctx, key, sharedCache, opts...); err != nil {
								return err
							}
						}
						*obj.(*agentsv1alpha1.Sandbox) = *sharedCache
						return nil
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					if tt.cancel {
						cancel()
					}
					if tt.apiOp == "patch" {
						return tt.apiErr
					}
					if tt.conflict || tt.onceConflict && patches == 1 {
						return apierrors.NewConflict(schema.GroupResource{Group: agentsv1alpha1.GroupVersion.Group, Resource: "sandboxes"}, box.Name, errors.New("concurrent update"))
					}
					payload, err := patch.Data(obj)
					if err != nil {
						return err
					}
					var body struct {
						Metadata struct {
							ResourceVersion string `json:"resourceVersion"`
						} `json:"metadata"`
					}
					require.NoError(t, json.Unmarshal(payload, &body))
					assert.NotEmpty(t, body.Metadata.ResourceVersion)
					if err := c.Patch(ctx, obj, patch, opts...); err != nil {
						return err
					}
					if tt.lostResponse && patches == 1 {
						return errors.New("completion response lost")
					}
					return nil
				},
			}).Build()
			err = RecordStartupDelivered(ctx, delivery, cli)
			wantGets := tt.wantGets
			if wantGets == 0 {
				wantGets = 2
			}
			assert.Equal(t, wantGets, gets, "semantic and authorization failures must not retry")
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				if tt.conflict {
					assert.True(t, apierrors.IsConflict(err))
					if !tt.cancel {
						assert.Equal(t, 6, patches)
					}
				}
				if tt.cancel {
					assert.ErrorIs(t, err, context.Canceled)
				}
				if tt.change != nil {
					assert.Zero(t, patches)
				}
				if tt.shallowCache {
					require.NotNil(t, sharedCache)
					assert.Equal(t, string(raw), sharedCache.Annotations[agentsv1alpha1.AnnotationInitRuntimeRequest])
					cached, readErr := GetInitRuntimeRequest(sharedCache)
					require.NoError(t, readErr)
					require.NotNil(t, cached)
					assert.False(t, cached.StartupDelivered)
				}
				return
			}
			require.NoError(t, err)
			stored := &agentsv1alpha1.Sandbox{}
			require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(box), stored))
			got, err := GetInitRuntimeRequest(stored)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.True(t, got.StartupDelivered)
			assert.Equal(t, "pod-uid", got.StartupPodUID)
			assert.Equal(t, request.Command, got.Command)
			assert.Equal(t, stored.Annotations, delivery.Annotations)
			assert.Equal(t, stored.ResourceVersion, delivery.ResourceVersion)
			assert.Equal(t, "preserved", stored.Annotations["unrelated"])
			assert.Equal(t, agentsv1alpha1.SandboxRunning, stored.Status.Phase)
			if tt.alreadyDelivered {
				assert.Zero(t, patches)
			} else if tt.onceConflict {
				assert.Equal(t, 2, patches)
			} else {
				assert.Equal(t, 1, patches)
			}
		})
	}
}
