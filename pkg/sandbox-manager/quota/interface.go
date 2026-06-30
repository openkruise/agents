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

type AcquireParams struct {
	User       string
	LockString string
	Footprint  map[quotaspec.QuotaDimension]int64
	Scopes     []quotaspec.QuotaScope
	Enforce    bool
	Limits     map[quotaspec.QuotaDimension]map[quotaspec.QuotaScope]int64
}

type Backend interface {
	Acquire(ctx context.Context, p AcquireParams) error
	Release(ctx context.Context, user, lockString string) error
	ListEntries(ctx context.Context, user string) (map[string]Entry, error)
	Cleanup(ctx context.Context, user string) error
}

type AcquireRequest struct {
	User       string
	LockString string
	Quota      *quotaspec.QuotaSpec
	Footprint  map[quotaspec.QuotaDimension]int64
	Scopes     []quotaspec.QuotaScope
}

type ReleaseRequest struct {
	User       string
	LockString string
}

type LiveSandboxCache interface {
	ListLiveSandboxesByOwner(ctx context.Context, owner string) ([]*agentsv1alpha1.Sandbox, error)
	SandboxInformerHealthy() bool
}

type PrimaryChecker interface {
	IsPrimary() bool
	WaitPrimary(ctx context.Context) error
	PrimaryChanged() <-chan struct{}
}
