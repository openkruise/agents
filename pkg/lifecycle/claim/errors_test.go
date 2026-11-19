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
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
)

const createContextMsg = "create sandbox"

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
			contextMsg: createContextMsg,
		},
		{
			name:           "interrupted wait returns original error unclassified",
			err:            wait.ErrWaitTimeout,
			contextMsg:     createContextMsg,
			expectCode:     "",
			expectContains: wait.ErrWaitTimeout.Error(),
		},
		{
			name:           "context canceled returns original error unclassified",
			err:            fmt.Errorf("context canceled before creating sandbox: %w", context.Canceled),
			contextMsg:     createContextMsg,
			expectCode:     "",
			expectContains: "context canceled before creating sandbox",
		},
		{
			name:           "server timeout is ambiguous Internal",
			err:            apierrors.NewServerTimeout(gr, "create", 1),
			contextMsg:     createContextMsg,
			expectCode:     CodeInternal,
			expectContains: "create sandbox",
		},
		{
			name:           "timeout is ambiguous Internal",
			err:            apierrors.NewTimeoutError("operation timed out", 1),
			contextMsg:     createContextMsg,
			expectCode:     CodeInternal,
			expectContains: "create sandbox",
		},
		{
			name:           "service unavailable is ambiguous Internal",
			err:            apierrors.NewServiceUnavailable("apiserver is down"),
			contextMsg:     createContextMsg,
			expectCode:     CodeInternal,
			expectContains: "create sandbox",
		},
		{
			name:           "too many requests is ambiguous Internal",
			err:            apierrors.NewTooManyRequests("rate limited", 1),
			contextMsg:     createContextMsg,
			expectCode:     CodeInternal,
			expectContains: "create sandbox",
		},
		{
			name:           "internal server error is ambiguous Internal",
			err:            apierrors.NewInternalError(fmt.Errorf("etcd boom")),
			contextMsg:     createContextMsg,
			expectCode:     CodeInternal,
			expectContains: "create sandbox",
		},
		{
			name:           "conflict is terminal Internal for create",
			err:            apierrors.NewConflict(gr, "test-sbx", fmt.Errorf("object was modified")),
			contextMsg:     createContextMsg,
			expectCode:     CodeInternal,
			expectContains: "create sandbox",
		},
		{
			name:           "already exists is terminal Conflict",
			err:            apierrors.NewAlreadyExists(gr, "test-sbx"),
			contextMsg:     createContextMsg,
			expectCode:     CodeConflict,
			expectContains: "create sandbox",
		},
		{
			name:           "invalid schema is terminal BadRequest",
			err:            apierrors.NewInvalid(gk, "test-sbx", nil),
			contextMsg:     createContextMsg,
			expectCode:     CodeBadRequest,
			expectContains: "create sandbox",
		},
		{
			name:           "bad request is terminal BadRequest",
			err:            apierrors.NewBadRequest("metadata.labels: Invalid value: unsupported label"),
			contextMsg:     createContextMsg,
			expectCode:     CodeBadRequest,
			expectContains: "create sandbox",
		},
		{
			name:           "wrapped bad request is terminal BadRequest",
			err:            fmt.Errorf("create request rejected: %w", apierrors.NewBadRequest("unsupported label")),
			contextMsg:     createContextMsg,
			expectCode:     CodeBadRequest,
			expectContains: "create sandbox",
		},
		{
			name:           "forbidden quota is platform Internal",
			err:            apierrors.NewForbidden(gr, "test-sbx", fmt.Errorf("exceeded quota: cpu-quota")),
			contextMsg:     createContextMsg,
			expectCode:     CodeInternal,
			expectContains: "platform configuration",
		},
		{
			name:           "forbidden RBAC is platform Internal",
			err:            apierrors.NewForbidden(gr, "test-sbx", fmt.Errorf("cannot create resource \"sandboxes\"")),
			contextMsg:     createContextMsg,
			expectCode:     CodeInternal,
			expectContains: "platform configuration",
		},
		{
			name:           "unauthorized is platform Internal",
			err:            apierrors.NewUnauthorized("missing token"),
			contextMsg:     createContextMsg,
			expectCode:     CodeInternal,
			expectContains: "platform configuration",
		},
		{
			name:           "not found is platform Internal",
			err:            apierrors.NewNotFound(gr, "test-sbx"),
			contextMsg:     createContextMsg,
			expectCode:     CodeInternal,
			expectContains: "platform configuration",
		},
		{
			name:           "method not supported is platform Internal",
			err:            apierrors.NewMethodNotSupported(gr, "create"),
			contextMsg:     createContextMsg,
			expectCode:     CodeInternal,
			expectContains: "platform configuration",
		},
		{
			name:           "non-status network error is ambiguous Internal",
			err:            fmt.Errorf("connection refused"),
			contextMsg:     createContextMsg,
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
			if tt.expectCode == "" {
				// Unclassified errors must be returned as-is so callers can
				// still match the original sentinel with errors.Is.
				assert.ErrorIs(t, result, tt.err)
			}
			if tt.expectContains != "" {
				assert.Contains(t, result.Error(), tt.expectContains)
			}
			assert.Contains(t, result.Error(), tt.err.Error())
		})
	}
}

func TestGetErrCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want ErrorCode
	}{
		{name: "nil error has no classification", err: nil, want: ""},
		{name: "plain error has no classification", err: fmt.Errorf("boom"), want: ""},
		{name: "classified error keeps its code", err: NewError(CodeBadRequest, "bad input"), want: CodeBadRequest},
		{
			name: "wrapped classified error keeps its code",
			err:  fmt.Errorf("outer: %w", WrapError(CodeConflict, fmt.Errorf("cause"), "duplicate")),
			want: CodeConflict,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, GetErrCode(tt.err))
		})
	}
}

// TestErrorMarkers pins the retry-boundary semantics shared by the claim and
// clone paths: retriable markers match by type and equal message, terminal
// markers keep their cause reachable through Unwrap.
func TestErrorMarkers(t *testing.T) {
	cause := fmt.Errorf("callback boom")
	classified := NewError(CodeBadRequest, "memory downscale")

	t.Run("RetriableError", func(t *testing.T) {
		err := RetriableError{Message: "no stock"}
		var target RetriableError
		assert.True(t, errors.As(err, &target))
		assert.True(t, errors.Is(err, RetriableError{Message: "no stock"}), "Is must match equal-message values")
		assert.False(t, errors.Is(err, RetriableError{Message: "other"}))
		wrapped := fmt.Errorf("attempt: %w", err)
		assert.True(t, errors.As(wrapped, &target), "type must survive wrapping")
		assert.True(t, errors.Is(wrapped, RetriableError{Message: "no stock"}))
	})

	t.Run("NoAvailableError is retriable", func(t *testing.T) {
		err := NoAvailableError("tpl", "no stock")
		var target RetriableError
		assert.True(t, errors.As(err, &target))
		assert.Contains(t, err.Error(), "no available sandboxes for template tpl (no stock)")
	})

	t.Run("TerminalMutationError", func(t *testing.T) {
		err := TerminalMutationError{Stage: "modifier", Err: cause}
		assert.Equal(t, "modifier callback failed: callback boom", err.Error())
		assert.ErrorIs(t, err, cause, "Unwrap must keep the callback cause")
		var target TerminalMutationError
		assert.True(t, errors.As(fmt.Errorf("outer: %w", err), &target))
	})

	t.Run("TerminalValidationError", func(t *testing.T) {
		err := TerminalValidationError{Err: classified}
		assert.Equal(t, "sandbox claim validation failed: BadRequest: memory downscale", err.Error())
		assert.ErrorIs(t, err, classified)
		assert.Equal(t, CodeBadRequest, GetErrCode(err), "classified code must survive the marker")
	})
}
