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
	unified SubjectBackend
}

func NewManager(backend Backend) *Manager {
	if backend == nil {
		backend = NoopBackend{}
	}
	return &Manager{backend: backend}
}

func (m *Manager) SetUnifiedBackend(backend SubjectBackend) {
	if m != nil {
		m.unified = backend
	}
}

func (m *Manager) Acquire(ctx context.Context, req AcquireRequest) error {
	if (req.Quota == nil || !req.Quota.IsLimited()) && !req.Subject.Enabled {
		acquireTotal.WithLabelValues("unlimited").Inc()
		return nil
	}
	if req.User == "" || req.LockString == "" {
		acquireTotal.WithLabelValues("error").Inc()
		return fmt.Errorf("%w: user=%q lockString=%q", ErrMissingIdentity, req.User, req.LockString)
	}

	legacyHeld := false
	if req.Quota != nil && req.Quota.IsLimited() {
		err := m.backendOrNoop().Acquire(ctx, AcquireParams{
			User:       req.User,
			LockString: req.LockString,
			Footprint:  normalizeFootprint(req.Footprint),
			Scopes:     normalizeScopes(req.Scopes),
			Enforce:    true,
			Limits:     req.Quota.LimitedPairs(),
			Subject:    req.Subject,
		})
		if err == nil {
			legacyHeld = true
			acquireTotal.WithLabelValues("allowed").Inc()
		} else if errors.Is(err, ErrQuotaExceeded) {
			acquireTotal.WithLabelValues("rejected").Inc()
			return ErrQuotaExceeded
		} else {
			backendErrorsTotal.WithLabelValues("acquire").Inc()
			acquireTotal.WithLabelValues("fail_open").Inc()
			klog.FromContext(ctx).Error(err, "quota acquire backend failed, fail open", "user", req.User)
		}
	}
	if req.Subject.Enabled && m.unified != nil {
		if err := m.unified.AcquireSubject(ctx, req.Subject, req.LockString); err != nil {
			if errors.Is(err, ErrQuotaExceeded) {
				if legacyHeld {
					_ = m.backendOrNoop().Release(ctx, req.User, req.LockString)
				}
				acquireTotal.WithLabelValues("rejected").Inc()
				return ErrQuotaExceeded
			}
			backendErrorsTotal.WithLabelValues("unified_acquire").Inc()
			acquireTotal.WithLabelValues("fail_open").Inc()
			klog.FromContext(ctx).Error(err, "unified quota backend failed, fail open", "owner", req.Subject.OwnerID)
		}
	}
	return nil
}

func (m *Manager) Release(ctx context.Context, req ReleaseRequest) error {
	if req.User == "" || req.LockString == "" {
		releaseTotal.WithLabelValues("skipped").Inc()
		return nil
	}
	var firstErr error
	if err := m.backendOrNoop().Release(ctx, req.User, req.LockString); err != nil {
		backendErrorsTotal.WithLabelValues("release").Inc()
		releaseTotal.WithLabelValues("error").Inc()
		klog.FromContext(ctx).Error(err, "quota release backend failed", "user", req.User)
		firstErr = err
	}
	if req.Subject.Enabled && m.unified != nil {
		if err := m.unified.ReleaseSubject(ctx, req.Subject, req.LockString); err != nil {
			backendErrorsTotal.WithLabelValues("unified_release").Inc()
			releaseTotal.WithLabelValues("error").Inc()
			klog.FromContext(ctx).Error(err, "failed to release unified quota", "owner", req.Subject.OwnerID)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		return firstErr
	}
	releaseTotal.WithLabelValues("released").Inc()
	return nil
}

func (m *Manager) Unknown(ctx context.Context, req UnknownRequest) error {
	if m == nil || m.unified == nil || !req.Subject.Enabled || req.LockString == "" {
		return nil
	}
	if err := m.unified.UnknownSubject(ctx, req.Subject, req.LockString); err != nil {
		backendErrorsTotal.WithLabelValues("unified_unknown").Inc()
		return err
	}
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
