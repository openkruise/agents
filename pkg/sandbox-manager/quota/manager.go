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
	"fmt"

	"k8s.io/klog/v2"
)

type Manager struct {
	backend Backend
}

func NewManager(backend Backend) *Manager {
	if backend == nil {
		backend = NoopBackend{}
	}
	return &Manager{backend: backend}
}

func (m *Manager) Acquire(ctx context.Context, req AcquireRequest) error {
	if req.Quota == nil || !req.Quota.IsLimited() {
		acquireTotal.WithLabelValues("unlimited").Inc()
		return nil
	}
	if req.User == "" || req.LockString == "" {
		acquireTotal.WithLabelValues("error").Inc()
		return fmt.Errorf("%w: user=%q lockString=%q", ErrMissingIdentity, req.User, req.LockString)
	}

	// Normalize before the backend call so every backend sees canonical inputs.
	err := m.backendOrNoop().Acquire(ctx, AcquireParams{
		User:       req.User,
		LockString: req.LockString,
		Footprint:  normalizeFootprint(req.Footprint),
		Scopes:     normalizeScopes(req.Scopes),
		Enforce:    true,
		Limits:     req.Quota.LimitedPairs(),
	})
	if err == nil {
		acquireTotal.WithLabelValues("allowed").Inc()
		return nil
	}
	if errors.Is(err, ErrQuotaExceeded) {
		acquireTotal.WithLabelValues("rejected").Inc()
		return ErrQuotaExceeded
	}

	backendErrorsTotal.WithLabelValues("acquire").Inc()
	acquireTotal.WithLabelValues("fail_open").Inc()
	klog.FromContext(ctx).Error(err, "quota acquire backend failed, fail open", "user", req.User)
	return nil
}

func (m *Manager) Release(ctx context.Context, req ReleaseRequest) error {
	if req.User == "" || req.LockString == "" {
		releaseTotal.WithLabelValues("skipped").Inc()
		return nil
	}
	if err := m.backendOrNoop().Release(ctx, req.User, req.LockString); err != nil {
		backendErrorsTotal.WithLabelValues("release").Inc()
		releaseTotal.WithLabelValues("error").Inc()
		klog.FromContext(ctx).Error(err, "quota release backend failed", "user", req.User)
		return err
	}
	releaseTotal.WithLabelValues("released").Inc()
	return nil
}

func (m *Manager) Cleanup(ctx context.Context, user string) error {
	if user == "" {
		return nil
	}
	if err := m.backendOrNoop().Cleanup(ctx, user); err != nil {
		backendErrorsTotal.WithLabelValues("cleanup").Inc()
		klog.FromContext(ctx).Error(err, "quota cleanup backend failed", "user", user)
		return err
	}
	return nil
}

func (m *Manager) backendOrNoop() Backend {
	if m == nil || m.backend == nil {
		return NoopBackend{}
	}
	return m.backend
}
