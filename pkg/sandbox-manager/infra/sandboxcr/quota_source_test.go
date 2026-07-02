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
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
)

func TestQuotaSnapshotFromSandbox(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*v1alpha1.Sandbox)
		wantOwner   string
		wantLock    string
		wantLive    bool
		wantRunning bool
		wantCPU     int64
		wantMemory  int64
	}{
		{
			name:        "running live sandbox",
			wantOwner:   "owner-1",
			wantLock:    "lock-1",
			wantLive:    true,
			wantRunning: true,
			wantCPU:     1500,
			wantMemory:  2,
		},
		{
			name: "paused live sandbox",
			mutate: func(sbx *v1alpha1.Sandbox) {
				sbx.Spec.Paused = true
				sbx.Status.Phase = v1alpha1.SandboxPaused
			},
			wantOwner:  "owner-1",
			wantLock:   "lock-1",
			wantLive:   true,
			wantCPU:    1500,
			wantMemory: 2,
		},
		{
			name: "reuse requested sandbox is not live",
			mutate: func(sbx *v1alpha1.Sandbox) {
				sbx.Annotations[v1alpha1.AnnotationReuse] = "true"
				sbx.Annotations[v1alpha1.AnnotationReuseEnabled] = "true"
			},
			wantOwner:  "owner-1",
			wantLock:   "lock-1",
			wantCPU:    1500,
			wantMemory: 2,
		},
		{
			name: "reusing sandbox is not live",
			mutate: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.Phase = v1alpha1.SandboxReusing
			},
			wantOwner:  "owner-1",
			wantLock:   "lock-1",
			wantCPU:    1500,
			wantMemory: 2,
		},
		{
			name: "terminating sandbox is not live",
			mutate: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.Phase = v1alpha1.SandboxTerminating
			},
			wantOwner:  "owner-1",
			wantLock:   "lock-1",
			wantCPU:    1500,
			wantMemory: 2,
		},
		{
			name: "empty owner and lock are preserved for quota skip",
			mutate: func(sbx *v1alpha1.Sandbox) {
				sbx.Annotations[v1alpha1.AnnotationOwner] = ""
				sbx.Annotations[v1alpha1.AnnotationLock] = ""
			},
			wantLive:    true,
			wantRunning: true,
			wantCPU:     1500,
			wantMemory:  2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := quotaSourceSandbox()
			if tt.mutate != nil {
				tt.mutate(sbx)
			}

			got, ok := quotaSnapshotFromSandbox(sbx)
			require.True(t, ok)
			assert.Equal(t, tt.wantOwner, got.Owner)
			assert.Equal(t, tt.wantLock, got.LockString)
			assert.Equal(t, tt.wantLive, got.Live)
			assert.Equal(t, tt.wantRunning, got.Running)
			assert.Equal(t, tt.wantCPU, got.Resource.Limits.CPUMilli)
			assert.Equal(t, tt.wantMemory, got.Resource.Limits.MemoryMB)
		})
	}
}

func TestQuotaSnapshotFromSandboxIsValue(t *testing.T) {
	sbx := quotaSourceSandbox()
	got, ok := quotaSnapshotFromSandbox(sbx)
	require.True(t, ok)

	sbx.Annotations[v1alpha1.AnnotationOwner] = "changed"
	sbx.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("64Mi")

	assert.Equal(t, "owner-1", got.Owner)
	assert.Equal(t, int64(2), got.Resource.Limits.MemoryMB)
}

func TestQuotaEventFromObject(t *testing.T) {
	tests := []struct {
		name        string
		obj         any
		deleted     bool
		wantOK      bool
		wantDeleted bool
	}{
		{
			name:        "delete sandbox emits tombstone event",
			obj:         quotaSourceSandbox(),
			deleted:     true,
			wantOK:      true,
			wantDeleted: true,
		},
		{
			name: "delete unknown tombstone emits tombstone event",
			obj: toolscache.DeletedFinalStateUnknown{
				Key: "default/sbx",
				Obj: quotaSourceSandbox(),
			},
			deleted:     true,
			wantOK:      true,
			wantDeleted: true,
		},
		{
			name:    "invalid tombstone is ignored",
			obj:     toolscache.DeletedFinalStateUnknown{Key: "default/sbx", Obj: &corev1.Pod{}},
			deleted: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := testutil.ToFloat64(quotaSourceEventDropTotal.WithLabelValues("invalid_tombstone"))
			got, ok := quotaEventFromObject(tt.obj, tt.deleted)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.wantDeleted, got.Deleted)
				assert.Equal(t, "owner-1", got.Snapshot.Owner)
				assert.Equal(t, "lock-1", got.Snapshot.LockString)
				return
			}
			assert.Equal(t, before+1, testutil.ToFloat64(quotaSourceEventDropTotal.WithLabelValues("invalid_tombstone")))
		})
	}
}

func TestQuotaEventFromUpdateObject(t *testing.T) {
	tests := []struct {
		name   string
		oldObj any
		newObj any
		wantOK bool
	}{
		{
			name:   "unchanged snapshot is ignored",
			oldObj: quotaSourceSandbox(),
			newObj: quotaSourceSandbox(),
		},
		{
			name:   "changed snapshot emits event",
			oldObj: quotaSourceSandbox(),
			newObj: func() *v1alpha1.Sandbox {
				sbx := quotaSourceSandbox()
				sbx.Spec.Paused = true
				sbx.Status.Phase = v1alpha1.SandboxPaused
				return sbx
			}(),
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := quotaEventFromUpdateObject(tt.oldObj, tt.newObj)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, "owner-1", got.Snapshot.Owner)
				assert.Equal(t, "lock-1", got.Snapshot.LockString)
			}
		})
	}
}

func TestQuotaSourceListLiveQuotaSandboxesByOwner(t *testing.T) {
	live := quotaSourceSandbox()
	dead := quotaSourceSandbox()
	dead.Name = "dead"
	dead.Annotations[v1alpha1.AnnotationLock] = "dead-lock"
	dead.Status.Phase = v1alpha1.SandboxReusing
	cache, _, err := cachetest.NewTestCache(t, live, dead)
	require.NoError(t, err)
	source := &Infra{Cache: cache}

	got, err := source.ListLiveQuotaSandboxesByOwner(t.Context(), "owner-1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "lock-1", got[0].LockString)
}

func quotaSourceSandbox() *v1alpha1.Sandbox {
	return &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx",
			Namespace: "default",
			Annotations: map[string]string{
				v1alpha1.AnnotationOwner: "owner-1",
				v1alpha1.AnnotationLock:  "lock-1",
			},
		},
		Spec: v1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("1Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1500m"),
								corev1.ResourceMemory: *resource.NewQuantity(1024*1024+1, resource.BinarySI),
							},
						},
					}}},
				},
			},
		},
		Status: v1alpha1.SandboxStatus{Phase: v1alpha1.SandboxRunning},
	}
}
