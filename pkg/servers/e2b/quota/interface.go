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
	"time"

	cachepkg "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

type Config struct {
	RedisAddr         string
	RedisUsername     string
	RedisPassword     string
	RedisDB           int
	OperationTimeout  time.Duration
	AntiDriftInterval time.Duration
	AntiDriftGrace    time.Duration
}

type AcquireRequest struct {
	APIKeyID   string
	LockString string
	Quota      *models.QuotaSpec
}

type ReleaseRequest struct {
	APIKeyID   string
	LockString string
}

type Backend interface {
	Acquire(ctx context.Context, apiKeyID, lockString string, limit int64) error
	Release(ctx context.Context, apiKeyID, lockString string) error
	AddObserved(ctx context.Context, apiKeyID, lockString string, acquiredAt time.Time) error
	List(ctx context.Context, apiKeyID string) (map[string]time.Time, error)
	DeleteSubject(ctx context.Context, apiKeyID string) error
}

type LimitedKeyStore interface {
	ListLimited(ctx context.Context) ([]*models.CreatedTeamAPIKey, error)
}

type LiveLockstringCache interface {
	ListLiveLockstringsByOwner(ctx context.Context, opts cachepkg.ListLiveLockstringsByOwnerOptions) ([]cachepkg.LiveLockstring, error)
	RemoveSafe() bool
}

type PrimaryChecker interface {
	IsPrimary() bool
}
