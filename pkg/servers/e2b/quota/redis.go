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
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	defaultAcquireTimeout              = 50 * time.Millisecond
	defaultMaintenanceOperationTimeout = 30 * time.Second
)

var (
	acquireRedisScript = redis.NewScript(`
if redis.call('HEXISTS', KEYS[1], ARGV[1]) == 1 then return 'OK' end
local lim = tonumber(ARGV[2])
if lim == 0 then return 'REJECTED' end
if lim > 0 and redis.call('HLEN', KEYS[1]) + 1 > lim then return 'REJECTED' end
redis.call('HSET', KEYS[1], ARGV[1], redis.call('TIME')[1])
return 'OK'
`)
	releaseRedisScript = redis.NewScript(`
return redis.call('HDEL', KEYS[1], ARGV[1])
`)
)

type RedisBackend struct {
	client  *redis.Client
	timeout time.Duration
}

func NewRedisBackend(client *redis.Client, timeout time.Duration) *RedisBackend {
	if timeout <= 0 {
		timeout = defaultAcquireTimeout
	}

	return &RedisBackend{
		client:  client,
		timeout: timeout,
	}
}

func (b *RedisBackend) Acquire(ctx context.Context, apiKeyID, lockString string, limit int64) error {
	acquireCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	result, err := acquireRedisScript.Run(acquireCtx, b.client, []string{liveKey(apiKeyID)}, lockString, limit).Text()
	if err != nil {
		return fmt.Errorf("%w: acquire quota in redis: %v", ErrBackendUnavailable, err)
	}

	switch result {
	case "OK":
		return nil
	case "REJECTED":
		return ErrQuotaExceeded
	default:
		return fmt.Errorf("%w: unexpected acquire result %q", ErrBackendUnavailable, result)
	}
}

func (b *RedisBackend) Release(ctx context.Context, apiKeyID, lockString string) error {
	releaseCtx, cancel := withOperationTimeout(ctx, defaultMaintenanceOperationTimeout)
	defer cancel()

	deleted, err := releaseRedisScript.Run(releaseCtx, b.client, []string{liveKey(apiKeyID)}, lockString).Int64()
	if err != nil {
		releaseTotal.WithLabelValues("error").Inc()
		return fmt.Errorf("%w: release quota in redis: %v", ErrBackendUnavailable, err)
	}

	if deleted == 1 {
		releaseTotal.WithLabelValues("released").Inc()
	} else {
		releaseTotal.WithLabelValues("noop").Inc()
	}

	return nil
}

func (b *RedisBackend) AddObserved(ctx context.Context, apiKeyID, lockString string, acquiredAt time.Time) error {
	opCtx, cancel := withOperationTimeout(ctx, defaultMaintenanceOperationTimeout)
	defer cancel()

	if err := b.client.HSet(opCtx, liveKey(apiKeyID), lockString, acquiredAt.Unix()).Err(); err != nil {
		backendErrorsTotal.WithLabelValues("add_observed").Inc()
		return fmt.Errorf("%w: add observed quota entry in redis: %v", ErrBackendUnavailable, err)
	}

	return nil
}

func (b *RedisBackend) List(ctx context.Context, apiKeyID string) (map[string]time.Time, error) {
	opCtx, cancel := withOperationTimeout(ctx, defaultMaintenanceOperationTimeout)
	defer cancel()

	values, err := b.client.HGetAll(opCtx, liveKey(apiKeyID)).Result()
	if err != nil {
		backendErrorsTotal.WithLabelValues("list").Inc()
		return nil, fmt.Errorf("%w: list quota entries in redis: %v", ErrBackendUnavailable, err)
	}

	live := make(map[string]time.Time, len(values))
	for lockString, rawValue := range values {
		seconds, parseErr := strconv.ParseInt(rawValue, 10, 64)
		if parseErr != nil {
			live[lockString] = time.Unix(0, 0)
			continue
		}
		live[lockString] = time.Unix(seconds, 0)
	}

	return live, nil
}

func (b *RedisBackend) DeleteSubject(ctx context.Context, apiKeyID string) error {
	opCtx, cancel := withOperationTimeout(ctx, defaultMaintenanceOperationTimeout)
	defer cancel()

	if err := b.client.Del(opCtx, liveKey(apiKeyID)).Err(); err != nil {
		return fmt.Errorf("%w: delete quota subject in redis: %v", ErrBackendUnavailable, err)
	}

	return nil
}

func liveKey(apiKeyID string) string {
	return "q:live:{" + apiKeyID + "}"
}

func withOperationTimeout(ctx context.Context, defaultTimeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultTimeout)
}
