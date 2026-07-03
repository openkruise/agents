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

package main

import (
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestValidateGeneralParams(t *testing.T) {
	tests := []struct {
		name    string
		csiReq  csi.NodePublishVolumeRequest
		wantErr bool
	}{
		{
			name: "valid request with pod uid",
			csiReq: csi.NodePublishVolumeRequest{
				VolumeContext: map[string]string{
					"csi.storage.k8s.io/pod.uid": "test-pod-uid",
				},
			},
			wantErr: false,
		},
		{
			name: "request without pod uid",
			csiReq: csi.NodePublishVolumeRequest{
				VolumeContext: map[string]string{
					"csi.storage.k8s.io/pod.uid": "",
				},
			},
			wantErr: true,
		},
		{
			name: "request without pod uid key",
			csiReq: csi.NodePublishVolumeRequest{
				VolumeContext: map[string]string{},
			},
			wantErr: true,
		},
		{
			name:    "nil volume context",
			csiReq:  csi.NodePublishVolumeRequest{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGeneralParams(tt.csiReq)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateGeneralParams() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
