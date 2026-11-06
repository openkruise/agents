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
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/semaphore"

	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"
)

const (
	defaultAcquireTimeout              = 50 * time.Millisecond
	defaultMaintenanceOperationTimeout = 30 * time.Second
	redisDimensionPlaceholder          = "-- QUOTA_DIMENSIONS --"
)

var (
	redisQuotaDimensions = []quotaspec.QuotaDimension{quotaspec.DimSandboxCount, quotaspec.DimLimitsCPU, quotaspec.DimLimitsMemory}
	redisLuaDimensions   = redisLuaDimensionTables(redisQuotaDimensions)

	acquireRedisScript = redis.NewScript(redisScriptWithDimensions(`
local lockString = ARGV[1]
local newEntry = cjson.decode(ARGV[2])
local enforce = ARGV[3] == '1'
local limits = {}
if ARGV[4] ~= '' then
	limits = cjson.decode(ARGV[4])
end
local oldRaw = redis.call('HGET', KEYS[1], lockString)
local oldEntry = nil
if oldRaw then
	oldEntry = cjson.decode(oldRaw)
end
-- QUOTA_DIMENSIONS --

local function amount(entry, dim)
	if entry == nil then
		return 0
	end
	if dim == 'sandbox.count' then
		return 1
	end
	if entry['d'] == nil then
		return 0
	end
	local value = entry['d'][dim]
	if value == nil then
		return 0
	end
	return tonumber(value) or 0
end

local function scopeSet(entry)
	local set = {all = true}
	if entry == nil or entry['s'] == nil then
		return set
	end
	for _, scope in ipairs(entry['s']) do
		set[scope] = true
	end
	return set
end

local oldScopes = scopeSet(oldEntry)
local newScopes = scopeSet(newEntry)

local function deltaFor(dim, scope)
	local delta = 0
	if newScopes[scope] then
		delta = delta + amount(newEntry, dim)
	end
	if oldScopes[scope] then
		delta = delta - amount(oldEntry, dim)
	end
	return delta
end

if enforce then
	for _, dim in ipairs(dims) do
		local limitByDim = limits[dim]
		if limitByDim ~= nil then
			for scope, limit in pairs(limitByDim) do
				local delta = deltaFor(dim, scope)
				if delta > 0 then
					local current = tonumber(redis.call('HGET', KEYS[keyIndex[dim]], scope) or '0')
					if current + delta > tonumber(limit) then
						return 'REJECTED'
					end
				end
			end
		end
	end
end

for _, dim in ipairs(dims) do
	local scopes = {}
	for scope, _ in pairs(oldScopes) do
		scopes[scope] = true
	end
	for scope, _ in pairs(newScopes) do
		scopes[scope] = true
	end
	for scope, _ in pairs(scopes) do
		local delta = deltaFor(dim, scope)
		if delta ~= 0 then
			local nextValue = redis.call('HINCRBY', KEYS[keyIndex[dim]], scope, delta)
			if tonumber(nextValue) < 0 then
				redis.call('HSET', KEYS[keyIndex[dim]], scope, 0)
			end
		end
	end
end

if oldRaw ~= ARGV[2] then
	redis.call('HSET', KEYS[1], lockString, ARGV[2])
end
return 'OK'
`))
	releaseRedisScript = redis.NewScript(redisScriptWithDimensions(`
local oldRaw = redis.call('HGET', KEYS[1], ARGV[1])
if not oldRaw then
	return 'OK'
end
local oldEntry = cjson.decode(oldRaw)
-- QUOTA_DIMENSIONS --
local scopes = {all = true}
if oldEntry['s'] ~= nil then
	for _, scope in ipairs(oldEntry['s']) do
		scopes[scope] = true
	end
end

local function amount(dim)
	if dim == 'sandbox.count' then
		return 1
	end
	if oldEntry['d'] == nil then
		return 0
	end
	local value = oldEntry['d'][dim]
	if value == nil then
		return 0
	end
	return tonumber(value) or 0
end

for _, dim in ipairs(dims) do
	local delta = amount(dim)
	if delta ~= 0 then
		for scope, _ in pairs(scopes) do
			local nextValue = redis.call('HINCRBY', KEYS[keyIndex[dim]], scope, -delta)
			if tonumber(nextValue) < 0 then
				redis.call('HSET', KEYS[keyIndex[dim]], scope, 0)
			end
		end
	end
end

redis.call('HDEL', KEYS[1], ARGV[1])
return 'OK'
`))
)

type RedisBackend struct {
	// Operations take one token; publication takes all tokens to drain them.
	// Unlike sync.RWMutex, queued readers and writers can honor their contexts.
	operationGate *semaphore.Weighted
	client        *redis.Client
	timeout       time.Duration
	// Serialize publication with shutdown, including forced close after timeout.
	lifecycleMu sync.Mutex
	closed      bool
}

type redisEntry struct {
	Footprint map[quotaspec.QuotaDimension]int64 `json:"d"`
	Scopes    []quotaspec.QuotaScope             `json:"s"`
}

func NewRedisBackend(client *redis.Client, timeout time.Duration) *RedisBackend {
	if timeout <= 0 {
		timeout = defaultAcquireTimeout
	}
	return &RedisBackend{operationGate: semaphore.NewWeighted(math.MaxInt64), client: client, timeout: timeout}
}

// Verify exercises quota permissions on an isolated owner before publication.
// Use the backend directly so fail-open admission cannot hide a failed probe.
func (b *RedisBackend) Verify(ctx context.Context) (err error) {
	user := "config-check-" + uuid.NewString()
	// Check cleanup permission before creating any disposable state.
	if err := b.Cleanup(ctx, user); err != nil {
		return fmt.Errorf("verify quota cleanup: %w", err)
	}
	// known-limit: failed cleanup can leave isolated probe keys. A future TTL
	// requires additional Redis permissions; no business owner's keys are used.
	defer func() { err = errors.Join(err, b.Cleanup(ctx, user)) }()
	p := AcquireParams{
		User: user, LockString: "first", Enforce: true,
		Scopes:    []quotaspec.QuotaScope{quotaspec.ScopeRunning},
		Footprint: map[quotaspec.QuotaDimension]int64{},
		Limits:    map[quotaspec.QuotaDimension]map[quotaspec.QuotaScope]int64{},
	}
	for _, dim := range redisQuotaDimensions {
		p.Footprint[dim] = 1
		p.Limits[dim] = map[quotaspec.QuotaScope]int64{quotaspec.ScopeAll: 1, quotaspec.ScopeRunning: 1}
	}
	if err := b.Acquire(ctx, p); err != nil {
		return fmt.Errorf("verify quota acquire: %w", err)
	}
	p.LockString = "second"
	if err := b.Acquire(ctx, p); !errors.Is(err, ErrQuotaExceeded) {
		if err != nil {
			return fmt.Errorf("verify quota enforcement: %w", err)
		}
		return fmt.Errorf("verify quota enforcement: expected quota exceeded")
	}
	entries, err := b.ListEntries(ctx, user)
	if err != nil {
		return fmt.Errorf("verify quota list: %w", err)
	}
	if _, ok := entries["first"]; !ok || len(entries) != 1 {
		return fmt.Errorf("verify quota list: unexpected entries")
	}
	if err := b.Release(ctx, user, "first"); err != nil {
		return fmt.Errorf("verify quota release: %w", err)
	}
	entries, err = b.ListEntries(ctx, user)
	if err != nil {
		return fmt.Errorf("verify quota release listing: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("verify quota release: entry remains")
	}
	return nil
}

func (b *RedisBackend) Acquire(ctx context.Context, p AcquireParams) error {
	acquireCtx, cancel := context.WithTimeout(ctx, b.acquireTimeout())
	defer cancel()
	if err := b.operationGate.Acquire(acquireCtx, 1); err != nil {
		return fmt.Errorf("%w: wait for quota connection: %w", ErrBackendUnavailable, err)
	}
	defer b.operationGate.Release(1)
	client, err := b.redisClient("acquire")
	if err != nil {
		return err
	}

	entryJSON, err := marshalEntry(entryFromAcquireParams(p))
	if err != nil {
		return fmt.Errorf("marshal quota entry: %w", err)
	}
	limitsJSON, err := marshalLimits(p.Limits)
	if err != nil {
		return fmt.Errorf("marshal quota limits: %w", err)
	}

	result, err := acquireRedisScript.Run(acquireCtx, client, redisKeys(p.User), p.LockString, entryJSON, boolToLuaFlag(p.Enforce), limitsJSON).Text()
	if err != nil {
		return fmt.Errorf("%w: acquire quota in redis: %v", ErrBackendUnavailable, err)
	}
	if result == "REJECTED" {
		return ErrQuotaExceeded
	}
	if result != "OK" {
		return fmt.Errorf("%w: unexpected acquire result %q", ErrBackendUnavailable, result)
	}
	return nil
}

func (b *RedisBackend) Release(ctx context.Context, user, lockString string) error {
	releaseCtx, cancel := context.WithTimeout(ctx, defaultMaintenanceOperationTimeout)
	defer cancel()
	if err := b.operationGate.Acquire(releaseCtx, 1); err != nil {
		return fmt.Errorf("%w: wait for quota connection: %w", ErrBackendUnavailable, err)
	}
	defer b.operationGate.Release(1)
	client, err := b.redisClient("release")
	if err != nil {
		return err
	}

	result, err := releaseRedisScript.Run(releaseCtx, client, redisKeys(user), lockString).Text()
	if err != nil {
		return fmt.Errorf("%w: release quota in redis: %v", ErrBackendUnavailable, err)
	}
	if result != "OK" {
		return fmt.Errorf("%w: unexpected release result %q", ErrBackendUnavailable, result)
	}

	return nil
}

func (b *RedisBackend) ListEntries(ctx context.Context, user string) (map[string]Entry, error) {
	opCtx, cancel := context.WithTimeout(ctx, defaultMaintenanceOperationTimeout)
	defer cancel()
	if err := b.operationGate.Acquire(opCtx, 1); err != nil {
		return nil, fmt.Errorf("%w: wait for quota connection: %w", ErrBackendUnavailable, err)
	}
	defer b.operationGate.Release(1)
	client, err := b.redisClient("list_entries")
	if err != nil {
		return nil, err
	}

	values, err := client.HGetAll(opCtx, liveKey(user)).Result()
	if err != nil {
		backendErrorsTotal.WithLabelValues("list_entries").Inc()
		return nil, fmt.Errorf("%w: list quota entries in redis: %v", ErrBackendUnavailable, err)
	}

	entries := make(map[string]Entry, len(values))
	for lockString, raw := range values {
		entry, decodeErr := unmarshalEntry(raw)
		if decodeErr != nil {
			backendErrorsTotal.WithLabelValues("list_entries_decode").Inc()
			continue
		}
		entries[lockString] = entry
	}
	return entries, nil
}

func (b *RedisBackend) Cleanup(ctx context.Context, user string) error {
	opCtx, cancel := context.WithTimeout(ctx, defaultMaintenanceOperationTimeout)
	defer cancel()
	if err := b.operationGate.Acquire(opCtx, 1); err != nil {
		return fmt.Errorf("%w: wait for quota connection: %w", ErrBackendUnavailable, err)
	}
	defer b.operationGate.Release(1)
	client, err := b.redisClient("cleanup")
	if err != nil {
		return err
	}

	if err := client.Del(opCtx, redisKeys(user)...).Err(); err != nil {
		return fmt.Errorf("%w: cleanup quota keys in redis: %v", ErrBackendUnavailable, err)
	}
	return nil
}

func (b *RedisBackend) acquireTimeout() time.Duration {
	if b == nil || b.timeout <= 0 {
		return defaultAcquireTimeout
	}
	return b.timeout
}

func (b *RedisBackend) redisClient(op string) (*redis.Client, error) {
	if b == nil || b.client == nil {
		return nil, fmt.Errorf("%w: redis client unavailable for %s", ErrBackendUnavailable, op)
	}
	return b.client, nil
}

// ReplaceClient publishes a prepared connection only after old operations finish.
// The backend object, breaker and all repair state remain stable.
// A false result leaves ownership of next with the caller. After publication,
// any error is only an old-client close failure and must not trigger a retry.
func (b *RedisBackend) ReplaceClient(ctx context.Context, next *redis.Client) (published bool, err error) {
	if err := b.operationGate.Acquire(ctx, math.MaxInt64); err != nil {
		return false, err
	}
	b.lifecycleMu.Lock()
	if b.closed {
		b.lifecycleMu.Unlock()
		b.operationGate.Release(math.MaxInt64)
		return false, redis.ErrClosed
	}
	old := b.client
	b.client = next
	b.lifecycleMu.Unlock()
	b.operationGate.Release(math.MaxInt64)
	if old != nil {
		return true, old.Close()
	}
	return true, nil
}

// Close drains operations within ctx's budget, then interrupts any remainder.
// Shutdown is terminal: a concurrent reload cannot reopen the backend.
func (b *RedisBackend) Close(ctx context.Context) error {
	err := b.operationGate.Acquire(ctx, math.MaxInt64)
	if err == nil {
		defer b.operationGate.Release(math.MaxInt64)
	}
	b.lifecycleMu.Lock()
	if b.closed {
		b.lifecycleMu.Unlock()
		return err
	}
	b.closed = true
	old := b.client
	// Keep the pointer stable for operations still holding a token. go-redis
	// supports closing a client concurrently with its commands.
	b.lifecycleMu.Unlock()
	if old != nil {
		return errors.Join(err, old.Close())
	}
	return err
}

func entryFromAcquireParams(p AcquireParams) Entry {
	return Entry{
		Footprint: normalizeFootprint(p.Footprint),
		Scopes:    normalizeScopes(p.Scopes),
	}
}

func marshalEntry(entry Entry) (string, error) {
	payload := redisEntry{
		Footprint: normalizeFootprint(entry.Footprint),
		Scopes:    normalizeScopes(entry.Scopes),
	}
	if payload.Footprint == nil {
		payload.Footprint = map[quotaspec.QuotaDimension]int64{}
	}
	if payload.Scopes == nil {
		payload.Scopes = []quotaspec.QuotaScope{}
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func unmarshalEntry(raw string) (Entry, error) {
	var payload redisEntry
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return Entry{}, err
	}
	return Entry{
		Footprint: normalizeFootprint(payload.Footprint),
		Scopes:    normalizeScopes(payload.Scopes),
	}, nil
}

func marshalLimits(limits map[quotaspec.QuotaDimension]map[quotaspec.QuotaScope]int64) (string, error) {
	if len(limits) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(limits)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func redisKeys(user string) []string {
	keys := make([]string, 0, len(redisQuotaDimensions)+1)
	keys = append(keys, liveKey(user))
	for _, dimension := range redisQuotaDimensions {
		keys = append(keys, sumKey(user, dimension))
	}
	return keys
}

func redisLuaDimensionTables(dimensions []quotaspec.QuotaDimension) string {
	dims := make([]string, 0, len(dimensions))
	indexes := make([]string, 0, len(dimensions))
	for i, dimension := range dimensions {
		quoted := fmt.Sprintf("%q", string(dimension))
		dims = append(dims, quoted)
		indexes = append(indexes, fmt.Sprintf("[%s] = %d", quoted, i+2))
	}
	return fmt.Sprintf("local dims = {%s}\nlocal keyIndex = {%s}", strings.Join(dims, ", "), strings.Join(indexes, ", "))
}

func redisScriptWithDimensions(script string) string {
	return strings.Replace(script, redisDimensionPlaceholder, redisLuaDimensions, 1)
}

func liveKey(user string) string {
	return "q:live:{" + user + "}"
}

func sumKey(user string, dimension quotaspec.QuotaDimension) string {
	return "q:sum:{" + user + "}:" + string(dimension)
}

func boolToLuaFlag(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
