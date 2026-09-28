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
	"errors"
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
		contextMsg      string
		expectRetryable bool
		expectErrorCode managererrors.ErrorCode
		expectContains  string
	}{
		{
			name:       "nil error returns nil",
			contextMsg: "create sandbox",
		},
		{
			name:            "interrupted wait returns original error",
			err:             wait.ErrWaitTimeout,
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorUnknown,
			expectContains:  wait.ErrWaitTimeout.Error(),
		},
		{
			name:            "quota exceeded forbidden is terminal Internal",
			err:             apierrors.NewForbidden(gr, "test-sbx", fmt.Errorf("exceeded quota: cpu-quota, requested: cpu=2, used: cpu=10, limited: cpu=10")),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "platform configuration",
		},
		{
			name:            "limitrange forbidden is terminal Internal",
			err:             apierrors.NewForbidden(gr, "test-sbx", fmt.Errorf("maximum cpu usage per Container is 1; limitrange max-resources")),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "platform configuration",
		},
		{
			name:            "RBAC forbidden is terminal Internal",
			err:             apierrors.NewForbidden(gr, "test-sbx", fmt.Errorf("User \"system:serviceaccount:default:manager\" cannot create resource \"sandboxes\" in API group \"agents.kruise.io\"")),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "platform configuration",
		},
		{
			name:            "PodSecurity forbidden is terminal Internal",
			err:             apierrors.NewForbidden(gr, "test-sbx", fmt.Errorf("violates PodSecurity \"restricted:latest\": allowPrivilegeEscalation != false")),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "platform configuration",
		},
		{
			name:            "validating admission policy forbidden is terminal Internal",
			err:             apierrors.NewForbidden(gr, "test-sbx", fmt.Errorf("ValidatingAdmissionPolicy 'sandbox-policy' denied the request")),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "platform configuration",
		},
		{
			name:            "validating webhook forbidden is terminal Internal",
			err:             apierrors.NewForbidden(gr, "test-sbx", fmt.Errorf("admission webhook \"sandbox.example.com\" denied the request: image is not allowed")),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "platform configuration",
		},
		{
			name:            "generic forbidden is terminal Internal",
			err:             apierrors.NewForbidden(gr, "test-sbx", fmt.Errorf("some random policy reason")),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "platform configuration",
		},
		{
			name:            "Invalid is terminal BadRequest",
			err:             apierrors.NewInvalid(gk, "test-sbx", nil),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorBadRequest,
			expectContains:  "create sandbox",
		},
		{
			name:            "BadRequest is terminal BadRequest",
			err:             apierrors.NewBadRequest("metadata.labels: Invalid value: unsupported label"),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorBadRequest,
			expectContains:  "create sandbox",
		},
		{
			name:            "wrapped BadRequest is terminal BadRequest",
			err:             fmt.Errorf("create request rejected: %w", apierrors.NewBadRequest("metadata.labels: Invalid value: unsupported label")),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorBadRequest,
			expectContains:  "create sandbox",
		},
		{
			name:            "Server timeout is terminal Unavailable",
			err:             apierrors.NewServerTimeout(gr, "create", 1),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorUnavailable,
			expectContains:  "create sandbox",
		},
		{
			name:            "Service unavailable is terminal Unavailable",
			err:             apierrors.NewServiceUnavailable("apiserver is down"),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorUnavailable,
			expectContains:  "create sandbox",
		},
		{
			name:            "Too many requests is terminal Unavailable",
			err:             apierrors.NewTooManyRequests("rate limited", 1),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorUnavailable,
			expectContains:  "create sandbox",
		},
		{
			name:            "Internal server error is terminal Internal",
			err:             apierrors.NewInternalError(fmt.Errorf("etcd boom")),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "create sandbox",
		},
		{
			name:            "Conflict is terminal Internal for Create",
			err:             apierrors.NewConflict(gr, "test-sbx", fmt.Errorf("object was modified")),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "create sandbox",
		},
		{
			name:            "AlreadyExists is terminal Conflict",
			err:             apierrors.NewAlreadyExists(gr, "test-sbx"),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorConflict,
			expectContains:  "create sandbox",
		},
		{
			name:            "Unauthorized is terminal Internal",
			err:             apierrors.NewUnauthorized("missing token"),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "platform configuration",
		},
		{
			name:            "NotFound is terminal Internal",
			err:             apierrors.NewNotFound(gr, "test-sbx"),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "platform configuration",
		},
		{
			name:            "MethodNotSupported is terminal Internal",
			err:             apierrors.NewMethodNotSupported(gr, "create"),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "platform configuration",
		},
		{
			name:            "non-StatusError network error is terminal Internal",
			err:             fmt.Errorf("connection refused"),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorInternal,
			expectContains:  "create sandbox",
		},
		{
			name:            "context canceled before creating sandbox returns original error",
			err:             fmt.Errorf("context canceled before creating sandbox: %w", context.Canceled),
			contextMsg:      "create sandbox",
			expectErrorCode: managererrors.ErrorUnknown,
			expectContains:  "context canceled before creating sandbox",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := classifyCreateError(tt.err, tt.contextMsg)
			if tt.err == nil {
				assert.Nil(t, result)
				return
			}
			if tt.expectRetryable {
				assert.True(t, errors.As(result, &retriableError{}), "expected retriableError, got %T: %v", result, result)
			} else {
				assert.Equal(t, tt.expectErrorCode, managererrors.GetErrCode(result), "expected ErrorCode mismatch for %v", result)
			}
			if tt.expectContains != "" {
				assert.Contains(t, result.Error(), tt.expectContains)
			}
			if tt.expectErrorCode == managererrors.ErrorUnavailable {
				assert.ErrorIs(t, result, tt.err)
			}
			assert.Contains(t, result.Error(), tt.err.Error())
		})
	}
}
