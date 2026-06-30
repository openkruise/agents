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

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"
)

type Entry struct {
	Footprint map[quotaspec.QuotaDimension]int64
	Scopes    []quotaspec.QuotaScope
}

// AcquireParams carries inputs used by Backend.Acquire.
type AcquireParams struct {
	// User identifies the quota owner.
	User string
	// LockString uniquely identifies one quota allocation for the user.
	LockString string
	// Footprint is the quota usage by dimension to reserve.
	Footprint map[quotaspec.QuotaDimension]int64
	// Scopes lists conditional quota scopes that also receive the footprint.
	Scopes []quotaspec.QuotaScope
	// Enforce controls whether the backend rejects requests exceeding limits.
	Enforce bool
	// Limits contains per-dimension, per-scope quota ceilings.
	Limits map[quotaspec.QuotaDimension]map[quotaspec.QuotaScope]int64
}

type Backend interface {
	// Acquire reserves or updates quota for one lock string.
	Acquire(ctx context.Context, p AcquireParams) error
	// Release removes quota reserved by one user lock string.
	Release(ctx context.Context, user, lockString string) error
	// ListEntries returns all quota entries for a user keyed by lock string.
	ListEntries(ctx context.Context, user string) (map[string]Entry, error)
	// Cleanup removes all quota state for a user.
	Cleanup(ctx context.Context, user string) error
}

// AcquireRequest carries inputs used by Manager.Acquire.
type AcquireRequest struct {
	// User identifies the quota owner.
	User string
	// LockString uniquely identifies one quota allocation for the user.
	LockString string
	// Quota describes the limits to enforce for this acquisition.
	Quota *quotaspec.QuotaSpec
	// Footprint is the quota usage by dimension to reserve.
	Footprint map[quotaspec.QuotaDimension]int64
	// Scopes lists conditional quota scopes that also receive the footprint.
	Scopes []quotaspec.QuotaScope
}

// ReleaseRequest carries inputs used by Manager.Release.
type ReleaseRequest struct {
	// User identifies the quota owner.
	User string
	// LockString identifies the quota allocation to release.
	LockString string
}

type LiveSandboxCache interface {
	// ListLiveSandboxesByOwner returns live sandboxes owned by owner.
	ListLiveSandboxesByOwner(ctx context.Context, owner string) ([]*agentsv1alpha1.Sandbox, error)
	// SandboxInformerHealthy reports whether the sandbox informer cache is usable.
	SandboxInformerHealthy() bool
}

type PrimaryChecker interface {
	// IsPrimary reports whether this instance currently owns primary duties.
	IsPrimary() bool
	// WaitPrimary blocks until this instance becomes primary or ctx is done.
	WaitPrimary(ctx context.Context) error
	// PrimaryChanged returns a channel closed or signaled when primary state changes.
	PrimaryChanged() <-chan struct{}
}
