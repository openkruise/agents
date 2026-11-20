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

package claim

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
)

func TestClassifyCreateError(t *testing.T) {
	gr := schema.GroupResource{Group: "agents.kruise.io", Resource: "sandboxes"}
	gk := schema.GroupKind{Group: "agents.kruise.io", Kind: "Sandbox"}

	tests := []struct {
		name           string
		err            error
		contextMsg     string
		expectCode     ErrorCode // empty when the error is returned as-is
		expectContains string
	}{
		{
			name:       "nil error returns nil",
			err:        nil,
			contextMsg: "create sandbox",
		},
		{
			name:           "interrupted wait returns original error unclassified",
			err:            wait.ErrWaitTimeout,
			contextMsg:     "create sandbox",
			expectCode:     "",
			expectContains: wait.ErrWaitTimeout.Error(),
		},
		{
			name:           "context canceled returns original error unclassified",
			err:            fmt.Errorf("context canceled before creating sandbox: %w", context.Canceled),
			contextMsg:     "create sandbox",
			expectCode:     "",
			expectContains: "context canceled before creating sandbox",
		},
		{
			name:           "server timeout is ambiguous Internal",
			err:            apierrors.NewServerTimeout(gr, "create", 1),
			contextMsg:     "create sandbox",
			expectCode:     CodeInternal,
			expectContains: "create sandbox",
		},
		{
			name:           "timeout is ambiguous Internal",
			err:            apierrors.NewTimeoutError("operation timed out", 1),
			contextMsg:     "create sandbox",
			expectCode:     CodeInternal,
			expectContains: "create sandbox",
		},
		{
			name:           "service unavailable is ambiguous Internal",
			err:            apierrors.NewServiceUnavailable("apiserver is down"),
			contextMsg:     "create sandbox",
			expectCode:     CodeInternal,
			expectContains: "create sandbox",
		},
		{
			name:           "too many requests is ambiguous Internal",
			err:            apierrors.NewTooManyRequests("rate limited", 1),
			contextMsg:     "create sandbox",
			expectCode:     CodeInternal,
			expectContains: "create sandbox",
		},
		{
			name:           "internal server error is ambiguous Internal",
			err:            apierrors.NewInternalError(fmt.Errorf("etcd boom")),
			contextMsg:     "create sandbox",
			expectCode:     CodeInternal,
			expectContains: "create sandbox",
		},
		{
			name:           "conflict is terminal Internal for create",
			err:            apierrors.NewConflict(gr, "test-sbx", fmt.Errorf("object was modified")),
			contextMsg:     "create sandbox",
			expectCode:     CodeInternal,
			expectContains: "create sandbox",
		},
		{
			name:           "already exists is terminal Conflict",
			err:            apierrors.NewAlreadyExists(gr, "test-sbx"),
			contextMsg:     "create sandbox",
			expectCode:     CodeConflict,
			expectContains: "create sandbox",
		},
		{
			name:           "invalid schema is terminal BadRequest",
			err:            apierrors.NewInvalid(gk, "test-sbx", nil),
			contextMsg:     "create sandbox",
			expectCode:     CodeBadRequest,
			expectContains: "create sandbox",
		},
		{
			name:           "bad request is terminal BadRequest",
			err:            apierrors.NewBadRequest("metadata.labels: Invalid value: unsupported label"),
			contextMsg:     "create sandbox",
			expectCode:     CodeBadRequest,
			expectContains: "create sandbox",
		},
		{
			name:           "wrapped bad request is terminal BadRequest",
			err:            fmt.Errorf("create request rejected: %w", apierrors.NewBadRequest("unsupported label")),
			contextMsg:     "create sandbox",
			expectCode:     CodeBadRequest,
			expectContains: "create sandbox",
		},
		{
			name:           "forbidden quota is platform Internal",
			err:            apierrors.NewForbidden(gr, "test-sbx", fmt.Errorf("exceeded quota: cpu-quota")),
			contextMsg:     "create sandbox",
			expectCode:     CodeInternal,
			expectContains: "platform configuration",
		},
		{
			name:           "forbidden RBAC is platform Internal",
			err:            apierrors.NewForbidden(gr, "test-sbx", fmt.Errorf("cannot create resource \"sandboxes\"")),
			contextMsg:     "create sandbox",
			expectCode:     CodeInternal,
			expectContains: "platform configuration",
		},
		{
			name:           "unauthorized is platform Internal",
			err:            apierrors.NewUnauthorized("missing token"),
			contextMsg:     "create sandbox",
			expectCode:     CodeInternal,
			expectContains: "platform configuration",
		},
		{
			name:           "not found is platform Internal",
			err:            apierrors.NewNotFound(gr, "test-sbx"),
			contextMsg:     "create sandbox",
			expectCode:     CodeInternal,
			expectContains: "platform configuration",
		},
		{
			name:           "method not supported is platform Internal",
			err:            apierrors.NewMethodNotSupported(gr, "create"),
			contextMsg:     "create sandbox",
			expectCode:     CodeInternal,
			expectContains: "platform configuration",
		},
		{
			name:           "non-status network error is ambiguous Internal",
			err:            fmt.Errorf("connection refused"),
			contextMsg:     "create sandbox",
			expectCode:     CodeInternal,
			expectContains: "create sandbox",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ClassifyCreateError(tt.err, tt.contextMsg)
			if tt.err == nil {
				assert.Nil(t, result)
				return
			}
			assert.Equal(t, tt.expectCode, GetErrCode(result), "classification mismatch for %v", result)
			if tt.expectContains != "" {
				assert.Contains(t, result.Error(), tt.expectContains)
			}
			assert.Contains(t, result.Error(), tt.err.Error())
		})
	}
}
