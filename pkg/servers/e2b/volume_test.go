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
	"net/http"
	"testing"

	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCreateVolume(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "test-user",
	}

	// Create StorageClass for validation tests
	fc := getTestCRClient(controller)
	immediateBinding := storagev1.VolumeBindingImmediate
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "standard",
		},
		Provisioner:       "kubernetes.io/no-provisioner",
		VolumeBindingMode: &immediateBinding,
	}
	err := fc.Create(context.Background(), sc)
	require.NoError(t, err)

	tests := []struct {
		name        string
		setupReq    func(t *testing.T) *http.Request
		expectError string
		errorCode   int
	}{
		{
			name: "user not authenticated",
			setupReq: func(t *testing.T) *http.Request {
				req := NewRequest(t, nil, models.NewVolumeRequest{
					Name: "test-volume",
				}, nil, nil)
				req.Header.Set(models.ExtensionHeaderVolumeSize, "1Gi")
				req.Header.Set(models.ExtensionHeaderVolumeStorageClass, "standard")
				req.Header.Set(models.ExtensionHeaderVolumeAccessMode, "ReadWriteOnce")
				return req
			},
			expectError: "User is empty",
			errorCode:   http.StatusUnauthorized,
		},
		{
			name: "invalid request body",
			setupReq: func(t *testing.T) *http.Request {
				return NewRequest(t, nil, "invalid-json-body", nil, user)
			},
			expectError: "invalid request body",
			errorCode:   http.StatusBadRequest,
		},
		{
			name: "invalid storage size",
			setupReq: func(t *testing.T) *http.Request {
				req := NewRequest(t, nil, models.NewVolumeRequest{
					Name: "test-volume-bad-size",
				}, nil, user)
				req.Header.Set(models.ExtensionHeaderVolumeStorageClass, "standard")
				req.Header.Set(models.ExtensionHeaderVolumeAccessMode, "ReadWriteOnce")
				return req
			},
			expectError: "invalid storage size",
			errorCode:   http.StatusBadRequest,
		},
		{
			name: "name is required",
			setupReq: func(t *testing.T) *http.Request {
				req := NewRequest(t, nil, models.NewVolumeRequest{
					Name: "",
				}, nil, user)
				req.Header.Set(models.ExtensionHeaderVolumeSize, "1Gi")
				req.Header.Set(models.ExtensionHeaderVolumeStorageClass, "standard")
				req.Header.Set(models.ExtensionHeaderVolumeAccessMode, "ReadWriteOnce")
				return req
			},
			expectError: "name is required",
			errorCode:   http.StatusBadRequest,
		},
		{
			name: "storage class is required",
			setupReq: func(t *testing.T) *http.Request {
				req := NewRequest(t, nil, models.NewVolumeRequest{
					Name: "test-volume-missing-sc",
				}, nil, user)
				req.Header.Set(models.ExtensionHeaderVolumeSize, "1Gi")
				req.Header.Set(models.ExtensionHeaderVolumeAccessMode, "ReadWriteOnce")
				return req
			},
			expectError: "storage class is required",
			errorCode:   http.StatusBadRequest,
		},
		{
			name: "access mode is required",
			setupReq: func(t *testing.T) *http.Request {
				req := NewRequest(t, nil, models.NewVolumeRequest{
					Name: "test-volume-missing-am",
				}, nil, user)
				req.Header.Set(models.ExtensionHeaderVolumeSize, "1Gi")
				req.Header.Set(models.ExtensionHeaderVolumeStorageClass, "standard")
				return req
			},
			expectError: "access mode is required",
			errorCode:   http.StatusBadRequest,
		},
		{
			name: "invalid access mode",
			setupReq: func(t *testing.T) *http.Request {
				req := NewRequest(t, nil, models.NewVolumeRequest{
					Name: "test-volume-bad-access",
				}, nil, user)
				req.Header.Set(models.ExtensionHeaderVolumeSize, "1Gi")
				req.Header.Set(models.ExtensionHeaderVolumeStorageClass, "standard")
				req.Header.Set(models.ExtensionHeaderVolumeAccessMode, "InvalidMode")
				return req
			},
			expectError: "Failed to create volume",
			errorCode:   http.StatusInternalServerError,
		},
		{
			name: "invalid wait bound seconds",
			setupReq: func(t *testing.T) *http.Request {
				req := NewRequest(t, nil, models.NewVolumeRequest{
					Name: "test-volume-bad-wait",
				}, nil, user)
				req.Header.Set(models.ExtensionHeaderVolumeSize, "1Gi")
				req.Header.Set(models.ExtensionHeaderVolumeStorageClass, "standard")
				req.Header.Set(models.ExtensionHeaderVolumeAccessMode, "ReadWriteOnce")
				req.Header.Set(models.ExtensionHeaderVolumeWaitSuccessSeconds, "-1")
				return req
			},
			expectError: "cannot be negative",
			errorCode:   http.StatusBadRequest,
		},
		{
			name: "storage class not found",
			setupReq: func(t *testing.T) *http.Request {
				req := NewRequest(t, nil, models.NewVolumeRequest{
					Name: "test-volume-bad-sc",
				}, nil, user)
				req.Header.Set(models.ExtensionHeaderVolumeSize, "1Gi")
				req.Header.Set(models.ExtensionHeaderVolumeStorageClass, "non-existent")
				req.Header.Set(models.ExtensionHeaderVolumeAccessMode, "ReadWriteOnce")
				return req
			},
			expectError: "Failed to create volume",
			errorCode:   http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.setupReq(t)
			resp, apiErr := controller.CreateVolume(req)

			if tt.expectError != "" {
				require.NotNil(t, apiErr, "expected error but got nil")
				if tt.errorCode != 0 {
					assert.Equal(t, tt.errorCode, apiErr.Code)
				}
				assert.Contains(t, apiErr.Message, tt.expectError)
			} else {
				require.Nil(t, apiErr, "unexpected error: %v", apiErr)
				require.NotNil(t, resp.Body, "response body should not be nil")
				assert.NotEmpty(t, resp.Body.VolumeID, "VolumeID should not be empty")
				assert.NotEmpty(t, resp.Body.Name, "Name should not be empty")
				assert.Equal(t, http.StatusCreated, resp.Code)
			}
		})
	}
}

func TestListVolumes(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "test-user",
	}

	tests := []struct {
		name        string
		setupReq    func(t *testing.T) *http.Request
		expectError string
		errorCode   int
	}{
		{
			name: "success with no volumes",
			setupReq: func(t *testing.T) *http.Request {
				return NewRequest(t, nil, nil, nil, user)
			},
			expectError: "",
		},
		{
			name: "user not authenticated",
			setupReq: func(t *testing.T) *http.Request {
				return NewRequest(t, nil, nil, nil, nil)
			},
			expectError: "User is empty",
			errorCode:   http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.setupReq(t)
			resp, apiErr := controller.ListVolumes(req)

			if tt.expectError != "" {
				require.NotNil(t, apiErr, "expected error but got nil")
				if tt.errorCode != 0 {
					assert.Equal(t, tt.errorCode, apiErr.Code)
				}
				assert.Contains(t, apiErr.Message, tt.expectError)
			} else {
				require.Nil(t, apiErr, "unexpected error: %v", apiErr)
				assert.NotNil(t, resp.Body, "response body should not be nil")
			}
		})
	}
}

func TestGetVolume(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "test-user",
	}

	tests := []struct {
		name        string
		setupReq    func(t *testing.T) *http.Request
		expectError string
		errorCode   int
	}{
		{
			name: "user not authenticated",
			setupReq: func(t *testing.T) *http.Request {
				return NewRequest(t, nil, nil, map[string]string{
					"volumeID": "some-id",
				}, nil)
			},
			expectError: "User is empty",
			errorCode:   http.StatusUnauthorized,
		},
		{
			name: "volumeID is empty",
			setupReq: func(t *testing.T) *http.Request {
				return NewRequest(t, nil, nil, map[string]string{
					"volumeID": "",
				}, user)
			},
			expectError: "volumeID is required",
			errorCode:   http.StatusBadRequest,
		},
		{
			name: "volume not found",
			setupReq: func(t *testing.T) *http.Request {
				return NewRequest(t, nil, nil, map[string]string{
					"volumeID": "non-existent-pv",
				}, user)
			},
			expectError: "Volume not found",
			errorCode:   http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.setupReq(t)
			resp, apiErr := controller.GetVolume(req)

			if tt.expectError != "" {
				require.NotNil(t, apiErr, "expected error but got nil")
				if tt.errorCode != 0 {
					assert.Equal(t, tt.errorCode, apiErr.Code)
				}
				assert.Contains(t, apiErr.Message, tt.expectError)
			} else {
				require.Nil(t, apiErr, "unexpected error: %v", apiErr)
				require.NotNil(t, resp.Body, "response body should not be nil")
			}
		})
	}
}

func TestDeleteVolume(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "test-user",
	}

	tests := []struct {
		name        string
		setupReq    func(t *testing.T) *http.Request
		expectError string
		errorCode   int
	}{
		{
			name: "user not authenticated",
			setupReq: func(t *testing.T) *http.Request {
				return NewRequest(t, nil, nil, map[string]string{
					"volumeID": "some-id",
				}, nil)
			},
			expectError: "User is empty",
			errorCode:   http.StatusUnauthorized,
		},
		{
			name: "volumeID is empty",
			setupReq: func(t *testing.T) *http.Request {
				return NewRequest(t, nil, nil, map[string]string{
					"volumeID": "",
				}, user)
			},
			expectError: "volumeID is required",
			errorCode:   http.StatusBadRequest,
		},
		{
			name: "volume not found",
			setupReq: func(t *testing.T) *http.Request {
				return NewRequest(t, nil, nil, map[string]string{
					"volumeID": "non-existent-pv",
				}, user)
			},
			expectError: "Volume not found",
			errorCode:   http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.setupReq(t)
			resp, apiErr := controller.DeleteVolume(req)

			if tt.expectError != "" {
				require.NotNil(t, apiErr, "expected error but got nil")
				if tt.errorCode != 0 {
					assert.Equal(t, tt.errorCode, apiErr.Code)
				}
				assert.Contains(t, apiErr.Message, tt.expectError)
			} else {
				require.Nil(t, apiErr, "unexpected error: %v", apiErr)
				assert.Equal(t, http.StatusNoContent, resp.Code)
			}
		})
	}
}
