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
	limit, limited := req.Quota.SandboxCountLimit()
	if !limited {
		acquireTotal.WithLabelValues("unlimited").Inc()
		return nil
	}

	if req.APIKeyID == "" || req.LockString == "" {
		acquireTotal.WithLabelValues("error").Inc()
		return fmt.Errorf("%w: apiKeyID=%q lockString=%q", ErrMissingIdentity, req.APIKeyID, req.LockString)
	}

	if err := m.backendOrNoop().Acquire(ctx, req.APIKeyID, req.LockString, limit); err != nil {
		if errors.Is(err, ErrQuotaExceeded) {
			acquireTotal.WithLabelValues("rejected").Inc()
			return ErrQuotaExceeded
		}

		backendErrorsTotal.WithLabelValues("acquire").Inc()
		acquireTotal.WithLabelValues("fail_open").Inc()
		klog.FromContext(ctx).Error(err, "quota acquire backend failed, fail open", "apiKeyID", req.APIKeyID)
		return nil
	}

	acquireTotal.WithLabelValues("allowed").Inc()
	return nil
}

func (m *Manager) Release(ctx context.Context, req ReleaseRequest) error {
	if req.APIKeyID == "" || req.LockString == "" {
		return nil
	}

	if err := m.backendOrNoop().Release(ctx, req.APIKeyID, req.LockString); err != nil {
		backendErrorsTotal.WithLabelValues("release").Inc()
		klog.FromContext(ctx).Error(err, "quota release backend failed", "apiKeyID", req.APIKeyID)
		return err
	}

	return nil
}

func (m *Manager) DeleteSubject(ctx context.Context, apiKeyID string) error {
	if apiKeyID == "" {
		return nil
	}

	if err := m.backendOrNoop().DeleteSubject(ctx, apiKeyID); err != nil {
		backendErrorsTotal.WithLabelValues("delete_subject").Inc()
		klog.FromContext(ctx).Error(err, "quota delete-subject backend failed", "apiKeyID", apiKeyID)
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
