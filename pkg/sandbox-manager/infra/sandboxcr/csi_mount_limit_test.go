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
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

func withCSIMountCountLimit(t *testing.T, limit int) {
	t.Helper()
	orig := csiMountCountLimit
	csiMountCountLimit = limit
	t.Cleanup(func() { csiMountCountLimit = orig })
}

func csiMountOptionsWithCount(n int) *config.CSIMountOptions {
	return &config.CSIMountOptions{MountOptionList: make([]config.MountConfig, n)}
}

func csiMountConfigAnnotation(t *testing.T, n int) string {
	t.Helper()
	cfgs := make([]v1alpha1.CSIMountConfig, n)
	for i := range cfgs {
		cfgs[i] = v1alpha1.CSIMountConfig{
			PvName:    fmt.Sprintf("pv-%d", i),
			MountPath: fmt.Sprintf("/mnt/%d", i),
		}
	}
	raw, err := json.Marshal(cfgs)
	require.NoError(t, err)
	return string(raw)
}

func TestValidateAndInitClaimOptions_CSIMountLimit(t *testing.T) {
	base := infra.ClaimSandboxOptions{
		User:        "test-user",
		Template:    "test-template",
		InitRuntime: &config.InitRuntimeOptions{},
	}

	tests := []struct {
		name      string
		limit     int
		mount     *config.CSIMountOptions
		wantError bool
	}{
		{name: "max int limit allows 11 mounts", limit: math.MaxInt, mount: csiMountOptionsWithCount(11)},
		{name: "limit 10 allows 10 mounts", limit: 10, mount: csiMountOptionsWithCount(10)},
		{name: "limit 10 rejects 11 mounts", limit: 10, mount: csiMountOptionsWithCount(11), wantError: true},
		{name: "nil CSIMount is unrestricted", limit: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withCSIMountCountLimit(t, tt.limit)
			opts := base
			opts.CSIMount = tt.mount
			_, err := ValidateAndInitClaimOptions(opts)
			if !tt.wantError {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, managererrors.ErrorBadRequest, managererrors.GetErrCode(err))
		})
	}
}

func TestCloneSandbox_CSIMountLimit(t *testing.T) {
	withCSIMountCountLimit(t, 10)

	tests := []struct {
		name       string
		request    *config.CSIMountOptions
		annoCount  int
		annoRaw    string
		wantCreate bool
		wantErr    string
		wantCode   managererrors.ErrorCode
	}{
		{
			name:     "request 11 rejected before create",
			request:  csiMountOptionsWithCount(11),
			wantErr:  "CSI mounts",
			wantCode: managererrors.ErrorBadRequest,
		},
		{
			name:       "request 3 overrides checkpoint 11",
			request:    csiMountOptionsWithCount(3),
			annoCount:  11,
			wantCreate: true,
		},
		{
			name:    "unparsable checkpoint annotation rejected before create",
			annoRaw: "not-json",
			wantErr: "csi mount config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testInfra, fc := NewTestInfra(t, config.SandboxManagerOptions{
				MaxClaimWorkers: 1,
				MaxCreateQPS:    1000,
			})
			// Each case builds a fresh NewTestInfra, so a constant ID cannot collide.
			const checkpointID = "csi-mnt-lim"
			raw := tt.annoRaw
			if tt.annoCount > 0 {
				raw = csiMountConfigAnnotation(t, tt.annoCount)
			}
			createCloneCheckpointWithCSIConfig(t, fc, testInfra.Cache, checkpointID, raw)

			var created atomic.Int32
			origCreateSandbox := DefaultCreateSandbox
			DefaultCreateSandbox = func(ctx context.Context, sbx *v1alpha1.Sandbox, c client.Client) (*v1alpha1.Sandbox, error) {
				created.Add(1)
				return nil, apierrors.NewBadRequest("sandbox create rejected")
			}
			t.Cleanup(func() { DefaultCreateSandbox = origCreateSandbox })

			opts, err := ValidateAndInitCloneOptions(infra.CloneSandboxOptions{
				User:             "test-user",
				CheckPointID:     checkpointID,
				WaitReadyTimeout: 20 * time.Millisecond,
				CloneTimeout:     time.Second,
				CSIMount:         tt.request,
			})
			require.NoError(t, err)

			sbx, _, err := testInfra.CloneSandbox(t.Context(), opts)
			require.Error(t, err)
			assert.Nil(t, sbx)
			if tt.wantCreate {
				assert.Equal(t, int32(1), created.Load(), "sandbox create should be attempted")
				assert.Contains(t, err.Error(), "sandbox create rejected")
				assert.NotContains(t, err.Error(), "CSI mounts")
				return
			}
			assert.Equal(t, int32(0), created.Load(), "sandbox create must not run")
			if tt.wantCode != "" {
				assert.Equal(t, tt.wantCode, managererrors.GetErrCode(err))
			}
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func createCloneCheckpointWithCSIConfig(t *testing.T, c client.Client, cache infracache.Provider, checkpointID, raw string) {
	t.Helper()
	sbt := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: checkpointID, Namespace: "default"},
		Spec: v1alpha1.SandboxTemplateSpec{
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "test-image"}},
				},
			},
		},
	}
	require.NoError(t, c.Create(t.Context(), sbt))

	cp := &v1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      checkpointID,
			Namespace: "default",
			Labels: map[string]string{
				v1alpha1.LabelSandboxTemplate: checkpointID,
			},
			Annotations: map[string]string{},
		},
		Status: v1alpha1.CheckpointStatus{CheckpointId: checkpointID},
	}
	if raw != "" {
		cp.Annotations[v1alpha1.AnnotationCSIVolumeConfig] = raw
	}
	require.NoError(t, c.Create(t.Context(), cp))
	require.Eventually(t, func() bool {
		got, err := cache.GetCheckpoint(t.Context(), infracache.GetCheckpointOptions{CheckpointID: checkpointID})
		if err != nil {
			return false
		}
		if raw == "" {
			return true
		}
		return got.GetAnnotations()[v1alpha1.AnnotationCSIVolumeConfig] == raw
	}, time.Second, 10*time.Millisecond)
}
