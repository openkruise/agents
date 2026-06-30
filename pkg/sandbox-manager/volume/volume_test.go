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

package volume

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

// fakeInfra implements infra.Infrastructure for the volume methods only.
// Non-volume methods are provided by the embedded interface; calling them panics.
type fakeInfra struct {
	infra.Infrastructure // embed nil — panics if non-volume methods are called
	registerFn           func(ctx context.Context, opts infra.RegisterVolumeOptions) (infra.VolumeInfo, error)
	listFn               func(ctx context.Context, opts infra.ListVolumesOptions) ([]infra.VolumeInfo, error)
	getFn                func(ctx context.Context, opts infra.GetVolumeOptions) (infra.VolumeInfo, error)
	deleteFn             func(ctx context.Context, opts infra.DeleteVolumeOptions) (infra.DeleteVolumeResult, error)
}

func (f *fakeInfra) RegisterVolume(ctx context.Context, opts infra.RegisterVolumeOptions) (infra.VolumeInfo, error) {
	return f.registerFn(ctx, opts)
}

func (f *fakeInfra) ListVolumes(ctx context.Context, opts infra.ListVolumesOptions) ([]infra.VolumeInfo, error) {
	return f.listFn(ctx, opts)
}

func (f *fakeInfra) GetVolume(ctx context.Context, opts infra.GetVolumeOptions) (infra.VolumeInfo, error) {
	return f.getFn(ctx, opts)
}

func (f *fakeInfra) DeleteVolume(ctx context.Context, opts infra.DeleteVolumeOptions) (infra.DeleteVolumeResult, error) {
	return f.deleteFn(ctx, opts)
}

// ---------------------------------------------------------------------------
// RegisterVolume
// ---------------------------------------------------------------------------

func TestManager_RegisterVolume(t *testing.T) {
	tests := []struct {
		name        string
		infraReturn infra.VolumeInfo
		infraErr    error
		opts        infra.RegisterVolumeOptions
		expectError string
	}{
		{
			name: "success — delegates to infra and returns VolumeInfo",
			infraReturn: infra.VolumeInfo{
				VolumeID: "pv-001",
				Name:     "my-vol",
				PvName:   "pv-001",
				SizeGB:   10,
			},
			infraErr:    nil,
			opts:        infra.RegisterVolumeOptions{Namespace: "team-a", Name: "my-vol", PvName: "pv-001", SizeGB: 10},
			expectError: "",
		},
		{
			name:        "failure — infra error is propagated as-is",
			infraReturn: infra.VolumeInfo{},
			infraErr:    errors.New("conflict: pv already registered"),
			opts:        infra.RegisterVolumeOptions{Namespace: "team-a", Name: "my-vol", PvName: "pv-001", SizeGB: 10},
			expectError: "conflict: pv already registered",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calledOpts infra.RegisterVolumeOptions
			fake := &fakeInfra{
				registerFn: func(_ context.Context, opts infra.RegisterVolumeOptions) (infra.VolumeInfo, error) {
					calledOpts = opts
					return tt.infraReturn, tt.infraErr
				},
			}
			mgr := NewManager(fake)

			info, err := mgr.RegisterVolume(context.Background(), tt.opts)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.infraReturn, info)
				// Verify opts were forwarded correctly
				assert.Equal(t, tt.opts, calledOpts)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ListVolumes
// ---------------------------------------------------------------------------

func TestManager_ListVolumes(t *testing.T) {
	tests := []struct {
		name        string
		infraReturn []infra.VolumeInfo
		infraErr    error
		opts        infra.ListVolumesOptions
		expectError string
	}{
		{
			name: "success — returns volume list from infra",
			infraReturn: []infra.VolumeInfo{
				{VolumeID: "pv-1", Name: "vol-1", PvName: "pv-1", SizeGB: 5},
				{VolumeID: "pv-2", Name: "vol-2", PvName: "pv-2", SizeGB: 10},
			},
			infraErr:    nil,
			opts:        infra.ListVolumesOptions{Namespace: "team-b"},
			expectError: "",
		},
		{
			name:        "failure — infra error is propagated as-is",
			infraReturn: nil,
			infraErr:    errors.New("internal: cache unavailable"),
			opts:        infra.ListVolumesOptions{Namespace: "team-b"},
			expectError: "internal: cache unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calledOpts infra.ListVolumesOptions
			fake := &fakeInfra{
				listFn: func(_ context.Context, opts infra.ListVolumesOptions) ([]infra.VolumeInfo, error) {
					calledOpts = opts
					return tt.infraReturn, tt.infraErr
				},
			}
			mgr := NewManager(fake)

			volumes, err := mgr.ListVolumes(context.Background(), tt.opts)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.infraReturn, volumes)
				assert.Equal(t, tt.opts, calledOpts)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GetVolume
// ---------------------------------------------------------------------------

func TestManager_GetVolume(t *testing.T) {
	tests := []struct {
		name        string
		infraReturn infra.VolumeInfo
		infraErr    error
		opts        infra.GetVolumeOptions
		expectError string
	}{
		{
			name: "success — returns VolumeInfo from infra",
			infraReturn: infra.VolumeInfo{
				VolumeID:  "pv-abc",
				Name:      "my-data",
				PvName:    "pv-abc",
				SizeGB:    20,
			},
			infraErr:    nil,
			opts:        infra.GetVolumeOptions{Namespace: "team-c", VolumeID: "pv-abc"},
			expectError: "",
		},
		{
			name:        "failure — not found error propagated as-is",
			infraReturn: infra.VolumeInfo{},
			infraErr:    errors.New("not found: volume pv-xyz does not exist"),
			opts:        infra.GetVolumeOptions{Namespace: "team-c", VolumeID: "pv-xyz"},
			expectError: "not found: volume pv-xyz does not exist",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calledOpts infra.GetVolumeOptions
			fake := &fakeInfra{
				getFn: func(_ context.Context, opts infra.GetVolumeOptions) (infra.VolumeInfo, error) {
					calledOpts = opts
					return tt.infraReturn, tt.infraErr
				},
			}
			mgr := NewManager(fake)

			info, err := mgr.GetVolume(context.Background(), tt.opts)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.infraReturn, info)
				assert.Equal(t, tt.opts, calledOpts)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// DeleteVolume
// ---------------------------------------------------------------------------

func TestManager_DeleteVolume(t *testing.T) {
	tests := []struct {
		name        string
		infraReturn infra.DeleteVolumeResult
		infraErr    error
		opts        infra.DeleteVolumeOptions
		expectError string
	}{
		{
			name: "success — non-forced delete with no mounts",
			infraReturn: infra.DeleteVolumeResult{
				AffectedSandboxIDs: nil,
				ForcedDelete:       false,
			},
			infraErr:    nil,
			opts:        infra.DeleteVolumeOptions{Namespace: "team-d", VolumeID: "pv-empty", Force: false},
			expectError: "",
		},
		{
			name: "success — forced delete with mounted sandboxes",
			infraReturn: infra.DeleteVolumeResult{
				AffectedSandboxIDs: []string{"sbx-001", "sbx-002"},
				ForcedDelete:       true,
			},
			infraErr:    nil,
			opts:        infra.DeleteVolumeOptions{Namespace: "team-d", VolumeID: "pv-mounted", Force: true},
			expectError: "",
		},
		{
			name:        "failure — conflict when mounted without force",
			infraReturn: infra.DeleteVolumeResult{},
			infraErr:    errors.New("conflict: volume is currently mounted by sbx-001"),
			opts:        infra.DeleteVolumeOptions{Namespace: "team-d", VolumeID: "pv-mounted", Force: false},
			expectError: "conflict: volume is currently mounted by sbx-001",
		},
		{
			name:        "failure — not found error propagated as-is",
			infraReturn: infra.DeleteVolumeResult{},
			infraErr:    errors.New("not found: volume pv-gone does not exist"),
			opts:        infra.DeleteVolumeOptions{Namespace: "team-d", VolumeID: "pv-gone", Force: false},
			expectError: "not found: volume pv-gone does not exist",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calledOpts infra.DeleteVolumeOptions
			fake := &fakeInfra{
				deleteFn: func(_ context.Context, opts infra.DeleteVolumeOptions) (infra.DeleteVolumeResult, error) {
					calledOpts = opts
					return tt.infraReturn, tt.infraErr
				},
			}
			mgr := NewManager(fake)

			result, err := mgr.DeleteVolume(context.Background(), tt.opts)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.infraReturn, result)
				assert.Equal(t, tt.opts, calledOpts)
			}
		})
	}
}
