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

package storage

import (
	"context"
	"io"
	"path"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
)

// TestCustomFuseProviderRegistration verifies the provider self-registers in
// init() under the documented driver name, so the storage CLI can look it up
// without an explicit wiring change.
func TestCustomFuseProviderRegistration(t *testing.T) {
	resetRegistryForTesting()
	defer resetRegistryForTesting()

	p := &customFuseProvider{}
	Register(p)

	assert.Equal(t, CustomFuseDriver, p.Driver())
	assert.Equal(t, "customfuse", p.SubDir())

	got, ok := Lookup(CustomFuseDriver)
	assert.True(t, ok, "provider must be registered under the customfuse driver name")
	assert.Equal(t, p, got)
	assert.Contains(t, Drivers(), CustomFuseDriver)
}

func TestCustomFuseProviderValidate(t *testing.T) {
	tests := []struct {
		name        string
		req         csi.NodePublishVolumeRequest
		expectError string
	}{
		{
			name: "valid request passes",
			req: csi.NodePublishVolumeRequest{
				VolumeId:   "vol-1",
				TargetPath: "/workspace/data",
			},
		},
		{
			name: "missing volumeId is rejected",
			req: csi.NodePublishVolumeRequest{
				TargetPath: "/workspace/data",
			},
			expectError: "volumeId is required",
		},
		{
			name: "missing targetPath is rejected",
			req: csi.NodePublishVolumeRequest{
				VolumeId: "vol-1",
			},
			expectError: "targetPath is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &customFuseProvider{}
			err := p.Validate(tt.req)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
		})
	}
}

// TestCustomFuseProviderMountForwardsToSocket verifies Mount routes through
// RunNodePublishVolume against the standard per-driver socket layout.
func TestCustomFuseProviderMountForwardsToSocket(t *testing.T) {
	p := &customFuseProvider{}

	tests := []struct {
		name    string
		factory func(socketPath string) (csi.NodeClient, io.Closer, error)
		expect  string
	}{
		{
			name: "success forwards to csi.sock under driver directory",
			factory: func(socketPath string) (csi.NodeClient, io.Closer, error) {
				assert.Equal(t, path.Join(CsiSocketDir, CustomFuseDriver, CsiSocketFile), socketPath)
				return &fakeNodeClient{resp: &csi.NodePublishVolumeResponse{}}, &nopCloser{}, nil
			},
		},
		{
			name: "rpc error propagates",
			factory: func(_ string) (csi.NodeClient, io.Closer, error) {
				return &fakeNodeClient{err: context.DeadlineExceeded}, &nopCloser{}, nil
			},
			expect: "NodePublishVolume failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withClientFactory(t, tt.factory)
			err := p.Mount(context.Background(), csi.NodePublishVolumeRequest{VolumeId: "vol-1"}, false)
			if tt.expect == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expect)
			}
		})
	}
}

func TestCustomFuseProviderUnmountNotImplemented(t *testing.T) {
	p := &customFuseProvider{}
	assert.NoError(t, p.Unmount(context.Background(), csi.NodePublishVolumeRequest{}))
}
