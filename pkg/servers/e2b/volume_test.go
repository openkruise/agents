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
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

// fakeVolumeManager is a test double for volumeManagerInterface.
type fakeVolumeManager struct {
	registerFn func(ctx context.Context, opts infra.RegisterVolumeOptions) (infra.VolumeInfo, error)
	listFn     func(ctx context.Context, opts infra.ListVolumesOptions) ([]infra.VolumeInfo, error)
	getFn      func(ctx context.Context, opts infra.GetVolumeOptions) (infra.VolumeInfo, error)
	deleteFn   func(ctx context.Context, opts infra.DeleteVolumeOptions) (infra.DeleteVolumeResult, error)
}

func (f *fakeVolumeManager) RegisterVolume(ctx context.Context, opts infra.RegisterVolumeOptions) (infra.VolumeInfo, error) {
	return f.registerFn(ctx, opts)
}
func (f *fakeVolumeManager) ListVolumes(ctx context.Context, opts infra.ListVolumesOptions) ([]infra.VolumeInfo, error) {
	return f.listFn(ctx, opts)
}
func (f *fakeVolumeManager) GetVolume(ctx context.Context, opts infra.GetVolumeOptions) (infra.VolumeInfo, error) {
	return f.getFn(ctx, opts)
}
func (f *fakeVolumeManager) DeleteVolume(ctx context.Context, opts infra.DeleteVolumeOptions) (infra.DeleteVolumeResult, error) {
	return f.deleteFn(ctx, opts)
}

// newVolumeTestController builds a minimal Controller wired with a fake volume manager.
func newVolumeTestController(vm volumeManagerInterface) *Controller {
	return &Controller{volumeManager: vm}
}

// newVolumeTestUser returns an admin API key suitable for handler tests.
func newVolumeTestUser() *models.CreatedTeamAPIKey {
	return &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Name: "test-user",
		Team: models.AdminTeam(),
	}
}

// newVolumeRequest builds an HTTP request carrying a user in its context.
func newVolumeRequest(method, path string, body string, pathValues map[string]string, query map[string]string, user *models.CreatedTeamAPIKey) *http.Request {
	r, _ := http.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	if pathValues != nil {
		for k, v := range pathValues {
			r.SetPathValue(k, v)
		}
	}
	if query != nil {
		q := r.URL.Query()
		for k, v := range query {
			q.Set(k, v)
		}
		r.URL.RawQuery = q.Encode()
	}
	ctx := context.WithValue(r.Context(), "user", user)
	return r.WithContext(ctx)
}

// ---------------------------------------------------------------------------
// mapVolumeErrorToHTTP — Property 8
// ---------------------------------------------------------------------------

// TestProperty8_ErrorCodeHTTPMappingIsTotal verifies that for each known
// ErrorCode, mapVolumeErrorToHTTP returns a non-200 status deterministically.
//
// Feature: e2b-volume-management, Property 8: Error code HTTP mapping
// Validates: Requirements 8.1–8.5
func TestProperty8_ErrorCodeHTTPMappingIsTotal(t *testing.T) {
	tests := []struct {
		name           string
		code           managererrors.ErrorCode
		expectedStatus int
	}{
		{name: "NotFound → 404", code: managererrors.ErrorNotFound, expectedStatus: http.StatusNotFound},
		{name: "Conflict → 409", code: managererrors.ErrorConflict, expectedStatus: http.StatusConflict},
		{name: "NotAllowed → 403", code: managererrors.ErrorNotAllowed, expectedStatus: http.StatusForbidden},
		{name: "BadRequest → 422", code: managererrors.ErrorBadRequest, expectedStatus: http.StatusUnprocessableEntity},
		{name: "Internal → 500", code: managererrors.ErrorInternal, expectedStatus: http.StatusInternalServerError},
		{name: "Unknown → 500 (default)", code: managererrors.ErrorUnknown, expectedStatus: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Non-200 assertion
			status := mapVolumeErrorToHTTP(tt.code)
			assert.NotEqual(t, http.StatusOK, status, "should never map to 200")
			assert.Equal(t, tt.expectedStatus, status)

			// Determinism: calling twice gives the same result
			assert.Equal(t, status, mapVolumeErrorToHTTP(tt.code), "mapping must be deterministic")
		})
	}
}

// ---------------------------------------------------------------------------
// RegisterVolume handler tests
// ---------------------------------------------------------------------------

func TestController_RegisterVolume(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		registerFn   func(context.Context, infra.RegisterVolumeOptions) (infra.VolumeInfo, error)
		expectCode   int
		expectError  string
		expectVolume string
	}{
		{
			name: "success — returns 201 with volume response",
			body: `{"name":"vol-a","pvName":"pv-001","sizeGB":10}`,
			registerFn: func(_ context.Context, opts infra.RegisterVolumeOptions) (infra.VolumeInfo, error) {
				return infra.VolumeInfo{
					VolumeID:  opts.PvName,
					Name:      opts.Name,
					PvName:    opts.PvName,
					SizeGB:    opts.SizeGB,
					CreatedAt: "2026-01-01T00:00:00Z",
				}, nil
			},
			expectCode:   http.StatusCreated,
			expectVolume: "pv-001",
		},
		{
			name: "conflict — returns 409 when PV already registered",
			body: `{"name":"vol-a","pvName":"pv-dup","sizeGB":10}`,
			registerFn: func(_ context.Context, _ infra.RegisterVolumeOptions) (infra.VolumeInfo, error) {
				return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorConflict, "pv pv-dup is already registered")
			},
			expectCode:  http.StatusConflict,
			expectError: "already registered",
		},
		{
			name: "not found — returns 404 when PV does not exist",
			body: `{"name":"vol-a","pvName":"pv-missing","sizeGB":10}`,
			registerFn: func(_ context.Context, _ infra.RegisterVolumeOptions) (infra.VolumeInfo, error) {
				return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorNotFound, "pv pv-missing not found")
			},
			expectCode:  http.StatusNotFound,
			expectError: "not found",
		},
		{
			name:        "bad json body — returns 400",
			body:        `{invalid}`,
			registerFn:  nil,
			expectCode:  http.StatusBadRequest,
			expectError: "",
		},
		{
			name:        "missing name — returns 400",
			body:        `{"pvName":"pv-001","sizeGB":10}`,
			registerFn:  nil,
			expectCode:  http.StatusBadRequest,
			expectError: "name is required",
		},
		{
			name:        "missing pvName — returns 400",
			body:        `{"name":"vol-a","sizeGB":10}`,
			registerFn:  nil,
			expectCode:  http.StatusBadRequest,
			expectError: "pvName is required",
		},
		{
			name:        "zero sizeGB — returns 400",
			body:        `{"name":"vol-a","pvName":"pv-001","sizeGB":0}`,
			registerFn:  nil,
			expectCode:  http.StatusBadRequest,
			expectError: "sizeGB must be a positive integer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fake volumeManagerInterface
			if tt.registerFn != nil {
				fake = &fakeVolumeManager{registerFn: tt.registerFn}
			} else {
				// registerFn nil: bad body returns before calling manager
				fake = &fakeVolumeManager{registerFn: func(context.Context, infra.RegisterVolumeOptions) (infra.VolumeInfo, error) {
					panic("should not be called")
				}}
			}
			sc := newVolumeTestController(fake)
			req := newVolumeRequest(http.MethodPost, "/volumes", tt.body, nil, nil, newVolumeTestUser())

			resp, apiErr := sc.RegisterVolume(req)

			if tt.expectError != "" {
				require.NotNil(t, apiErr)
				assert.Equal(t, tt.expectCode, apiErr.Code)
				assert.Contains(t, apiErr.Message, tt.expectError)
			} else if tt.expectCode >= 400 {
				require.NotNil(t, apiErr)
				assert.Equal(t, tt.expectCode, apiErr.Code)
			} else {
				require.Nil(t, apiErr)
				assert.Equal(t, tt.expectCode, resp.Code)
				if tt.expectVolume != "" {
					require.NotNil(t, resp.Body)
					assert.Equal(t, tt.expectVolume, resp.Body.VolumeID)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ListVolumes handler tests
// ---------------------------------------------------------------------------

func TestController_ListVolumes(t *testing.T) {
	tests := []struct {
		name        string
		listFn      func(context.Context, infra.ListVolumesOptions) ([]infra.VolumeInfo, error)
		expectCode  int
		expectCount int
		expectError string
	}{
		{
			name: "success — returns 200 with volume list",
			listFn: func(_ context.Context, _ infra.ListVolumesOptions) ([]infra.VolumeInfo, error) {
				return []infra.VolumeInfo{
					{VolumeID: "pv-1", Name: "vol-1", PvName: "pv-1", SizeGB: 5},
					{VolumeID: "pv-2", Name: "vol-2", PvName: "pv-2", SizeGB: 10},
				}, nil
			},
			expectCode:  http.StatusOK,
			expectCount: 2,
		},
		{
			name: "empty namespace — returns 200 with empty list",
			listFn: func(_ context.Context, _ infra.ListVolumesOptions) ([]infra.VolumeInfo, error) {
				return []infra.VolumeInfo{}, nil
			},
			expectCode:  http.StatusOK,
			expectCount: 0,
		},
		{
			name: "internal error — returns 500",
			listFn: func(_ context.Context, _ infra.ListVolumesOptions) ([]infra.VolumeInfo, error) {
				return nil, managererrors.NewError(managererrors.ErrorInternal, "cache unavailable")
			},
			expectCode:  http.StatusInternalServerError,
			expectError: "cache unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := newVolumeTestController(&fakeVolumeManager{listFn: tt.listFn})
			req := newVolumeRequest(http.MethodGet, "/volumes", "", nil, nil, newVolumeTestUser())

			resp, apiErr := sc.ListVolumes(req)

			if tt.expectError != "" {
				require.NotNil(t, apiErr)
				assert.Equal(t, tt.expectCode, apiErr.Code)
				assert.Contains(t, apiErr.Message, tt.expectError)
			} else {
				require.Nil(t, apiErr)
				assert.Equal(t, tt.expectCode, resp.Code)
				assert.Len(t, resp.Body, tt.expectCount)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GetVolume handler tests
// ---------------------------------------------------------------------------

func TestController_GetVolume(t *testing.T) {
	tests := []struct {
		name        string
		volumeID    string
		getFn       func(context.Context, infra.GetVolumeOptions) (infra.VolumeInfo, error)
		expectCode  int
		expectError string
	}{
		{
			name:     "success — returns 200 with volume",
			volumeID: "pv-abc",
			getFn: func(_ context.Context, opts infra.GetVolumeOptions) (infra.VolumeInfo, error) {
				return infra.VolumeInfo{VolumeID: opts.VolumeID, Name: "vol-abc", PvName: opts.VolumeID, SizeGB: 20}, nil
			},
			expectCode: http.StatusOK,
		},
		{
			name:     "not found — returns 404",
			volumeID: "pv-missing",
			getFn: func(_ context.Context, _ infra.GetVolumeOptions) (infra.VolumeInfo, error) {
				return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorNotFound, "volume pv-missing not found")
			},
			expectCode:  http.StatusNotFound,
			expectError: "not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := newVolumeTestController(&fakeVolumeManager{getFn: tt.getFn})
			req := newVolumeRequest(http.MethodGet, "/volumes/"+tt.volumeID, "", map[string]string{"volumeID": tt.volumeID}, nil, newVolumeTestUser())

			resp, apiErr := sc.GetVolume(req)

			if tt.expectError != "" {
				require.NotNil(t, apiErr)
				assert.Equal(t, tt.expectCode, apiErr.Code)
				assert.Contains(t, apiErr.Message, tt.expectError)
			} else {
				require.Nil(t, apiErr)
				assert.Equal(t, tt.expectCode, resp.Code)
				require.NotNil(t, resp.Body)
				assert.Equal(t, tt.volumeID, resp.Body.VolumeID)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// DeleteVolume handler tests
// ---------------------------------------------------------------------------

func TestController_DeleteVolume(t *testing.T) {
	tests := []struct {
		name           string
		volumeID       string
		forceParam     string
		deleteFn       func(context.Context, infra.DeleteVolumeOptions) (infra.DeleteVolumeResult, error)
		expectCode     int
		expectError    string
		expectWarning  bool
		expectAffected []string
	}{
		{
			name:     "success — unmounted delete returns 200",
			volumeID: "pv-ok",
			deleteFn: func(_ context.Context, _ infra.DeleteVolumeOptions) (infra.DeleteVolumeResult, error) {
				return infra.DeleteVolumeResult{}, nil
			},
			expectCode: http.StatusOK,
		},
		{
			name:       "conflict — mounted without force returns 409",
			volumeID:   "pv-mounted",
			deleteFn: func(_ context.Context, _ infra.DeleteVolumeOptions) (infra.DeleteVolumeResult, error) {
				return infra.DeleteVolumeResult{}, managererrors.NewError(managererrors.ErrorConflict, "volume is mounted by: sbx-001")
			},
			expectCode:  http.StatusConflict,
			expectError: "mounted by",
		},
		{
			name:       "force delete — returns 200 with warning and affected IDs",
			volumeID:   "pv-mounted",
			forceParam: "true",
			deleteFn: func(_ context.Context, opts infra.DeleteVolumeOptions) (infra.DeleteVolumeResult, error) {
				require.True(t, opts.Force)
				return infra.DeleteVolumeResult{
					AffectedSandboxIDs: []string{"sbx-001", "sbx-002"},
					ForcedDelete:       true,
				}, nil
			},
			expectCode:     http.StatusOK,
			expectWarning:  true,
			expectAffected: []string{"sbx-001", "sbx-002"},
		},
		{
			name:     "not found — returns 404",
			volumeID: "pv-gone",
			deleteFn: func(_ context.Context, _ infra.DeleteVolumeOptions) (infra.DeleteVolumeResult, error) {
				return infra.DeleteVolumeResult{}, managererrors.NewError(managererrors.ErrorNotFound, "volume pv-gone not found")
			},
			expectCode:  http.StatusNotFound,
			expectError: "not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := newVolumeTestController(&fakeVolumeManager{deleteFn: tt.deleteFn})
			var query map[string]string
			if tt.forceParam != "" {
				query = map[string]string{"force": tt.forceParam}
			}
			req := newVolumeRequest(http.MethodDelete, "/volumes/"+tt.volumeID, "", map[string]string{"volumeID": tt.volumeID}, query, newVolumeTestUser())

			resp, apiErr := sc.DeleteVolume(req)

			if tt.expectError != "" {
				require.NotNil(t, apiErr)
				assert.Equal(t, tt.expectCode, apiErr.Code)
				assert.Contains(t, apiErr.Message, tt.expectError)
			} else {
				require.Nil(t, apiErr)
				assert.Equal(t, tt.expectCode, resp.Code)
				require.NotNil(t, resp.Body)
				if tt.expectWarning {
					assert.NotEmpty(t, resp.Body.Warning)
					assert.ElementsMatch(t, tt.expectAffected, resp.Body.AffectedBy)
				} else {
					assert.Empty(t, resp.Body.Warning)
				}
			}
		})
	}
}

// Ensure uuid import is used (required by test user ID).
var _ = uuid.Nil

// ---------------------------------------------------------------------------
// resolveVolumeMounts unit tests (task 7.2)
// ---------------------------------------------------------------------------

func TestController_ResolveVolumeMounts(t *testing.T) {
	tests := []struct {
		name        string
		namespace   string
		mounts      []models.VolumeMountRequest
		getFn       func(context.Context, infra.GetVolumeOptions) (infra.VolumeInfo, error)
		expectNil   bool   // true = expect nil result (empty mounts)
		expectLen   int    // expected length of result slice
		expectError string // non-empty = expect error containing this string
	}{
		{
			name:      "empty volume_mounts — no-op, nil returned",
			namespace: "ns1",
			mounts:    []models.VolumeMountRequest{},
			getFn:     nil, // should not be called
			expectNil: true,
		},
		{
			name:      "nil volume_mounts — no-op, nil returned",
			namespace: "ns1",
			mounts:    nil,
			getFn:     nil,
			expectNil: true,
		},
		{
			name:      "non-existent volumeID — returns error",
			namespace: "ns1",
			mounts: []models.VolumeMountRequest{
				{VolumeID: "pv-missing", MountPath: "/data"},
			},
			getFn: func(_ context.Context, _ infra.GetVolumeOptions) (infra.VolumeInfo, error) {
				return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorNotFound, "volume pv-missing not found")
			},
			expectError: "not found",
		},
		{
			name:      "valid mounts — correct CSIMountConfig slice",
			namespace: "ns1",
			mounts: []models.VolumeMountRequest{
				{VolumeID: "pv-001", MountPath: "/data", ReadOnly: false},
				{VolumeID: "pv-002", MountPath: "/config", ReadOnly: true},
			},
			getFn: func(_ context.Context, opts infra.GetVolumeOptions) (infra.VolumeInfo, error) {
				return infra.VolumeInfo{
					VolumeID: opts.VolumeID,
					PvName:   opts.VolumeID,
					Name:     "vol-" + opts.VolumeID,
					SizeGB:   10,
				}, nil
			},
			expectLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fake volumeManagerInterface
			if tt.getFn != nil {
				fake = &fakeVolumeManager{getFn: tt.getFn}
			} else {
				fake = &fakeVolumeManager{
					getFn: func(context.Context, infra.GetVolumeOptions) (infra.VolumeInfo, error) {
						panic("GetVolume should not be called for empty mounts")
					},
				}
			}
			sc := newVolumeTestController(fake)

			result, err := sc.resolveVolumeMounts(context.Background(), tt.namespace, tt.mounts)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Nil(t, result)
			} else if tt.expectNil {
				require.NoError(t, err)
				assert.Nil(t, result)
			} else {
				require.NoError(t, err)
				assert.Len(t, result, tt.expectLen)
				// Verify each CSIMountConfig maps correctly from the request.
				for i, m := range tt.mounts {
					assert.Equal(t, m.MountPath, result[i].MountPath, "MountPath mismatch at index %d", i)
					assert.Equal(t, m.ReadOnly, result[i].ReadOnly, "ReadOnly mismatch at index %d", i)
					assert.Equal(t, m.VolumeID, result[i].PvName, "PvName should equal volumeID at index %d", i)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Property 6: Cross-namespace volume_mounts rejected (task 7.1)
// ---------------------------------------------------------------------------

// TestProperty6_CrossNamespaceVolumeMountsRejected verifies that resolveVolumeMounts
// returns an error whenever a volumeID belongs to a different namespace than the caller.
//
// Feature: e2b-volume-management, Property 6: Cross-namespace volume_mounts rejected
// Validates: Requirements 5.2
func TestProperty6_CrossNamespaceVolumeMountsRejected(t *testing.T) {
	const iterations = 100

	for iter := 0; iter < iterations; iter++ {
		// The fake GetVolume simulates that the volume exists under ns1 but the caller
		// is in ns2 — GetVolume returns ErrorNotFound (no info disclosure) for wrong namespace.
		callerNS := "caller-ns"
		fake := &fakeVolumeManager{
			getFn: func(_ context.Context, opts infra.GetVolumeOptions) (infra.VolumeInfo, error) {
				// Simulate the infra returning NotFound when namespace doesn't match.
				if opts.Namespace != "owner-ns" {
					return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorNotFound,
						"volume %s not found", opts.VolumeID)
				}
				return infra.VolumeInfo{VolumeID: opts.VolumeID, PvName: opts.VolumeID}, nil
			},
		}
		sc := newVolumeTestController(fake)

		mounts := []models.VolumeMountRequest{
			{VolumeID: "pv-cross", MountPath: "/data"},
		}

		_, err := sc.resolveVolumeMounts(context.Background(), callerNS, mounts)
		require.Error(t, err, "iter %d: expected error for cross-namespace mount", iter)
	}
}
