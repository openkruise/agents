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

package quota

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

type managerTestBackend struct {
	acquireCalls int
	releaseCalls int
	deleteCalls  int

	acquireErr error
	releaseErr error
	deleteErr  error
}

func (b *managerTestBackend) Acquire(context.Context, string, string, int64) error {
	b.acquireCalls++
	return b.acquireErr
}

func (b *managerTestBackend) Release(context.Context, string, string) error {
	b.releaseCalls++
	return b.releaseErr
}

func (b *managerTestBackend) AddObserved(context.Context, string, string, time.Time) error {
	return nil
}

func (b *managerTestBackend) List(context.Context, string) (map[string]time.Time, error) {
	return map[string]time.Time{}, nil
}

func (b *managerTestBackend) DeleteSubject(context.Context, string) error {
	b.deleteCalls++
	return b.deleteErr
}

func TestManagerAcquireUnlimitedZeroBackendIO(t *testing.T) {
	tests := []struct {
		name  string
		quota *models.QuotaSpec
	}{
		{
			name:  "nil quota",
			quota: nil,
		},
		{
			name:  "empty quota",
			quota: &models.QuotaSpec{},
		},
		{
			name: "limited fields absent",
			quota: &models.QuotaSpec{
				Limits: []models.QuotaLimit{{Dimension: models.DimSandboxCount}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &managerTestBackend{}
			manager := NewManager(backend)

			before := testutil.ToFloat64(acquireTotal.WithLabelValues("unlimited"))

			err := manager.Acquire(context.Background(), AcquireRequest{
				APIKeyID:   "",
				LockString: "",
				Quota:      tt.quota,
			})
			require.NoError(t, err)

			assert.Zero(t, backend.acquireCalls)
			assert.Equal(t, before+1, testutil.ToFloat64(acquireTotal.WithLabelValues("unlimited")))
		})
	}
}

func TestManagerAcquireLimitedRejectAndFailOpen(t *testing.T) {
	tests := []struct {
		name                string
		acquireErr          error
		expectErr           error
		expectNil           bool
		expectBackendCalls  int
		expectAcquireLabel  string
		expectBackendErrors int
	}{
		{
			name:               "quota exceeded rejects",
			acquireErr:         ErrQuotaExceeded,
			expectErr:          ErrQuotaExceeded,
			expectBackendCalls: 1,
			expectAcquireLabel: "rejected",
		},
		{
			name:                "backend unavailable fails open",
			acquireErr:          ErrBackendUnavailable,
			expectNil:           true,
			expectBackendCalls:  1,
			expectAcquireLabel:  "fail_open",
			expectBackendErrors: 1,
		},
		{
			name:                "unexpected backend error fails open",
			acquireErr:          errors.New("boom"),
			expectNil:           true,
			expectBackendCalls:  1,
			expectAcquireLabel:  "fail_open",
			expectBackendErrors: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &managerTestBackend{acquireErr: tt.acquireErr}
			manager := NewManager(backend)

			beforeAcquire := testutil.ToFloat64(acquireTotal.WithLabelValues(tt.expectAcquireLabel))
			beforeBackendErrors := testutil.ToFloat64(backendErrorsTotal.WithLabelValues("acquire"))

			err := manager.Acquire(context.Background(), AcquireRequest{
				APIKeyID:   "key-1",
				LockString: "lock-1",
				Quota: &models.QuotaSpec{
					Limits: []models.QuotaLimit{{Dimension: models.DimSandboxCount, Limit: int64Ptr(1)}},
				},
			})

			if tt.expectNil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tt.expectErr)
			}

			assert.Equal(t, tt.expectBackendCalls, backend.acquireCalls)
			assert.Equal(t, beforeAcquire+1, testutil.ToFloat64(acquireTotal.WithLabelValues(tt.expectAcquireLabel)))
			assert.Equal(t, beforeBackendErrors+float64(tt.expectBackendErrors), testutil.ToFloat64(backendErrorsTotal.WithLabelValues("acquire")))
		})
	}
}

func TestManagerAcquireLimitedMissingIdentityErrors(t *testing.T) {
	tests := []struct {
		name     string
		apiKeyID string
		lock     string
	}{
		{
			name:     "missing api key id",
			apiKeyID: "",
			lock:     "lock-1",
		},
		{
			name:     "missing lock string",
			apiKeyID: "key-1",
			lock:     "",
		},
		{
			name:     "missing both",
			apiKeyID: "",
			lock:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &managerTestBackend{}
			manager := NewManager(backend)

			before := testutil.ToFloat64(acquireTotal.WithLabelValues("error"))

			err := manager.Acquire(context.Background(), AcquireRequest{
				APIKeyID:   tt.apiKeyID,
				LockString: tt.lock,
				Quota: &models.QuotaSpec{
					Limits: []models.QuotaLimit{{Dimension: models.DimSandboxCount, Limit: int64Ptr(1)}},
				},
			})

			require.ErrorIs(t, err, ErrMissingIdentity)
			assert.Contains(t, err.Error(), tt.apiKeyID)
			assert.Contains(t, err.Error(), tt.lock)
			assert.Zero(t, backend.acquireCalls)
			assert.Equal(t, before+1, testutil.ToFloat64(acquireTotal.WithLabelValues("error")))
		})
	}
}

func int64Ptr(v int64) *int64 {
	return &v
}
