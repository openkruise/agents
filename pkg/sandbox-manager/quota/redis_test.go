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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquireUpsert(t *testing.T) {
	tests := []struct {
		name        string
		seed        []AcquireParams
		op          AcquireParams
		wantSum     map[string]map[string]int64
		expectError string
	}{
		{
			name: "first create charges count all and running",
			op: AcquireParams{
				User:       "K",
				LockString: "l1",
				Scopes:     []quotaspec.QuotaScope{quotaspec.ScopeRunning},
				Enforce:    true,
				Limits: map[quotaspec.QuotaDimension]map[quotaspec.QuotaScope]int64{
					quotaspec.DimSandboxCount: {quotaspec.ScopeAll: 10},
				},
			},
			wantSum: map[string]map[string]int64{
				"sandbox.count": {"all": 1, "running": 1},
			},
		},
		{
			name: "idempotent reacquire is zero delta",
			seed: []AcquireParams{{
				User:       "K",
				LockString: "l1",
				Scopes:     []quotaspec.QuotaScope{quotaspec.ScopeRunning},
			}},
			op: AcquireParams{
				User:       "K",
				LockString: "l1",
				Scopes:     []quotaspec.QuotaScope{quotaspec.ScopeRunning},
			},
			wantSum: map[string]map[string]int64{
				"sandbox.count": {"all": 1, "running": 1},
			},
		},
		{
			name: "pause drops running and keeps all",
			seed: []AcquireParams{{
				User:       "K",
				LockString: "l1",
				Scopes:     []quotaspec.QuotaScope{quotaspec.ScopeRunning},
			}},
			op: AcquireParams{
				User:       "K",
				LockString: "l1",
			},
			wantSum: map[string]map[string]int64{
				"sandbox.count": {"all": 1, "running": 0},
			},
		},
		{
			name: "enforce rejects at all scope cap",
			seed: []AcquireParams{{
				User:       "K",
				LockString: "l1",
				Scopes:     []quotaspec.QuotaScope{quotaspec.ScopeRunning},
			}},
			op: AcquireParams{
				User:       "K",
				LockString: "l2",
				Scopes:     []quotaspec.QuotaScope{quotaspec.ScopeRunning},
				Enforce:    true,
				Limits: map[quotaspec.QuotaDimension]map[quotaspec.QuotaScope]int64{
					quotaspec.DimSandboxCount: {quotaspec.ScopeAll: 1},
				},
			},
			expectError: "quota exceeded",
		},
		{
			name: "cpu footprint charges all and running",
			op: AcquireParams{
				User:       "K",
				LockString: "l1",
				Footprint: map[quotaspec.QuotaDimension]int64{
					quotaspec.DimLimitsCPU: 2000,
				},
				Scopes: []quotaspec.QuotaScope{quotaspec.ScopeRunning},
			},
			wantSum: map[string]map[string]int64{
				"sandbox.count": {"all": 1, "running": 1},
				"limits.cpu":    {"all": 2000, "running": 2000},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend, client := newTestRedisBackend(t)
			ctx := context.Background()

			for _, seed := range tt.seed {
				require.NoError(t, backend.Acquire(ctx, seed))
			}

			err := backend.Acquire(ctx, tt.op)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
			}

			for dim, scopes := range tt.wantSum {
				for scope, want := range scopes {
					assert.Equal(t, want, readSum(t, client, "K", quotaspec.QuotaDimension(dim), quotaspec.QuotaScope(scope)))
				}
			}
		})
	}
}

func TestRedisKeysFollowDimensionOrder(t *testing.T) {
	keys := redisKeys("K")

	require.Len(t, keys, len(redisQuotaDimensions)+1)
	assert.Equal(t, liveKey("K"), keys[0])
	for i, dimension := range redisQuotaDimensions {
		assert.Equal(t, sumKey("K", dimension), keys[i+1])
		assert.Contains(t, redisLuaDimensions, string(dimension))
	}
}

func TestRedisScriptWithDimensionsPreservesLuaPercent(t *testing.T) {
	got := redisScriptWithDimensions("local x = 5 % 2\n" + redisDimensionPlaceholder)

	assert.True(t, strings.Contains(got, "5 % 2"))
	assert.NotContains(t, got, redisDimensionPlaceholder)
	assert.Contains(t, got, "local dims =")
}

func TestReleaseSubtractsAllScopes(t *testing.T) {
	backend, client := newTestRedisBackend(t)
	ctx := context.Background()

	require.NoError(t, backend.Acquire(ctx, AcquireParams{
		User:       "K",
		LockString: "l1",
		Footprint: map[quotaspec.QuotaDimension]int64{
			quotaspec.DimLimitsCPU:    2000,
			quotaspec.DimLimitsMemory: 4096,
		},
		Scopes: []quotaspec.QuotaScope{quotaspec.ScopeRunning},
	}))

	require.NoError(t, backend.Release(ctx, "K", "l1"))
	require.NoError(t, backend.Release(ctx, "K", "l1"))

	assert.Equal(t, int64(0), readSum(t, client, "K", quotaspec.DimSandboxCount, quotaspec.ScopeAll))
	assert.Equal(t, int64(0), readSum(t, client, "K", quotaspec.DimSandboxCount, quotaspec.ScopeRunning))
	assert.Equal(t, int64(0), readSum(t, client, "K", quotaspec.DimLimitsCPU, quotaspec.ScopeAll))
	assert.Equal(t, int64(0), readSum(t, client, "K", quotaspec.DimLimitsCPU, quotaspec.ScopeRunning))
	assert.Equal(t, int64(0), readSum(t, client, "K", quotaspec.DimLimitsMemory, quotaspec.ScopeAll))
	assert.Equal(t, int64(0), readSum(t, client, "K", quotaspec.DimLimitsMemory, quotaspec.ScopeRunning))

	entries, err := backend.ListEntries(ctx, "K")
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestListEntriesReturnsDecodedEntries(t *testing.T) {
	backend, _ := newTestRedisBackend(t)
	ctx := context.Background()

	require.NoError(t, backend.Acquire(ctx, AcquireParams{
		User:       "K",
		LockString: "l1",
		Footprint: map[quotaspec.QuotaDimension]int64{
			quotaspec.DimLimitsCPU: 2000,
		},
		Scopes: []quotaspec.QuotaScope{quotaspec.ScopeRunning},
	}))
	require.NoError(t, backend.Acquire(ctx, AcquireParams{
		User:       "K",
		LockString: "l2",
		Scopes:     nil,
	}))

	entries, err := backend.ListEntries(ctx, "K")
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, Entry{
		Footprint: map[quotaspec.QuotaDimension]int64{quotaspec.DimLimitsCPU: 2000},
		Scopes:    []quotaspec.QuotaScope{quotaspec.ScopeRunning},
	}, entries["l1"])
	assert.Equal(t, Entry{}, entries["l2"])
}

func TestListEntriesNormalizesConditionalScopesOnly(t *testing.T) {
	backend, _ := newTestRedisBackend(t)
	ctx := context.Background()

	require.NoError(t, backend.Acquire(ctx, AcquireParams{
		User:       "K",
		LockString: "l1",
		Scopes: []quotaspec.QuotaScope{
			quotaspec.ScopeAll,
			quotaspec.ScopeRunning,
			quotaspec.ScopeRunning,
		},
	}))

	entries, err := backend.ListEntries(ctx, "K")
	require.NoError(t, err)
	require.Contains(t, entries, "l1")
	assert.Equal(t, Entry{
		Scopes: []quotaspec.QuotaScope{quotaspec.ScopeRunning},
	}, entries["l1"])
}

func TestListEntriesNormalizesFootprintDimensions(t *testing.T) {
	backend, _ := newTestRedisBackend(t)
	ctx := context.Background()

	require.NoError(t, backend.Acquire(ctx, AcquireParams{
		User:       "K",
		LockString: "l1",
		Footprint: map[quotaspec.QuotaDimension]int64{
			quotaspec.DimSandboxCount:               1,
			quotaspec.DimLimitsCPU:                  2000,
			quotaspec.DimLimitsMemory:               4096,
			quotaspec.QuotaDimension("limits.gpu"):  8,
			quotaspec.QuotaDimension("unknown.dim"): -1,
		},
	}))

	entries, err := backend.ListEntries(ctx, "K")
	require.NoError(t, err)
	require.Contains(t, entries, "l1")
	assert.Equal(t, Entry{
		Footprint: map[quotaspec.QuotaDimension]int64{
			quotaspec.DimLimitsCPU:    2000,
			quotaspec.DimLimitsMemory: 4096,
		},
	}, entries["l1"])
}

func TestListEntriesSkipsMalformedEntries(t *testing.T) {
	srv := miniredis.RunT(t)
	backend := NewRedisBackend(redis.NewClient(&redis.Options{Addr: srv.Addr()}), time.Second)

	ctx := context.Background()
	beforeDecodeErrors := testutil.ToFloat64(backendErrorsTotal.WithLabelValues("list_entries_decode"))
	require.NoError(t, backend.Acquire(ctx, AcquireParams{
		User:       "K",
		LockString: "good",
		Footprint:  map[quotaspec.QuotaDimension]int64{quotaspec.DimLimitsCPU: 1000},
		Scopes:     []quotaspec.QuotaScope{quotaspec.ScopeRunning},
	}))
	srv.HSet(liveKey("K"), "bad", "{")

	entries, err := backend.ListEntries(ctx, "K")
	require.NoError(t, err)
	require.Contains(t, entries, "good")
	require.NotContains(t, entries, "bad")
	assert.Equal(t, beforeDecodeErrors+1, testutil.ToFloat64(backendErrorsTotal.WithLabelValues("list_entries_decode")))
}

func TestCleanupDeletesLiveAndAllSums(t *testing.T) {
	backend, client := newTestRedisBackend(t)
	ctx := context.Background()

	require.NoError(t, backend.Acquire(ctx, AcquireParams{
		User:       "K",
		LockString: "l1",
		Footprint: map[quotaspec.QuotaDimension]int64{
			quotaspec.DimLimitsCPU: 2000,
		},
		Scopes: []quotaspec.QuotaScope{quotaspec.ScopeRunning},
	}))
	require.NoError(t, backend.Cleanup(ctx, "K"))

	assert.Equal(t, int64(0), readSum(t, client, "K", quotaspec.DimSandboxCount, quotaspec.ScopeAll))
	assert.Equal(t, int64(0), readSum(t, client, "K", quotaspec.DimLimitsCPU, quotaspec.ScopeAll))
	assert.False(t, keyExists(t, client, liveKey("K")))
	assert.False(t, keyExists(t, client, sumKey("K", quotaspec.DimSandboxCount)))
	assert.False(t, keyExists(t, client, sumKey("K", quotaspec.DimLimitsCPU)))
	assert.False(t, keyExists(t, client, sumKey("K", quotaspec.DimLimitsMemory)))
}

func TestNoopBackend(t *testing.T) {
	backend := NoopBackend{}
	ctx := context.Background()

	require.NoError(t, backend.Acquire(ctx, AcquireParams{User: "K", LockString: "l1"}))
	require.NoError(t, backend.Release(ctx, "K", "l1"))
	require.NoError(t, backend.Cleanup(ctx, "K"))

	entries, err := backend.ListEntries(ctx, "K")
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func newTestRedisBackend(t *testing.T) (*RedisBackend, *redis.Client) {
	t.Helper()

	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
		srv.Close()
	})
	return NewRedisBackend(client, 50*time.Millisecond), client
}

func readSum(t *testing.T, client *redis.Client, user string, dimension quotaspec.QuotaDimension, scope quotaspec.QuotaScope) int64 {
	t.Helper()

	value, err := client.HGet(context.Background(), sumKey(user, dimension), string(scope)).Result()
	if err == redis.Nil {
		return 0
	}
	require.NoError(t, err)

	sum, err := strconv.ParseInt(value, 10, 64)
	require.NoError(t, err)
	return sum
}

func keyExists(t *testing.T, client *redis.Client, key string) bool {
	t.Helper()

	exists, err := client.Exists(context.Background(), key).Result()
	require.NoError(t, err)
	return exists == 1
}
