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
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	defaultAcquireTimeout              = 50 * time.Millisecond
	defaultMaintenanceOperationTimeout = 30 * time.Second
	redisDimensionPlaceholder          = "-- QUOTA_DIMENSIONS --"
)

var (
	redisQuotaDimensions = []QuotaDimension{DimSandboxCount, DimLimitsCPU, DimLimitsMemory}
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
	client  *redis.Client
	timeout time.Duration
}

type redisEntry struct {
	Footprint map[QuotaDimension]int64 `json:"d"`
	Scopes    []QuotaScope             `json:"s"`
}

func NewRedisBackend(client *redis.Client, timeout time.Duration) *RedisBackend {
	if timeout <= 0 {
		timeout = defaultAcquireTimeout
	}
	return &RedisBackend{client: client, timeout: timeout}
}

func (b *RedisBackend) Acquire(ctx context.Context, p AcquireParams) error {
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

	acquireCtx, cancel := context.WithTimeout(ctx, b.acquireTimeout())
	defer cancel()

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
	client, err := b.redisClient("release")
	if err != nil {
		return err
	}

	releaseCtx, cancel := withOperationTimeout(ctx, defaultMaintenanceOperationTimeout)
	defer cancel()

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
	client, err := b.redisClient("list_entries")
	if err != nil {
		return nil, err
	}

	opCtx, cancel := withOperationTimeout(ctx, defaultMaintenanceOperationTimeout)
	defer cancel()

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
	client, err := b.redisClient("cleanup")
	if err != nil {
		return err
	}

	opCtx, cancel := withOperationTimeout(ctx, defaultMaintenanceOperationTimeout)
	defer cancel()

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
		payload.Footprint = map[QuotaDimension]int64{}
	}
	if payload.Scopes == nil {
		payload.Scopes = []QuotaScope{}
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

func marshalLimits(limits map[QuotaDimension]map[QuotaScope]int64) (string, error) {
	if len(limits) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(limits)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func normalizeFootprint(in map[QuotaDimension]int64) map[QuotaDimension]int64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[QuotaDimension]int64, len(in))
	for dim, amount := range in {
		if dim != DimLimitsCPU && dim != DimLimitsMemory {
			continue
		}
		if amount == 0 {
			continue
		}
		out[dim] = amount
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeScopes(in []QuotaScope) []QuotaScope {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[QuotaScope]struct{}, len(in))
	out := make([]QuotaScope, 0, len(in))
	for _, scope := range in {
		if scope == ScopeAll {
			continue
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		out = append(out, scope)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i] < out[j]
	})
	return out
}

func redisKeys(user string) []string {
	keys := make([]string, 0, len(redisQuotaDimensions)+1)
	keys = append(keys, liveKey(user))
	for _, dimension := range redisQuotaDimensions {
		keys = append(keys, sumKey(user, dimension))
	}
	return keys
}

func redisLuaDimensionTables(dimensions []QuotaDimension) string {
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

func sumKey(user string, dimension QuotaDimension) string {
	return "q:sum:{" + user + "}:" + string(dimension)
}

func boolToLuaFlag(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func withOperationTimeout(ctx context.Context, defaultTimeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultTimeout)
}
