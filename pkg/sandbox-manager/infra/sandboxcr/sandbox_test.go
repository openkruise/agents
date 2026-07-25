/*
Copyright 2025.

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
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/proxyutils"
	"github.com/openkruise/agents/pkg/utils/runtime"
	"github.com/openkruise/agents/pkg/utils/timeout"
	testutils "github.com/openkruise/agents/test/utils"
)

func ConvertPodToSandboxCR(pod *corev1.Pod) *v1alpha1.Sandbox {
	sbx := &v1alpha1.Sandbox{
		ObjectMeta: pod.ObjectMeta,
		Spec: v1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: pod.Spec,
				},
			},
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxPhase(pod.Status.Phase),
			PodInfo: v1alpha1.PodInfo{
				PodIP: pod.Status.PodIP,
			},
		},
	}
	cond := utils.GetPodCondition(&pod.Status, corev1.PodReady)
	if cond != nil {
		sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
			Type:   string(v1alpha1.SandboxConditionReady),
			Status: metav1.ConditionStatus(cond.Status),
		})
	}
	if strings.HasPrefix(pod.Name, "paused") {
		sbx.Spec.Paused = true
	}
	return sbx
}

func TestSandbox_SaveTimeout(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	tests := []struct {
		name          string
		current       timeout.Options
		requested     timeout.Options
		expectUpdated bool
		expectTimeout timeout.Options
	}{
		{
			name: "updates when requested timeout differs",
			current: timeout.Options{
				ShutdownTime: base.Add(10 * time.Minute),
			},
			requested: timeout.Options{
				ShutdownTime: base.Add(20 * time.Minute),
			},
			expectUpdated: true,
			expectTimeout: timeout.Options{ShutdownTime: base.Add(20 * time.Minute)},
		},
		{
			name: "skips when requested timeout matches current",
			current: timeout.Options{
				ShutdownTime: base.Add(10 * time.Minute),
			},
			requested: timeout.Options{
				ShutdownTime: base.Add(10 * time.Minute),
			},
			expectUpdated: false,
			expectTimeout: timeout.Options{ShutdownTime: base.Add(10 * time.Minute)},
		},
		{
			name: "directly overwrites with shorter timeout",
			current: timeout.Options{
				ShutdownTime: base.Add(25 * time.Minute),
			},
			requested: timeout.Options{
				ShutdownTime: base.Add(10 * time.Minute),
			},
			expectUpdated: true,
			expectTimeout: timeout.Options{ShutdownTime: base.Add(10 * time.Minute)},
		},
		{
			name: "directly clears finite fields when requested",
			current: timeout.Options{
				PauseTime:    base.Add(10 * time.Minute),
				ShutdownTime: base.Add(20 * time.Minute),
			},
			requested:     timeout.Options{},
			expectUpdated: true,
			expectTimeout: timeout.Options{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infraInstance, fc := NewTestInfra(t)

			sbx := createTestSandboxWithDefaults("test-sandbox", "default")
			setTimeout(sbx, infra.SetTimeoutOptions{Timeout: tt.current})
			CreateSandboxWithStatus(t, fc, sbx)

			var sandbox infra.Sandbox
			require.Eventually(t, func() bool {
				var err error
				sandbox, err = infraInstance.GetSandbox(t.Context(), infra.GetSandboxOptions{
					SandboxID: utils.GetSandboxID(sbx),
					Namespace: sbx.Namespace,
				})
				return err == nil
			}, time.Second, 10*time.Millisecond)

			result, err := sandbox.SaveTimeout(t.Context(), infra.SetTimeoutOptions{
				Timeout: tt.requested,
			})
			require.NoError(t, err)
			assert.Equal(t, tt.expectUpdated, result.Updated)
			assert.True(t, timeout.Equal(tt.expectTimeout, sandbox.GetTimeout()))

			var updated v1alpha1.Sandbox
			require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: sbx.Namespace, Name: sbx.Name}, &updated))
			assert.True(t, timeout.Equal(tt.expectTimeout, timeout.GetTimeoutFromSandbox(&updated)))
		})
	}
}

const testRetentionAnnotation = "example.openkruise.io/retention"

func TestSandbox_SaveTimeoutExtraAnnotations(t *testing.T) {
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name                string
		current             timeout.Options
		requested           timeout.Options
		initialAnnotations  map[string]string
		extraAnnotations    map[string]string
		expectUpdated       bool
		expectedAnnotations map[string]string
	}{
		{
			name:                "accepted write adds extra annotation",
			current:             timeout.Options{ShutdownTime: base.Add(time.Hour)},
			requested:           timeout.Options{ShutdownTime: base.Add(2 * time.Hour)},
			extraAnnotations:    map[string]string{testRetentionAnnotation: "30m", "example.openkruise.io/extra": "yes"},
			expectUpdated:       true,
			expectedAnnotations: map[string]string{testRetentionAnnotation: "30m", "example.openkruise.io/extra": "yes"},
		},
		{
			name:                "accepted write overwrites existing annotation",
			current:             timeout.Options{ShutdownTime: base.Add(time.Hour)},
			requested:           timeout.Options{ShutdownTime: base.Add(2 * time.Hour)},
			initialAnnotations:  map[string]string{testRetentionAnnotation: "10m"},
			extraAnnotations:    map[string]string{testRetentionAnnotation: "30m"},
			expectUpdated:       true,
			expectedAnnotations: map[string]string{testRetentionAnnotation: "30m"},
		},
		{
			name:                "equal timeout still backfills missing extra annotation",
			current:             timeout.Options{ShutdownTime: base.Add(time.Hour)},
			requested:           timeout.Options{ShutdownTime: base.Add(time.Hour)},
			extraAnnotations:    map[string]string{testRetentionAnnotation: "30m"},
			expectUpdated:       true,
			expectedAnnotations: map[string]string{testRetentionAnnotation: "30m"},
		},
		{
			name:                "equal timeout and matching annotation skips update",
			current:             timeout.Options{ShutdownTime: base.Add(time.Hour)},
			requested:           timeout.Options{ShutdownTime: base.Add(time.Hour)},
			initialAnnotations:  map[string]string{testRetentionAnnotation: "30m"},
			extraAnnotations:    map[string]string{testRetentionAnnotation: "30m"},
			expectUpdated:       false,
			expectedAnnotations: map[string]string{testRetentionAnnotation: "30m"},
		},
		{
			name:                "equal timeout still overwrites differing annotation",
			current:             timeout.Options{ShutdownTime: base.Add(time.Hour)},
			requested:           timeout.Options{ShutdownTime: base.Add(time.Hour)},
			initialAnnotations:  map[string]string{testRetentionAnnotation: "10m"},
			extraAnnotations:    map[string]string{testRetentionAnnotation: "30m"},
			expectUpdated:       true,
			expectedAnnotations: map[string]string{testRetentionAnnotation: "30m"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infraInstance, fc := NewTestInfra(t)
			sbx := createTestSandboxWithDefaults("test-sandbox", "default")
			setTimeout(sbx, infra.SetTimeoutOptions{Timeout: tt.current})
			if len(tt.initialAnnotations) > 0 {
				sbx.Annotations = tt.initialAnnotations
			}
			CreateSandboxWithStatus(t, fc, sbx)

			var sandbox infra.Sandbox
			require.Eventually(t, func() bool {
				var err error
				sandbox, err = infraInstance.GetSandbox(t.Context(), infra.GetSandboxOptions{
					SandboxID: utils.GetSandboxID(sbx),
					Namespace: sbx.Namespace,
				})
				return err == nil
			}, time.Second, 10*time.Millisecond)

			result, err := sandbox.SaveTimeout(t.Context(), infra.SetTimeoutOptions{
				Timeout:          tt.requested,
				ExtraAnnotations: tt.extraAnnotations,
			})
			require.NoError(t, err)
			assert.Equal(t, tt.expectUpdated, result.Updated)

			var updated v1alpha1.Sandbox
			require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: sbx.Namespace, Name: sbx.Name}, &updated))
			for key, want := range tt.expectedAnnotations {
				assert.Equal(t, want, updated.Annotations[key])
			}
		})
	}
}

func TestSandbox_SaveTimeout_OnConflict(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	current := timeout.Options{ShutdownTime: base.Add(10 * time.Minute)}

	tests := []struct {
		name           string
		winner         timeout.Options
		requested      timeout.Options
		useGetter      bool
		expectUpdated  bool
		expectTimeout  timeout.Options
		expectAttempts int32
	}{
		{
			name:           "static timeout overwrites fresh winner after conflict",
			winner:         timeout.Options{ShutdownTime: base.Add(30 * time.Minute)},
			requested:      timeout.Options{ShutdownTime: base.Add(20 * time.Minute)},
			expectUpdated:  true,
			expectTimeout:  timeout.Options{ShutdownTime: base.Add(20 * time.Minute)},
			expectAttempts: 2,
		},
		{
			name:           "getter updates when fresh winner is earlier than requested",
			winner:         timeout.Options{ShutdownTime: base.Add(20 * time.Minute)},
			requested:      timeout.Options{ShutdownTime: base.Add(30 * time.Minute)},
			useGetter:      true,
			expectUpdated:  true,
			expectTimeout:  timeout.Options{ShutdownTime: base.Add(30 * time.Minute)},
			expectAttempts: 2,
		},
		{
			name:           "getter skips when fresh winner is later than requested",
			winner:         timeout.Options{ShutdownTime: base.Add(40 * time.Minute)},
			requested:      timeout.Options{ShutdownTime: base.Add(30 * time.Minute)},
			useGetter:      true,
			expectUpdated:  false,
			expectTimeout:  timeout.Options{ShutdownTime: base.Add(40 * time.Minute)},
			expectAttempts: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := createTestSandboxWithDefaults("test-sandbox", "default")
			setTimeout(sbx, infra.SetTimeoutOptions{Timeout: current})
			key := types.NamespacedName{Namespace: sbx.Namespace, Name: sbx.Name}

			var updateAttempts atomic.Int32
			builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sbx)
			builder = builder.WithInterceptorFuncs(interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if _, ok := obj.(*v1alpha1.Sandbox); ok {
						if updateAttempts.Add(1) == 1 {
							latest := &v1alpha1.Sandbox{}
							require.NoError(t, c.Get(ctx, key, latest))
							patched := latest.DeepCopy()
							setTimeout(patched, infra.SetTimeoutOptions{Timeout: tt.winner})
							require.NoError(t, c.Update(ctx, patched))
							return apierrors.NewConflict(
								schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "sandboxes"},
								obj.GetName(),
								errors.New("forced conflict"),
							)
						}
					}
					return c.Update(ctx, obj, opts...)
				},
			})
			fc := builder.Build()
			testCache := &retryUpdateTestProvider{
				client:         fc,
				apiReader:      &countingReader{Reader: fc},
				claimedSandbox: sbx.DeepCopy(),
			}

			sandbox := AsSandbox(sbx.DeepCopy(), testCache)
			opts := infra.SetTimeoutOptions{Timeout: tt.requested}
			if tt.useGetter {
				opts = infra.SetTimeoutOptions{TimeoutGetter: func(snapshot infra.TimeoutSnapshot) timeout.Options {
					if snapshot.Timeout.ShutdownTime.Before(tt.requested.ShutdownTime) {
						return tt.requested
					}
					return snapshot.Timeout
				}}
			}
			result, err := sandbox.SaveTimeout(t.Context(), opts)
			require.NoError(t, err)
			assert.Equal(t, tt.expectUpdated, result.Updated)
			assert.Equal(t, tt.expectAttempts, updateAttempts.Load())
			assert.Equal(t, int32(1), testCache.apiReader.Calls())
			assert.True(t, timeout.Equal(tt.expectTimeout, sandbox.GetTimeout()))

			var updated v1alpha1.Sandbox
			require.NoError(t, fc.Get(t.Context(), key, &updated))
			assert.True(t, timeout.Equal(tt.expectTimeout, timeout.GetTimeoutFromSandbox(&updated)))
		})
	}
}

type countingReader struct {
	client.Reader

	getCalls atomic.Int32
}

func (r *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	r.getCalls.Add(1)
	return r.Reader.Get(ctx, key, obj, opts...)
}

func (r *countingReader) Calls() int32 {
	return r.getCalls.Load()
}

type retryUpdateTestProvider struct {
	infracache.Provider

	client         client.Client
	apiReader      *countingReader
	clientGetCalls atomic.Int32
	claimedSandbox *v1alpha1.Sandbox
}

func (p *retryUpdateTestProvider) GetClaimedSandbox(_ context.Context, options infracache.GetClaimedSandboxOptions) (*v1alpha1.Sandbox, error) {
	expectedID := utils.GetSandboxID(p.claimedSandbox)
	if options.SandboxID != expectedID {
		return nil, errors.New("unexpected sandbox ID")
	}
	return p.claimedSandbox.DeepCopy(), nil
}

func (p *retryUpdateTestProvider) GetClient() client.Client {
	return p.client
}

func (p *retryUpdateTestProvider) GetAPIReader() client.Reader {
	return p.apiReader
}

func newRetryUpdateTestCache(
	t *testing.T,
	sbx *v1alpha1.Sandbox,
	claimedSandbox *v1alpha1.Sandbox,
	updateInterceptor func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error,
) (*retryUpdateTestProvider, client.Client) {
	t.Helper()

	scheme := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, idx := range infracache.GetIndexFuncs() {
		builder = builder.WithIndex(idx.Obj, idx.FieldName, idx.Extract)
	}
	builder = builder.WithStatusSubresource(&v1alpha1.Sandbox{})
	builder = builder.WithObjects(sbx)
	if updateInterceptor != nil {
		builder = builder.WithInterceptorFuncs(interceptor.Funcs{Update: updateInterceptor})
	}

	fc := builder.Build()
	provider := &retryUpdateTestProvider{
		apiReader:      &countingReader{Reader: fc},
		claimedSandbox: claimedSandbox,
	}
	provider.client = interceptor.NewClient(fc, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			provider.clientGetCalls.Add(1)
			return c.Get(ctx, key, obj, opts...)
		},
	})
	return provider, fc
}

func TestSandbox_retryUpdate(t *testing.T) {
	tests := []struct {
		name              string
		initialPaused     bool
		wrapperPaused     *bool
		claimedPaused     *bool
		modifier          func(t *testing.T) ModifierFunc
		conflictPatch     func(t *testing.T, c client.WithWatch, latest *v1alpha1.Sandbox)
		expectUpdated     bool
		expectUpdateCalls int32
		expectClientGets  int32
		expectAPIReader   int32
		expectPaused      bool
		expectError       string
	}{
		{
			name:          "modifier returns false skips update and refreshes sandbox",
			initialPaused: false,
			modifier: func(t *testing.T) ModifierFunc {
				return func(sbx *v1alpha1.Sandbox) (bool, error) {
					assert.False(t, sbx.Spec.Paused)
					return false, nil
				}
			},
			expectUpdated:     false,
			expectUpdateCalls: 0,
			expectClientGets:  1,
			expectPaused:      false,
		},
		{
			name:          "modifier returns true updates sandbox",
			initialPaused: true,
			modifier: func(t *testing.T) ModifierFunc {
				return func(sbx *v1alpha1.Sandbox) (bool, error) {
					sbx.Spec.Paused = false
					return true, nil
				}
			},
			expectUpdated:     true,
			expectUpdateCalls: 1,
			expectClientGets:  1,
			expectPaused:      false,
		},
		{
			name:          "modifier error aborts update",
			initialPaused: true,
			modifier: func(t *testing.T) ModifierFunc {
				return func(sbx *v1alpha1.Sandbox) (bool, error) {
					sbx.Spec.Paused = false
					return false, errors.New("modifier failed")
				}
			},
			expectUpdated:     false,
			expectUpdateCalls: 0,
			expectClientGets:  1,
			expectPaused:      true,
			expectError:       "modifier failed",
		},
		{
			name:          "conflict refreshes from api reader",
			initialPaused: true,
			modifier: func(t *testing.T) ModifierFunc {
				return func(sbx *v1alpha1.Sandbox) (bool, error) {
					if !sbx.Spec.Paused {
						return false, nil
					}
					sbx.Spec.Paused = false
					return true, nil
				}
			},
			conflictPatch: func(t *testing.T, c client.WithWatch, latest *v1alpha1.Sandbox) {
				patched := latest.DeepCopy()
				patched.Spec.Paused = false
				require.NoError(t, c.Patch(t.Context(), patched, client.MergeFrom(latest)))
			},
			expectUpdated:     false,
			expectUpdateCalls: 1,
			expectClientGets:  1,
			expectAPIReader:   1,
			expectPaused:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var updateCalls atomic.Int32
			sbx := createTestSandboxWithDefaults("test-sandbox", "default")
			sbx.Spec.Paused = tt.initialPaused
			claimedSandbox := sbx.DeepCopy()
			if tt.claimedPaused != nil {
				claimedSandbox.Spec.Paused = *tt.claimedPaused
			}
			key := types.NamespacedName{Namespace: sbx.Namespace, Name: sbx.Name}

			testCache, fc := newRetryUpdateTestCache(t, sbx, claimedSandbox, func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if updateCalls.Add(1) == 1 && tt.conflictPatch != nil {
					latest := &v1alpha1.Sandbox{}
					require.NoError(t, c.Get(ctx, key, latest))
					tt.conflictPatch(t, c, latest)
					return apierrors.NewConflict(
						schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "sandboxes"},
						obj.GetName(),
						errors.New("forced conflict"),
					)
				}
				return c.Update(ctx, obj, opts...)
			})
			wrapper := sbx.DeepCopy()
			if tt.wrapperPaused != nil {
				wrapper.Spec.Paused = *tt.wrapperPaused
			}
			s := AsSandbox(wrapper, testCache)

			updated, err := s.retryUpdate(t.Context(), tt.modifier(t))
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.expectUpdated, updated)
			assert.Equal(t, tt.expectUpdateCalls, updateCalls.Load())
			assert.Equal(t, tt.expectClientGets, testCache.clientGetCalls.Load())
			assert.Equal(t, tt.expectAPIReader, testCache.apiReader.Calls())
			assert.Equal(t, tt.expectPaused, s.Sandbox.Spec.Paused)

			var stored v1alpha1.Sandbox
			require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: sbx.Namespace, Name: sbx.Name}, &stored))
			assert.Equal(t, tt.expectPaused, stored.Spec.Paused)
		})
	}
}

func TestSandbox_GetTemplate(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "returns sandbox pool label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelSandboxTemplate: "test-template",
					},
				},
			},
			want: "test-template",
		},
		{
			name: "empty template",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelSandboxTemplate: "",
					},
				},
			},
			want: "",
		},
		{
			name: "no template label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{},
				},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := AsSandbox(ConvertPodToSandboxCR(tt.pod), nil)
			if got := s.GetTemplate(); got != tt.want {
				t.Errorf("GetTemplate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSandbox_GetResource(t *testing.T) {
	cpuQuantity1, _ := resource.ParseQuantity("1000m")
	cpuQuantity2, _ := resource.ParseQuantity("500m")
	memoryQuantity1, _ := resource.ParseQuantity("1024Mi")
	memoryQuantity2, _ := resource.ParseQuantity("512Mi")

	tests := []struct {
		name string
		pod  *corev1.Pod
		want infra.SandboxResource
	}{
		{
			name: "single container with resources",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQuantity1,
									corev1.ResourceMemory: memoryQuantity1,
								},
							},
						},
					},
				},
			},
			want: infra.SandboxResource{
				Requests: infra.ResourceList{
					CPUMilli: 1000,
					MemoryMB: 1024,
				},
			},
		},
		{
			name: "requests and limits are reported separately",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1500m"),
								corev1.ResourceMemory: resource.MustParse("1537Mi"),
							},
						},
					}},
				},
			},
			want: infra.SandboxResource{
				Requests: infra.ResourceList{
					CPUMilli: 500,
					MemoryMB: 512,
				},
				Limits: infra.ResourceList{
					CPUMilli: 1500,
					MemoryMB: 1537,
				},
			},
		},
		{
			name: "request memory floors while limit memory ceilings",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: *resource.NewQuantity(1024*1024+1, resource.BinarySI),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: *resource.NewQuantity(1024*1024+1, resource.BinarySI),
							},
						},
					}},
				},
			},
			want: infra.SandboxResource{
				Requests: infra.ResourceList{
					MemoryMB: 1,
				},
				Limits: infra.ResourceList{
					MemoryMB: 2,
				},
			},
		},
		{
			name: "multiple containers with resources",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQuantity1,
									corev1.ResourceMemory: memoryQuantity1,
								},
							},
						},
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQuantity2,
									corev1.ResourceMemory: memoryQuantity2,
								},
							},
						},
					},
				},
			},
			want: infra.SandboxResource{
				Requests: infra.ResourceList{
					CPUMilli: 1500,
					MemoryMB: 1536,
				},
			},
		},
		{
			name: "no containers",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{},
				},
			},
			want: infra.SandboxResource{},
		},
		{
			name: "containers without resources",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{},
							},
						},
					},
				},
			},
			want: infra.SandboxResource{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := AsSandbox(ConvertPodToSandboxCR(tt.pod), nil)
			got := s.GetResource()
			assert.Equal(t, tt.want.Requests, got.Requests)
			assert.Equal(t, tt.want.Limits, got.Limits)
		})
	}
}

func TestSandbox_InplaceRefresh(t *testing.T) {
	initialSandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				"initial":                      "value",
				v1alpha1.LabelSandboxIsClaimed: "true",
			},
		},
	}

	cache, fc, err := cachetest.NewTestCache(t)
	assert.NoError(t, err)
	require.NoError(t, cache.Run(t.Context()))
	defer cache.Stop(t.Context())
	require.NoError(t, fc.Create(t.Context(), initialSandbox))
	time.Sleep(10 * time.Millisecond)

	updatedSandbox := initialSandbox.DeepCopy()
	updatedSandbox.Labels["updated"] = "new-value"
	require.NoError(t, fc.Update(t.Context(), updatedSandbox))
	time.Sleep(10 * time.Millisecond)

	s := AsSandbox(initialSandbox, cache)

	assert.Equal(t, "value", s.Sandbox.Labels["initial"])
	assert.Empty(t, s.Sandbox.Labels["updated"])

	err = s.InplaceRefresh(t.Context(), false)
	assert.NoError(t, err)

	assert.Equal(t, "value", s.Sandbox.Labels["initial"])
	assert.Equal(t, "new-value", s.Sandbox.Labels["updated"])

	err = s.InplaceRefresh(t.Context(), true)
	assert.NoError(t, err)
	assert.Equal(t, "value", s.Sandbox.Labels["initial"])
	assert.Equal(t, "new-value", s.Sandbox.Labels["updated"])
}

//goland:noinspection GoDeprecation
func TestSandbox_Kill(t *testing.T) {
	tests := []struct {
		name              string
		hasDeletionTime   bool
		expectDeleteError bool
	}{
		{
			name:              "normal deletion",
			hasDeletionTime:   false,
			expectDeleteError: false,
		},
		{
			name:              "already marked for deletion",
			hasDeletionTime:   true,
			expectDeleteError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
			}

			if tt.hasDeletionTime {
				now := metav1.Now()
				sandbox.DeletionTimestamp = &now
			}

			cache, fc, err := cachetest.NewTestCache(t)
			require.NoError(t, err)
			require.NoError(t, cache.Run(t.Context()))
			defer cache.Stop(t.Context())
			require.NoError(t, fc.Create(t.Context(), sandbox))

			s := AsSandbox(sandbox, cache)

			var getSbx v1alpha1.Sandbox
			err = fc.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "test-sandbox"}, &getSbx)
			assert.NoError(t, err)

			err = s.Kill(t.Context())
			if tt.expectDeleteError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			if !tt.hasDeletionTime {
				err = fc.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "test-sandbox"}, &getSbx)
				assert.Error(t, err)
				assert.True(t, strings.Contains(err.Error(), "not found"))
			}
		})
	}
}

func TestSandbox_GetTimeout(t *testing.T) {
	now := metav1.Now()
	future := metav1.NewTime(now.Add(time.Hour))

	tests := []struct {
		name     string
		sandbox  *v1alpha1.Sandbox
		expected timeout.Options
	}{
		{
			name: "with timeout set",
			sandbox: &v1alpha1.Sandbox{
				Spec: v1alpha1.SandboxSpec{
					ShutdownTime: &future,
					PauseTime:    &now,
				},
			},
			expected: timeout.Options{
				ShutdownTime: future.Time,
				PauseTime:    now.Time,
			},
		},
		{
			name: "without shutdown time",
			sandbox: &v1alpha1.Sandbox{
				Spec: v1alpha1.SandboxSpec{
					ShutdownTime: nil,
				},
			},
			expected: timeout.Options{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Sandbox{
				Sandbox: tt.sandbox,
			}
			result := s.GetTimeout()
			assert.True(t, timeout.Equal(tt.expected, result))
		})
	}
}

func TestSandbox_SetImageAndGetImage(t *testing.T) {
	t.Run("set and get image with template", func(t *testing.T) {
		s := &Sandbox{
			Sandbox: &v1alpha1.Sandbox{
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "app",
										Image: "nginx:old",
									},
								},
							},
						},
					},
				},
			},
		}

		s.SetImage("nginx:new")
		assert.Equal(t, "nginx:new", s.GetImage())
	})

	t.Run("set and get image with nil template", func(t *testing.T) {
		s := &Sandbox{
			Sandbox: &v1alpha1.Sandbox{
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: nil,
					},
				},
			},
		}

		assert.NotPanics(t, func() {
			s.SetImage("nginx:new")
		})
		assert.Equal(t, "", s.GetImage())
	})
}

func TestSandbox_GetSandboxID(t *testing.T) {
	s := &Sandbox{
		Sandbox: &v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test-sandbox",
			},
		},
	}

	assert.Equal(t, "default--test-sandbox", s.GetSandboxID())
}

func TestSandbox_Request(t *testing.T) {
	orig := proxyutils.DefaultRequestFunc
	t.Cleanup(func() {
		proxyutils.DefaultRequestFunc = orig
	})

	proxyutils.DefaultRequestFunc = func(ctx context.Context, sbx *v1alpha1.Sandbox, method, path string, port int, body io.Reader) (*http.Response, error) {
		assert.Equal(t, "GET", method)
		assert.Equal(t, "/healthz", path)
		assert.Equal(t, 8080, port)
		assert.Equal(t, "default", sbx.Namespace)
		assert.Equal(t, "test-sandbox", sbx.Name)
		return &http.Response{StatusCode: http.StatusOK}, nil
	}

	s := &Sandbox{
		Sandbox: &v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test-sandbox",
			},
		},
	}
	resp, err := s.Request(context.Background(), "GET", "/healthz", 8080, nil)
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestSandbox_SetTimeout(t *testing.T) {
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name               string
		initialTimeout     timeout.Options
		initialAnnotations map[string]string
		opts               infra.SetTimeoutOptions
		expectChanged      bool
		expectTimeout      timeout.Options
		expectAnnotations  map[string]string
	}{
		{
			name: "sets timeout on sandbox without existing timeout",
			opts: infra.SetTimeoutOptions{
				Timeout: timeout.Options{
					ShutdownTime: base.Add(2 * time.Hour),
					PauseTime:    base.Add(time.Hour),
				},
			},
			expectChanged: true,
			expectTimeout: timeout.Options{
				ShutdownTime: base.Add(2 * time.Hour),
				PauseTime:    base.Add(time.Hour),
			},
		},
		{
			name: "returns false when timeout and annotations already match",
			initialTimeout: timeout.Options{
				ShutdownTime: base.Add(2 * time.Hour),
			},
			initialAnnotations: map[string]string{testRetentionAnnotation: "30m"},
			opts: infra.SetTimeoutOptions{
				Timeout:          timeout.Options{ShutdownTime: base.Add(2 * time.Hour)},
				ExtraAnnotations: map[string]string{testRetentionAnnotation: "30m"},
			},
			expectTimeout:     timeout.Options{ShutdownTime: base.Add(2 * time.Hour)},
			expectAnnotations: map[string]string{testRetentionAnnotation: "30m"},
		},
		{
			name: "getter resolves target from current timeout",
			initialTimeout: timeout.Options{
				ShutdownTime: base.Add(time.Hour),
			},
			opts: infra.SetTimeoutOptions{
				TimeoutGetter: func(snapshot infra.TimeoutSnapshot) timeout.Options {
					return timeout.Options{ShutdownTime: snapshot.Timeout.ShutdownTime.Add(time.Hour)}
				},
			},
			expectChanged: true,
			expectTimeout: timeout.Options{ShutdownTime: base.Add(2 * time.Hour)},
		},
		{
			name: "clears existing timeout",
			initialTimeout: timeout.Options{
				ShutdownTime: base.Add(2 * time.Hour),
				PauseTime:    base.Add(time.Hour),
			},
			opts:          infra.SetTimeoutOptions{Timeout: timeout.Options{}},
			expectChanged: true,
			expectTimeout: timeout.Options{},
		},
		{
			name:               "updates annotations when timeout matches",
			initialTimeout:     timeout.Options{ShutdownTime: base.Add(2 * time.Hour)},
			initialAnnotations: map[string]string{testRetentionAnnotation: "10m"},
			opts: infra.SetTimeoutOptions{
				Timeout:          timeout.Options{ShutdownTime: base.Add(2 * time.Hour)},
				ExtraAnnotations: map[string]string{testRetentionAnnotation: "30m", "example.openkruise.io/extra": "yes"},
			},
			expectChanged: true,
			expectTimeout: timeout.Options{ShutdownTime: base.Add(2 * time.Hour)},
			expectAnnotations: map[string]string{
				testRetentionAnnotation:       "30m",
				"example.openkruise.io/extra": "yes",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &v1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Annotations: tt.initialAnnotations}}
			setTimeout(sbx, infra.SetTimeoutOptions{Timeout: tt.initialTimeout})
			s := &Sandbox{Sandbox: sbx}

			changed := s.SetTimeout(tt.opts)

			assert.Equal(t, tt.expectChanged, changed)
			assert.True(t, timeout.Equal(tt.expectTimeout, s.GetTimeout()))
			for key, value := range tt.expectAnnotations {
				assert.Equal(t, value, s.Annotations[key])
			}
		})
	}
}

func TestSandbox_GetClaimTime(t *testing.T) {
	now := time.Now()
	claimTimeString := now.Format(time.RFC3339)

	tests := []struct {
		name     string
		sandbox  *v1alpha1.Sandbox
		expected time.Time
	}{
		{
			name: "with claim time annotation",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1alpha1.AnnotationClaimTime: claimTimeString,
					},
				},
			},
			expected: now,
		},
		{
			name: "without claim time annotation",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			expected: time.Time{},
		},
		{
			name: "with invalid claim time annotation",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1alpha1.AnnotationClaimTime: "invalid-time-format",
					},
				},
			},
			expected: time.Time{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Sandbox{
				Sandbox: tt.sandbox,
			}
			result, _ := s.GetClaimTime()
			if tt.name == "with claim time annotation" {
				assert.WithinDuration(t, tt.expected, result, time.Second)
			} else {
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestSandbox_GetRoute(t *testing.T) {
	tests := []struct {
		name          string
		sandbox       *v1alpha1.Sandbox
		expectedRoute proxy.Route
	}{
		{
			name: "available sandbox with owner",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "available-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						v1alpha1.AnnotationOwner: "test-owner",
					},
					OwnerReferences: GetSbsOwnerReference(),
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
					PodInfo: v1alpha1.PodInfo{
						PodIP: "10.0.0.1",
					},
				},
			},
			expectedRoute: proxy.Route{
				IP:    "10.0.0.1",
				ID:    "default--available-sandbox",
				Owner: "test-owner",
				State: v1alpha1.SandboxStateAvailable,
			},
		},
		{
			name: "running sandbox without owner",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "running-sandbox",
					Namespace: "default",
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
					PodInfo: v1alpha1.PodInfo{
						PodIP: "10.0.0.2",
					},
				},
			},
			expectedRoute: proxy.Route{
				IP:    "10.0.0.2",
				ID:    "default--running-sandbox",
				Owner: "",
				State: v1alpha1.SandboxStateRunning,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Sandbox{
				Sandbox: tt.sandbox,
			}

			route := s.GetRoute()
			assert.Equal(t, tt.expectedRoute, route)
		})
	}
}

func TestSandbox_CSIMount(t *testing.T) {
	tests := []struct {
		name         string
		result       runtime.RunCommandResult
		processError *string
		driver       string
		req          *csi.NodePublishVolumeRequest
		expectError  string
	}{
		{
			name: "successful csi mount",
			result: runtime.RunCommandResult{
				ExitCode: 0,
				Exited:   true,
			},
			driver: "csi-driver",
			req: &csi.NodePublishVolumeRequest{
				VolumeId: "volume-id",
			},
		},
		{
			name: "exits non-zero",
			result: runtime.RunCommandResult{
				ExitCode: 1,
				Exited:   true,
			},
			driver: "csi-driver",
			req: &csi.NodePublishVolumeRequest{
				VolumeId: "volume-id",
			},
			expectError: "command failed: [1]",
		},
		{
			name: "with process error",
			result: runtime.RunCommandResult{
				ExitCode: 0,
				Exited:   true,
			},
			req: &csi.NodePublishVolumeRequest{
				VolumeId: "volume-id",
			},
			processError: ptr.To("some error"),
			expectError:  "some error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtimeOpts := testutils.TestRuntimeServerOptions{
				RunCommandResult:      tt.result,
				RunCommandImmediately: true,
				RunCommandError:       tt.processError,
			}
			server := testutils.NewTestRuntimeServer(runtimeOpts)
			defer server.Close()

			cache, _, err := cachetest.NewTestCache(t)
			assert.NoError(t, err)
			require.NoError(t, cache.Run(t.Context()))
			defer cache.Stop(t.Context())
			sbx := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-sandbox",
					Annotations: map[string]string{
						v1alpha1.AnnotationRuntimeURL:         server.URL,
						v1alpha1.AnnotationRuntimeAccessToken: runtime.AccessToken,
					},
				},
			}
			sandbox := AsSandbox(sbx, cache)
			request, err := utils.EncodeBase64Proto(tt.req)
			assert.NoError(t, err)
			err = sandbox.CSIMount(t.Context(), tt.driver, request)
			if tt.expectError != "" {
				assert.Error(t, err)
				assert.ErrorContains(t, err, tt.expectError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestSandbox_TriggerRecycle(t *testing.T) {
	tests := []struct {
		name               string
		initialAnnotations map[string]string
		patchInterceptor   func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error
		expectError        string
	}{
		{
			name:               "success with nil annotations",
			initialAnnotations: nil,
		},
		{
			name:               "success with existing annotations",
			initialAnnotations: map[string]string{"existing": "value"},
		},
		{
			name:               "patch error returns error",
			initialAnnotations: nil,
			patchInterceptor: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				return errors.New("forced patch error")
			},
			expectError: "forced patch error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := createTestSandboxWithDefaults("test-sandbox", "default")
			sbx.Annotations = tt.initialAnnotations

			scheme := k8sruntime.NewScheme()
			utilruntime.Must(clientgoscheme.AddToScheme(scheme))
			utilruntime.Must(v1alpha1.AddToScheme(scheme))

			builder := fake.NewClientBuilder().WithScheme(scheme)
			for _, idx := range infracache.GetIndexFuncs() {
				builder = builder.WithIndex(idx.Obj, idx.FieldName, idx.Extract)
			}
			builder = builder.WithStatusSubresource(&v1alpha1.Sandbox{})
			builder = builder.WithObjects(sbx)
			if tt.patchInterceptor != nil {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{Patch: tt.patchInterceptor})
			}
			fc := builder.Build()

			provider := &retryUpdateTestProvider{
				client:         fc,
				apiReader:      &countingReader{Reader: fc},
				claimedSandbox: sbx,
			}
			s := AsSandbox(sbx, provider)

			err := s.TriggerRecycle(t.Context())
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
				// Verify annotation is set on the in-memory object
				assert.Equal(t, "true", s.Sandbox.Annotations[v1alpha1.AnnotationCleanup])
				// Verify annotation is persisted to the fake client
				var stored v1alpha1.Sandbox
				require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: sbx.Namespace, Name: sbx.Name}, &stored))
				assert.Equal(t, "true", stored.Annotations[v1alpha1.AnnotationCleanup])
				// Verify existing annotations are preserved
				for k, v := range tt.initialAnnotations {
					assert.Equal(t, v, stored.Annotations[k])
				}
			}
		})
	}
}
