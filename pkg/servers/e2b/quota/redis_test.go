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
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedisBackendAcquireCountOnly(t *testing.T) {
	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	backend := NewRedisBackend(client, 50*time.Millisecond)
	ctx := context.Background()

	tests := []struct {
		name        string
		lockString  string
		limit       int64
		expectError error
	}{
		{name: "first acquire allowed", lockString: "lock-1", limit: 1},
		{name: "same lock idempotent", lockString: "lock-1", limit: 1},
		{name: "second lock rejected", lockString: "lock-2", limit: 1, expectError: ErrQuotaExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := backend.Acquire(ctx, "key-1", tt.lockString, tt.limit)
			if tt.expectError != nil {
				require.ErrorIs(t, err, tt.expectError)
				return
			}
			require.NoError(t, err)
		})
	}

	got, err := backend.List(ctx, "key-1")
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

func TestRedisBackendAcquireDoesNotRecordManagerDecisionMetrics(t *testing.T) {
	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	backend := NewRedisBackend(client, 50*time.Millisecond)

	beforeAllowed := testutil.ToFloat64(acquireTotal.WithLabelValues("allowed"))
	beforeRejected := testutil.ToFloat64(acquireTotal.WithLabelValues("rejected"))
	beforeErrors := testutil.ToFloat64(backendErrorsTotal.WithLabelValues("acquire"))

	require.NoError(t, backend.Acquire(context.Background(), "key-1", "lock-1", 1))
	require.ErrorIs(t, backend.Acquire(context.Background(), "key-1", "lock-2", 1), ErrQuotaExceeded)

	assert.Equal(t, beforeAllowed, testutil.ToFloat64(acquireTotal.WithLabelValues("allowed")))
	assert.Equal(t, beforeRejected, testutil.ToFloat64(acquireTotal.WithLabelValues("rejected")))
	assert.Equal(t, beforeErrors, testutil.ToFloat64(backendErrorsTotal.WithLabelValues("acquire")))
}

func TestRedisBackendWrappedErrorsDoNotRecordManagerBackendErrorMetrics(t *testing.T) {
	srv := miniredis.RunT(t)
	addr := srv.Addr()
	srv.Close()

	client := redis.NewClient(&redis.Options{Addr: addr})
	backend := NewRedisBackend(client, 10*time.Millisecond)
	beforeReleaseErrors := testutil.ToFloat64(backendErrorsTotal.WithLabelValues("release"))
	beforeDeleteSubjectErrors := testutil.ToFloat64(backendErrorsTotal.WithLabelValues("delete_subject"))
	beforeReleaseTotalErrors := testutil.ToFloat64(releaseTotal.WithLabelValues("error"))

	require.ErrorIs(t, backend.Release(context.Background(), "key-1", "lock-1"), ErrBackendUnavailable)
	require.ErrorIs(t, backend.DeleteSubject(context.Background(), "key-1"), ErrBackendUnavailable)

	assert.Equal(t, beforeReleaseErrors, testutil.ToFloat64(backendErrorsTotal.WithLabelValues("release")))
	assert.Equal(t, beforeDeleteSubjectErrors, testutil.ToFloat64(backendErrorsTotal.WithLabelValues("delete_subject")))
	assert.Equal(t, beforeReleaseTotalErrors+1, testutil.ToFloat64(releaseTotal.WithLabelValues("error")))
}

func TestRedisBackendHardZeroAndRelease(t *testing.T) {
	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	backend := NewRedisBackend(client, 50*time.Millisecond)
	ctx := context.Background()

	require.ErrorIs(t, backend.Acquire(ctx, "key-1", "lock-1", 0), ErrQuotaExceeded)
	require.NoError(t, backend.AddObserved(ctx, "key-1", "lock-1", time.Unix(10, 0)))
	require.NoError(t, backend.Release(ctx, "key-1", "lock-1"))
	require.NoError(t, backend.Release(ctx, "key-1", "lock-1"))

	got, err := backend.List(ctx, "key-1")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestRedisBackendTransportErrorClassifiedUnavailable(t *testing.T) {
	srv := miniredis.RunT(t)
	addr := srv.Addr()
	srv.Close()

	client := redis.NewClient(&redis.Options{Addr: addr})
	backend := NewRedisBackend(client, 10*time.Millisecond)

	err := backend.Acquire(context.Background(), "key-1", "lock-1", 1)
	require.ErrorIs(t, err, ErrBackendUnavailable)
}

func TestRedisBackendListAddObservedAndDeleteSubject(t *testing.T) {
	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	backend := NewRedisBackend(client, 50*time.Millisecond)
	ctx := context.Background()

	now := time.Unix(123, 0)
	require.NoError(t, backend.AddObserved(ctx, "key-1", "lock-1", now))
	require.NoError(t, backend.AddObserved(ctx, "key-1", "lock-2", now.Add(time.Second)))

	got, err := backend.List(ctx, "key-1")
	require.NoError(t, err)
	assert.Equal(t, now, got["lock-1"])
	assert.Equal(t, now.Add(time.Second), got["lock-2"])

	require.NoError(t, backend.DeleteSubject(ctx, "key-1"))

	got, err = backend.List(ctx, "key-1")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestRedisBackendListInvalidTimestampPreservesMembershipAsAncient(t *testing.T) {
	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	backend := NewRedisBackend(client, 50*time.Millisecond)

	srv.HSet("q:live:{key-1}", "lock-1", "bad-ts")

	got, err := backend.List(context.Background(), "key-1")
	require.NoError(t, err)
	assert.Equal(t, time.Unix(0, 0), got["lock-1"])
}

func TestNoopBackend(t *testing.T) {
	backend := NoopBackend{}
	ctx := context.Background()

	require.NoError(t, backend.Acquire(ctx, "key-1", "lock-1", 1))
	require.NoError(t, backend.Release(ctx, "key-1", "lock-1"))
	require.NoError(t, backend.AddObserved(ctx, "key-1", "lock-1", time.Unix(1, 0)))
	require.NoError(t, backend.DeleteSubject(ctx, "key-1"))

	got, err := backend.List(ctx, "key-1")
	require.NoError(t, err)
	assert.Empty(t, got)
}
