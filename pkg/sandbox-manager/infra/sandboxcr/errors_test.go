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

package sandboxcr

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"

	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
)

func TestClassifyCreateError(t *testing.T) {
	gr := schema.GroupResource{Group: "agents.kruise.io", Resource: "sandboxes"}
	gk := schema.GroupKind{Group: "agents.kruise.io", Kind: "Sandbox"}

	tests := []struct {
		name            string
		err             error
		expectErrorCode managererrors.ErrorCode
		expectContains  string
	}{
		{
			name: "nil error returns nil",
		},
		{
			name:            "interrupted wait returns original error",
			err:             wait.ErrWaitTimeout,
			expectErrorCode: managererrors.ErrorUnknown,
			expectContains:  wait.ErrWaitTimeout.Error(),
		},
		{
			name:            "Forbidden is terminal Internal",
			err:             apierrors.NewForbidden(gr, "test-sbx", fmt.Errorf("request denied")),
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "platform configuration",
		},
		{
			name:            "Invalid is terminal BadRequest",
			err:             apierrors.NewInvalid(gk, "test-sbx", nil),
			expectErrorCode: managererrors.ErrorBadRequest,
			expectContains:  "create sandbox",
		},
		{
			name:            "BadRequest is terminal BadRequest",
			err:             apierrors.NewBadRequest("metadata.labels: Invalid value: unsupported label"),
			expectErrorCode: managererrors.ErrorBadRequest,
			expectContains:  "create sandbox",
		},
		{
			name:            "wrapped BadRequest is terminal BadRequest",
			err:             fmt.Errorf("create request rejected: %w", apierrors.NewBadRequest("metadata.labels: Invalid value: unsupported label")),
			expectErrorCode: managererrors.ErrorBadRequest,
			expectContains:  "create sandbox",
		},
		{
			name:            "Server timeout is terminal Internal (ambiguous create)",
			err:             apierrors.NewServerTimeout(gr, "create", 1),
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "create sandbox",
		},
		{
			name:            "Service unavailable is terminal Internal (ambiguous create)",
			err:             apierrors.NewServiceUnavailable("apiserver is down"),
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "create sandbox",
		},
		{
			name:            "Too many requests is terminal Internal (ambiguous create)",
			err:             apierrors.NewTooManyRequests("rate limited", 1),
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "create sandbox",
		},
		{
			name:            "Internal server error is terminal Internal (ambiguous create)",
			err:             apierrors.NewInternalError(fmt.Errorf("etcd boom")),
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "create sandbox",
		},
		{
			name:            "Conflict is terminal Internal for Create",
			err:             apierrors.NewConflict(gr, "test-sbx", fmt.Errorf("object was modified")),
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "create sandbox",
		},
		{
			name:            "AlreadyExists is terminal Conflict",
			err:             apierrors.NewAlreadyExists(gr, "test-sbx"),
			expectErrorCode: managererrors.ErrorConflict,
			expectContains:  "create sandbox",
		},
		{
			name:            "Unauthorized is terminal Internal",
			err:             apierrors.NewUnauthorized("missing token"),
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "platform configuration",
		},
		{
			name:            "NotFound is terminal Internal",
			err:             apierrors.NewNotFound(gr, "test-sbx"),
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "platform configuration",
		},
		{
			name:            "MethodNotSupported is terminal Internal",
			err:             apierrors.NewMethodNotSupported(gr, "create"),
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "platform configuration",
		},
		{
			name:            "non-StatusError network error is terminal Internal",
			err:             fmt.Errorf("connection refused"),
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "create sandbox",
		},
		{
			name:            "context canceled before creating sandbox returns original error",
			err:             fmt.Errorf("context canceled before creating sandbox: %w", context.Canceled),
			expectErrorCode: managererrors.ErrorUnknown,
			expectContains:  "context canceled before creating sandbox",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := classifyCreateError(tt.err, "create sandbox")
			if tt.err == nil {
				assert.Nil(t, result)
				return
			}
			assert.Equal(t, tt.expectErrorCode, managererrors.GetErrCode(result), "expected ErrorCode mismatch for %v", result)
			if tt.expectContains != "" {
				assert.Contains(t, result.Error(), tt.expectContains)
			}
			assert.Contains(t, result.Error(), tt.err.Error())
		})
	}
}
